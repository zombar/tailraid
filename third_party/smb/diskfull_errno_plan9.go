// SPDX-License-Identifier: BSD-3-Clause

package smb

// Plan 9's syscall has no errno numbers: its errors are strings, and none of
// them is one of the three diskfull_errno.go names. Nothing matches.
var diskFullErrnos []error
