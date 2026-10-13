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
  times and `robocopy /COPY:DAT` preserves them, through the flush that commits the
  copied data (04 §6, I-230).
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
- `file.go`, `info.go`: a `DeleteChecker` capability on the filesystem
  (`CanDelete(path) error`) is asked when a client marks a handle delete-on-close, at
  CREATE or through SET_INFO disposition, and its error is the reply's status. SMB2
  deletes at CLOSE, whose reply carries no failure, so without it a delete the backend
  refuses (a protected folder, docs/02, I-154) answered success and the file stayed.
  `TestDeleteOnClose_AsksTheBackend`, `TestSMB_Protected_AccessDenied`.
- `info.go`: a QUERY_INFO answer is bounded by the request's OutputBufferLength, as
  MS-SMB2 3.3.5.20 says: an answer that fits is sent whole, one longer than the buffer
  but whose fixed part fits is cut to the buffer with `STATUS_BUFFER_OVERFLOW`, and a
  buffer shorter than the fixed part is refused with `STATUS_INFO_LENGTH_MISMATCH`.
  Upstream ignored the field. Windows asks for `FileFsVolumeInformation` with a buffer
  sized for an empty label whenever a program calls `GetFileInformationByHandle`, which
  Go's `os.Open` of a directory does; the share's label made the answer longer than the
  buffer, the redirector took it for a malformed reply and failed the call with error 58
  ("The specified server cannot perform the requested operation") without a log line.
  Every `os.ReadDir` on the mapping failed: the three-VM rounds caught it through
  `verify` on Windows, where only the soak's 30 s retry had hidden it (I-213).
  `TestQueryInfo_OutputBufferLength`.

The copy is updated by re-applying these seven changes to a newer upstream; the `replace`
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

Every command but a WRITE holds the share's lock for its whole duration: reads share it,
anything that changes the volume holds it exclusively (a WRITE needs none: the vfs locks
per inode, I-217). Two clients (macOS opens two connections for one
mount) therefore never interleave inside one command on one gateway; between gateways
the FSM decides (an exclusive create, I-222; two rewrites of one file, I-223). The share
also keeps a list of its open handles, each with the inode it opened: a rename moves
every handle (and every byte-range lock) on the renamed path or under it to the new one,
and a delete-on-close removes the handle's inode or nothing, so what one handle does to a
file is judged by the file, not by the name it had when opened (I-224, I-225). A rename
made on another gateway, over NFS or in the browser moves no handle here: such a handle
keeps answering by its old path until it is reopened.

