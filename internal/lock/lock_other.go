//go:build !linux

package lock

// findLockHolder is a stub on non-Linux platforms where /proc/locks is not available.
func findLockHolder(lockPath string) (int, string) {
	return 0, ""
}
