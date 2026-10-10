//go:build !windows

package mount

// Elevated is false off Windows: drive letters are a Windows matter.
func Elevated() bool { return false }