| SMB2 command | What the server calls on the adapter |
|---|---|
| CREATE (open) | `Stat(path)`; on a miss, the case-insensitive walk of §5; the share-access check against this share's opens of the file, `STATUS_SHARING_VIOLATION` on a conflict before anything changes (I-265); then `OpenFile(path)` for a file, which the adapter answers with a `WritableFile` over `vfs.OpenFile(path, O_RDWR)` |
| CREATE (create) | `FILE_CREATE`: `CreateExcl(path)` (`vfs.OpenFile` with `O_CREATE\|O_EXCL`), or `MkDirExcl(path, 0755)` (`vfs.MkdirExcl`) for a directory; an existing name, seen by this gateway or by the leader, is `STATUS_OBJECT_NAME_COLLISION` (I-222); then `Stat` and `OpenFile` as above |
| CREATE (open-if) | a missing name: `CreateExcl(path)`; a name another gateway created first opens that file, never truncated; a directory: `MkDir(path, 0755)` |
| CREATE (overwrite, supersede, overwrite-if) | `WriteFile(path, nil, 0644)` (`vfs.OpenFile` with `O_CREATE\|O_TRUNC`): on an existing file the truncate starts a rewrite, so two clients replacing one file leave one whole copy (04 §6, I-223) |
| READ | `File.ReadAt(buf, offset)`; a short read at the end is `STATUS_END_OF_FILE`, which is how a listing of bytes ends |
| WRITE | `WritableFile.WriteAt(data, offset)` then `Sync()`; the reply is sent only after both return. `WriteAt` is `vfs.File.WriteAt`, durable in the WAL when it returns (I-7), so `Sync` is a no-op |
| FLUSH | `Sync()`, success |
| CLOSE | `File.Close()`; with delete-on-close set, `DeleteFileIno(path, ino)` (`vfs.RemoveIno`) of the handle's current path, for a file or a directory, with the handle's inode as `Expect`: nothing is removed when another file holds that name (I-225). An open its client leaves without a CLOSE is closed the same way, delete-on-close included, when the connection drops, its session logs off or its tree disconnects (MS-SMB2 3.3.7.1, 3.3.5.6, 3.3.5.8, I-240); until 2026-10-13 those only released the driver's handle, so a copy's guard file outlived a lost connection full size and made of holes |
| SET_INFO end-of-file | `WritableFile.Truncate(size)` (`vfs.File.Truncate`); past `MaxFileSize` the vfs answers `EFBIG`, which is `STATUS_DISK_FULL` on the wire |
| SET_INFO allocation | a reservation, not an end of file (MS-FSA 2.1.5.15.1, I-235): one at or above the file's size changes nothing, one below it is `Truncate(size)`. It had been the end of file, so a program that preallocated and wrote less left zeros at the end |
| SET_INFO rename | `Rename(old, new)` when `ReplaceIfExists` is set; without it `RenameNoReplace(old, new)` (the FSM's `NoReplace`, docs/02), so an existing `new`, created on this gateway or another, is `STATUS_OBJECT_NAME_COLLISION` and nothing moves (I-224). Every open handle of the file on the share follows the new path, not only the renaming one |
| SET_INFO disposition | marks the handle delete-on-close (SMB2 has no delete command); a folder that still holds entries is `STATUS_DIRECTORY_NOT_EMPTY` here and stays (`CanDelete`, I-236), where it had answered success and kept the folder |
| SET_INFO basic (times, attributes) | the access and write times go to `Chtimes` (0, −1 and −2 leave a time alone); attributes are dropped (§8) |
| QUERY_INFO file classes | `Stat(path)`: size, the type and permission bits as Windows attributes (`DIRECTORY`, `ARCHIVE`, `READONLY` when no write bit is set), the inode number as the FileId, the inode's own times (creation is the change time: there is no birth time) |
| QUERY_INFO filesystem size | the share's `WithCapacityFunc`, which the adapter answers with `meta.Usage(volume, nodes, stats)`: the same total and free bytes as NFS `FSSTAT` and `tailraid status` |
| QUERY_DIRECTORY | `ListDir(dir)` then `Stat` of every entry, kept server-side per handle and paged with `STATUS_NO_MORE_FILES` at the end; "." and ".." are added by the server; entries are sorted case-insensitively so paging is stable; the search pattern matches as MS-FSA 2.1.4.4 has it, without case: `*`, `?` and the DOS wildcards `<`, `>` and `"` that FindFirstFile writes for `*.txt` and `a?c`, every other character literal (I-237; Go's `path.Match` had read `[` as a class, so `Photo [1].jpg` was not found by its name) |
| SYMLINK, READLINK | Windows creates reparse points through ioctl, which the server does not implement; the adapter's `Symlink`/`ReadLink` are reachable from macOS and Linux clients only |
| IOCTL `FSCTL_QUERY_FILE_REGIONS` | `File.ValidDataLength()` of the handle's file (`vfs.File.ValidDataLength`), answered as MS-FSA computes the regions from a valid data length (I-227) |
| IOCTL, any other | `FSCTL_VALIDATE_NEGOTIATE_INFO`, `FSCTL_PIPE_TRANSCEIVE` and server-side copy (`FSCTL_SRV_REQUEST_RESUME_KEY`, `FSCTL_SRV_COPYCHUNK[_WRITE]`) are answered; every other request gets the status MS-SMB2 §3.3.5.15 names for it (I-228) |

**IOCTLs.** Windows' copy engine (Windows 11 24H2 Explorer, drag and drop or paste of
many files) probes each destination before writing it: it creates the file, sets its
end-of-file to the source's size, marks the handle delete-on-close as a guard against a
copy that dies halfway, and asks `FSCTL_QUERY_FILE_REGIONS` for the file's valid data. A
refusal, with either `STATUS_NOT_SUPPORTED` or `STATUS_INVALID_DEVICE_REQUEST`, fails
that attempt: the engine closes the handle, the guard deletes the file, and it retries
the file later or not at all. Until 2026-10-12 the server refused the FSCTL, and a large
Explorer copy put every file it created in the trash while its dialog counted on. The
answer is the one MS-FSA gives: the regions before the file's valid data length (VDL)
carry the asked usage, the rest up to the end of file usage 0. Here the VDL is the end of
the last byte written: the end of the last committed chunk that is not a hole, held
below a truncate still in the overlay, or the end of the last unflushed write, whichever
is larger, and never past the size. Everything at or past it reads as zeros, which is
what a VDL promises; a chunk written as zeros counts as a hole (the flusher stores it as
one, 04 §4), so the VDL may fall short of a write of zeros but never short of a byte
that reads otherwise. A file Explorer has just created and sized has a VDL of 0, as on
NTFS. Any other FSCTL the server does not implement is `STATUS_INVALID_DEVICE_REQUEST`
("allowed but not supported on this file system"), an IOCTL that is not an FSCTL is
`STATUS_NOT_SUPPORTED`, and an input buffer that is misaligned or outside the message is
`STATUS_INVALID_PARAMETER` (I-228).

**Explorer's first pass.** For a batch of files the copy engine runs two passes. The
first creates each destination, sets its end-of-file to the source's size, marks it
delete-on-close, asks `FSCTL_QUERY_FILE_REGIONS` and closes it without a write, so the
file goes again; the progress bar counts this pass. Only when it reaches the end does the
bar reset and the second pass create and write every file. Each first-pass file costs a
`Create` and a `Trash` here, a Raft round trip each, so a batch of thousands spends
minutes before a byte moves; a copy cancelled then has copied nothing. Such a file never
held data, so its delete leaves nothing in the trash: the gateway purges the inode once
the name is gone (I-229). Until 2026-10-12 each one stayed in the trash, about 22,000
empty entries after a few large drags.

**Request trace.** At `log.level: debug` the server logs one line per CREATE, SET_INFO,
CLOSE, READ, WRITE (and the final status of a WRITE answered later), FLUSH, QUERY_INFO,
QUERY_DIRECTORY, IOCTL and LOCK, with the session, the handle, the file's path and
inode, the request's parameters and the status answered, plus a refused signature, a
credit charge too low and a chained request after a failure. It is how the probe above
was found; it is verbose, so leave the level at `info` otherwise.

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

A path that is partly new resolves its existing folders the same way and keeps the new
part as the client spelled it: a create of `photos\new.jpg` with `Photos` there makes
`/Photos/new.jpg`. A folder on the way that does not exist is
`STATUS_OBJECT_PATH_NOT_FOUND` (MS-FSA 2.1.5.1), never made: the vfs's `OpenFile` and
`Rename` make missing parents for NFS and the browser, and the server checks first. A
rename's destination folder resolves the same way; a destination leaf that names another
file only by case is that file, so a rename without `ReplaceIfExists` collides with it and
one with it replaces it, never leaving two names that differ by case; a destination that
differs only by case from the file itself renames it. Until 2026-10-12 any partly new path
was taken as typed, and a wrong-case parent became a second folder beside the first
(I-233).

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

A drive letter belongs to the token that mapped it: under UAC an administrator's
elevated window and their normal one are two logon sessions, so a `T:` mapped from an
elevated window ("Run as administrator", the window `tailraid install` needs) is invisible
to Explorer, which runs unelevated. `tailraid mount` therefore checks its own token
(`TokenElevation`) and, when elevated, prints a warning naming the letter and the way out
(`net use T: /delete` there, then `tailraid mount` from a normal window) before mapping
anyway: a script or service that wants the elevated mapping still gets it.

A mapping needs a logon session the SMB redirector can attach credentials to: a
password or interactive logon has one, a key-authenticated SSH logon does not and fails
with error 1312 whatever the password (docs/vms.md).

**The drive at logon.** A mapping belongs to the logon session that made it, and it is
not persistent, so it is gone after a reboot or a sign-out; with UAC on, an elevated
prompt is a session of its own, and a drive it maps is invisible to Explorer, which runs
unelevated. `install` therefore registers a scheduled task, `tailraid-mount`, for the
installing user: triggered at that user's logon, run in the interactive session with the
user's limited token (Explorer's), it runs `tailraid mount --wait 5m --config <path>`
under `conhost.exe --headless`, so no console window opens; `install` starts it once so
the drive appears without signing out, and `uninstall` removes it. `--wait` maps only
once the gateway accepts a connection, since the service may start after the logon. A
`tailraid mount` from an elevated prompt warns first, as above, and maps as before. Verified on the Windows VM with UAC on (2026-10-10): after a
reboot the task started at the automatic logon, waited two minutes for the delayed-auto
service, and mapped the drive; the limited session saw it and the elevated one did not.

