//go:build windows

package lock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func flockExclusiveNB(f *os.File) error {
	var ol windows.Overlapped
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol)
}

func flockUnlock(f *os.File) error {
	var ol windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
}

func isFlockBlocked(err error) bool {
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return true
	}
	var errno windows.Errno
	if errors.As(err, &errno) && errno == windows.ERROR_LOCK_VIOLATION {
		return true
	}
	return false
}
