# 15 — SMB gateway (Windows)

On Windows the volume is mounted through the operating system's SMB client instead of its
NFS client. Every node that runs `tailraid serve` with `listen.smb` set exposes an SMB2
server on loopback backed by the shared `vfs` (docs/04), the same filesystem the NFS
gateway (docs/07) and the web UI (docs/13) serve. macOS and Linux keep NFSv3; the SMB
gateway can be enabled on any OS (the tests run it on all three), but only Windows mounts
it by default.

## 1. Why SMB on Windows

- The Windows NFS client is an optional feature of the Pro, Enterprise and Education
  editions. It needs `nfsadmin` set to TCP, two registry values for the anonymous uid and
  gid, and a portmapper on port 111 because `mount.exe` cannot be told a port. Home has no
  NFS client at all. None of it was ever verified on hardware.
- Every Windows edition ships the SMB client, and Explorer, `net use` and
  `New-SmbMapping` are native to it.
- Windows' own Server service owns port 445 on every interface, so a user-space SMB server
  cannot bind it. Since Windows 11 24H2 (build 26100) the SMB client can be told a port
  (`net use … /TCPPORT:`, `New-SmbMapping -TcpPort`), which is why 24H2 is the floor:
  the gateway listens on `127.0.0.1:20445` and the mount names the port. Older builds get
  a message saying so from `tailraid mount`.

## 2. Library

`github.com/go-filesystems/smb` v0.7.0 (BSD-3, pure Go, no cgo), pinned. It serves SMB
dialects 2.1, 3.0 and 3.0.2 with NTLMv2 over SPNEGO and signing (HMAC-SHA256 for 2.x,
AES-CMAC for 3.x, which Windows 11 24H2 requires), one goroutine per connection, 1 MiB
`MaxReadSize`/`MaxWriteSize`, byte-range locks, change notification and share enumeration.
It refuses guest and anonymous sessions and SMB1. Not implemented: dialect 3.1.1,
encryption, oplocks and leases (so a client caches nothing and every operation is a round
trip), alternate data streams, security descriptors.

The library is young (first commit September 2026, one maintainer). It was adopted after
reading its server, file, info, directory, path and case-folding code against this
document, and the adapter is two small files so that a replacement server is cheap. What
the library does not do is listed under §8 rather than hidden.

The tree carries its own copy of v0.7.0 in `third_party/smb` (a `replace` in every
module file, vendored from there), with the changes this gateway needs until they are
upstream, each with a test in the copy:

- `credit.go`: the credit charge of a WRITE and a SET_INFO is measured by its payload, as
  MS-SMB2 3.1.5.2 says, not by the whole body. Upstream counts the 48-byte WRITE header,
  so a write of exactly 64 KiB (what Windows and the Linux client send) was refused with
  `STATUS_INVALID_PARAMETER`; `TestSMB_WriteSize_Boundaries` found it.
- `file.go`, `info.go`, `dir.go`, `pipe.go`: a `Timestamper` capability on a `Stat`
  (`Times() (atime, mtime, ctime)`) fills the four timestamps a client shows, and SET_INFO
  basic forwards the access and write times a client sets to `MetadataSetter.Chtimes`
  (0, −1 and −2 mean "leave it", per MS-FSCC 2.4.7); without the capability the server
  keeps reporting the time of the reply. Explorer therefore shows the FSM's modification
  times and `robocopy /COPY:DAT` preserves them.
- `file.go`, `status.go`: a READ the backend fails (here: a chunk with too few shards,
  `EIO`) answers `STATUS_UNEXPECTED_IO_ERROR`, not `STATUS_END_OF_FILE`, which upstream
  used as the fallback for every read error. A client takes END_OF_FILE for a shorter
  file and hands its caller a short, wrong buffer; the three-VM rounds caught it through
  a Windows mount in the corruption phase (I-40). `TestSMB_UnreadableChunk_IsIoError`.

