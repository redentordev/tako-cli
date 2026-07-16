//go:build linux

package platform

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestStagePlatformBinaryRejectsSymlinkSourceBeforeDestinationMutation(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "tako-real")
	if err := os.WriteFile(target, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "tako")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	err := stagePlatformBinary(link, DefaultBinaryPath)
	if err == nil || !strings.Contains(err.Error(), "open source Tako binary") {
		t.Fatalf("symlink source error = %v", err)
	}
}

func TestProtectedBinaryDirectoryRejectsWorldWritableAncestor(t *testing.T) {
	if err := ensureProtectedBinaryDirectory("/tmp"); err == nil {
		t.Fatal("world-writable binary ancestor was accepted")
	}
}

func TestValidatePublishedBinaryRejectsWritableAndSymlinkFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tako")
	if err := os.WriteFile(path, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Lstat(path)
	stat := info.Sys().(*syscall.Stat_t)
	if err := validatePublishedBinary(info, stat.Uid, stat.Gid); err != nil {
		t.Fatalf("protected executable rejected: %v", err)
	}
	if err := os.Chmod(path, 0775); err != nil {
		t.Fatal(err)
	}
	info, _ = os.Lstat(path)
	if err := validatePublishedBinary(info, stat.Uid, stat.Gid); err == nil {
		t.Fatal("group-writable executable accepted")
	}
	link := filepath.Join(dir, "tako-link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	info, _ = os.Lstat(link)
	if err := validatePublishedBinary(info, stat.Uid, stat.Gid); err == nil {
		t.Fatal("symlink executable accepted")
	}
}
