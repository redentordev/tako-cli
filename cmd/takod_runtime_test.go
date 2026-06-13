package cmd

import (
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestRequireTakodRuntimeAllowsDefaultTakod(t *testing.T) {
	if err := requireTakodRuntime(&config.Config{}); err != nil {
		t.Fatalf("requireTakodRuntime returned error for default runtime: %v", err)
	}
}

func TestRequireTakodRuntimeRejectsNonTakod(t *testing.T) {
	cfg := &config.Config{
		Runtime: &config.RuntimeConfig{Mode: "legacy"},
	}

	err := requireTakodRuntime(cfg)
	if err == nil {
		t.Fatal("requireTakodRuntime should reject non-takod runtime")
	}
	if !strings.Contains(err.Error(), "runtime.mode=legacy") {
		t.Fatalf("error = %q, want runtime mode included", err.Error())
	}
}
