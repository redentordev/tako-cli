package takod

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestTemporaryDockerConfigContainsRegistryAuth(t *testing.T) {
	dir, cleanup, err := writeTemporaryDockerConfig(RegistryAuth{
		Server:   "registry.example.com",
		Username: "deploy",
		Password: "secret-token",
	})
	if err != nil {
		t.Fatalf("writeTemporaryDockerConfig returned error: %v", err)
	}
	defer cleanup()

	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("failed to read Docker config: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, `"registry.example.com"`) ||
		!strings.Contains(content, `"username": "deploy"`) ||
		!strings.Contains(content, `"password": "secret-token"`) {
		t.Fatalf("Docker config missing expected auth fields: %s", content)
	}
}

func TestPullImageWithRegistryAuthUsesTemporaryDockerConfig(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "docker.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_REQUIRE_DOCKER_CONFIG", "registry.example.com")

	if _, err := pullImage(context.Background(), "registry.example.com/demo/web:1", &RegistryAuth{
		Server:   "registry.example.com",
		Username: "deploy",
		Password: "secret-token",
	}); err != nil {
		t.Fatalf("pullImage returned error: %v", err)
	}

	entries := readCommandLog(t, logPath)
	if !slices.Contains(entries, "docker pull registry.example.com/demo/web:1") {
		t.Fatalf("docker log missing pull command: %#v", entries)
	}
	for _, entry := range entries {
		if strings.Contains(entry, "secret-token") {
			t.Fatalf("registry secret leaked into command log: %#v", entries)
		}
	}
}
