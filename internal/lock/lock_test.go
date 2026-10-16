package lock

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCheckAvailableNonExistent(t *testing.T) {
	tmp := t.TempDir()
	lp := CodexLockPath(tmp, "01a127cf-7325-7a93-a4fc-63d244f874b5")
	if err := CheckAvailable("codex", "01a127cf-7325-7a93-a4fc-63d244f874b5", lp); err != nil {
		t.Fatalf("expected nil for missing lock file, got: %v", err)
	}
	if _, err := os.Stat(lp); !os.IsNotExist(err) {
		t.Fatalf("CheckAvailable should not create missing lock file")
	}
}

func TestLockContentionAndRelease(t *testing.T) {
	// Use small grace period for fast test execution.
	origGrace := DefaultGracePeriod
	origPoll := DefaultPollInterval
	DefaultGracePeriod = 200 * time.Millisecond
	DefaultPollInterval = 20 * time.Millisecond
	defer func() {
		DefaultGracePeriod = origGrace
		DefaultPollInterval = origPoll
	}()

	tmp := t.TempDir()
	sessionID := "e1e555ab-8771-4848-bb6c-5404c45a9cd6"
	lp := AGYLockPath(tmp, sessionID)

	release, err := AcquireExclusive("agy", sessionID, lp)
	if err != nil {
		t.Fatalf("AcquireExclusive failed: %v", err)
	}
	defer release()

	if !IsLocked("agy", sessionID, lp) {
		t.Fatalf("expected IsLocked to return true while lock is held")
	}

	err = CheckAvailableInstant("agy", sessionID, lp)
	if err == nil {
		t.Fatalf("expected LockedError when lock is held, got nil")
	}
	if !IsLockedError(err) {
		t.Fatalf("expected IsLockedError(err) == true, got %T: %v", err, err)
	}

	var lockedErr *LockedError
	if errors.As(err, &lockedErr) {
		if runtime.GOOS == "linux" {
			if lockedErr.HolderPID != os.Getpid() {
				t.Errorf("expected HolderPID %d, got %d", os.Getpid(), lockedErr.HolderPID)
			}
		}
	}

	if _, err2 := AcquireExclusive("agy", sessionID, lp); !IsLockedError(err2) {
		t.Fatalf("second AcquireExclusive should fail with LockedError, got: %v", err2)
	}

	// Release the lock and verify it becomes available immediately.
	release()

	if IsLocked("agy", sessionID, lp) {
		t.Fatalf("expected IsLocked to return false after release")
	}
	if err := CheckAvailableInstant("agy", sessionID, lp); err != nil {
		t.Fatalf("CheckAvailableInstant after release failed: %v", err)
	}
}

func TestCheckAvailableGracePeriodSucceeds(t *testing.T) {
	tmp := t.TempDir()
	sessionID := "test-grace-session"
	lp := CodexLockPath(tmp, sessionID)

	release, err := AcquireExclusive("codex", sessionID, lp)
	if err != nil {
		t.Fatalf("AcquireExclusive failed: %v", err)
	}

	// Release the lock asynchronously after 60ms to simulate a graceful shutdown.
	go func() {
		time.Sleep(60 * time.Millisecond)
		release()
	}()

	// With a timeout of 500ms, CheckAvailableWithTimeout should wait and succeed.
	err = CheckAvailableWithTimeout("codex", sessionID, lp, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("expected lock to clear within grace period, got: %v", err)
	}
}

func TestLockedErrorFormatting(t *testing.T) {
	errWithoutPID := &LockedError{
		Provider:  "codex",
		SessionID: "sess-1",
		LockPath:  "/tmp/sess-1.lock",
	}
	if !strings.Contains(errWithoutPID.Error(), "active agent process (/tmp/sess-1.lock)") {
		t.Errorf("unexpected error format without PID: %s", errWithoutPID.Error())
	}

	errWithPID := &LockedError{
		Provider:  "codex",
		SessionID: "sess-2",
		LockPath:  "/tmp/sess-2.lock",
		HolderPID: 12345,
		HolderCmd: "codex",
	}
	if !strings.Contains(errWithPID.Error(), "PID 12345 (codex)") {
		t.Errorf("unexpected error format with PID: %s", errWithPID.Error())
	}
}
