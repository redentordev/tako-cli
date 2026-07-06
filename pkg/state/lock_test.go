package state

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestStateLockAcquireBlocksSecondActiveHolder(t *testing.T) {
	dir := t.TempDir()
	first := NewStateLock(dir)
	lockInfo, err := first.Acquire("deploy")
	if err != nil {
		t.Fatalf("first Acquire returned error: %v", err)
	}
	t.Cleanup(func() { _ = first.ForceRelease() })

	second := NewStateLock(dir)
	if _, err := second.Acquire("deploy"); err == nil {
		t.Fatal("second Acquire succeeded while first holder is active")
	}

	if err := first.Release(lockInfo); err != nil {
		t.Fatalf("Release returned error: %v", err)
	}
	if first.IsLocked() {
		t.Fatal("lock is still reported active after release")
	}
}

func TestStateLockAcquireReplacesStaleLockFile(t *testing.T) {
	dir := t.TempDir()
	lock := NewStateLock(dir)
	stale := &LockInfo{
		ID:        "stale-lock",
		Operation: "deploy",
		Who:       "tester@example",
		Created:   time.Now().Add(-LockTimeout - time.Minute),
		PID:       1,
	}
	writeLockFileForTest(t, lock, stale)

	if lock.IsLocked() {
		t.Fatal("stale lock should not be reported active")
	}
	acquired, err := lock.Acquire("deploy")
	if err != nil {
		t.Fatalf("Acquire returned error for stale lock: %v", err)
	}
	t.Cleanup(func() { _ = lock.ForceRelease() })
	if acquired.ID == stale.ID {
		t.Fatalf("Acquire reused stale lock ID %q", acquired.ID)
	}
	current, err := lock.GetLockInfo()
	if err != nil {
		t.Fatalf("GetLockInfo returned error: %v", err)
	}
	if current == nil || current.ID != acquired.ID {
		t.Fatalf("current lock = %#v, want acquired ID %q", current, acquired.ID)
	}
}

func TestStateLockForceReleaseClearsActiveAndStaleLocks(t *testing.T) {
	t.Run("active", func(t *testing.T) {
		dir := t.TempDir()
		lock := NewStateLock(dir)
		if _, err := lock.Acquire("deploy"); err != nil {
			t.Fatalf("Acquire returned error: %v", err)
		}
		if !lock.IsLocked() {
			t.Fatal("lock should be active before ForceRelease")
		}
		if err := lock.ForceRelease(); err != nil {
			t.Fatalf("ForceRelease returned error: %v", err)
		}
		if lock.IsLocked() {
			t.Fatal("lock still active after ForceRelease")
		}
		second := NewStateLock(dir)
		if _, err := second.Acquire("deploy"); err != nil {
			t.Fatalf("Acquire after ForceRelease returned error: %v", err)
		}
		_ = second.ForceRelease()
	})

	t.Run("stale", func(t *testing.T) {
		dir := t.TempDir()
		lock := NewStateLock(dir)
		writeLockFileForTest(t, lock, &LockInfo{
			ID:        "stale-lock",
			Operation: "deploy",
			Who:       "tester@example",
			Created:   time.Now().Add(-LockTimeout - time.Minute),
			PID:       1,
		})
		if err := lock.ForceRelease(); err != nil {
			t.Fatalf("ForceRelease returned error: %v", err)
		}
		if info, err := lock.GetLockInfo(); err != nil || info != nil {
			t.Fatalf("lock info after ForceRelease = %#v, %v; want nil, nil", info, err)
		}
	})
}

func TestStateLockReleaseMismatchedIDFails(t *testing.T) {
	dir := t.TempDir()
	lock := NewStateLock(dir)
	lockInfo, err := lock.Acquire("deploy")
	if err != nil {
		t.Fatalf("Acquire returned error: %v", err)
	}
	t.Cleanup(func() { _ = lock.ForceRelease() })

	wrong := *lockInfo
	wrong.ID = "wrong-id"
	err = lock.Release(&wrong)
	if err == nil {
		t.Fatal("Release with mismatched lock ID succeeded")
	}
	if !strings.Contains(err.Error(), "lock ID mismatch") {
		t.Fatalf("Release error = %q, want lock ID mismatch", err)
	}
	if !lock.IsLocked() {
		t.Fatal("lock should remain held after mismatched release")
	}
}

func writeLockFileForTest(t *testing.T, lock *StateLock, info *LockInfo) {
	t.Helper()
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("Marshal lock info: %v", err)
	}
	if err := os.WriteFile(lock.lockFilePath(), data, 0600); err != nil {
		t.Fatalf("write lock file: %v", err)
	}
}
