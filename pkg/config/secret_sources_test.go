package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const secretSourcesTestConfig = `project:
  name: demo
  version: 1.0.0
secretSources:
  - provider: doppler
    project: api
    providerEnv: prd
servers:
  node-a:
    host: 10.0.0.1
    user: deploy
    password: sshpass
environments:
  production:
    servers: [node-a]
    secretSources:
      - provider: aws-ssm
        path: /app/prod
        prefixStrip: /app/prod
        region: us-east-1
        map:
          /app/prod/db/url: %s
    services:
      web:
        image: nginx:alpine
        secrets: [DATABASE_URL]
`

func writeSecretSourcesTestConfig(t *testing.T, mapTarget string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tako.yaml")
	content := strings.Replace(secretSourcesTestConfig, "%s", mapTarget, 1)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadConfigParsesSecretSourcesInOrder(t *testing.T) {
	cfg, err := LoadConfig(writeSecretSourcesTestConfig(t, "DATABASE_URL"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	sources := cfg.SecretSourcesFor("production")
	if len(sources) != 2 || sources[0].Provider != "doppler" || sources[1].Provider != "aws-ssm" {
		t.Fatalf("sources = %+v, want top-level doppler then environment aws-ssm", sources)
	}
	if sources[1].Map["/app/prod/db/url"] != "DATABASE_URL" || sources[1].Region != "us-east-1" {
		t.Fatalf("environment source = %+v", sources[1])
	}
	if got := cfg.SecretSourcesFor("staging"); len(got) != 1 {
		t.Fatalf("unknown environment should only get top-level sources, got %+v", got)
	}
}

func TestLoadConfigRejectsInvalidSecretSourceMapTarget(t *testing.T) {
	_, err := LoadConfig(writeSecretSourcesTestConfig(t, "bad-key"))
	if err == nil || !strings.Contains(err.Error(), "environments.production.secretSources[0]") {
		t.Fatalf("LoadConfig error = %v, want map target rejection", err)
	}
}

func TestValidateSecretSourcesRequiresProvider(t *testing.T) {
	err := validateSecretSources("secretSources", []SecretSource{{Path: "/app"}})
	if err == nil || !strings.Contains(err.Error(), "provider is required") {
		t.Fatalf("error = %v", err)
	}
}
