package takod

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestValidateImageName(t *testing.T) {
	for _, image := range []string{
		"demo/web:abc123",
		"postgres:15",
		"ghcr.io/org/app:v1.2.3",
		"registry.example.com:5000/demo/web@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"demo/web:v1..2",
	} {
		if err := validateImageName(image); err != nil {
			t.Fatalf("expected image name %q to be valid: %v", image, err)
		}
	}
	for _, image := range []string{
		"",
		"--help",
		"-demo/web:abc",
		"demo web:abc",
		"demo\tweb:abc",
		"demo\nweb:abc",
		"demo\rweb:abc",
		"demo\x00web:abc",
		strings.Repeat("a", maxImageRefLength+1),
	} {
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

func TestBuildImageUsesCustomDockerfilePath(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	archive := testBuildContextArchive(t, map[string]string{
		"packages/web/Dockerfile": "FROM scratch\n",
		"packages/web/server.js":  "console.log('ok')\n",
	})
	response, err := BuildImage(context.Background(), "demo/web:abc", bytes.NewReader(archive), "packages/web/Dockerfile")
	if err != nil {
		t.Fatalf("BuildImage returned error: %v", err)
	}
	if response.Image != "demo/web:abc" {
		t.Fatalf("image = %q, want demo/web:abc", response.Image)
	}

	entries := readCommandLog(t, logPath)
	want := "docker build -t demo/web:abc -f packages/web/Dockerfile ."
	if !slices.Contains(entries, want) {
		t.Fatalf("commands = %#v, want %q", entries, want)
	}
}

func TestBuildImageWithOptionsUsesSortedBuildArgsAndTarget(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	archive := testBuildContextArchive(t, map[string]string{"Dockerfile": "FROM scratch\n"})
	_, err := BuildImageWithOptions(context.Background(), "demo/web:abc", bytes.NewReader(archive), nil, ImageBuildOptions{
		BuildArgs: map[string]string{"SENTRY_IMAGE": "getsentry/sentry:26.6.0", "A": "first"},
		Target:    "runtime",
	})
	if err != nil {
		t.Fatalf("BuildImageWithOptions: %v", err)
	}
	want := "docker build -t demo/web:abc --build-arg A=first --build-arg SENTRY_IMAGE=getsentry/sentry:26.6.0 --target runtime ."
	if entries := readCommandLog(t, logPath); !slices.Contains(entries, want) {
		t.Fatalf("commands = %#v, want %q", entries, want)
	}
}

func TestValidateImportedPublicationRejectsFinalTagRace(t *testing.T) {
	expected := "sha256:" + strings.Repeat("a", 64)
	replaced := "sha256:" + strings.Repeat("b", 64)
	loaded := &ImageDescriptor{Exists: true, ImageID: expected, OS: "linux", Architecture: "amd64"}
	if err := validateImportedPublication("demo/web:latest", expected, loaded, &ImageDescriptor{
		Exists: true, ImageID: replaced, OS: "linux", Architecture: "amd64",
	}); err == nil || !strings.Contains(err.Error(), "changed during import") {
		t.Fatalf("final tag race validation error = %v", err)
	}
	if err := validateImportedPublication("demo/web:latest", expected, loaded, loaded); err != nil {
		t.Fatalf("matching publication rejected: %v", err)
	}
}

func TestValidateImageBuildOptionsRejectsUnsafeDirectPayloads(t *testing.T) {
	for _, options := range []ImageBuildOptions{
		{BuildArgs: map[string]string{"--secret": "value"}},
		{BuildArgs: map[string]string{"VALID": "line\nbreak"}},
		{Target: "--output"},
		{Target: "bad target"},
	} {
		if err := validateImageBuildOptions(options); err == nil {
			t.Fatalf("unsafe build options accepted: %#v", options)
		}
	}
}

func TestBuildImageRejectsMissingCustomDockerfilePath(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	archive := testBuildContextArchive(t, map[string]string{
		"packages/web/server.js": "console.log('ok')\n",
	})
	_, err := BuildImage(context.Background(), "demo/web:abc", bytes.NewReader(archive), "packages/web/Dockerfile")
	if err == nil {
		t.Fatal("BuildImage should reject missing custom Dockerfile")
	}
	if !strings.Contains(err.Error(), "dockerfile does not exist") {
		t.Fatalf("error = %q, want missing dockerfile context", err)
	}

	data, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatalf("failed to read command log: %v", err)
	}
	entries := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, entry := range entries {
		if strings.Contains(entry, "docker build") {
			t.Fatalf("docker build should not run for missing dockerfile: %#v", entries)
		}
	}
}

func TestBuildImageRejectsUnsafeCustomDockerfilePath(t *testing.T) {
	archive := testBuildContextArchive(t, map[string]string{
		"Dockerfile": "FROM scratch\n",
	})
	_, err := BuildImage(context.Background(), "demo/web:abc", bytes.NewReader(archive), "../Dockerfile")
	if err == nil {
		t.Fatal("BuildImage should reject unsafe custom Dockerfile path")
	}
	if !strings.Contains(err.Error(), "must stay inside") {
		t.Fatalf("error = %q, want safety context", err)
	}
}

func TestBuildImageAddsBuildKitHint(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_DOCKER_BUILD_ERROR", "the --chmod option requires BuildKit")

	archive := testBuildContextArchive(t, map[string]string{
		"Dockerfile": "FROM alpine\nCOPY --chmod=0644 app /app\n",
		"app":        "ok\n",
	})
	_, err := BuildImage(context.Background(), "demo/web:abc", bytes.NewReader(archive))
	if err == nil {
		t.Fatal("BuildImage should fail when docker build fails")
	}
	for _, want := range []string{"the --chmod option requires BuildKit", "Hint:", "Docker buildx", "COPY --chmod"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err.Error(), want)
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
