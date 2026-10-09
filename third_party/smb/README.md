<p align="center"><img src="https://raw.githubusercontent.com/go-filesystems/brand/main/social/go-filesystems-smb.png" alt="go-filesystems/smb" width="720"></p>

# smb

A pure-Go **SMB2** server that exports any
[`go-filesystems`](https://github.com/go-filesystems) `Filesystem`, so a disk
image can be mounted by the file manager of Windows, macOS or Linux with
nothing installed on the client side.

Sibling of [`nfs`](https://github.com/go-filesystems/nfs),
[`webdav`](https://github.com/go-filesystems/webdav) and
[`sftp`](https://github.com/go-filesystems/sftp), and the one those three
cannot replace: SMB is what Windows speaks natively, and what macOS's Finder
speaks best.

## Why not "cifs"

`mount -t cifs` is what a Linux user types, and this is the server it connects
to — but CIFS names SMB **1**, which Windows removes by default and which this
server will not speak. The one SMB1 frame it answers is the legacy greeting,
and the answer is "let us speak SMB2".

Measured on macOS 26's client: it opens with an SMB1 `NEGOTIATE` offering
`NT LM 0.12`, `SMB 2.002` and `SMB 2.???`, and once told to use SMB2 it offers
dialects `0x0202`, `0x0210`, `0x0300`, `0x0302` and `0x0311` — no CIFS in
sight.

## Status

**A share mounts, and you can work in it.**

Verified with the macOS kernel client on macOS 26 — `mount_smbfs`, then `ls`,
`cat`, a write, and a 512 KiB copy whose sha256 matches — over a FAT32 image
served by [`go-filesystems/fat32`](https://github.com/go-filesystems/fat32).
`smbutil statshares` reports `SMB_3.0.2` and `SIGNING_SUPPORTED TRUE`.

And with the Linux kernel client in CI, which mounts it with **no dialect
named**:

```
//127.0.0.1/disk on /mnt type cifs (rw,vers=default,username=alice,…)
```

**And with Windows** — the client this was written for. Windows 11 ARM64 25H2,
`New-SmbMapping`, then `dir`, `type`, a subdirectory, a write, a **rename**, a
delete, and a 512 KiB copy whose sha256 matches the host's. The client reports
what it negotiated:

```
ServerName ShareName Dialect Signed Encrypted
---------- --------- ------- ------ ---------
10.0.2.100 IPC$      3.0.2     True     False
10.0.2.100 shared    3.0.2     True     False
```

**Large reads and writes.** The server claims multi-credit operations
(`SMB2_GLOBAL_CAP_LARGE_MTU`, since v0.6.0) for 2.1 and 3.x, so a client
reads and writes up to `MaxReadSize`/`MaxWriteSize` (1 MiB) per request.
Without the claim, go-smb2 and the Linux kernel client both hold every
request to 64 KiB. A request whose `CreditCharge` does not pay for what it
sends or asks for is refused with `STATUS_INVALID_PARAMETER`
(MS-SMB2 3.3.5.2.5). Measured through go-fileshare/fileshare: about ×2.2.

**Server-side copy.** `FSCTL_SRV_REQUEST_RESUME_KEY` and
`FSCTL_SRV_COPYCHUNK`/`_WRITE` are answered (since v0.7.0), so a copy within
a share -- Explorer's, or the Linux client's `copy_file_range(2)` -- is made on
the server through [`hostcopy`](https://github.com/go-filesystems/hostcopy):
a megabyte at a time, or `copy_file_range(2)` in the kernel between two files
of the host, which shares blocks on btrfs and XFS. The bytes never cross the
network. A request is held to Windows' limits (256 chunks, 1 MiB each, 16 MiB
in all), a resume key names an open of the same session only, and byte-range
locks hold as they do for READ and WRITE.

`Signed True` is why signing is implemented: Windows requires it. The per-user
lists hold there too — a reader's write comes back as "The media is write
protected", and a share `allow` does not name them as "Access is denied". See
[docs/verifying-with-windows.md](docs/verifying-with-windows.md) for the recipe
and the **two traps** that make this hard to do at all.

| | |
|---|---|
| dialect negotiation | including the 1996 greeting a modern client still opens with |
| NTLMv2 over SPNEGO | the password never leaves the server |
| opening, reading, writing | positional through `Opener`/`WritableFile`, whole-file where a driver has neither |
| listing, renaming, truncating, deleting | including the chained requests macOS sends on every open, and the ones Windows sends whose FIRST operation is meant to fail |
| signing | **HMAC-SHA256** for 2.x, **AES-CMAC** for 3.x, both implemented here |
| dialects | 2.1, 3.0 and 3.0.2 — Linux mounts with no `vers=` at all, macOS settles on 3.0.2 |
| encryption and 3.1.1 | **not yet** — 3.1.1 changes the shape of the exchange, and naming it without pre-authentication integrity would promise what is not there |
| byte-range locks | taken, released and **enforced** on reads and writes, including *waiting* for one |
| change notification | on changes that go **through this server** — one made in the image by something else is invisible, because nothing underneath tells us |
| asynchronous replies | `STATUS_PENDING` with an AsyncId, and `CANCEL` |
| per-user access | who may connect (`allow`) and who may write (`writers`), per share — a reader is told so in the access mask rather than one refusal at a time |
| share enumeration | `NetrShareEnum` over DCE/RPC on `\srvsvc`, listing what **this user** may connect to — verified against Samba's own client |
| streams, oplocks and leases | **not yet**, and each answers by name rather than by silence |

Enumeration is verified with Samba's `smbclient -L`, which is the reference
implementation of the client side. macOS cannot judge it: `smbutil view` binds
with `ncacn_np:HOST[\pipe\srvsvc]`, and a named-pipe binding has **no port
field**, so it dials 445 whatever the URL said — against a server on a high
port it logs `RPC to srvsrvc gave error 0x16c9a034`, falls back to the SMB1
`\PIPE\LANMAN` call, and prints "unable to list resources: Broken pipe". A
proxy between the two shows the tree connect to IPC$ and then nothing: the
pipe is never opened. Serving on 445 needs privilege, so that check is a
person's to run.

Windows found one defect nothing else could, and it is the reason to test
against an operating system rather than a library: **a chained request whose
predecessor failed was carried out anyway**. Windows checks that a rename's
target name is free with a compounded CREATE + CLOSE in one message, and the
CREATE is *supposed* to fail. The CLOSE that follows carries an all-ones file
id — "the file the previous operation opened" — and with no such file it
resolved to whatever the connection opened last: the source file, still held by
the client. The next `SET_INFO` came back `FILE_CLOSED` and the rename failed
with "The handle is invalid", about a handle the server had shut behind the
client's back. macOS and Linux never send a chain whose first operation fails,
and the Go client never chains at all.

What is still unchecked: this ran through a QEMU guest-forward rather than on
port 445 itself, and encryption and 3.1.1 are absent whatever the client is.

## Serving one from the command line

The command lives in its own product now:
**[`go-fileshare/fileshare`](https://github.com/go-fileshare/fileshare)**, which
serves the same images over SMB, NFS and WebDAV from one configuration — the
same users, the same per-share access, in one place.

```sh
go install -tags nonfs,nowebdav github.com/go-fileshare/fileshare@latest   # SMB only

fileshare --image disk.img --user alice --password-file pw   # one image, now
fileshare --config /etc/fileshare.d                          # several, with users
fileshare check /etc/fileshare.d                             # before restarting it
```

`cmd/smb-server` used to live here and has been removed. Two reasons, and the
second is the one that matters:

- **It could never be installed.** Its go.mod carried
  `replace github.com/go-filesystems/smb => ../..` so that it always built
  against this library's HEAD — and `go install pkg@latest` refuses a module
  with a replace directive outright. The line in this README telling people to
  run it was wrong for its whole life.
- **A person wants to share an image, not to run the SMB one.** Which protocol
  carries it is a property of the client at the other end. One command that
  serves an image over SMB, NFS and WebDAV — with one set of users and one set
  of access rules — is the thing that was actually wanted, and a per-protocol
  command is that thing minus two protocols.

Building only SMB into it is a build tag, so the binary is not carrying what
you did not ask for.

## Serving one from Go

```go
fs, err := fat32.Open("disk.img", -1)
if err != nil {
	return err
}
defer fs.Close()

srv := smb.New()
srv.AddUser("alice", "hunter2")
if err := srv.Share("disk", fs); err != nil {
	return err
}
return srv.ListenAndServe("127.0.0.1:4445")
```

Port 445 is the one a client dials without being told, and it needs privilege
on every operating system — so the examples use a high port, and the mount
command names it.

### The size a client is told

The filesystem interface has no `statfs`, so a share reports a placeholder
size (4 GiB, 2 GiB free) unless the caller says otherwise.
`smb.WithCapacity(total, avail)` sets fixed numbers. `smb.WithCapacityFunc(f)`
asks `f` at every size query (`FileFsSizeInformation`,
`FileFsFullSizeInformation`), for a share whose free space or quota changes
while it is served. `f` must be safe for concurrent use and must not block.

```go
var total, avail atomic.Uint64 // kept up to date elsewhere
srv.Share("data", fs, smb.WithCapacityFunc(func() (uint64, uint64) {
	return total.Load(), avail.Load()
}))
```

## Licence

BSD-3-Clause.
