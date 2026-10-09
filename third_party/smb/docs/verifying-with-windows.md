# Verifying with Windows

Windows is the client this server was written for, and the hardest one to put
in front of it. Two things make it hard, and neither is about SMB.

## Trap 1: the redirector has no port option

`\\host\share` always dials **445**. There is no `-o port=` and no syntax for
one, so a server on a high port is unreachable from Windows — and the guest's
own 445 belongs to the kernel's `srvnet` driver, which does not let go:
`Stop-Service LanmanServer -Force` reports `STOP_PENDING`, then `RUNNING`.

Under QEMU, slirp can provide the address instead. This makes `10.0.2.100:445`
*inside* the guest mean `127.0.0.1:4499` on the host, one process per
connection:

```sh
qemu-system-aarch64 … \
  -netdev "user,id=net0,hostfwd=tcp::2222-:22,guestfwd=tcp:10.0.2.100:445-cmd:/usr/bin/nc 127.0.0.1 4499" \
  -device virtio-net-pci,netdev=net0
```

A chardev target (`-tcp:127.0.0.1:4499`) carries only ONE connection; a client
opens more, so the `-cmd:` form is the one that works.

## Trap 2: `net use * < password.txt` authenticates with an EMPTY password

This looks like the way to keep a password off the command line:

```powershell
cmd /c 'net use Z: \\10.0.2.100\shared * /user:alice < C:\pw.txt'
```

It fails with **"System error 86 — The specified network password is not
correct"**, and the server is right to refuse it: `net use` does not read the
redirected stdin, and what reaches the wire is an NTLMv2 proof over an **empty**
password. Replaying the captured blob against the server's own `ntowfv2` is what
settled it — no user/domain/password combination matched except `password=""`.

A password must not go on a command line, so read it in the script and let the
API take it:

```powershell
$pw = (Get-Content C:\smbtest\alice.pw -Raw).Trim()
New-SmbMapping -LocalPath Z: -RemotePath \\10.0.2.100\shared `
               -UserName alice -Password $pw -Persistent $false
```

## The check

`docs/verify.ps1` is what was run. It maps as a writer, prints what the client
says it negotiated, lists, reads, copies 512 KiB, reads a subdirectory, writes,
renames, deletes — then maps as a reader and confirms the refusals:

```
===== what the client says it negotiated
ServerName ShareName Dialect Signed Encrypted
10.0.2.100 shared    3.0.2     True     False

===== copy 512 KiB out and hash it
2343F8FE1500D79361B375476E604A84E66EDE4D8507DBA1A7B019F2E2B9FE73   (= the host's)

===== bob writes -- must fail
Set-Content : The media is write protected.

===== bob on a share he is not allowed on -- must be refused
refused: Access is denied.
```

## Put a witness in the middle

Windows says "The handle is invalid" for a defect three messages earlier. A
proxy that prints each command and status — and, for SESSION_SETUP, the
security blob — is what turned that into a diagnosis. And put a **second
client** through the same path first: a Go client (`cloudsoda/go-smb2`) built
for `windows/arm64` and run in the same guest proves the network path before
Windows is asked to judge anything. Here it passed while the redirector failed,
which is what said "this is about Windows, not about slirp".
