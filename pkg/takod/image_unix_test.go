//go:build unix

package takod

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestExtractTarGzPreservesFileModeWithRestrictiveUmask(t *testing.T) {
	oldUmask := syscall.Umask(0077)
	defer syscall.Umask(oldUmask)

	dest := t.TempDir()
	archive := testBuildContextArchive(t, map[string]string{
		"index.html": "ok\n",
	})

	if err := extractTarGz(bytes.NewReader(archive), dest); err != nil {
		t.Fatalf("extractTarGz returned error: %v", err)
	}
	info, err := os.Stat(filepath.Join(dest, "index.html"))
	if err != nil {
		t.Fatalf("failed to stat extracted file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0644 {
		t.Fatalf("extracted mode = %o, want 0644", got)
	}
}