The SMB client keeps one set of credentials per server name, whatever the port: a session
that already has a connection to `\\127.0.0.1` under another user (Windows' own shares
opened in Explorer by that address, or an old mapping) cannot map the gateway, and
`New-SmbMapping` fails with system error 1219. `mount` then says which server and to
remove the other connection (`net use` lists it).

Whether the Windows redirector accepts `127.0.0.1` for a non-445 loopback target, or
needs `localhost` or the machine name, is unverified until the checklist runs
(`docs/windows-checklist.md`). On macOS `mount_smbfs //tailraid@127.0.0.1:20445/tailraid
~/TailRAID` and on Linux `mount -t cifs //127.0.0.1/tailraid <dir> -o port=20445,
username=tailraid,vers=3.0` mount the same share when `listen.smb` is set there.

## 7. Large copies and the caps

A WRITE that meets the WAL cap or `MaxDirty` waits for the flusher for up to `CapWait`
(20 s, docs/04 §3, I-79) before it is answered. It waits off the connection's read loop,
and outside the share lock for a backend that declares its positional writes safe
alongside its other calls (`ConcurrentWriter`; the adapter does, the overlay locking per
inode; a backend that does not keeps every WRITE under the share lock): a WRITE on a positional handle that has not finished within
`WriteGrace` (5 ms) is answered `STATUS_PENDING` with an AsyncId at once and gets its
real reply when the write returns, at most `AsyncWrites` (64) such WRITEs per connection,
a CLOSE of the handle waiting for its own (I-217). Windows sends every copy to a server
over one connection, so before this one copy's WRITE waiting at its cap held up every
other copy's CREATE, end of file and WRITE behind it, and seven copies at once through a
gateway whose flusher drained 16 MB/s left 18 files committed with their length and none
of their bytes (2026-10-12). The wait must stay inside the client's request timeout. When a write-path operation
does fail, the adapter logs why (I-218): the server records nothing of the statuses it
answers, and the incident above left only the client's "An unexpected network error
occurred" (`STATUS_UNEXPECTED_IO_ERROR`) to go on.

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
  delete while the gateway is stale, or of an entry under a protected folder (docs/02,
  I-154; the vfs answers `EPERM`), answers success and the file stays. The next listing
  shows it. SET_INFO disposition, which Explorer sends before the close, does carry the
  refusal: the adapter checks the protection there and answers `STATUS_ACCESS_DENIED`, so
  Explorer shows "Access denied" for a protected entry rather than a silent no-op. The
  one delete a protected folder takes is a failed copy's own cleanup: a handle whose
  CREATE created the file may mark it delete-on-close and the CLOSE trashes it (I-220),
  so Skip or Cancel in Explorer leaves no half-copied file behind; before 2026-10-12 it
  did, and the integrity panel showed them as files of holes.
