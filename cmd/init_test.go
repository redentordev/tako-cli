package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"gopkg.in/yaml.v3"
)

func TestGenerateYAMLConfigOmitsRuntimeBlocksAndDefaultsAfterValidation(t *testing.T) {
	content := generateYAMLConfig("smoke-app")
	var cfg config.Config
	decoder := yaml.NewDecoder(strings.NewReader(content))
	decoder.KnownFields(true)

	if err := decoder.Decode(&cfg); err != nil {
		t.Fatalf("generated YAML did not parse: %v", err)
	}
	if cfg.Runtime != nil {
		t.Fatal("generated YAML should omit runtime config by default")
	}
	if cfg.State != nil {
		t.Fatal("generated YAML should omit state config by default")
	}
	if cfg.Mesh != nil {
		t.Fatal("generated YAML should omit mesh config by default")
	}
	if !strings.Contains(content, "# runtime:") || !strings.Contains(content, "# mesh:") {
		t.Fatal("generated YAML should include commented advanced runtime overrides")
	}
	loaded := loadGeneratedConfigWithEnv(t, "tako.yaml", content)
	if loaded.Runtime == nil || loaded.Runtime.Agent == nil {
		t.Fatal("validation should default runtime agent config")
	}
	if loaded.Runtime.Proxy != config.RuntimeProxyTako {
		t.Fatalf("defaulted YAML runtime proxy = %q, want %q", loaded.Runtime.Proxy, config.RuntimeProxyTako)
	}
	if loaded.State == nil {
		t.Fatal("validation should default state config")
	}
	if loaded.Mesh == nil {
		t.Fatal("validation should default mesh config")
	}
	if !strings.Contains(content, "# dynamicDomains:") || !strings.Contains(content, "#   ask: admin:/api/domains/authorize") {
		t.Fatal("generated YAML should include commented dynamic-domain proxy example")
	}
}

func TestGenerateJSONConfigOmitsRuntimeBlocksAndDefaultsAfterValidation(t *testing.T) {
	content := generateJSONConfig("smoke-app")
	if strings.Contains(content, "\t") {
		t.Fatal("generated JSON should not contain tab indentation")
	}

	var cfg config.Config
	if err := json.Unmarshal([]byte(content), &cfg); err != nil {
		t.Fatalf("generated JSON did not parse: %v", err)
	}
	if cfg.Runtime != nil {
		t.Fatal("generated JSON should omit runtime config by default")
	}
	if cfg.State != nil {
		t.Fatal("generated JSON should omit state config by default")
	}
	if cfg.Mesh != nil {
		t.Fatal("generated JSON should omit mesh config by default")
	}
	loaded := loadGeneratedConfigWithEnv(t, "tako.json", content)
	if loaded.Runtime == nil || loaded.Runtime.Agent == nil {
		t.Fatal("validation should default runtime agent config")
	}
	if loaded.Runtime.Proxy != config.RuntimeProxyTako {
		t.Fatalf("defaulted JSON runtime proxy = %q, want %q", loaded.Runtime.Proxy, config.RuntimeProxyTako)
	}
	if loaded.State == nil {
		t.Fatal("validation should default state config")
	}
	if loaded.Mesh == nil {
		t.Fatal("validation should default mesh config")
	}
}

func loadGeneratedConfigWithEnv(t *testing.T, filename string, content string) *config.Config {
	t.Helper()

	root := t.TempDir()
	keyPath := filepath.Join(root, "id_ed25519")
	if err := os.WriteFile(keyPath, []byte("test-key"), 0600); err != nil {
		t.Fatalf("failed to write key fixture: %v", err)
	}
	configPath := filepath.Join(root, filename)
	if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
		t.Fatalf("failed to write generated config: %v", err)
	}
	t.Setenv("TAKO_PRODUCTION_HOST", "203.0.113.10")
	t.Setenv("TAKO_SSH_KEY", keyPath)
	t.Setenv("LETSENCRYPT_EMAIL", "ops@example.com")

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig(%s) returned error: %v", filename, err)
	}
	return cfg
}

func TestInitNextStepsIncludeGitSetupAndDeployFlow(t *testing.T) {
	steps := initNextSteps("tako.yaml")

	for _, want := range []string{
		"  3. Commit your app and config changes",
		"  4. Run 'tako setup -e production' once per server",
		"  5. Run 'tako deploy -e production' to deploy",
	} {
		if !slices.Contains(steps, want) {
			t.Fatalf("init next steps missing %q: %#v", want, steps)
		}
	}
}
