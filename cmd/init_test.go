package cmd

import (
	"encoding/json"
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
	if cfg.State == nil {
		t.Fatal("generated JSON should include state config")
	}
	if cfg.Mesh == nil {
		t.Fatal("generated JSON should include mesh config")
	}
}
