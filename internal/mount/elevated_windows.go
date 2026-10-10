package mount

import "golang.org/x/sys/windows"

// Elevated reports whether this process runs with an elevated (administrator) token.
func Elevated() bool { return windows.GetCurrentProcessToken().IsElevated() }
