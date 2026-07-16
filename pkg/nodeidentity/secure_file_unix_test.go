//go:build linux || darwin

package nodeidentity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCreateRejectsNestedUntrustedSymlinkComponent(t *testing.T) {
	root := t.TempDir()
	protected := filepath.Join(root, "protected")
	target := filepath.Join(root, "target")
	if err := os.Mkdir(protected, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(protected, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	identity, err := New(testClusterID, testNodeID, "node-1", []string{RoleWorker}, testTime())
	if err != nil {
		t.Fatal(err)
	}
	err = Create(filepath.Join(protected, "link", "missing", "identity.json"), *identity)
	if err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("Create error = %v, want nested symlink rejection", err)
	}
	if _, statErr := os.Lstat(filepath.Join(target, "missing")); !os.IsNotExist(statErr) {
		t.Fatalf("rejected symlink caused directory creation in target: %v", statErr)
	}
}

func TestDescriptorWalkRejectsComponentSwap(t *testing.T) {
	root := t.TempDir()
	protected := filepath.Join(root, "protected")
	victim := filepath.Join(protected, "victim")
	moved := filepath.Join(protected, "victim-moved")
	target := filepath.Join(root, "target")
	for _, dir := range []string{protected, victim, target} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	swapped := false
	fd, err := openProtectedDirectoryWithHook(victim, func(component string) {
		if component != "victim" || swapped {
			return
		}
		swapped = true
		if renameErr := os.Rename(victim, moved); renameErr != nil {
			t.Fatalf("rename path component: %v", renameErr)
		}
		if symlinkErr := os.Symlink(target, victim); symlinkErr != nil {
			t.Fatalf("swap path component: %v", symlinkErr)
		}
	})
	if fd >= 0 {
		_ = unix.Close(fd)
	}
	if err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("descriptor walk error = %v, want swapped symlink rejection", err)
	}
}

func TestDescriptorRelativeCreateRejectsParentSwap(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "identity")
	moved := filepath.Join(root, "identity-moved")
	substitute := filepath.Join(root, "substitute")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(substitute, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "identity.json")
	err := createSecureFileWithHook(path, []byte("test"), func() {
		if renameErr := os.Rename(dir, moved); renameErr != nil {
			t.Fatalf("rename protected directory: %v", renameErr)
		}
		if symlinkErr := os.Symlink(substitute, dir); symlinkErr != nil {
			t.Fatalf("substitute protected directory: %v", symlinkErr)
		}
	})
	if err == nil || !strings.Contains(err.Error(), "changed before publication") {
		t.Fatalf("create error = %v, want parent-swap rejection", err)
	}
	for _, candidate := range []string{filepath.Join(moved, "identity.json"), filepath.Join(substitute, "identity.json")} {
		if _, statErr := os.Lstat(candidate); !os.IsNotExist(statErr) {
			t.Fatalf("identity was published through swapped path %s: %v", candidate, statErr)
		}
	}
}

func testTime() (now time.Time) {
	return time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
}