- `server.go`: every reply of a compounded response is signed after its NextCommand and
  its padding are in place, over the padded bytes (MS-SMB2 3.3.4.1.1). Upstream signed
  first and wrote the offset afterwards, so the signature covered a message that was never
  sent; Windows checks every reply of a chain, logged "signing validation failed" for a
  CREATE + CLOSE compound whose CREATE failed, and reported error 58 ("The specified
  server cannot perform the requested operation") instead of the CREATE's status. The
  three-VM rounds caught it through `verify` on Windows. `TestASignedChainVerifiesEveryReply`.

- `attrs.go`, `dir.go`, `file.go`, `info.go`: a name whose last element starts with a dot
  (other than `.` and `..`) carries `FILE_ATTRIBUTE_HIDDEN`, as Samba's `hide dot files`
  does by default. Windows has no dot convention, so Explorer showed every `._<name>`
  file macOS writes beside a file it copies over NFS (its extended attributes, which
  NFSv3 cannot carry), every `.DS_Store`, and every Unix dot file. Nothing is left out
  of a listing; Explorer's "Hidden items" shows them (I-141).
  `TestAttributes_DotFilesHidden`, `TestSMB_DotFilesHidden`.

The copy is updated by re-applying these five changes to a newer upstream; the `replace`
goes when upstream carries them.

It exports a backend through the `github.com/go-filesystems/interface` contract: the
required `Filesystem` (`ReadFile`, `WriteFile`, `ListDir`, `Stat`, `ReadLink`, `MkDir`,
`DeleteFile`, `DeleteDir`, `Rename`, `Close`) plus the optional capabilities it probes for:
`Opener` (positional handles: `File{ReadAt, Size, Close}` and `WritableFile{WriteAt,
Truncate, Sync}`), `Truncater`, `Symlinker` and `MetadataSetter`. A backend error is mapped
to an NT status by `errors.Is`: `fs.ErrNotExist`, `fs.ErrExist`, `fs.ErrPermission`, and
`ENOSPC`/`EDQUOT`/`EFBIG` as "disk full"; anything else takes the operation's default.

Adapter: `internal/smbgw` implements that contract over `*vfs.FS` and `*vfs.File`.

## 3. What the server does on the backend (verified against v0.7.0)

Every command holds the share's lock for its whole duration: reads share it, anything that
changes the volume holds it exclusively. Two clients (macOS opens two connections for one
mount) therefore never interleave inside one command.

| SMB2 command | What the server calls on the adapter |
|---|---|
| CREATE (open) | `Stat(path)`; on a miss, the case-insensitive walk of §5; then `OpenFile(path)` for a file, which the adapter answers with a `WritableFile` over `vfs.OpenFile(path, O_RDWR)` |
| CREATE (create, overwrite, supersede) | `WriteFile(path, nil, 0644)` (`vfs.OpenFile` with `O_CREATE\|O_TRUNC`), or `MkDir(path, 0755)` for a directory; then `Stat` and `OpenFile` as above. `FILE_CREATE` on an existing name is `STATUS_OBJECT_NAME_COLLISION` before the backend is touched |
| READ | `File.ReadAt(buf, offset)`; a short read at the end is `STATUS_END_OF_FILE`, which is how a listing of bytes ends |
| WRITE | `WritableFile.WriteAt(data, offset)` then `Sync()`; the reply is sent only after both return. `WriteAt` is `vfs.File.WriteAt`, durable in the WAL when it returns (I-7), so `Sync` is a no-op |
| FLUSH | `Sync()`, success |
| CLOSE | `File.Close()`; with delete-on-close set, `DeleteFile` or `DeleteDir` |
| SET_INFO end-of-file, allocation | `WritableFile.Truncate(size)` (`vfs.File.Truncate`); past `MaxFileSize` the vfs answers `EFBIG`, which is `STATUS_DISK_FULL` on the wire |
| SET_INFO rename | `Rename(old, new)`; the handle follows the new path |
| SET_INFO disposition | marks the handle delete-on-close (SMB2 has no delete command) |
| SET_INFO basic (times, attributes) | the access and write times go to `Chtimes` (0, −1 and −2 leave a time alone); attributes are dropped (§8) |
| QUERY_INFO file classes | `Stat(path)`: size, the type and permission bits as Windows attributes (`DIRECTORY`, `ARCHIVE`, `READONLY` when no write bit is set), the inode number as the FileId, the inode's own times (creation is the change time: there is no birth time) |
| QUERY_INFO filesystem size | the share's `WithCapacityFunc`, which the adapter answers with `meta.Usage(volume, nodes, stats)`: the same total and free bytes as NFS `FSSTAT` and `tailraid status` |
| QUERY_DIRECTORY | `ListDir(dir)` then `Stat` of every entry, kept server-side per handle and paged with `STATUS_NO_MORE_FILES` at the end; "." and ".." are added by the server; entries are sorted case-insensitively so paging is stable |
| SYMLINK, READLINK | Windows creates reparse points through ioctl, which the server does not implement; the adapter's `Symlink`/`ReadLink` are reachable from macOS and Linux clients only |

Paths arrive with backslashes relative to the share root; the server turns them into
`/sub/file` and refuses a name that climbs out of the root or names an alternate data
stream (`file:stream`). `meta.SplitPath` would have folded the backslashes too.

## 4. Adapter

```go
package smbgw

const ShareName = "tailraid"
const User = "tailraid"

type FS struct { fs *vfs.FS; st *meta.Store }                 // filesystem.Filesystem, Opener, Truncater, Symlinker, MetadataSetter
func New(fs *vfs.FS, st *meta.Store) *FS
func NewServer(fs *vfs.FS, st *meta.Store, password string) (*smb.Server, error)
func Listen(ctx context.Context, addr string) (net.Listener, error) // loopback only (I-110, I-50)
func Serve(ctx context.Context, l net.Listener, srv *smb.Server) error
```

- `Stat` and `ListDir` come from `vfs.Lstat` and `vfs.ReadDir`; the inode number and the
  inode's own times are read from the `vfs` file info through small optional interfaces
  (`Ino()`, `Times()`), the way the web UI reads `Dirty()`, and the stat result carries
  the times for the server's `Timestamper`. The mode is the POSIX type bits plus the
  permission bits.
- `file` wraps `*vfs.File`: `ReadAt` and `WriteAt` are the vfs calls; `Size` is the
  overlay size while dirty, else the committed size; `Sync` returns nil (docs/04: the WAL
  fsync happened inside `WriteAt`); `Close` is the vfs close.
- Every error passes through one `wrap`: `vfs.ErrReadOnly` (`EROFS`, the stale gateway of
  docs/02) is wrapped so that it also satisfies `fs.ErrPermission` and reaches the wire as
  `STATUS_ACCESS_DENIED`, which Windows shows as "access denied"; `ENOENT`, `EEXIST`,
  `ENOSPC` and `EFBIG` already satisfy the errors the server recognises.
- `WriteFile(path, data, perm)` opens with `O_RDWR|O_CREATE|O_TRUNC`, writes `data` when
  there is any, and closes; `MkDir` is `MkdirAll`; `DeleteFile` and `DeleteDir` are
  `Remove`; `Truncate(path, size)` opens write-only and truncates.
- The share is `tailraid`, exported to the one user `tailraid` with the password from the
  config, with the capacity function above. `Serve` runs the server until `ctx` is done,
  then `Close` drops the listener and every connection (as the NFS gateway does).
- The library has no log hook; nothing it does is logged.

## 5. Case-insensitive names

SMB tells the client that names are case-preserving and compared without case, and the
FSM is case-sensitive. The server resolves an exact miss by listing the parent and taking
the alphabetically first entry that matches without case, one `ListDir` per path component
on the miss only. So `A.txt` and `a.txt` can both exist (created from macOS or Linux), a
Windows client opening either name by its exact spelling gets that file, and a lookup in a
third spelling (`A.TXT`) gets `A.txt`. Explorer probes names such as `desktop.ini` in
every folder it shows, so a folder of many entries costs one listing per probe; a
directory listing itself is one `ListDir` and one `Stat` per entry from the FSM (no I/O).

## 6. Configuration, password and the Windows mount

```yaml
listen:
  nfs: ""                 # loopback host:port of the NFS gateway; "" disables it (Windows default)
  smb: 127.0.0.1:20445    # loopback host:port of the SMB gateway; "" disables it (macOS/Linux default)
smb:
  password: <24 chars>    # generated by init and join when listen.smb is set
mount:
  path: "T:"
```

At least one gateway must be set. `init` and `join` generate `smb.password` (24 base32
characters from `crypto/rand`) when `listen.smb` is set and the config has none, and save
it in the 0600 config file; `serve` refuses to start the SMB gateway without one. The password is the SMB client's requirement, not a security
boundary of ours: the listener is loopback-only and the OS is the boundary (docs/08). It
is never logged and never part of `status`; the `mount with` log line prints it as
`<smb.password>`.

`tailraid mount` on Windows runs

```
powershell -NoProfile -NonInteractive -Command New-SmbMapping -LocalPath T: -RemotePath \\127.0.0.1\tailraid -TcpPort 20445 -UserName tailraid -Password $env:TAILRAID_SMB_PASSWORD -Persistent $false
```

with the password in the child's environment (`TAILRAID_SMB_PASSWORD`), which only its
own user and administrators can read, so it is never in the process list. `net use … *`
was the first choice and is still the console form, but it reads the password from the
console only, never from standard input, so a mount from any non-console session (an SSH
session, a script, the three-VM rig) failed with "System error 86". `tailraid umount`
runs `net use T: /delete /y`. Before either, `mount` checks the Windows build
(`RtlGetVersion`) and refuses below 26100 with the reason. When PowerShell cannot be run
the console form is printed instead:

```
net use T: \\127.0.0.1\tailraid /TCPPORT:20445 /USER:tailraid * /PERSISTENT:NO
```

A mapping needs a logon session the SMB redirector can attach credentials to: a
password or interactive logon has one, a key-authenticated SSH logon does not and fails
with error 1312 whatever the password (docs/vms.md).

Whether the Windows redirector accepts `127.0.0.1` for a non-445 loopback target, or
needs `localhost` or the machine name, is unverified until the checklist runs
(`docs/windows-checklist.md`). On macOS `mount_smbfs //tailraid@127.0.0.1:20445/tailraid
~/TailRAID` and on Linux `mount -t cifs //127.0.0.1/tailraid <dir> -o port=20445,
username=tailraid,vers=3.0` mount the same share when `listen.smb` is set there.

## 7. Large copies and the caps

A WRITE that meets the WAL cap or `MaxDirty` waits for the flusher for up to `CapWait`
(20 s, docs/04 §3, I-79) before it is answered; the server holds the share lock meanwhile,
so every other operation on the share pauses as every NFS RPC did. The wait must stay
inside the client's request timeout:

| Client | Request timeout before it gives up | Verified |
|---|---|---|
| Windows (`SessTimeout`, default 60 s) | 60 s per request | `docs/windows-checklist.md` |
| macOS `mount_smbfs` | 60 s per request (kernel default) | manual check |
| Linux `cifs` | the echo interval, 60 s | by the in-process tests only |

When the cluster is full the WRITE answers `STATUS_DISK_FULL`, which Windows shows as
"There is not enough space on T:", never an I/O error.

## 8. Known gaps

- **Timestamps** are the FSM's through the in-tree server copy (§2); upstream v0.7.0
  reports the time of the reply and drops the times a client sets. There is no birth
  time: creation is reported as the change time.
- **A delete that fails is not reported.** SMB2 deletes by marking a handle delete-on-close,
  and the server does not carry the failure of that delete into the CLOSE reply, so a
  delete while the gateway is stale answers success and the file stays. The next listing
  shows it.
- **Mode, owner, attributes.** SMB2 carries no POSIX mode or owner; the READONLY attribute
  is derived from the permission bits and a change of it is dropped. `chmod` and `chown`
  from a Windows client do not reach the FSM.
- **Symlinks** are reachable from macOS and Linux SMB clients only (§3).
- **No oplocks or leases**, so the client caches nothing: every read, write and attribute
  query is a round trip over loopback.
- **Hard links, extended attributes, locks across devices** are out of scope as for NFS;
  SMB byte-range locks are enforced within one gateway only.
- **Cross-device visibility** is as for NFS: another device's writes are visible after the
  Raft commit replicates and the client asks again.
- **A gateway restart drops the mapping for a moment.** With no durable handles, the
  redirector loses its session when `serve` restarts and reconnects on a later access;
  for the few seconds it remembers the failure, `T:` answers "The network path was not
  found" (error 53) or "The specified server cannot perform the requested operation"
  (58). Explorer and the next command recover on their own; `verify` on Windows retries
  such errors for up to 30 s, which is how its restart-storm phase passes there (the
  NFS clients on macOS and Linux ride out a restart without help).

## Invariants

- **I-106** — Every SMB WRITE is applied at exactly the requested offset and length through
  `vfs.File.WriteAt`; all other bytes of the file are unchanged, the reply is sent only
  after the WAL record is fsynced (I-7), and writes of 0, 1, `MaxWriteSize−1`,
  `MaxWriteSize` and `MaxWriteSize+1` bytes at a non-zero offset read back identically.
- **I-107** — SET_INFO end-of-file (growing and shrinking), rename and the access and
  write times through the SMB gateway are applied to the FSM and visible on every gateway
  after commit; the times the gateway reports are the inode's own, never a server
  default; an end-of-file past `MaxFileSize` answers `STATUS_DISK_FULL` and changes
  nothing.
- **I-108** — QUERY_DIRECTORY paging returns every entry of a directory exactly once for a
  directory not modified during the listing, for directories of any size, and every
  entry's FileId is the inode number, unchanged by a rename of the file or any ancestor.
- **I-109** — While stale (I-39) a metadata mutation through the SMB gateway answers
  `STATUS_ACCESS_DENIED` while data WRITEs and end-of-file changes to existing files keep
  landing in the WAL; a
  WRITE refused because the cluster is full answers `STATUS_DISK_FULL` (I-79); a missing
  path answers `STATUS_OBJECT_NAME_NOT_FOUND` and a colliding create
  `STATUS_OBJECT_NAME_COLLISION`; the share's total and free bytes are `meta.Usage`.
- **I-110** — The SMB listener binds only a loopback address and exports one share,
  `tailraid`, to one user, `tailraid`, whose password is generated by `init` and `join`
  whenever `listen.smb` is set and the config has none, lives only in the 0600 config
  file and appears in no log line or `status` report; a session with a wrong password
  fails with `STATUS_LOGON_FAILURE`, and no guest or anonymous session is accepted.
- **I-141** — Every attribute the SMB gateway reports for a name whose last element begins
  with `.` (other than `.` and `..`), in a directory listing, a CREATE reply or a
  QUERY_INFO, includes `FILE_ATTRIBUTE_HIDDEN`, and no other name carries it; the entry
  is still listed and opens, reads and writes as any other.

## Tests

| Invariant | Tests |
|---|---|
| I-106 | `TestSMB_WriteSequence_MultiRequest`, `TestSMB_WriteSize_Boundaries`, `TestVFS_WriteAt_ExactOffset_PosUnchanged` |
| I-107 | `TestSMB_SetInfo_EndOfFile`, `TestSMB_RenameKeepsFileIdAndContent`, `TestSMB_Times_FromFSMAndSetInfo` |
| I-108 | `TestSMB_QueryDirectory_LargeDirectory`, `TestSMB_RenameKeepsFileIdAndContent` |
| I-109 | `TestSMB_ReadOnly_WireStatuses`, `TestSMB_WriteWhenFull_IsDiskFull`, `TestSMB_WriteAtCap_WaitsThenSucceeds`, `TestSMB_Statfs_FromUsage` |
| I-110 | `TestSMB_Auth_WrongPasswordRefused`, `TestSMB_ListenRefusesNonLoopback`, `TestConfig_Gateways`, `TestCLI_Init_GeneratesSMBPassword` |
| I-141 | `TestSMB_DotFilesHidden` |
