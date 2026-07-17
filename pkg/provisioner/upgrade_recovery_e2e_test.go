//go:build !windows

package provisioner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBootRecoveryScriptRestoresInterruptedPublishedUpgrade(t *testing.T) {
	root := t.TempDir()
	takod := filepath.Join(root, "usr", "local", "bin", "tako")
	worker := filepath.Join(root, "usr", "local", "lib", "tako", "tako")
	manifest := filepath.Join(root, "etc", "tako", "version.json")
	dir := filepath.Join(root, "var", "lib", "tako", "node-upgrade")
	for _, path := range []string{filepath.Dir(takod), filepath.Dir(worker), filepath.Dir(manifest), dir} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeRecoveryFixture(t, takod, "new-takod", 0755)
	writeRecoveryFixture(t, worker, "new-worker", 0755)
	writeRecoveryFixture(t, manifest, "new-manifest", 0644)
	writeRecoveryFixture(t, filepath.Join(dir, "previous-takod"), "old-takod", 0755)
	writeRecoveryFixture(t, filepath.Join(dir, "previous-platform-worker"), "old-worker", 0755)
	writeRecoveryFixture(t, filepath.Join(dir, "previous-version-manifest"), "old-manifest", 0644)
	for _, marker := range []string{"pending", "had-platform-worker", "had-version-manifest"} {
		writeRecoveryFixture(t, filepath.Join(dir, marker), "", 0600)
	}
	for _, scope := range []string{"cluster", "node"} {
		lock := filepath.Join(dir, "locks", scope)
		if err := os.MkdirAll(lock, 0700); err != nil {
			t.Fatal(err)
		}
		writeRecoveryFixture(t, filepath.Join(lock, "owner"), "abandoned", 0600)
	}

	script := bootRecoverTakodUpgradeScript()
	replacements := []struct{ old, new string }{
		{"/var/lib/tako", filepath.Join(root, "var", "lib", "tako")},
		{"/usr/local/bin", filepath.Join(root, "usr", "local", "bin")},
		{"/usr/local/lib/tako", filepath.Join(root, "usr", "local", "lib", "tako")},
		{"/etc/tako", filepath.Join(root, "etc", "tako")},
	}
	for _, replacement := range replacements {
		script = strings.ReplaceAll(script, replacement.old, replacement.new)
	}
	command := exec.Command("sh", "-c", script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("boot recovery failed: %v\n%s", err, output)
	}
	for path, want := range map[string]string{takod: "old-takod", worker: "old-worker", manifest: "old-manifest"} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Fatalf("recovered %s = %q, %v; want %q", path, data, err, want)
		}
	}
	for _, marker := range []string{"pending", "rolled-back", "previous-takod", "previous-platform-worker", "previous-version-manifest"} {
		if _, err := os.Stat(filepath.Join(dir, marker)); !os.IsNotExist(err) {
			t.Fatalf("recovery evidence %s was not finalized: %v", marker, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "locks", "node")); !os.IsNotExist(err) {
		t.Fatalf("abandoned node lease was not cleared: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "locks", "cluster")); err != nil {
		t.Fatalf("external controller lease was erased during node boot recovery: %v", err)
	}
}

func writeRecoveryFixture(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
