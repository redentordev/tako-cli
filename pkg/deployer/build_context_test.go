package deployer

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateCrossPlatformTarGzRespectsIgnoreFiles(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "Dockerfile"), "FROM scratch\n")
	mustWriteFile(t, filepath.Join(root, "app", "main.go"), "package main\n")
	mustWriteFile(t, filepath.Join(root, "secret.txt"), "do-not-ship\n")
	mustWriteFile(t, filepath.Join(root, ".dockerignore"), "secret.txt\n")

	archivePath := filepath.Join(t.TempDir(), "context.tar.gz")
	if err := createCrossPlatformTarGz(root, archivePath); err != nil {
		t.Fatalf("createCrossPlatformTarGz returned error: %v", err)
	}

	names := readTarGzNames(t, archivePath)
	for _, expected := range []string{"Dockerfile", "app/", "app/main.go"} {
		if !names[expected] {
			t.Fatalf("archive missing %q; names=%#v", expected, names)
		}
	}
	if names["secret.txt"] {
		t.Fatalf("archive included ignored file; names=%#v", names)
	}
}

func TestCreateCrossPlatformTarGzWithLimitsRejectsLargeFile(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "Dockerfile"), strings.Repeat("A", 6))

	archivePath := filepath.Join(t.TempDir(), "context.tar.gz")
	err := createCrossPlatformTarGzWithLimits(root, archivePath, buildContextArchiveLimits{
		MaxBytes:     100,
		MaxFileBytes: 5,
		MaxEntries:   10,
	})
	if err == nil {
		t.Fatal("expected oversized file to be rejected")
	}
	if !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("error = %q, want max file size context", err)
	}
}

func TestCreateCrossPlatformTarGzWithLimitsRejectsTotalSize(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), strings.Repeat("A", 4))
	mustWriteFile(t, filepath.Join(root, "b.txt"), strings.Repeat("B", 4))

	archivePath := filepath.Join(t.TempDir(), "context.tar.gz")
	err := createCrossPlatformTarGzWithLimits(root, archivePath, buildContextArchiveLimits{
		MaxBytes:     7,
		MaxFileBytes: 10,
		MaxEntries:   10,
	})
	if err == nil {
		t.Fatal("expected oversized build context to be rejected")
	}
	if !strings.Contains(err.Error(), "maximum total size") {
		t.Fatalf("error = %q, want total size context", err)
	}
}

func TestCreateCrossPlatformTarGzWithLimitsRejectsEntryCount(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), "A")
	mustWriteFile(t, filepath.Join(root, "b.txt"), "B")

	archivePath := filepath.Join(t.TempDir(), "context.tar.gz")
	err := createCrossPlatformTarGzWithLimits(root, archivePath, buildContextArchiveLimits{
		MaxBytes:     100,
		MaxFileBytes: 10,
		MaxEntries:   1,
	})
	if err == nil {
		t.Fatal("expected too many entries to be rejected")
	}
	if !strings.Contains(err.Error(), "maximum entry count") {
		t.Fatalf("error = %q, want entry count context", err)
	}
}

func mustWriteFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("failed to create parent directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("failed to write %s: %v", path, err)
	}
}

func readTarGzNames(t *testing.T, path string) map[string]bool {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("failed to open archive: %v", err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatalf("failed to create gzip reader: %v", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	names := map[string]bool{}
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatalf("failed to read tar entry: %v", err)
		}
		names[header.Name] = true
	}
}
