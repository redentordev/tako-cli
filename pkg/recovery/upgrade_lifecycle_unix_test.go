//go:build !windows

package recovery

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNodeUpgradeLifecycleExclusionRejectsActiveLeaseAndReapsExpiredLease(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "tako-data")
	if err := os.Mkdir(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(dataDir, "node-upgrade", "locks")
	if err := os.MkdirAll(filepath.Join(base, "node"), 0700); err != nil {
		t.Fatal(err)
	}
	expiryPath := filepath.Join(base, "node", "expiry")
	if err := os.WriteFile(expiryPath, []byte(epochString(time.Now().Add(time.Minute))+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireNodeUpgradeLifecycleExclusionForDataDir(dataDir); err == nil || !strings.Contains(err.Error(), "active node upgrade lease") {
		t.Fatalf("active node lease was not rejected: %v", err)
	}
	if err := os.WriteFile(expiryPath, []byte(epochString(time.Now().Add(-time.Minute))+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	release, err := AcquireNodeUpgradeLifecycleExclusionForDataDir(dataDir)
	if err != nil {
		t.Fatalf("expired node lease was not recoverable: %v", err)
	}
	defer release()
	if _, err := os.Lstat(filepath.Join(base, "node")); !os.IsNotExist(err) {
		t.Fatalf("expired node lease remains: %v", err)
	}
}

func epochString(value time.Time) string {
	return strconv.FormatInt(value.Unix(), 10)
}

func TestNodeUpgradeLifecycleExclusionRejectsUnsafeLeasePath(t *testing.T) {
	base := filepath.Join(t.TempDir(), "node-upgrade", "locks")
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(base, "node")); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireNodeUpgradeLifecycleExclusion(base); err == nil || !strings.Contains(err.Error(), "unsafe node upgrade lease path") {
		t.Fatalf("unsafe node lease was not rejected: %v", err)
	}
}
