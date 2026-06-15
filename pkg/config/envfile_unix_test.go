//go:build !windows

package config

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestLoadEnvFileRejectsFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env.fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatalf("failed to create fifo: %v", err)
	}

	_, err := LoadEnvFile(path)
	if err == nil {
		t.Fatal("LoadEnvFile should reject FIFO env files before opening")
	}
	if !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("error = %q, want regular file guidance", err)
	}
}

func TestValidateConfigRejectsFIFOEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service-env.fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatalf("failed to create fifo: %v", err)
	}

	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	service := production.Services["web"]
	service.EnvFile = path
	production.Services["web"] = service
	cfg.Environments["production"] = production

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("ValidateConfig should reject FIFO service envFile")
	}
	if !strings.Contains(err.Error(), "envFile") || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("error = %q, want envFile regular file guidance", err)
	}
}
