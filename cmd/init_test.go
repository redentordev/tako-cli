package cmd

import (
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
