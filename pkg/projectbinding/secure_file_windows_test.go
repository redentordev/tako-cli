//go:build windows

package projectbinding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWindowsCreatePublishesCompleteExclusiveBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".tako", "platform-cluster.json")
	first, _ := New("demo", testPlatformContext(), time.Now())
	if _, err := Create(path, *first); err != nil {
		t.Fatal(err)
	}
	other := testPlatformContext()
	other.ClusterID = "44444444-4444-4444-8444-444444444444"
	second, _ := New("demo", other, time.Now())
	winner, err := Create(path, *second)
	if err != nil {
		t.Fatal(err)
	}
	if winner.ClusterID != first.ClusterID {
		t.Fatalf("exclusive publication replaced winner: %#v", winner)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".platform-cluster.*.tmp"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary files remain: %v, err = %v", matches, err)
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("published binding: %v, err = %v", info, err)
	}
}

func TestWindowsReadRejectsDirectoryReparseSwap(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".tako", "platform-cluster.json")
	binding, _ := New("demo", testPlatformContext(), time.Now())
	if _, err := Create(path, *binding); err != nil {
		t.Fatal(err)
	}
	secureBindingWindowsTestHook = func(stage string) {
		if stage != "read-directory-open" {
			return
		}
		secureBindingWindowsTestHook = nil
		if err := os.Rename(filepath.Join(root, ".tako"), filepath.Join(root, ".tako-old")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, ".tako"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { secureBindingWindowsTestHook = nil })
	if _, err := ReadOptional(path); err == nil || !strings.Contains(err.Error(), "changed during binding operation") {
		t.Fatalf("directory swap error = %v", err)
	}
}

func TestWindowsCreateRejectsDirectorySwapBeforePublication(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".tako", "platform-cluster.json")
	binding, _ := New("demo", testPlatformContext(), time.Now())
	secureBindingWindowsTestHook = func(stage string) {
		if stage != "create-temporary-synced" {
			return
		}
		secureBindingWindowsTestHook = nil
		if err := os.Rename(filepath.Join(root, ".tako"), filepath.Join(root, ".tako-old")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, ".tako"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { secureBindingWindowsTestHook = nil })
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

func TestWindowsCreateRollsBackWhenDirectorySwapsAfterWinnerRead(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".tako", "platform-cluster.json")
	binding, _ := New("demo", testPlatformContext(), time.Now())
	secureBindingWindowsTestHook = func(stage string) {
		if stage != "create-winner-read" {
			return
		}
		secureBindingWindowsTestHook = nil
		if err := os.Rename(filepath.Join(root, ".tako"), filepath.Join(root, ".tako-old")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, ".tako"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { secureBindingWindowsTestHook = nil })
	if _, err := Create(path, *binding); err == nil || !strings.Contains(err.Error(), "changed during binding operation") {
		t.Fatalf("final directory swap error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("binding published into replacement directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".tako-old", "platform-cluster.json")); !os.IsNotExist(err) {
		t.Fatalf("binding remained after detached-directory rollback: %v", err)
	}
}
