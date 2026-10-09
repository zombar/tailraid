// SPDX-License-Identifier: BSD-3-Clause

//go:build !plan9

package smb

import "syscall"

// diskFullErrnos are the errors a filesystem returns when a write cannot be
// stored for want of room, and which a client must read as STATUS_DISK_FULL.
//
// The set is Samba's (source3/lib/errmap_unix.c, unix_nt_errmap):
// ENOSPC, EDQUOT and EFBIG all map to NT_STATUS_DISK_FULL there -- EDQUOT
// with the note "Windows apps need this, not NT_STATUS_QUOTA_EXCEEDED".
// Windows clients are tested against Samba, so this server says the same.
var diskFullErrnos = []error{syscall.ENOSPC, syscall.EDQUOT, syscall.EFBIG}
