//go:build linux || darwin

package platform

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestEnsureOwnedDurableFileRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operations.fifo")
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- ensureOwnedDurableFile(path, os.Geteuid(), os.Getegid()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO was accepted as a durable file")
		}
	case <-time.After(time.Second):
		t.Fatal("durable file initialization blocked on a FIFO")
	}
}

func TestEnsureOwnedDurableFileRejectsHardLinkedFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "operations.jsonl")
	if err := os.WriteFile(target, []byte("protected"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, link); err != nil {
		t.Fatal(err)
	}
	if err := ensureOwnedDurableFile(link, os.Geteuid(), os.Getegid()); err == nil {
		t.Fatal("hard-linked durable file was accepted")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "protected" {
		t.Fatalf("hard-link target changed to %q: %v", data, err)
	}
}
