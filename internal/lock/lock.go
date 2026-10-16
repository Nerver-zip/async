package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// LockedError is returned when a conversation lock file is actively held by a running process.
type LockedError struct {
	Provider  string
	SessionID string
	LockPath  string
}

func (e *LockedError) Error() string {
	return fmt.Sprintf("session %s (%s) is currently locked by an active agent process (%s); close or finish the session before syncing", e.SessionID, e.Provider, e.LockPath)
}

// IsLockedError reports whether err is or wraps a *LockedError.
func IsLockedError(err error) bool {
	var target *LockedError
	return errors.As(err, &target)
}

// CodexLockPath returns the native writer lock path for a Codex thread.
func CodexLockPath(codexHome, threadID string) string {
	return filepath.Join(codexHome, "thread-writer-locks", threadID+".lock")
}

// AGYLockPath returns the native presence lock path for an Antigravity CLI conversation.
func AGYLockPath(agyHome, convID string) string {
	return filepath.Join(agyHome, "presence", convID+".lock")
}

// LockPathFor returns the lock file path for a given provider ("codex" or "agy").
func LockPathFor(provider, providerHome, id string) (string, error) {
	switch provider {
	case "codex":
		return CodexLockPath(providerHome, id), nil
	case "agy":
		return AGYLockPath(providerHome, id), nil
	default:
		return "", fmt.Errorf("unsupported provider %q", provider)
	}
}

// CheckAvailable verifies that the lock file at lockPath is not held by another process.
// If the lock file does not exist, it returns nil without creating it.
func CheckAvailable(provider, sessionID, lockPath string) error {
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0o600)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		// Fallback to read-only open if permissions restrict write.
		f, err = os.Open(lockPath)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("opening lock file %s: %w", lockPath, err)
		}
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return &LockedError{
				Provider:  provider,
				SessionID: sessionID,
				LockPath:  lockPath,
			}
		}
		return fmt.Errorf("checking flock on %s: %w", lockPath, err)
	}

	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return nil
}

// IsLocked returns true if the lock file exists and holds an active kernel flock.
func IsLocked(provider, sessionID, lockPath string) bool {
	return IsLockedError(CheckAvailable(provider, sessionID, lockPath))
}

// AcquireExclusive creates/opens lockPath and acquires a non-blocking exclusive flock.
// The caller must invoke the returned release function when finished.
func AcquireExclusive(provider, sessionID, lockPath string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("creating lock dir for %s: %w", lockPath, err)
	}

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening lock file %s: %w", lockPath, err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, &LockedError{
				Provider:  provider,
				SessionID: sessionID,
				LockPath:  lockPath,
			}
		}
		return nil, fmt.Errorf("acquiring flock on %s: %w", lockPath, err)
	}

	released := false
	release := func() {
		if released {
			return
		}
		released = true
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	return release, nil
}
