//go:build linux

package admin

// platformEncryption follows the mount under path through /proc and /sys.
func platformEncryption(path string) Encryption {
	return linuxEncryption(path, "/proc/self/mountinfo", "/sys/class/block")
}
