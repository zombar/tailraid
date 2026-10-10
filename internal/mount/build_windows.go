package mount

import "golang.org/x/sys/windows"

// WindowsBuild is the running Windows build number (26100 is 24H2).
func WindowsBuild() uint32 { return windows.RtlGetVersion().BuildNumber }
