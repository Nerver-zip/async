package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DefaultGracePeriod is the maximum time CheckAvailable will wait for a transient lock
// (e.g. an agent process currently performing graceful shutdown after terminal exit) to clear.
var DefaultGracePeriod = 1500 * time.Millisecond

// DefaultPollInterval is the interval between lock checks during the grace period.
var DefaultPollInterval = 100 * time.Millisecond

// LockedError is returned when a conversation lock file is actively held by a running process.
type LockedError struct {
	Provider  string
	SessionID string
	LockPath  string
	HolderPID int
	HolderCmd string
}

func (e *LockedError) Error() string {
	var proc string
	if e.HolderPID > 0 {
		proc = fmt.Sprintf("PID %d", e.HolderPID)
		if e.HolderCmd != "" {
			proc += fmt.Sprintf(" (%s)", e.HolderCmd)
		}
	} else {
		proc = "an active agent process"
	}

	hint := ""
	if e.Provider == "codex" {
		hint = " (if you recently closed the terminal, Codex's background daemon keeps an idle hold on the session for ~60s before releasing it; please wait a moment and retry)"
	}

	return fmt.Sprintf("session %s (%s) is currently locked by %s (%s); close or finish the session before syncing%s",
		e.SessionID, e.Provider, proc, e.LockPath, hint)
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
// If the lock is held, it retries up to DefaultGracePeriod to allow transient graceful
// shutdowns (such as terminal closure) to complete before failing.
// If the lock file does not exist, it returns nil without creating it.
func CheckAvailable(provider, sessionID, lockPath string) error {
	return CheckAvailableWithTimeout(provider, sessionID, lockPath, DefaultGracePeriod)
}

// CheckAvailableInstant verifies the lock immediately without waiting for any grace period.
func CheckAvailableInstant(provider, sessionID, lockPath string) error {
	return CheckAvailableWithTimeout(provider, sessionID, lockPath, 0)
}

// CheckAvailableWithTimeout tries to verify that the lock file at lockPath is available,
// retrying up to timeout with DefaultPollInterval if it is initially held.
func CheckAvailableWithTimeout(provider, sessionID, lockPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := tryCheckAvailable(provider, sessionID, lockPath)
		if err == nil {
			return nil
		}
		if !IsLockedError(err) || timeout <= 0 || time.Now().After(deadline) {
			return err
		}
		time.Sleep(DefaultPollInterval)
	}
}

func tryCheckAvailable(provider, sessionID, lockPath string) error {
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

	if err := flockExclusiveNB(f); err != nil {
		if isFlockBlocked(err) {
			pid, cmd := findLockHolder(lockPath)
			return &LockedError{
				Provider:  provider,
				SessionID: sessionID,
				LockPath:  lockPath,
				HolderPID: pid,
				HolderCmd: cmd,
			}
		}
		return fmt.Errorf("checking flock on %s: %w", lockPath, err)
	}

	_ = flockUnlock(f)
	return nil
}

// IsLocked returns true if the lock file exists and holds an active kernel flock.
// It checks instantly without waiting for any grace period.
func IsLocked(provider, sessionID, lockPath string) bool {
	return IsLockedError(CheckAvailableInstant(provider, sessionID, lockPath))
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

	if err := flockExclusiveNB(f); err != nil {
		_ = f.Close()
		if isFlockBlocked(err) {
			pid, cmd := findLockHolder(lockPath)
			return nil, &LockedError{
				Provider:  provider,
				SessionID: sessionID,
				LockPath:  lockPath,
				HolderPID: pid,
				HolderCmd: cmd,
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
		_ = flockUnlock(f)
		_ = f.Close()
	}
	return release, nil
}
