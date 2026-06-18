package cmd

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"gopkg.in/yaml.v3"
)

func TestGenerateYAMLConfigParsesRuntimeBlocks(t *testing.T) {
	var cfg config.Config
	decoder := yaml.NewDecoder(strings.NewReader(generateYAMLConfig("smoke-app")))
	decoder.KnownFields(true)

	if err := decoder.Decode(&cfg); err != nil {
		t.Fatalf("generated YAML did not parse: %v", err)
	}
	if cfg.Runtime == nil || cfg.Runtime.Agent == nil {
		t.Fatal("generated YAML should include runtime agent config")
	}
	if cfg.Runtime.Proxy != config.RuntimeProxyTako {
		t.Fatalf("generated YAML runtime proxy = %q, want %q", cfg.Runtime.Proxy, config.RuntimeProxyTako)
	}
	if cfg.State == nil {
		t.Fatal("generated YAML should include state config")
	}
	if cfg.Mesh == nil {
		t.Fatal("generated YAML should include mesh config")
	}
}

func TestGenerateJSONConfigParsesRuntimeBlocks(t *testing.T) {
	content := generateJSONConfig("smoke-app")
	if strings.Contains(content, "\t") {
		t.Fatal("generated JSON should not contain tab indentation")
	}

	var cfg config.Config
	if err := json.Unmarshal([]byte(content), &cfg); err != nil {
		t.Fatalf("generated JSON did not parse: %v", err)
	}
	if cfg.Runtime == nil || cfg.Runtime.Agent == nil {
		t.Fatal("generated JSON should include runtime agent config")
	}
	if cfg.Runtime.Proxy != config.RuntimeProxyTako {
		t.Fatalf("generated JSON runtime proxy = %q, want %q", cfg.Runtime.Proxy, config.RuntimeProxyTako)
	}
	if cfg.State == nil {
		t.Fatal("generated JSON should include state config")
	}
	if cfg.Mesh == nil {
		t.Fatal("generated JSON should include mesh config")
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
