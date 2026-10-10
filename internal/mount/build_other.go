//go:build !windows

package mount

// WindowsBuild is 0 off Windows: no build to check.
func WindowsBuild() uint32 { return 0 }

// Elevated is false off Windows: a mount there is not per logon session.
func Elevated() bool { return false }