- **No share modes or delete-pending.** A handle's share access is not enforced and a
  file marked delete-on-close stays openable until the CLOSE, which deletes it then by
  inode (I-225); a client that renamed it away in the meantime loses it, one that
  created a new file at its old name keeps that one. Across gateways the delete is a
  Trash with `Expect`, so the same holds whichever gateway the other client used.
- **A delete goes to the trash** (docs/02 §Apply rules): `DeleteFile` and `DeleteDir`
  propose `Trash` for every name but the ephemeral ones of docs/06 §1, and the share sees
  a delete: the name is `STATUS_OBJECT_NAME_NOT_FOUND`, the listing omits it, a create of
  the name succeeds, the share's free space waits for the purge. The browser UI (docs/13
  §4) restores it; the SMB gateway has no view of the trash.
- **Snapshots are a folder, not the Previous Versions tab.** `\.snapshots\<mirror>\<name>\`
  reads through the adapter as on NFS, read-only (`EROFS` reaches the wire as
  `STATUS_ACCESS_DENIED`, docs/07, I-185: the adapter opens a snapshot file read-only
  when the read-write open is refused, and `CanDelete` refuses a delete-on-close under
  it; an attribute-only SET_INFO is ignored there as everywhere on the share), and the
  browser UI lists a file's versions
  (docs/16 §6). Explorer's Previous Versions tab needs VSS over SMB
  (`FSCTL_SRV_ENUMERATE_SNAPSHOTS`, `@GMT-` path tokens), which the server does not
  implement; out of scope.
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
  NFS clients on macOS and Linux ride out a restart without help). A copy in flight does
  not survive the restart; `update --restart` therefore waits for the gateway to be idle
  first (docs/12 §5, I-221).

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
- **I-142** — On Windows, with `listen.smb` and `mount.path` set, the install plan registers
  the task `tailraid-mount` for the installing user, triggered at that user's logon,
  running in the interactive session with the limited token, executing
  `conhost.exe --headless "<exe>" mount --wait 5m --config "<config>"`, and starts it;
  the uninstall plan removes it and succeeds when it is absent. `mount --wait d` runs the
  mapping only once the gateway's address accepts a TCP connection and otherwise fails
  after `d` with an error naming the address. A mapping Windows refuses with system error
  1219 fails with an error naming the server and how to remove the other connection.
- **I-213** — A QUERY_INFO answer never exceeds the request's OutputBufferLength: an
  answer of n bytes to a buffer of b bytes is sent whole when n ≤ b, cut to its first b
  bytes with `STATUS_BUFFER_OVERFLOW` when the class's fixed part fits in b < n, and
  refused with `STATUS_INFO_LENGTH_MISMATCH` and no data when b is shorter than the fixed
  part; for every file and filesystem class the server answers.

- **I-217** — A WRITE waiting in the gateway delays no other request (the share lock is
  skipped only for a `ConcurrentWriter` backend, as the adapter is): while one handle's
  WRITE waits (at a cap, on a fsync), the same connection's CREATE, SET_INFO, READ, WRITE
  and CLOSE of other handles are answered; a slow WRITE is answered `STATUS_PENDING` and
  then exactly once with its real status under the same MessageId and AsyncId, signed
  with its own session's key; a CLOSE answers only after every WRITE of its handle has;
  and files copied concurrently on one connection, each created, sized with SET_INFO
  end-of-file and written front to back while the flusher runs, read back byte for byte.

- **I-224** — A rename through SET_INFO without `ReplaceIfExists` onto an existing name,
  whichever gateway created it, is `STATUS_OBJECT_NAME_COLLISION` and changes nothing
  (the FSM's `Rename` with `NoReplace` is `ErrExist`, codec 9; before it, the gateway's
  own lookup); with it the replaced file goes to
  the trash as any rename over (docs/02); and after a rename every open handle of that
  file on the share, of any session, answers QUERY_INFO, READ and a whole-file read for
  the file at its new name, never for whatever then holds the old one.
- **I-225** — A delete-on-close removes the file its handle opened or nothing: the CLOSE
  (and the own cleanup of I-220) proposes `Trash` of the handle's current path with the
  handle's inode as `Expect` while every live member reads codec 9, and checks the name
  still resolves to that inode before proposing otherwise; when another file holds the
  name the delete is skipped and logged (I-218), so a file created at the old name by
  another session or gateway survives and a protected folder never loses a file through
  another file's own cleanup.
- **I-227** — `FSCTL_QUERY_FILE_REGIONS` on a file is answered as [MS-FSA's FSCTL_QUERY_FILE_REGIONS](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-fsa/acadee02-c40d-4f73-92d0-0f10e866e11f)
  computes it from the file's valid data length V and end of file E: with no input the
  range is [0, MAXLONGLONG) with usage `FILE_REGION_USAGE_VALID_CACHED_DATA`; an input
  shorter than `FILE_REGION_INPUT` (24 bytes) or an output buffer shorter than
  `FILE_REGION_OUTPUT` with one region (40 bytes) is `STATUS_BUFFER_TOO_SMALL`; a length
  ≤ 0, an offset plus length past 63 bits, a usage without the cached-data flag or a
  directory handle is `STATUS_INVALID_PARAMETER`; an offset past E, or at E with E > 0,
  is success with no output; otherwise the regions are [offset, min(V, end)) with the
  asked usage when offset < V, then [max(offset, V), min(E, end)) with usage 0, the
  second answered `STATUS_BUFFER_OVERFLOW` with the total count when it does not fit. V
  never exceeds E and is never below the end of a byte that reads as anything but zero:
  a file created and sized without a write has V = 0, a write at offset o of n bytes
  makes V ≥ o + n until a truncate below it, before and after its flush; a file whose
  handle cannot report V has the FSCTL refused with `STATUS_INVALID_DEVICE_REQUEST`.
- **I-229** — A delete-on-close by the handle whose CREATE made the file, of a file that
  holds no data (its valid data length is 0: no committed chunk but holes and no dirty
  byte, so every byte reads as zero), removes the name and then purges the inode, so the
  trash does not grow; a file into which any byte other than zero was written, at any
  offset, before or after its flush, goes to the trash as every other delete (I-220), and
  a protected folder keeps it in the trash. The name is free again for the second pass.
- **I-228** — An IOCTL whose Flags are not `SMB2_0_IOCTL_IS_FSCTL` is
  `STATUS_NOT_SUPPORTED`; an FSCTL the server does not implement is
  `STATUS_INVALID_DEVICE_REQUEST`; a request with a non-zero InputCount whose InputOffset
  is inside the fixed header, not a multiple of 8, or whose input runs past the message is
  `STATUS_INVALID_PARAMETER`; an answer never carries more output than
  MaxOutputResponse, and an answer cut to fit carries the bytes that fit with
  `STATUS_BUFFER_OVERFLOW` ([MS-SMB2 §3.3.5.15](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/c348a671-7750-49f8-9e61-cc59fa1a5701)).
- **I-233** — A CREATE or a rename's destination spelled with existing folders in another
  case than theirs lands in those folders and never makes a folder or file that differs
  from an existing one only by case; a folder on the way that does not exist is
  `STATUS_OBJECT_PATH_NOT_FOUND` and nothing is made; a rename onto another file's name in
  another case collides without `ReplaceIfExists` and replaces that file with it; a rename
  that changes only the case of the file's own name renames it.
- **I-235** — SET_INFO `FileAllocationInformation` with a size at or above the file's
  leaves the file's size and bytes as they are; with a size below it the file ends there.
- **I-236** — Marking a folder that holds an entry delete-on-close (SET_INFO disposition
  or the CREATE option) is `STATUS_DIRECTORY_NOT_EMPTY`, and the folder and its entries
  stay; an empty folder is deleted.
- **I-237** — A QUERY_DIRECTORY pattern matches a name exactly as MS-FSA 2.1.4.4's
  wildcards do, without case: `*` and `*.*` every name, `*` any run, `?` one character,
  `<` any run up to and including the name's last dot, `>` one character or none at a
  dot or the end, `"` a dot or nothing at the end, every other character itself.
