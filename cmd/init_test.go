package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/internal/configtest"
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
	// Forward slashes keep the path valid inside JSON strings on Windows.
	t.Setenv("TAKO_SSH_KEY", filepath.ToSlash(keyPath))
	t.Setenv("LETSENCRYPT_EMAIL", "ops@example.com")

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig(%s) returned error: %v", filename, err)
	}
	return cfg
}

func TestInitVariantsValidate(t *testing.T) {
	for _, format := range []string{"yaml", "json"} {
		for _, name := range []string{"my-app", "shop-api"} {
			t.Run(format+"/"+name, func(t *testing.T) {
				configtest.SetupEnv(t)
				filename := "tako." + format
				content := generateYAMLConfig(name)
				if format == "json" {
					content = generateJSONConfig(name)
				}
				cfg, warnings, err := configtest.LoadAndValidate(t, t.TempDir(), filename, []byte(content))
				if err != nil {
					t.Fatalf("generated %s does not validate: %v", filename, err)
				}
				if len(warnings) != 0 {
					t.Fatalf("generated %s has warnings: %#v", filename, warnings)
				}
				if cfg.Project.Name != name {
					t.Fatalf("project name = %q, want %q", cfg.Project.Name, name)
				}
				if format != "yaml" {
					return
				}
				for _, example := range initExamples() {
					t.Run(example.Title, func(t *testing.T) {
						if !strings.Contains(content, commentBlock(example.Body)) {
							t.Fatal("generated YAML does not contain the rendered example")
						}
						merged, err := configtest.MergeYAML([]byte(content), []byte(example.Body))
						if err != nil {
							t.Fatalf("merge example: %v", err)
						}
						_, warnings, err := configtest.LoadAndValidate(t, t.TempDir(), "tako.yaml", merged)
						if err != nil {
							t.Fatalf("example does not validate once merged: %v\n%s", err, merged)
						}
						if len(warnings) != 0 {
							t.Fatalf("example has warnings: %#v", warnings)
						}
					})
				}
			})
		}
	}
}

func TestGenerateYAMLConfigHasNoDeadOrStaleKeys(t *testing.T) {
	content := generateYAMLConfig("my-app")
	for _, forbidden := range []string{
		"registry:",
		"url: ghcr.io",
		"301",
		"# deployment:\n#   strategy",
		"parallel:",
		"maxConcurrent",
		"cache:\n",
		"# port: 3000      # Optional: specify if different from service port",
	} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("generated YAML contains stale text %q", forbidden)
		}
	}
	for _, want := range []string{
		"# registries:\n#   ghcr.io:\n#     username: ${REGISTRY_USER}\n#     password: ${REGISTRY_TOKEN}\n",
		"308 permanent redirect",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("generated YAML missing %q", want)
		}
	}
	// ProxyConfig has no port key, so no commented example may suggest one.
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# port:") {
			t.Fatalf("generated YAML has a commented proxy port line: %q", line)
		}
	}
}

func TestRunInitRejectsInvalidProjectNameBeforeWriting(t *testing.T) {
	root := switchToTempDir(t)
	oldUseJSON := useJSON
	t.Cleanup(func() { useJSON = oldUseJSON })
	for _, jsonMode := range []bool{false, true} {
		useJSON = jsonMode
		err := runInit(initCmd, []string{"My_App"})
		if err == nil {
			t.Fatal("runInit should reject My_App")
		}
		if !strings.Contains(err.Error(), "must start with a lowercase letter, contain only lowercase letters, numbers, and hyphens") {
			t.Fatalf("error = %q, want project-name rule", err)
		}
		for _, name := range []string{"tako.yaml", "tako.json", ".env.example", ".gitignore"} {
			if _, statErr := os.Stat(filepath.Join(root, name)); statErr == nil {
				t.Fatalf("runInit wrote %s for an invalid project name", name)
			}
		}
	}
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
