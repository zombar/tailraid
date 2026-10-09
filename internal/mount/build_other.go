//go:build !windows

package mount

// WindowsBuild is 0 off Windows: no build to check.
func WindowsBuild() uint32 { return 0 }
