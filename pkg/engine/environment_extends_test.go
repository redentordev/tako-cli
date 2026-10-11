package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takodstate"
)

const extendsEngineHeader = `project:
  name: demo
  version: 1.0.0
servers:
  node-a:
    host: 192.0.2.10
    user: root
    password: ${TAKO_ENGINE_EXTENDS_PASSWORD}
  node-b:
    host: 192.0.2.11
    user: root
    password: ${TAKO_ENGINE_EXTENDS_PASSWORD}
`

func loadExtendsEngineConfig(t *testing.T, environments string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tako.yaml")
	if err := os.WriteFile(path, []byte(extendsEngineHeader+environments), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

func desiredRevisionJSON(t *testing.T, cfg *config.Config, environment string) string {
	t.Helper()
	services, err := cfg.GetServices(environment)
	if err != nil {
		t.Fatal(err)
	}
	servers, err := cfg.GetEnvironmentServers(environment)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := takodstate.BuildDesiredRevision(cfg, environment, "deploy", services, nil, servers, takodstate.GitInfo{})
	if err != nil {
		t.Fatalf("BuildDesiredRevision: %v", err)
	}
	revision.CreatedAt = time.Time{}
	revision.RevisionID = ""
	data, err := json.Marshal(revision)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestExtendsDesiredStateMatchesHandFlattenedEnvironment(t *testing.T) {
	t.Setenv("TAKO_ENGINE_EXTENDS_PASSWORD", "example-password")
	inheriting := loadExtendsEngineConfig(t, `x-image: &image ghcr.io/example/app:1.4.2
environments:
  base:
    abstract: true
    services:
      web:
        image: *image
        port: 3000
        env:
          LOG_LEVEL: info
          REGION: eu
      worker:
        image: *image
        command: ["bin/worker"]
  production:
    extends: base
    servers: [node-a]
    services:
      web:
        replicas: 2
  staging:
    extends: production
    servers: [node-b]
    services:
      web:
        replicas: 1
        env:
          LOG_LEVEL: debug
      worker: null
`)
	flattened := loadExtendsEngineConfig(t, `environments:
  production:
    servers: [node-a]
    services:
      web:
        image: ghcr.io/example/app:1.4.2
        port: 3000
        replicas: 2
        env:
          LOG_LEVEL: info
          REGION: eu
      worker:
        image: ghcr.io/example/app:1.4.2
        command: ["bin/worker"]
  staging:
    servers: [node-b]
    services:
      web:
        image: ghcr.io/example/app:1.4.2
        port: 3000
        replicas: 1
        env:
          LOG_LEVEL: debug
          REGION: eu
`)

	for _, environment := range []string{"production", "staging"} {
		got := desiredRevisionJSON(t, inheriting, environment)
		want := desiredRevisionJSON(t, flattened, environment)
		if got != want {
			t.Fatalf("%s desired state differs from the hand-flattened config:\n got: %s\nwant: %s", environment, got, want)
		}
		if strings.Contains(got, "extends") || strings.Contains(got, "abstract") || strings.Contains(got, `"base"`) {
			t.Fatalf("%s desired state leaks inheritance metadata: %s", environment, got)
		}
	}
	if _, err := inheriting.GetServices("base"); err == nil {
		t.Fatal("abstract base must not be deployable")
	}
}
