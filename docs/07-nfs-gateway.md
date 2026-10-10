# 07 — NFS gateway

The NFS gateway is how a macOS or Linux device mounts the volume as a local filesystem.
Every node that runs `tailraid serve` with `listen.nfs` set exposes an NFSv3 server on
loopback backed by the shared `vfs` (`docs/04`). The OS's own NFS client mounts it; nothing
is installed on the OS. Windows mounts over SMB instead (docs/15).

Library: `github.com/willscott/go-nfs` v0.0.4 (pure Go, NFSv3 + MOUNT on one TCP port,
no portmapper, no NLM). Adapter: `internal/nfsgw` implements `nfs.Handler`;
`internal/vfs` implements `billy.Filesystem`, `billy.File` and `billy.Change`.

## What go-nfs actually does (verified against v0.0.4 source)

These facts drive the vfs design and are the reason the previous attempt corrupted data.

| NFS procedure | go-nfs calls on the billy filesystem |
|---|---|
| WRITE (every RPC, ≤ wsize, typically 64 KiB–1 MiB) | `Stat(path)` → `OpenFile(path, O_RDWR, 0)` → `file.Seek(offset, 0)` → `file.Write(data)` → `file.Close()`; replies **FILE_SYNC** unconditionally; a write past `MaxFileSize` (I-64) fails with `EFBIG` → `NFS3ERR_FBIG` |
| COMMIT | no-op; never touches billy |
| READ | `Open(path)` → `file.ReadAt(buf, offset)` → `file.Close()` |
| SETATTR size | `OpenFile(path, O_WRONLY\|O_EXCL, 0)` → `file.Truncate(size)` → `file.Close()`; a size past `MaxFileSize` (I-64) fails with `EFBIG` |
| SETATTR mode/uid/gid/times | `Handler.Change(fs)` → `Chmod`/`Lchown`/`Chtimes`; if `Change` returns nil every such call fails with NFS3ERR_NOTSUPP |
| CREATE | `fs.Create(path)` → `Close()` (then SETATTR for mode) |
| MKDIR / RMDIR / REMOVE / RENAME | `MkdirAll`, `Remove`, `Remove`, `Rename` |
| LOOKUP / GETATTR / READDIR(PLUS) | `Lstat`, `ReadDir`; fileid from `FileInfo.Sys()` when it returns `*file.FileInfo{Fileid, Nlink, UID, GID}` (go-nfs's own type), otherwise an FNV hash of the path |
| LINK | optional `UnixChange.Link`; not implemented → NFS3ERR_NOTSUPP |
| SYMLINK / READLINK | `Symlink`, `Readlink` (billy `Symlink` capability) |

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
- **FSStat**: `Total = Σ Node.Capacity · K/(K+M)` over Active nodes, `Free = Total − stored·K/(K+M)`,
  `Files = inodes`, `FreeFiles = 1<<32 − inodes`. Degraded chunks count at their nominal size.
- **READDIR / READDIRPLUS**: go-nfs pages with cookies over the `[]os.FileInfo` returned by
  `ReadDir`, which the FSM returns sorted by name. The handler implements go-nfs's
  `CachingHandler` verifier hooks only: the cookie verifier is a hash of the directory
  inode's identity and `Version` (bumped by every name-table change), and the listing is kept (64 most recent)
  so the following pages reuse it instead of listing and sorting the directory again; a
  change to the directory changes the verifier, so a stale cookie gets
  `NFS3ERR_BAD_COOKIE` (I-47, `TestNFS_ReadDirPlus_RemoveBetweenPages`). Directories of
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
  turns into `ACCES` for the SETATTR change hook; macOS and Linux show "Operation not
  permitted". CREATE, WRITE and a RENAME into the folder succeed.

## Ports and mount commands

NFS and MOUNT are served on the same port. No portmapper is run; the mount options pin
the ports instead.

| OS | Listener | Mount |
|---|---|---|
| macOS | `127.0.0.1:20490` | `mount -t nfs -o port=20490,mountport=20490,vers=3,tcp,nolocks,soft 127.0.0.1:/ ~/TailRAID` |
| Linux | `127.0.0.1:20490` | `mount -t nfs -o port=20490,mountport=20490,nfsvers=3,tcp,nolock,noacl,soft 127.0.0.1:/ <dir>` |

`tailraid mount` runs these for you (`internal/mount`); it retries via `sudo` when
`mount` returns EPERM, printing the exact command first.

Why `nolock`: there is no Network Lock Manager. Without `nolock` the client's lock daemon
tries to reach NLM and `fcntl` locks hang or fail. With it, locks are local to the client
machine. Cross-device locking is out of scope.

Why `soft`: if the gateway process dies, a hard mount would hang every process touching the
mount until it returns. `soft` returns EIO after retries instead. `tailraid serve` restarts
quickly under the service manager; the mount survives because handles are stable (I-16).

**Large copies and the caps.** A local `cp` of a multi-GiB file outruns replication over
the tailnet; the gateway then throttles the client instead of failing it: a WRITE that
meets the WAL cap or `MaxDirty` blocks for up to `CapWait` (20 s) until the flusher has
committed a batch (docs/04 §3, I-79). go-nfs handles one RPC at a time per connection, so
every operation on the mount pauses for that wait; batches are at most 32 MiB of chunks so a
wait is a few seconds even on a slow DERP path. `NFS3ERR_JUKEBOX` ("try again later")
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
- **atime and ctime on the wire are the mtime.** go-nfs fills every reply's `atime`, `mtime` and `ctime` from the billy `ModTime()`; a client-supplied atime is stored in the FSM (and seen by every gateway through `tailraid` itself) but GETATTR reports it as the mtime. No consumer of this volume is known to depend on atime.
  window; the FSM itself is linearizable through Raft.
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

## Tests

| Invariant | Tests |
|---|---|
| I-16 | `FuzzDecodeHandle`, `TestHandle_RoundTrip_BigEndian`, `TestNFS_RenameKeepsHandleValid` |
| I-17 | `FuzzDecodeHandle`, `TestNFS_RemovedHandleIsStale` |
| I-45 | `TestNFS_WriteSequence_MultiRPC`, `TestVFS_PerRPCOpenCloseSequence`, `TestVFS_WriteAtNonZeroOffset_PreservesPrefix` |
| I-46 | `TestNFS_SetattrOwnerAndTimes`, `TestNFS_SetattrSizeTruncates`, `TestVFS_SymlinkAndChange` |
| I-47 | `TestNFS_ReadDirPlusLargeDirectory`, `TestNFS_ReadDirPlus_RemoveBetweenPages` |
| I-100 | `TestServe_ClosesConnectionsOnShutdown` |
