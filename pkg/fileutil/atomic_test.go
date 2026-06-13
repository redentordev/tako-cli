package fileutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFileAtomicWritesModeAndCleansTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	if err := WriteFileAtomic(path, []byte("first"), 0600); err != nil {
		t.Fatalf("WriteFileAtomic returned error: %v", err)
	}
	if err := WriteFileAtomic(path, []byte("second"), 0640); err != nil {
		t.Fatalf("WriteFileAtomic overwrite returned error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}
	if string(data) != "second" {
		t.Fatalf("content = %q, want second", data)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("failed to stat file: %v", err)
	}
	if info.Mode().Perm() != 0640 {
		t.Fatalf("mode = %04o, want 0640", info.Mode().Perm())
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("failed to read dir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp") {
			t.Fatalf("unexpected temp file left behind: %s", entry.Name())
		}
	}
}

func TestWriteFileAtomicRequiresExistingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "state.json")

	if err := WriteFileAtomic(path, []byte("state"), 0600); err == nil {
		t.Fatal("expected missing parent directory to fail")
	}
}
