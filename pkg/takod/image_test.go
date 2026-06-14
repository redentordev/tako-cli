package takod

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateImageName(t *testing.T) {
	if err := validateImageName("demo/web:abc123"); err != nil {
		t.Fatalf("expected image name to be valid: %v", err)
	}
	for _, image := range []string{"", "demo\nweb:abc", "demo\rweb:abc", "demo\x00web:abc"} {
		if err := validateImageName(image); err == nil {
			t.Fatalf("expected image name %q to be rejected", image)
		}
	}
}

func TestSanitizeImageArchiveName(t *testing.T) {
	got := sanitizeImageArchiveName("registry.example.com/demo/web:abc123")
	want := "registry.example.com-demo-web-abc123"
	if got != want {
		t.Fatalf("sanitizeImageArchiveName() = %q, want %q", got, want)
	}
}

func TestMaxBytesReaderAllowsExactLimit(t *testing.T) {
	reader := newMaxBytesReader(strings.NewReader("12345"), 5, "test stream")

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll returned error: %v", err)
	}
	if string(data) != "12345" {
		t.Fatalf("data = %q, want exact payload", data)
	}
}

func TestMaxBytesReaderRejectsOverflow(t *testing.T) {
	reader := newMaxBytesReader(strings.NewReader("123456"), 5, "test stream")

	_, err := io.ReadAll(reader)
	if err == nil {
		t.Fatal("expected overflow to be rejected")
	}
	if !strings.Contains(err.Error(), "test stream exceeds maximum size 5 bytes") {
		t.Fatalf("error = %q, want size limit context", err)
	}
}

func TestSafeArchiveTargetRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	valid, err := safeArchiveTarget(root, "app/Dockerfile")
	if err != nil {
		t.Fatalf("expected valid archive path: %v", err)
	}
	if filepath.Dir(valid) != filepath.Join(root, "app") {
		t.Fatalf("unexpected valid target: %s", valid)
	}

	for _, name := range []string{"../Dockerfile", "/etc/passwd", "app/../../secret"} {
		if _, err := safeArchiveTarget(root, name); err == nil {
			t.Fatalf("expected archive path %q to be rejected", name)
		}
	}
}

func TestExtractTarGzWithLimitsRejectsLargeFile(t *testing.T) {
	archive := testBuildContextArchive(t, map[string]string{
		"Dockerfile": strings.Repeat("A", 6),
	})

	err := extractTarGzWithLimits(bytes.NewReader(archive), t.TempDir(), buildContextLimits{
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

func TestExtractTarGzWithLimitsRejectsTotalSize(t *testing.T) {
	archive := testBuildContextArchive(t, map[string]string{
		"a.txt": strings.Repeat("A", 4),
		"b.txt": strings.Repeat("B", 4),
	})

	err := extractTarGzWithLimits(bytes.NewReader(archive), t.TempDir(), buildContextLimits{
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

func TestExtractTarGzWithLimitsRejectsEntryCount(t *testing.T) {
	archive := testBuildContextArchive(t, map[string]string{
		"a.txt": "A",
		"b.txt": "B",
	})

	err := extractTarGzWithLimits(bytes.NewReader(archive), t.TempDir(), buildContextLimits{
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

func testBuildContextArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	gzipWriter := gzip.NewWriter(&buf)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, content := range files {
		if err := tarWriter.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0644,
			Size: int64(len(content)),
		}); err != nil {
			t.Fatalf("failed to write tar header: %v", err)
		}
		if _, err := tarWriter.Write([]byte(content)); err != nil {
			t.Fatalf("failed to write tar content: %v", err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("failed to close tar writer: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("failed to close gzip writer: %v", err)
	}
	return buf.Bytes()
}
