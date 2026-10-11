package secrets

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestPreviewSecretsUseBaseFiles(t *testing.T) {
	withTempWorkingDir(t)
	if err := os.WriteFile("tako.yaml", []byte(`project:
  name: demo
  version: 1.0.0
servers:
  node-a:
    host: 192.0.2.10
    user: deploy
    password: test-password
environments:
  staging:
    servers: [node-a]
    services:
      web:
        image: app:1
  preview:
    abstract: true
    extends: staging
    preview: {}
`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(".tako", 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"secrets":         "SHARED=shared\nDB_PASSWORD=from-shared\n",
		"secrets.preview": "DB_PASSWORD=from-preview\n",
		"secrets.staging": "DB_PASSWORD=from-staging\nSTAGING_ONLY=leak\n",
	} {
		if err := os.WriteFile(filepath.Join(".tako", name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.LoadConfigWithOptions("tako.yaml", config.LoadOptions{Preview: &config.PreviewInstance{Name: "pr-42"}})
	if err != nil {
		t.Fatalf("LoadConfigWithOptions: %v", err)
	}
	manager, err := NewManagerForConfig(cfg, "preview-pr-42")
	if err != nil {
		t.Fatalf("NewManagerForConfig: %v", err)
	}
	if got, _ := manager.Get("DB_PASSWORD"); got != "from-preview" {
		t.Fatalf("DB_PASSWORD = %q, want the preview base's value", got)
	}
	if got, _ := manager.Get("SHARED"); got != "shared" {
		t.Fatalf("SHARED = %q", got)
	}
	if manager.Has("STAGING_ONLY") {
		t.Fatal("preview read .tako/secrets.staging")
	}
}
