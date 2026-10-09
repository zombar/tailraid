// SPDX-License-Identifier: BSD-3-Clause

package smb

// NT status codes. SMB2 carries them in the header, and a client reads them
// far more closely than any text: the difference between "no such file" and
// "not a directory" is what makes a file manager show an empty folder rather
// than an error.
const (
	statusSuccess          uint32 = 0x00000000
	statusPending          uint32 = 0x00000103
	statusNoMoreFiles      uint32 = 0x80000006
	statusNotImplemented   uint32 = 0xC0000002
	statusInvalidInfoClass uint32 = 0xC0000003
	statusInvalidParameter uint32 = 0xC000000D
	statusNoSuchFile       uint32 = 0xC000000F
	statusEndOfFile        uint32 = 0xC0000011
	// BUFFER_OVERFLOW is not an error: the high bit is a WARNING, and a
	// client reads the reply that comes with it and then asks for the rest.
	statusBufferOverflow       uint32 = 0x80000005
	statusMoreProcessing       uint32 = 0xC0000016
	statusAccessDenied         uint32 = 0xC0000022
	statusObjectNameNotFound   uint32 = 0xC0000034
	statusObjectNameCollision  uint32 = 0xC0000035
	statusObjectPathNotFound   uint32 = 0xC000003A
	statusSharingViolation     uint32 = 0xC0000043
	statusDeletePending        uint32 = 0xC0000056
	statusLogonFailure         uint32 = 0xC000006D
	statusBadNetworkName       uint32 = 0xC00000CC
	statusNotADirectory        uint32 = 0xC0000103
	statusFileIsADirectory     uint32 = 0xC00000BA
	statusDirectoryNotEmpty    uint32 = 0xC0000101
	statusNotSupported         uint32 = 0xC00000BB
	statusUserSessionDeleted   uint32 = 0xC0000203
	statusNetworkNameDeleted   uint32 = 0xC00000C9
	statusInvalidDeviceRequest uint32 = 0xC0000010
	statusDiskFull             uint32 = 0xC000007F
	statusMediaWriteProtected  uint32 = 0xC00000A2
	statusFileClosed           uint32 = 0xC0000128
	statusInvalidHandle        uint32 = 0xC0000008
	statusLockNotGranted       uint32 = 0xC0000055
	statusRangeNotLocked       uint32 = 0xC000007E
	statusFileLockConflict     uint32 = 0xC0000054
	statusCancelled            uint32 = 0xC0000120
	statusNotifyCleanup        uint32 = 0x0000010C
	statusNotifyEnumDir        uint32 = 0x0000010D
)
