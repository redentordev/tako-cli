package takod

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
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

func TestBuildImagePassesPlatformToDocker(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "docker.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	archive := testBuildContextArchive(t, map[string]string{
		"Dockerfile": "FROM scratch\n",
	})
	response, err := BuildImage(context.Background(), "demo/web:abc123", "linux/amd64", bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("BuildImage returned error: %v", err)
	}
	if response.Image != "demo/web:abc123" {
		t.Fatalf("image = %q, want demo/web:abc123", response.Image)
	}

	entries := readCommandLog(t, logPath)
	want := "docker build --platform linux/amd64 -t demo/web:abc123 ."
	if !slices.Contains(entries, want) {
		t.Fatalf("docker log missing platform build %q in %#v", want, entries)
	}
}

func TestBuildImagePassesDockerfileToDocker(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "docker.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	archive := testBuildContextArchive(t, map[string]string{
		"Dockerfile.renderer": "FROM scratch\n",
	})
	response, err := BuildImageWithOptions(context.Background(), "demo/renderer:abc123", ImageBuildOptions{
		Dockerfile: "Dockerfile.renderer",
	}, bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("BuildImageWithOptions returned error: %v", err)
	}
	if response.Image != "demo/renderer:abc123" {
		t.Fatalf("image = %q, want demo/renderer:abc123", response.Image)
	}

	entries := readCommandLog(t, logPath)
	want := "docker build --file Dockerfile.renderer -t demo/renderer:abc123 ."
	if !slices.Contains(entries, want) {
		t.Fatalf("docker log missing dockerfile build %q in %#v", want, entries)
	}
}

func TestBuildImageRejectsUnsafeDockerfilePath(t *testing.T) {
	archive := testBuildContextArchive(t, map[string]string{
		"Dockerfile": "FROM scratch\n",
	})
	_, err := BuildImageWithOptions(context.Background(), "demo/web:abc123", ImageBuildOptions{
		Dockerfile: "../Dockerfile",
	}, bytes.NewReader(archive))
	if err == nil {
		t.Fatal("expected unsafe dockerfile path to fail")
	}
	if !strings.Contains(err.Error(), "relative path inside the build context") {
		t.Fatalf("error = %q, want build context validation", err)
	}
}

func TestBuildImageRejectsMissingDockerfilePath(t *testing.T) {
	archive := testBuildContextArchive(t, map[string]string{
		"Dockerfile": "FROM scratch\n",
	})
	_, err := BuildImageWithOptions(context.Background(), "demo/web:abc123", ImageBuildOptions{
		Dockerfile: "Dockerfile.renderer",
	}, bytes.NewReader(archive))
	if err == nil {
		t.Fatal("expected missing dockerfile to fail")
	}
	if !strings.Contains(err.Error(), "dockerfile does not exist in build context") {
		t.Fatalf("error = %q, want missing dockerfile validation", err)
	}
}

func TestBuildImageRejectsInvalidPlatform(t *testing.T) {
	if _, err := BuildImage(context.Background(), "demo/web:abc123", "darwin/amd64", strings.NewReader("")); err == nil {
		t.Fatal("BuildImage should reject invalid platform")
	}
}

func TestBuildImageUsesExistingLocalCache(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "docker.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	archive := testBuildContextArchive(t, map[string]string{
		"Dockerfile": "FROM scratch\n",
	})
	_, err := BuildImageWithOptions(context.Background(), "demo/web:next", ImageBuildOptions{
		CacheFrom: []string{"demo/web:previous"},
	}, bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("BuildImageWithOptions returned error: %v", err)
	}

	entries := readCommandLog(t, logPath)
	for _, want := range []string{
		"docker image inspect demo/web:previous",
		"docker build --cache-from demo/web:previous -t demo/web:next .",
	} {
		if !slices.Contains(entries, want) {
			t.Fatalf("docker log missing %q in %#v", want, entries)
		}
	}
}

func TestBuildImageUsesBuildxRegistryCache(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "docker.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	archive := testBuildContextArchive(t, map[string]string{
		"Dockerfile": "FROM scratch\n",
	})
	_, err := BuildImageWithOptions(context.Background(), "registry.example.com/demo/web:next", ImageBuildOptions{
		Platform:  "linux/amd64",
		CacheFrom: []string{"type=registry,ref=registry.example.com/demo/web:buildcache"},
		CacheTo:   []string{"type=registry,ref=registry.example.com/demo/web:buildcache,mode=max"},
		Builder:   "mesh-builder",
		UseBuildx: true,
	}, bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("BuildImageWithOptions returned error: %v", err)
	}

	entries := readCommandLog(t, logPath)
	for _, want := range []string{
		"docker buildx version",
		"docker buildx build --load --builder mesh-builder --platform linux/amd64 --cache-from type=registry,ref=registry.example.com/demo/web:buildcache --cache-to type=registry,ref=registry.example.com/demo/web:buildcache,mode=max -t registry.example.com/demo/web:next .",
	} {
		if !slices.Contains(entries, want) {
			t.Fatalf("docker log missing %q in %#v", want, entries)
		}
	}
}

func TestBuildImageReportsMissingBuildx(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "docker.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_BUILDX_UNAVAILABLE", "1")

	archive := testBuildContextArchive(t, map[string]string{
		"Dockerfile": "FROM scratch\n",
	})
	_, err := BuildImageWithOptions(context.Background(), "demo/web:next", ImageBuildOptions{
		CacheTo:   []string{"type=registry,ref=registry.example.com/demo/web:buildcache,mode=max"},
		UseBuildx: true,
	}, bytes.NewReader(archive))
	if err == nil {
		t.Fatal("expected missing buildx to fail")
	}
	if !strings.Contains(err.Error(), "docker buildx is required") {
		t.Fatalf("error = %q, want buildx requirement", err)
	}
}

func TestNodeInfoNormalizesDockerPlatform(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "docker.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	t.Setenv("TAKO_FAKE_INFO_OUTPUT", `{"OSType":"linux","Architecture":"x86_64","OperatingSystem":"Fake Linux","ServerVersion":"27.0.0","DockerRootDir":"/var/lib/docker"}`+"\n")
	t.Setenv("TAKO_FAKE_BUILDX_OUTPUT", "github.com/docker/buildx v0.18.0\n")

	info, err := NodeInfo(context.Background(), "node-a")
	if err != nil {
		t.Fatalf("NodeInfo returned error: %v", err)
	}
	if info.Node != "node-a" || info.Platform != "linux/amd64" {
		t.Fatalf("unexpected node info: %#v", info)
	}
	if !info.BuildxAvailable || !strings.Contains(info.BuildxVersion, "buildx") {
		t.Fatalf("expected buildx details in node info: %#v", info)
	}
	if info.Docker.RootDir != "/var/lib/docker" {
		t.Fatalf("docker root dir = %q, want /var/lib/docker", info.Docker.RootDir)
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
