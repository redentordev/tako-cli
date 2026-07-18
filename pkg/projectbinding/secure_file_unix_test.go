//go:build !windows

package projectbinding

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestReadRejectsMultiplyLinkedBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".tako", "platform-cluster.json")
	binding, _ := New("demo", testPlatformContext(), time.Now())
	if _, err := Create(path, *binding); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, filepath.Join(filepath.Dir(path), "second-link.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadOptional(path); err == nil || !strings.Contains(err.Error(), "exactly one link") {
		t.Fatalf("multi-link error = %v", err)
	}
}

func TestCreateRollsBackPublishedBindingWhenTemporaryUnlinkFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".tako", "platform-cluster.json")
	binding, _ := New("demo", testPlatformContext(), time.Now())
	original := secureBindingUnlinkat
	failed := false
	secureBindingUnlinkat = func(dirFD int, name string, flags int) error {
		if !failed && strings.HasPrefix(name, ".platform-cluster.") {
			failed = true
			return unix.EIO
		}
		return original(dirFD, name, flags)
	}
	t.Cleanup(func() { secureBindingUnlinkat = original })
	if _, err := Create(path, *binding); err == nil || !strings.Contains(err.Error(), "publication rollback") {
		t.Fatalf("temporary unlink failure = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("binding remained after publication rollback: %v", err)
	}
}

func TestCreateRollsBackPublishedBindingWhenDirectorySyncFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".tako", "platform-cluster.json")
	binding, _ := New("demo", testPlatformContext(), time.Now())
	original := secureBindingFsync
	failed := false
	secureBindingFsync = func(fd int) error {
		if !failed {
			failed = true
			return unix.EIO
		}
		return original(fd)
	}
	t.Cleanup(func() { secureBindingFsync = original })
	if _, err := Create(path, *binding); err == nil || !strings.Contains(err.Error(), "publication rollback") {
		t.Fatalf("directory sync failure = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("binding remained after directory sync rollback: %v", err)
	}
}

func TestCreateRollsBackPublishedBindingWhenWinnerValidationFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".tako", "platform-cluster.json")
	binding, _ := New("demo", testPlatformContext(), time.Now())
	original := secureBindingWinnerRead
	secureBindingWinnerRead = func(int, string) ([]byte, error) {
		return nil, errors.New("injected winner validation failure")
	}
	t.Cleanup(func() { secureBindingWinnerRead = original })
	if _, err := Create(path, *binding); err == nil || !strings.Contains(err.Error(), "injected winner validation failure") {
		t.Fatalf("winner validation failure = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("binding remained after winner validation rollback: %v", err)
	}
}

func TestReadRejectsDirectorySwapAfterDescriptorOpen(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".tako", "platform-cluster.json")
	binding, _ := New("demo", testPlatformContext(), time.Now())
	if _, err := Create(path, *binding); err != nil {
		t.Fatal(err)
	}
	secureBindingTestHook = func(stage string) {
		if stage != "read-directory-open" {
			return
		}
		secureBindingTestHook = nil
		if err := os.Rename(filepath.Join(root, ".tako"), filepath.Join(root, ".tako-old")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, ".tako"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { secureBindingTestHook = nil })
	if _, err := ReadOptional(path); err == nil || !strings.Contains(err.Error(), "changed during binding operation") {
		t.Fatalf("directory swap error = %v", err)
	}
}

func TestCreateRejectsDirectorySwapBeforePublication(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".tako", "platform-cluster.json")
	binding, _ := New("demo", testPlatformContext(), time.Now())
	secureBindingTestHook = func(stage string) {
		if stage != "create-temporary-synced" {
			return
		}
		secureBindingTestHook = nil
		if err := os.Rename(filepath.Join(root, ".tako"), filepath.Join(root, ".tako-old")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, ".tako"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { secureBindingTestHook = nil })
	if _, err := Create(path, *binding); err == nil || !strings.Contains(err.Error(), "changed during binding operation") {
		t.Fatalf("directory swap error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("binding published into replacement directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".tako-old", "platform-cluster.json")); !os.IsNotExist(err) {
		t.Fatalf("binding remained in detached original directory: %v", err)
	}
}
