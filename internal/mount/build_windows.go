package mount

import "golang.org/x/sys/windows"

// WindowsBuild is the running Windows build number (26100 is 24H2).
func WindowsBuild() uint32 { return windows.RtlGetVersion().BuildNumber }

// Elevated reports an elevated (administrator) process: a drive it maps belongs to the
// elevated logon session, which Explorer does not see (docs/15 §6).
func Elevated() bool { return windows.GetCurrentProcessToken().IsElevated() }
