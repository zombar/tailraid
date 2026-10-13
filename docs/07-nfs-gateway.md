# 07 — NFS gateway

The NFS gateway is how a macOS or Linux device mounts the volume as a local filesystem.
Every node that runs `tailraid serve` with `listen.nfs` set exposes an NFSv3 server on
loopback backed by the shared `vfs` (`docs/04`). The OS's own NFS client mounts it; nothing
is installed on the OS. Windows mounts over SMB instead (docs/15).

Library: `github.com/willscott/go-nfs` v0.0.4 (pure Go, NFSv3 + MOUNT on one TCP port,
no portmapper, no NLM). Adapter: `internal/nfsgw` implements `nfs.Handler`;
`internal/vfs` implements `billy.Filesystem`, `billy.File` and `billy.Change`.

The tree carries its own copy of the library in `third_party/nfs` (a `replace` in every
module file, vendored from there, as `third_party/smb` is), with the changes this gateway
needs, each with a test in the copy:

- `conn.go` reads each request frame whole from the wire (at most `MaxFrame`, 1 MiB plus
  8 KiB of headers; a larger frame closes the connection) and dispatches it on its own
  goroutine, at most `InFlight` (16) per connection, every reply going through the
  connection's single writer in completion order. NFSv3 over TCP matches replies to
  requests by XID (RFC 1813 §1.6 and RFC 5531), so a server may answer in any order; every
  kernel client pipelines its requests on one connection and never has two WRITEs for one
  page in flight, so two overlapping WRITEs applying in either order is the same as on any
  multi-threaded NFS server. Upstream handled one request at a time per connection, which
  made every cold READ and every cap wait a stall of the whole mount.
- `nfs_onfsinfo.go` advertises `rtmax`/`wtmax`/`rtpref`/`wtpref` of 1 MiB instead of
  1 GiB, so the frame bound is the protocol's own: a client that honours FSINFO never sends
  a WRITE larger than `MaxFrame`.

## What go-nfs actually does (verified against v0.0.4 source)

These facts drive the vfs design and are the reason the previous attempt corrupted data.

