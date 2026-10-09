// SPDX-License-Identifier: BSD-3-Clause

// Package smb implements an SMB2 server that exports any
// [github.com/go-filesystems/interface.Filesystem], in pure Go with
// CGO_ENABLED=0 and no dependency outside the standard library.
//
// It is the sibling of go-filesystems/nfs, /webdav and /sftp, and the one they
// cannot replace: SMB is what Windows speaks natively — its NFS client is an
// optional feature and its WebDAV redirector is fragile — and it is what the
// macOS Finder speaks best.
//
// # Why the package is not called cifs
//
// `mount -t cifs` is what a Linux user types, and this is the server it
// connects to. But CIFS names SMB 1, which Windows removes by default and
// which this server will not speak. Exactly one SMB1 frame is answered here:
// the legacy greeting, whose answer is "let us speak SMB2".
//
// That is not a reading of the specification, it is what the client on the
// desk did. macOS 26 opens with an SMB1 NEGOTIATE offering "NT LM 0.12",
// "SMB 2.002" and "SMB 2.???"; told to use SMB2, it offers 0x0202, 0x0210,
// 0x0300, 0x0302 and 0x0311, and nothing older.
//
// # What is implemented
//
// Dialects 2.1, 3.0 and 3.0.2; NTLMv2 authentication over SPNEGO or raw, as
// the client prefers; signing, with HMAC-SHA256 or AES-CMAC as the dialect
// requires; and the file operations a file manager performs: opening,
// reading, writing, listing, renaming, truncating and deleting.
//
// That is enough for `mount -t cifs` on Linux -- with no vers= at all -- and
// for mount_smbfs on macOS, which settles on 3.0.2 signed, and for the Windows
// redirector, which reports 3.0.2 with Signed True.
//
// Several requests can arrive in ONE message, and the ones after the first may
// say they are RELATED: they carry no session or tree of their own, and an
// all-ones file id means "the file the previous operation opened". A related
// request whose predecessor FAILED is refused with the predecessor's status
// rather than carried out -- there is no such file, and doing it anyway acts on
// whatever the connection opened last. Windows checks a rename's target with a
// compounded CREATE + CLOSE whose CREATE is meant to fail; carrying out that
// CLOSE shut the client's source handle and the rename came back "The handle
// is invalid".
//
// 3.1.1 is not here. It adds pre-authentication integrity and negotiate
// contexts, which change the shape of the exchange itself; naming it without
// them would promise what is not there. Nor is encryption.
//
// Byte-range locks are here, and enforced: a read crosses a shared lock and
// stops at an exclusive one, a write stops at either, and a handle never
// conflicts with itself. A client that asks to WAIT for one is promised an
// answer and gets it when the holder lets go, or STATUS_CANCELLED if it gives
// up first.
//
// Change notification is here too, on the same machinery, with one limit
// stated where it will be met: the changes reported are the ones that go
// THROUGH THIS SERVER. A file written into the image by something else is
// invisible, because nothing underneath tells us.
//
// Both work because a reply may now be sent later: an interim STATUS_PENDING
// with an AsyncId, the loop carrying on reading, and the real answer whenever
// it is ready. CANCEL ends one, and so does closing the handle it was taken
// on.
//
// A client can also ask what shares there ARE, rather than being told a name:
// TREE_CONNECT to IPC$, open \srvsvc, and call NetrShareEnum over DCE/RPC.
// The list is the shares this user may connect to -- see access.go -- and
// srvsvc.go is the three formats that carry it.
//
// Not here: alternate data streams, security descriptors, oplocks and leases
// (so a client caches nothing), and the other pipes a client may ask for
// (\wkssvc, \lsarpc). Each of those answers by name rather than by silence.
//
// # Serving one
//
//	fs, err := fat32.Open("disk.img", -1)
//	if err != nil {
//		return err
//	}
//	defer fs.Close()
//
//	srv := smb.New()
//	srv.AddUser("alice", "hunter2")
//	if err := srv.Share("disk", fs); err != nil {
//		return err
//	}
//	return srv.ListenAndServe("127.0.0.1:4445")
//
// Port 445 is the one a client dials without being told, and it needs
// privilege on every operating system -- so the examples use a high port, and
// the mount command names it:
//
//	mount_smbfs //alice@127.0.0.1:4445/disk /Volumes/disk          # macOS
//	mount -t cifs //127.0.0.1/disk /mnt -o port=4445,vers=2.1,...   # Linux
//
// # Where a password comes from
//
// [Server.AddUser] takes the password. [Server.AddUserHash] takes its MD4 --
// the "NT hash" -- which is what a directory keeps when it holds enough for
// SMB without holding the password: Samba's sambaNTPassword attribute, or a
// column beside it in a database.
//
// It exists because NTLMv2 is a challenge-response. The client never sends the
// password, so a server must compute MD4(UTF16LE(password)) itself -- which
// means an LDAP BIND cannot authenticate an SMB session, and neither can a
// bcrypt. Worth being plain about: the hash IS the credential, and anybody
// holding it can authenticate as that person exactly as if they held the
// password.
//
// # Who gets what
//
// A share with no lists on it is every authenticated user's, read-write.
// [AllowUsers] and [WriteUsers] narrow that, and compose:
//
//	srv.Share("photos", photos, smb.AllowUsers("alice", "bob"), smb.WriteUsers("alice"))
//
// Bob may connect and read; Carol is refused at TREE_CONNECT with
// ACCESS_DENIED, which a client shows as a permission rather than a missing
// share. [ReadOnly] outranks both.
//
// A reader is told so in the access mask of the reply that grants the share,
// not one refusal at a time: a client that was granted the write bits offers
// the actions and fails on each, which looks like a broken share.
//
// # When the disk is full
//
// A driver error that is, or wraps, ENOSPC, EDQUOT or EFBIG answers
// STATUS_DISK_FULL (0xC000007F), as Samba's map_nt_error_from_unix does.
// EDQUOT deliberately does not answer STATUS_QUOTA_EXCEEDED: Samba notes that
// Windows applications need DISK_FULL there.
package smb
