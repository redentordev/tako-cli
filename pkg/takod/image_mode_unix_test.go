//go:build unix

package takod

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestExtractTarGzNormalizesModesUnderRestrictiveUmask(t *testing.T) {
	var buf bytes.Buffer
	gzipWriter := gzip.NewWriter(&buf)
	tarWriter := tar.NewWriter(gzipWriter)
	entries := []struct {
		header  tar.Header
		content string
	}{
		{header: tar.Header{Name: "src/", Typeflag: tar.TypeDir, Mode: 0700}},
		{header: tar.Header{Name: "src/index.js", Typeflag: tar.TypeReg, Mode: 0600}, content: "console.log(1)\n"},
		{header: tar.Header{Name: "bin/run.sh", Typeflag: tar.TypeReg, Mode: 0700}, content: "#!/bin/sh\n"},
		{header: tar.Header{Name: "Dockerfile", Typeflag: tar.TypeReg, Mode: 0664}, content: "FROM scratch\n"},
	}
	for _, entry := range entries {
		header := entry.header
		header.Size = int64(len(entry.content))
		if err := tarWriter.WriteHeader(&header); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := tarWriter.Write([]byte(entry.content)); err != nil {
			t.Fatalf("write content: %v", err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	oldUmask := syscall.Umask(0077)
	err := extractTarGz(bytes.NewReader(buf.Bytes()), dest)
	syscall.Umask(oldUmask)
	if err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}

	for path, want := range map[string]os.FileMode{
		"src":          0755,
		"bin":          0755,
		"src/index.js": 0644,
		"bin/run.sh":   0755,
		"Dockerfile":   0644,
	} {
		info, err := os.Stat(filepath.Join(dest, path))
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s mode = %o, want %o", path, got, want)
		}
	}
}