| NFS procedure | go-nfs calls on the billy filesystem |
|---|---|
| WRITE (every RPC, ≤ wsize, typically 64 KiB–1 MiB) | `Stat(path)` → `OpenFile(path, O_RDWR, 0)` → `file.Seek(offset, 0)` → `file.Write(data)` → `file.Close()`; replies **FILE_SYNC** unconditionally; a write past `MaxFileSize` (I-64) fails with `EFBIG` → `NFS3ERR_FBIG` |
| COMMIT | no-op; never touches billy |
| READ | `Open(path)` → `file.ReadAt(buf, offset)` → `file.Close()` |
| SETATTR size | `OpenFile(path, O_WRONLY\|O_EXCL, 0)` → `file.Truncate(size)` → `file.Close()`; a size past `MaxFileSize` (I-64) fails with `EFBIG`, answered `NFS3ERR_FBIG` (`ENOSPC` `NFS3ERR_NOSPC`) as an NFS status, where go-nfs had passed the error up as an RPC failure (I-238) |
| SETATTR mode/uid/gid/times | `Handler.Change(fs)` → `Chmod`/`Lchown`/`Chtimes`; if `Change` returns nil every such call fails with NFS3ERR_NOTSUPP |
| CREATE | `UNCHECKED`: `fs.Create(path)` → `Close()` (then SETATTR for mode); on an existing file that is the truncate a rewrite starts with (04 §6, I-223). `GUARDED`: `fs.OpenFile(path, O_RDWR\|O_CREATE\|O_EXCL)`, `EEXIST` → `NFS3ERR_EXIST` whether this gateway or the leader saw the name first (I-222). `EXCLUSIVE` (a verifier, RFC 1813 §3.3.8, I-231): the same exclusive create, the verifier kept in the new file's times; a name that holds a file is `NFS3ERR_EXIST` unless its mtime still carries the request's verifier (a retransmission), which answers the file. Until 2026-10-12 it answered `NFS3ERR_NOTSUPP`, on which Linux retried `GUARDED` but macOS retried `UNCHECKED`, truncating a file another device had made |
| MKDIR / RMDIR / REMOVE / RENAME | `MkdirExcl` (exclusive across gateways, as CREATE `GUARDED`; a name holding a file or a folder is `NFS3ERR_EXIST`), `Remove`, `Remove`, `Rename`; a failure answers the RFC 1813 status of its reason (I-238): a folder with entries `NFS3ERR_NOTEMPTY` for RMDIR and `NFS3ERR_EXIST` as a RENAME target (§3.3.14), a folder moved under itself `NFS3ERR_INVAL`, a name too long `NFS3ERR_NAMETOOLONG`, where all but "not found" and "permission" had been `NFS3ERR_IO`; a rename onto its own name changes nothing and keeps an ephemeral file's unflushed bytes |
| LOOKUP / GETATTR / READDIR(PLUS) | `Lstat`, `ReadDir`; fileid from `FileInfo.Sys()` when it returns `*file.FileInfo{Fileid, Nlink, UID, GID}` (go-nfs's own type), otherwise an FNV hash of the path |
| LINK | optional `UnixChange.Link`; not implemented → NFS3ERR_NOTSUPP |
| SYMLINK / READLINK | `Symlink`, `Readlink` (billy `Symlink` capability) |
| any procedure | dispatched concurrently with the connection's other requests, at most `InFlight` at once (the in-tree copy, I-167); a request that blocks (a cold READ, a WRITE waiting at a cap) delays no other |

Consequences:

- A file's `Write` must honour the current position and leave every other byte intact.
  Open-to-close buffering does not exist: each WRITE is its own open/close cycle.
- Because the reply is FILE_SYNC, `Close()` is the durability point. `vfs.File.Close` returns
  only after the WAL record is fsynced (I-7, `docs/04`).
- `ReadAt` must be O(len) at any offset, never a stream from byte 0.
- `Change` must be implemented or every `chmod`, `touch`, `cp -p` fails.
- `Sys()` must return go-nfs's `FileInfo` so fileids are inode numbers, not path hashes
  (otherwise a rename changes the fileid and clients see a different file).

**Error mapping.** go-nfs maps a filesystem error to an NFS3 status per operation and has
no `ROFS` mapping, so the gateway's `EROFS` (stale, docs/02) reaches the wire as
`NFS3ERR_ACCES` from CREATE, MKDIR and SYMLINK and as `NFS3ERR_IO` from REMOVE, RMDIR and
RENAME; WRITE errors other than `ENOSPC`/`EDQUOT`/`EFBIG` are `NFS3ERR_IO`. A change hook's
error (SETATTR mode/owner/times) would be passed through unmapped as an RPC `GARBAGE_ARGS`,
so the handler's `Change` adapter maps it itself: `ENOENT` → `NFS3ERR_NOENT`, `EROFS` →
`NFS3ERR_ACCES`, anything else → `NFS3ERR_IO`. `TestNFS_ReadOnly_WireStatuses` pins all of
this.

## Handler

```go
type Handler struct { fs *vfs.FS; st *meta.Store; local *chunk.Store }

func (h *Handler) Mount(ctx, conn net.Conn, req nfs.MountRequest) (nfs.MountStatus, billy.Filesystem, []nfs.AuthFlavor)
func (h *Handler) Change(billy.Filesystem) billy.Change          // returns h.fs, never nil
func (h *Handler) FSStat(ctx, fs billy.Filesystem, s *nfs.FSStat) error
func (h *Handler) ToHandle(fs billy.Filesystem, path []string) []byte
func (h *Handler) FromHandle(fh []byte) (billy.Filesystem, []string, error)
func (h *Handler) InvalidateHandle(billy.Filesystem, []byte) error  // no-op
func (h *Handler) HandleLimit() int                                 // 1 << 20
func Serve(ctx context.Context, addr string, h *Handler) error      // net.Listen("tcp", addr); nfs.Serve
```

- **Mount**: a single export `/`. Any dirpath the client sends is accepted. Auth flavor
  `AuthFlavorNull`; the
  listener is bound to `127.0.0.1` so the OS, not NFS, is the security boundary (`docs/08`).
- **Handles are 16 bytes**: `magic u32 | gen u32 | ino u64`, big-endian. `ToHandle` resolves
  the path in the FSM and encodes (ino, gen). `FromHandle` looks up `Inodes[ino]`; if the
  inode is missing or `gen` differs it returns `&nfs.NFSStatusError{NFSStatusStale}`. The
  path is reconstructed from the inode's `Parent` chain. Handles therefore survive gateway
  restarts, renames and even a different gateway on another device, because they encode
  cluster-wide inode numbers (I-16). Inode numbers are never reused and `gen` is the low 32
  bits of the creating Raft index, so a handle can never resolve to a different file (I-17).
  go-nfs's `CachingHandler` (random UUID → path LRU) is **not** used.
- **`/.snapshots`** (docs/16 §6) is synthesized by the vfs at the root: absent from the
  root's READDIR, found by LOOKUP, holding one directory per mirror (its row name with `/`
  as `-`, `pike-m1`) and in each one directory per complete or incomplete snapshot, by
  name, from the catalogue. A snapshot's tree is read from its mirror's trees (a gateway
  caches them by hash); a file's bytes come from the pool while the chunk table still
  holds the chunk (the reader and its cache, no readahead) and otherwise from the mirror
  object, read from the disk on the mirror's host and over `GET
  /v1/mirrors/{row}/objects|trees/{hash}` from any other, verified against its hash. A
  handle under it resolves to a read-only view of the vfs (no write capability), so go-nfs
  answers every mutation there (CREATE, WRITE, SETATTR, REMOVE, RMDIR, RENAME, MKDIR,
  SYMLINK, LINK, MKNOD) `NFS3ERR_ROFS` with no change to the in-tree copy; a RENAME whose
  two directories lie on either side of the boundary is `NFS3ERR_NOTSUPP` (two
  filesystems), and the vfs refuses every such call with `EROFS` before it reaches the
  FSM. Modes are shown without their write bits. Inside an offline mirror's snapshot every
  read is `NFS3ERR_IO`, cached trees included; its listing of snapshot names still works.
  Entries carry 24-byte handles (docs/01 §NFS file handle), `STALE` once the catalogue row
  is gone or deleting, or the mirror offline; the fileid of an entry is its recorded
  `Ino`, and the synthetic directories carry fixed fileids above any real inode. The
  READDIR cookie verifier of a snapshot's directory is its tree's hash. `.snapshots` is a
  reserved name at the root: CREATE, MKDIR, SYMLINK, RENAME onto it and REMOVE of it are
  `EROFS`, and a directory of that name made before the reservation is no longer listed.
- **FSStat**: `Total = Σ Node.Capacity · K/(K+M)` over Active nodes, `Free = Total − stored·K/(K+M)`,
  `Files = inodes`, `FreeFiles = 1<<32 − inodes`. Degraded chunks count at their nominal size.
- **READDIR / READDIRPLUS**: go-nfs pages with cookies over the `[]os.FileInfo` returned by
  `ReadDir`, which the FSM returns sorted by name. The handler implements go-nfs's
  `CachingHandler` verifier hooks only: the cookie verifier is a hash of the directory
  inode's identity and `Version` (bumped by every name-table change), and the listing is kept (the 64 most recently used)
  so the following pages reuse it instead of listing and sorting the directory again. A
  listing continued after its directory changed goes on through the kept listing, a
  consistent snapshot, every surviving entry once and an entry removed meanwhile left out
  (go-nfs's `ListingPruner` hook, which keeps every cookie's place); a new listing
  (cookie 0) lists afresh whatever verifier it carries. Only a listing evicted from the
  cache gets `NFS3ERR_BAD_COOKIE` (I-47, I-232). Until 2026-10-12 any change got it, and
  macOS ends a listing there (Linux after one retry), so a folder changing under a Finder
  copy, an `ls` or an rsync showed truncated. Directories of
  10k+ entries are tested (`TestNFS_ReadDirPlusLargeDirectory`).
- **Attribute caching**: the gateway sets no special attribute TTL; clients use their
  defaults (`actimeo` 3–60 s). Another device's writes become visible on this device within
  the client's attribute cache window after the Raft commit replicates. Stat of a dirty file
  returns the overlay size and mtime so the local client sees its own writes immediately.
- **Unsupported**: hard links (LINK → NFS3ERR_NOTSUPP), MKNOD → NFS3ERR_NOTSUPP, locks
  (no NLM; mount with `nolock`), ACLs (`noacl`), xattrs (not in NFSv3).
- **REMOVE and RMDIR are a delete, and the volume keeps a trash** (docs/02 §Apply rules):
  the vfs proposes `Trash` for every name but the ephemeral ones of docs/06 §1, and the
  mount sees exactly what POSIX promises: LOOKUP of the name is `NFS3ERR_NOENT`, READDIR
  omits it, the file's handle is `NFS3ERR_STALE`, a CREATE of the name succeeds, and
  FSSTAT's free space does not grow until the entry is purged (the one visible
  difference). The browser UI (docs/13 §4) and `tailraid trash` are where the entry is
  seen and restored; a restored file's old handle resolves again (I-155). RENAME over an
  existing file sends the replaced file to the trash the same way.
- **A protected folder** (docs/02, I-154) answers REMOVE, RMDIR, a RENAME of the folder,
  a RENAME out of it or over an entry inside it with `NFS3ERR_ACCES`: the vfs maps
  `ErrProtected` to `EPERM`, which satisfies `os.ErrPermission`, the error go-nfs already
  turns into `ACCES` for the SETATTR change hook; the kernel client maps the status to
  `EACCES`, so macOS and Linux show "Permission denied". CREATE, WRITE and a RENAME into
  the folder succeed. NFSv3 has no open handle to tie a delete to its creator, so the own
  cleanup of I-220 is SMB's alone: a copy that fails into a protected folder over NFS
  leaves its partial file, and Finder's cleanup of it is refused.

## Ports and mount commands

NFS and MOUNT are served on the same port. No portmapper is run; the mount options pin
the ports instead.

| OS | Listener | Mount |
|---|---|---|
| macOS | `127.0.0.1:20490` | `mount -t nfs -o port=20490,mountport=20490,vers=3,tcp,nolocks,soft,rsize=1048576,wsize=1048576 127.0.0.1:/ ~/TailRAID` |
| Linux | `127.0.0.1:20490` | `mount -t nfs -o port=20490,mountport=20490,nfsvers=3,tcp,nolock,noacl,soft 127.0.0.1:/ <dir>` |

`tailraid mount` runs these for you (`internal/mount`); it retries via `sudo` when
`mount` returns EPERM, printing the exact command first, and on macOS retries without
the transfer sizes if the kernel refuses them.

Why `rsize`/`wsize` on macOS: its client defaults to 32 KiB on TCP and, unlike Linux, does
not take the gateway's FSINFO maximum, so every WRITE was 32 KiB per WAL fsync and every
READ its own RPC. macOS 26 keeps 1 MiB (`nfsstat -m` shows `rsize=1048576,wsize=1048576`;
its `readahead=16` is then 16 MiB). Linux negotiates 1 MiB from FSINFO on its own. The
transfer size is independent of the chunk size (4 MiB by default, 01): a chunk arrives as
several WRITEs the WAL and overlay coalesce, and a READ fetches its whole chunk into the
cache, so the following READs of it are served from memory. 1 MiB is also the ceiling of
both clients (macOS `vfs.generic.nfs.client.iosize`, the Linux rsize/wsize cap), so a
larger chunk never asks for a larger transfer.

Why `nolock`: there is no Network Lock Manager. Without `nolock` the client's lock daemon
tries to reach NLM and `fcntl` locks hang or fail. With it, locks are local to the client
machine. Cross-device locking is out of scope.

Why `soft`: if the gateway process dies, a hard mount would hang every process touching the
mount until it returns. `soft` returns EIO after retries instead. `tailraid serve` restarts
quickly under the service manager; the mount survives because handles are stable (I-16).

**Large copies and the caps.** A local `cp` of a multi-GiB file outruns replication over
the tailnet; the gateway then throttles the client instead of failing it: a WRITE that
meets the WAL cap or `MaxDirty` blocks for up to `CapWait` (20 s) until the flusher has
committed a batch (docs/04 §3, I-79). A waiting WRITE holds one of its connection's
`InFlight` slots (I-167) and the client keeps that page's write-back waiting; the rest of
the mount goes on. Batches are at most 32 MiB of chunks so a wait is a few seconds even on
a slow DERP path. `NFS3ERR_JUKEBOX` ("try again later")
cannot be used: go-nfs maps the handler's error to a status itself and never emits it. The
bound must stay inside each client's soft-mount timeout, which is what makes the wait
transparent rather than an I/O error:

| Client | Soft timeout before `EIO` | Verified |
|---|---|---|
| Linux (`timeo=600,retrans=2`, TCP default) | 60 s per RPC, retried twice | by the compose soak (phase 12) |
| macOS (`timeo=600` default on TCP) | 60 s per RPC | manual check below |

Manual check (macOS, before a release): copy a file larger than 512 MiB onto the mount while
the uplink is throttled to ~5 MiB/s; `cp` slows down but finishes without an I/O error and
`sha256` matches. When the cluster is full the copy stops with `ENOSPC` ("No space left on
device"), not `EIO`.

## Known POSIX gaps (documented, tested where they return an error)

- No hard links, no `mknod`, no file locks across devices, no xattrs, no ACLs.
- `rename` of a directory into its own subtree → EINVAL (`ErrLoop`).
- `mtime` resolution is nanoseconds in the FSM but clients may truncate to microseconds.
- Permissions are stored and returned but not enforced by the gateway: the loopback
  listener trusts the local OS, which already enforces access to the mount point.
- Cross-device visibility is eventually consistent within the client's attribute cache
  window; the FSM itself is linearizable through Raft.
- **atime and ctime on the wire are the mtime.** go-nfs fills every reply's `atime`, `mtime` and `ctime` from the billy `ModTime()`; a client-supplied atime is stored in the FSM (and seen by every gateway through `tailraid` itself) but GETATTR reports it as the mtime. No consumer of this volume is known to depend on atime.
- **`UNCHECKED` on an existing file truncates it.** RFC 1813 §3.3.8 has an `UNCHECKED`
  CREATE of an existing name apply only the attributes it carries. Neither the macOS nor
  the Linux client puts a size in them: an `O_TRUNC` open of a file the client knows
  sends its own `SETATTR` size 0. So the server's truncate matters only when the client
  believed the name free and another device created it meanwhile, and there a copy's
  `O_TRUNC` is the common case and truncating is the safer answer (the copy's bytes
  replace the other device's, never mix with them); an `O_CREAT` without `O_TRUNC` (a
  shell's `>>`) in that window loses the other device's bytes. Kept deliberately; an
  `O_EXCL` create no longer reaches it (I-231).
- **Name length.** go-nfs refuses a name longer than 255 bytes with `NFS3ERR_NAMETOOLONG` before it reaches the volume; the FSM enforces the same limit (I-66) for every other proposer. A path longer than 4096 bytes is refused the same way at CREATE, MKDIR, SYMLINK and RENAME (I-115, docs/01); depth alone is not limited, each LOOKUP resolves one component.

## Invariants

- **I-16** — A file handle is a pure encoding of (inode, generation). It stays valid across
  rename of the file or any ancestor, across gateway process restarts, and across gateways on
  different devices; it never depends on the path or on in-memory state.
- **I-17** — A handle whose inode no longer exists or is in the trash, whose generation
  differs, or whose bytes are malformed yields NFS3ERR_STALE. It never resolves to a
  different file and never panics.
- **I-45** — Every NFS WRITE is applied at exactly the requested offset and length; all other
  bytes of the file are unchanged (the per-RPC open/seek/write/close cycle is honoured).
- **I-46** — `Change` is non-nil: SETATTR of mode, owner and times succeeds and is visible
  on the next GETATTR from any gateway after commit.
- **I-47** — READDIR/READDIRPLUS paging returns every entry exactly once for a directory that
  is not modified during the listing, for directories of any size.
- **I-100** — When `Serve` stops, the listener and every connection it accepted are
  closed before it returns: a mounted client is never answered by a gateway whose
  flusher has already stopped, and a stuck NFS connection cannot keep the process alive.
- **I-167** — The NFS gateway serves one connection's requests concurrently, at most
  `InFlight` at a time, each read whole from the wire (a frame over `MaxFrame` closes the
  connection) before its handler runs, with replies written by the connection's single
  writer in completion order; a blocked request delays neither its connection's other
  requests nor other connections, FSINFO advertises `MaxFrame`'s payload as the largest
  transfer, and a connection holds at most `InFlight × MaxFrame` request bytes.

- **I-185** — `/.snapshots` is absent from the root's READDIR, resolves by name and lists
  every mirror and, in each, its complete and incomplete snapshots from the catalogue;
  every file under a snapshot of an online mirror reads exactly the snapshot's bytes
  through every gateway, from the pool while the chunk is there and from the mirror
  otherwise; inside an offline mirror's snapshot reads fail with `EIO`; every mutation
  under `/.snapshots` is `EROFS` (`NFS3ERR_ROFS`, or `NFS3ERR_NOTSUPP` for a RENAME
  across its boundary; `STATUS_ACCESS_DENIED` on SMB) and reaches no FSM; a snapshot
  handle is 24 bytes with its own magic and is `STALE` once the row is gone or deleting
  or the mirror offline; and `.snapshots` cannot be created, renamed onto or removed at
  the root.
- **I-231** — CREATE `EXCLUSIVE` of a free name creates the file exclusively across
  gateways (as `GUARDED`, I-222) and keeps the request's verifier in its mtime; the same
  request again, before the client's SETATTR replaces the times, answers that file; a
  name holding a file whose mtime is not the verifier, or a directory, is
  `NFS3ERR_EXIST` and the file is untouched.
- **I-232** — A READDIR or READDIRPLUS continued with the verifier of a listing still
  kept (the 64 most recently used) answers from that listing even after its directory
  changed: every entry of it that still names the same inode is listed exactly once
  across the pages, an entry removed or replaced meanwhile is left out, and no page
  answers `NFS3ERR_BAD_COOKIE`; a listing started with cookie 0 lists the directory
  afresh whatever verifier it carries.
- **I-238** — RMDIR of a folder with an entry answers `NFS3ERR_NOTEMPTY`, a RENAME onto
  such a folder `NFS3ERR_EXIST`, a folder renamed under itself `NFS3ERR_INVAL`, MKDIR of
  a name that holds a file or a folder `NFS3ERR_EXIST` (also when another gateway made it
  between this gateway's lookup and its mkdir), and a SETATTR size past the largest file
  `NFS3ERR_FBIG`, each as an NFS status; a stale gateway's refusals keep their statuses
  (I-39); a rename of a file onto its own name leaves its bytes, flushed or not.
- **I-242** — A CREATE or MKDIR refused once its directory resolved (`NFS3ERR_EXIST`
  above all) carries the directory's attributes after the call in its `dir_wcc` (RFC 1813
  CREATE3resfail, MKDIR3resfail), so a client that thought the name free sees the
  directory changed and looks it up again; a `mkdir -p` then finds the folder another
  gateway made a moment before, rather than failing on "File exists".
- **I-244** — A length a request names (an opaque, a string, an array) is believed only
  as far as the request frame holds the bytes: a longer one fails to decode without
  allocating it. Before, a 44-byte GETATTR whose handle claimed 4 GiB allocated 4 GiB.
- **I-245** — Every RPC reply carries the RFC 5531 code, big-endian: PROG_UNAVAIL for an
  unknown program, PROG_MISMATCH with the range for a version other than NFS 3 or MOUNT 3,
  PROC_UNAVAIL for an unknown procedure, GARBAGE_ARGS for arguments that do not decode, and
  MSG_DENIED RPC_MISMATCH 2..2 for an RPC version other than 2. LINK decodes LINK3args
  and answers NFS3ERR_NOTSUPP (hard links are out of scope). Before, the accept_stat values
  were shifted by one (a missing procedure went out as PROG_MISMATCH without its range),
  versions were never checked, a denial carried a reject_stat of 5 in little-endian, and
  LINK decoded SYMLINK's arguments.
- **I-248** — A READDIR or READDIRPLUS reply holds as many entries as the client's
  count (READDIR) or dircount and maxcount (READDIRPLUS) admit by the entries' real XDR
  sizes, never a byte more, and FSINFO prefers 1 MiB listings (`dtpref`, as `rtpref`).
  Before, every entry was charged 512 bytes, so a READDIR of short names returned about a
  sixteenth of the entries its count had room for and a READDIRPLUS about a third, and
  `dtpref` was 8 KiB: a folder of thousands of files took many times the round trips.

## Tests

| Invariant | Tests |
|---|---|
| I-16 | `FuzzDecodeHandle`, `TestHandle_RoundTrip_BigEndian`, `TestNFS_RenameKeepsHandleValid` |
| I-17 | `FuzzDecodeHandle`, `TestNFS_RemovedHandleIsStale` |
| I-45 | `TestNFS_WriteSequence_MultiRPC`, `TestVFS_PerRPCOpenCloseSequence`, `TestVFS_WriteAtNonZeroOffset_PreservesPrefix` |
| I-46 | `TestNFS_SetattrOwnerAndTimes`, `TestNFS_SetattrSizeTruncates`, `TestVFS_SymlinkAndChange` |
| I-47 | `TestNFS_ReadDirPlusLargeDirectory`, `TestNFS_ReadDirPlus_RemoveBetweenPages` |
| I-100 | `TestServe_ClosesConnectionsOnShutdown` |
| I-231 | `TestNFS_CreateExclusive_Verifier` |
| I-242 | `TestNFS_ExistReply_CarriesParentAttributes` |
| I-244 | `TestRead_LengthBoundedByBody`, `TestConn_RPCReplyCodes` |
| I-245 | `TestConn_RPCReplyCodes`, `TestNFS_FSStatAndUnsupported` |
| I-248 | `TestNFS_ListingFillsTheClientsCount`, `TestListEntrySizes_MatchTheEncoder` |
| I-238 | `TestNFS_NamespaceErrors_RFCStatuses`, `TestNFS_Mkdir_RacingBackend_Exist`, `TestVFS_RenameOntoItself_KeepsDirtyEphemeral` |
| I-232 | `TestNFS_ReadDir_ChangedMidListing_NoBadCookie`, `TestNFS_ReadDirPlus_RemoveBetweenPages` |
| I-167 | `TestNFS_ConcurrentRPCsOnOneConnection`, `TestNFS_InFlightBound`, `TestNFS_FSInfo_AdvertisesFrameCap`, `TestConn_RepliesInCompletionOrder`, `TestConn_InFlightBound`, `TestConn_ReadsWholeFrame_CapsIt` |
| I-185 | `TestSoak_AllPhasesAgainstHarness` (phase 19), `TestVFS_SnapshotsDir_HiddenResolvesLists`, `TestVFS_SnapshotRead_PoolThenMirror`, `TestVFS_SnapshotOfflineMirror_EIO`, `TestVFS_SnapshotMutation_IsROFS`, `TestVFS_SnapshotHandle_StaleTable`, `TestHandle_Snapshot_RoundTrip`, `FuzzDecodeHandle`, `TestNFS_Snapshots_OverTheWire`, `TestSMB_SnapshotsDir_ReadOnly`, `TestMirrorRPC_ObjectsAndTrees`, `TestMirrorSource_LocalDetachedRemote`, `TestMember_AddMirror_RetryKeepsRowAndResolvesToHost`, `TestCluster_SnapshotReadsAsOfIndexOnEveryGateway`, `TestModelCheck_Long` |
