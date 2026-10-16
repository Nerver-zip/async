package lock

import (
	"os"
	"testing"
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

	err = CheckAvailable("agy", sessionID, lp)
	if err == nil {
		t.Fatalf("expected LockedError when lock is held, got nil")
	}
	if !IsLockedError(err) {
		t.Fatalf("expected IsLockedError(err) == true, got %T: %v", err, err)
	}

	if _, err2 := AcquireExclusive("agy", sessionID, lp); !IsLockedError(err2) {
		t.Fatalf("second AcquireExclusive should fail with LockedError, got: %v", err2)
	}

	// Release the lock and verify it becomes available immediately.
	release()

	if IsLocked("agy", sessionID, lp) {
		t.Fatalf("expected IsLocked to return false after release")
	}
	if err := CheckAvailable("agy", sessionID, lp); err != nil {
		t.Fatalf("CheckAvailable after release failed: %v", err)
	}
}