- **I-240** — An open whose client never sends its CLOSE is closed when the client's
  connection ends, when its session logs off and when its tree disconnects, exactly as a
  CLOSE would: its delete-on-close removes the file (by inode, I-225), its locks are
  released, and the handle answers nothing after a logoff or a tree disconnect.
- **I-254** — A READ through a positional handle runs off the connection's read loop:
  one that finishes within the grace is answered as any other, a slower one
  `STATUS_PENDING` at once and its data later under the same MessageId and AsyncId, at
  most `asyncReads` (32) per connection; a CLOSE waits for the handle's READs as for its
  WRITEs; a CANCEL of a waiting READ is answered `STATUS_CANCELLED`, a read changing
  nothing. A driver that declares `ConcurrentReads` (the tailraid adapter does) has its
  READs run under no share lock; any other keeps the share's read lock around them.
  Before, every READ ran on the read loop under the share's read lock: one cold chunk's
  network fetch held up every other request on the connection, and with a CREATE, CLOSE
  or rename waiting for the lock, every new request on the share.
- **I-265** — A CREATE of an existing file whose data access (read, write, append,
  execute, delete; generic rights spelled out, MAXIMUM_ALLOWED as all) another open of
  the file on this share does not share, or whose share access does not admit the data
  access another such open holds, is refused with `STATUS_SHARING_VIOLATION` before it
  changes anything (MS-FSA 2.1.5.1.2.1); an open for attributes only never conflicts.
  The opens compared are this gateway's, of every connection and session; opens through
  another gateway or NFS are not seen. A rename or delete of a file another open holds
  therefore needs that open to share delete, as on Windows (Explorer's opens do; a
  program that holds a document without it makes the rename "file in use"). Before,
  share access was not read: two programs that each opened a file without sharing both
  got it, and an overwrite truncated a file another held exclusively.
