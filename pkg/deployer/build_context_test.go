package deployer

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
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

func TestCreateCrossPlatformTarGzForcedIncludeKeepsIgnoredDockerfile(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "Dockerfile"), "FROM scratch\n")
	mustWriteFile(t, filepath.Join(root, "app", "main.go"), "package main\n")
	mustWriteFile(t, filepath.Join(root, ".dockerignore"), "Dockerfile\n")

	archivePath := filepath.Join(t.TempDir(), "context.tar.gz")
	if err := createCrossPlatformTarGzWithForcedIncludes(root, archivePath, []string{"Dockerfile"}); err != nil {
		t.Fatalf("createCrossPlatformTarGzWithForcedIncludes returned error: %v", err)
	}

	names := readTarGzNames(t, archivePath)
	if !names["Dockerfile"] {
		t.Fatalf("archive missing forced Dockerfile; names=%#v", names)
	}
	if !names["app/main.go"] {
		t.Fatalf("archive missing normal context file; names=%#v", names)
	}
}

func TestCreateCrossPlatformTarGzNormalizesFileModesForDocker(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "index.html"), "ok\n")
	mustWriteFile(t, filepath.Join(root, "entrypoint.sh"), "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(root, "index.html"), 0600); err != nil {
		t.Fatalf("failed to chmod index: %v", err)
	}
	if err := os.Chmod(filepath.Join(root, "entrypoint.sh"), 0700); err != nil {
		t.Fatalf("failed to chmod script: %v", err)
	}

	archivePath := filepath.Join(t.TempDir(), "context.tar.gz")
	if err := createCrossPlatformTarGz(root, archivePath); err != nil {
		t.Fatalf("createCrossPlatformTarGz returned error: %v", err)
	}

	modes := readTarGzModes(t, archivePath)
	if got := modes["index.html"]; got != 0644 {
		t.Fatalf("index mode = %o, want 0644", got)
	}
	if got := modes["entrypoint.sh"]; got != 0755 {
		t.Fatalf("entrypoint mode = %o, want 0755", got)
	}
}

func TestCreateGitSourceContextUsesCommittedFilesOnly(t *testing.T) {
	root := initGitBuildContextRepo(t)
	mustWriteFile(t, filepath.Join(root, "Dockerfile"), "FROM scratch\n")
	mustWriteFile(t, filepath.Join(root, "app", "main.go"), "package main\n")
	mustWriteFile(t, filepath.Join(root, "secret.txt"), "do-not-ship\n")
	mustWriteFile(t, filepath.Join(root, ".dockerignore"), "secret.txt\n")
	gitBuildContextCommitAll(t, root)
	mustWriteFile(t, filepath.Join(root, "app", "main.go"), "package main\n// uncommitted\n")
	mustWriteFile(t, filepath.Join(root, "app", "local.txt"), "local only\n")

	contextDir, cleanup, err := createGitSourceContext(root, buildContextArchiveLimits{
		MaxBytes:     1000,
		MaxFileBytes: 1000,
		MaxEntries:   20,
	})
	if err != nil {
		t.Fatalf("createGitSourceContext returned error: %v", err)
	}
	defer cleanup()

	committed, err := os.ReadFile(filepath.Join(contextDir, "app", "main.go"))
	if err != nil {
		t.Fatalf("failed to read committed file: %v", err)
	}
	if strings.Contains(string(committed), "uncommitted") {
		t.Fatalf("git source context included uncommitted content: %q", string(committed))
	}
	info, err := os.Stat(filepath.Join(contextDir, "app", "main.go"))
	if err != nil {
		t.Fatalf("failed to stat committed file: %v", err)
	}
	if got := info.Mode().Perm(); got&0044 != 0044 {
		t.Fatalf("committed file mode = %o, want group/world readable", got)
	}
	if _, err := os.Stat(filepath.Join(contextDir, "app", "local.txt")); !os.IsNotExist(err) {
		t.Fatalf("git source context included untracked file, stat err=%v", err)
	}
}

func TestCreateGitSourceTarGzAppliesDockerignoreAfterArchive(t *testing.T) {
	root := initGitBuildContextRepo(t)
	mustWriteFile(t, filepath.Join(root, "Dockerfile"), "FROM scratch\n")
	mustWriteFile(t, filepath.Join(root, "app", "main.go"), "package main\n")
	mustWriteFile(t, filepath.Join(root, "secret.txt"), "tracked secret\n")
	mustWriteFile(t, filepath.Join(root, ".dockerignore"), "secret.txt\n")
	gitBuildContextCommitAll(t, root)

	archivePath := filepath.Join(t.TempDir(), "context.tar.gz")
	if err := createGitSourceTarGzWithForcedIncludes(root, archivePath, []string{"Dockerfile"}); err != nil {
		t.Fatalf("createGitSourceTarGzWithForcedIncludes returned error: %v", err)
	}

	names := readTarGzNames(t, archivePath)
	if !names["Dockerfile"] || !names["app/main.go"] {
		t.Fatalf("archive missing committed context files: %#v", names)
	}
	if names["secret.txt"] {
		t.Fatalf("archive included dockerignored committed file: %#v", names)
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

func initGitBuildContextRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runGitBuildContextCommand(t, root, "init")
	runGitBuildContextCommand(t, root, "config", "user.name", "Test User")
	runGitBuildContextCommand(t, root, "config", "user.email", "test@example.com")
	return root
}

func gitBuildContextCommitAll(t *testing.T, root string) {
	t.Helper()
	runGitBuildContextCommand(t, root, "add", ".")
	runGitBuildContextCommand(t, root, "commit", "-m", "initial")
}

func runGitBuildContextCommand(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
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

func readTarGzModes(t *testing.T, path string) map[string]int64 {
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
	modes := map[string]int64{}
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return modes
		}
		if err != nil {
			t.Fatalf("failed to read tar entry: %v", err)
		}
		modes[header.Name] = header.Mode
	}
}