- **I-247** — A CANCEL of a WRITE answered `STATUS_PENDING` takes nothing back: the
  write is already in the driver, so the CANCEL is answered nothing and the WRITE its real
  reply with the bytes it wrote (MS-SMB2 3.3.5.16); a lock wait or a change notification
  is still cancelled with `STATUS_CANCELLED`. In a chain, only a request that failed (an
  error severity) stops the related requests after it; one that answered a warning, such
  as `STATUS_BUFFER_OVERFLOW`, is a success with partial output and the chain goes on
  (MS-SMB2 3.3.5.2.7.2). Before, a cancelled WRITE was answered `STATUS_CANCELLED` while
  its bytes landed, and a CLOSE chained after an overflowing QUERY_INFO was skipped,
  leaving the handle open.
- **I-218** — Every failure the SMB gateway answers for a write, an end-of-file change, a
  create, a delete (a refusal by a protected folder included) or a rename is logged at
  Warn with the operation, the path and the error, at most one line a minute per kind of
  operation, the next line carrying the count it swallowed; a lookup that misses (a
  client probing whether a name exists) is not such a failure and logs nothing.

## Tests

| Invariant | Tests |
|---|---|
| I-106 | `TestSMB_WriteSequence_MultiRequest`, `TestSMB_WriteSize_Boundaries`, `TestVFS_WriteAt_ExactOffset_PosUnchanged` |
| I-107 | `TestSMB_SetInfo_EndOfFile`, `TestSMB_RenameKeepsFileIdAndContent`, `TestSMB_Times_FromFSMAndSetInfo` |
| I-108 | `TestSMB_QueryDirectory_LargeDirectory`, `TestSMB_RenameKeepsFileIdAndContent` |
| I-109 | `TestSMB_ReadOnly_WireStatuses`, `TestSMB_WriteWhenFull_IsDiskFull`, `TestSMB_WriteAtCap_WaitsThenSucceeds`, `TestSMB_Statfs_FromUsage` |
| I-110 | `TestSMB_Auth_WrongPasswordRefused`, `TestSMB_ListenRefusesNonLoopback`, `TestConfig_Gateways`, `TestCLI_Init_GeneratesSMBPassword` |
| I-141 | `TestSMB_DotFilesHidden` |
| I-142 | `TestSvc_Windows_LogonMount`, `TestMount_WaitGateway`, `TestMount_Explain1219` |
| I-213 | `TestQueryInfo_OutputBufferLength` |
| I-224 | `TestFSM_Rename_NoReplace_Exist`, `TestEncodeCommand_RewriteIsCodec9`, `TestRename_NoReplace_Collision`, `TestRename_OtherHandlesFollow`, `TestSMB_TwoSessions_RenameMovesOtherHandle`, `TestSoak_MultiUserAgainstHarness` |
| I-225 | `TestFSM_Trash_Expect_Mismatch`, `TestEncodeCommand_RewriteIsCodec9`, `TestDeleteOnClose_RenamedAway_NewFileSurvives`, `TestSMB_OwnCleanup_ByInode_UnderProtection`, `TestSoak_MultiUserAgainstHarness` |
| I-217 | `TestSMB_ConcurrentCopies_EveryByteLands`, `TestSMB_WriteWaitingAtCap_DoesNotStallOtherFiles`, `TestWrite_Async_InterimThenFinal`, `TestClose_WaitsForItsWrites`, `TestSeveralClientsOneShare` |
| I-218 | `TestSMB_RefusedWrite_IsLogged` |
| I-227 | `TestQueryFileRegions_FSA`, `TestVFS_ValidDataLength`, `TestSMB_QueryFileRegions_ExplorerProbe` |
| I-228 | `TestIoctl_Statuses` |
| I-229 | `TestSMB_ScratchCreate_NotTrashed` |
| I-233 | `TestSMB_CaseFold_NoTwins` |
| I-240 | `TestSMB_DeleteOnClose_RunsOnDropLogoffTreeDisconnect` |
| I-254 | `TestRead_Async_InterimThenFinal_NoShareLock`, `TestRead_SerialDriver_KeepsTheShareLock`, `TestRead_Async_CloseWaits_CancelCancels`, `TestFS_DeclaresConcurrentReadsAndWrites` |
| I-265 | `TestCreate_ShareAccess`, `TestSMB_TwoSessions_RenameMovesOtherHandle`, `TestSoak_MultiUserAgainstHarness` |
| I-247 | `TestCancel_AsyncWrite_CompletesNotCancelled`, `TestChain_GoesOnAfterAWarning` |
| I-235 | `TestSetInfo_Allocation_NotEndOfFile` |
| I-236 | `TestSMB_DeleteNonEmptyFolder_NotEmpty` |
| I-237 | `TestWildcards` |
