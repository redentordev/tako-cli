package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestConfigExplainCommandRegistered(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"config", "explain"})
	if err != nil {
		t.Fatalf("Find(config explain) returned error: %v", err)
	}
	if cmd != configExplainCmd {
		t.Fatalf("config explain command was not registered")
	}
}

func TestRunConfigExplainShowsDefaultsAndSources(t *testing.T) {
	root := switchToTempDir(t)
	sshKey := filepath.Join(root, "id_ed25519")
	if err := os.WriteFile(sshKey, []byte("test-key"), 0600); err != nil {
		t.Fatalf("failed to write ssh key fixture: %v", err)
	}
	t.Setenv("TAKO_PRODUCTION_HOST", "203.0.113.10")
	t.Setenv("TAKO_SSH_KEY", sshKey)

	configData := []byte(`project:
  name: demo
  version: 1.0.0
servers:
  node-a:
    host: ${TAKO_PRODUCTION_HOST}
    user: root
    sshKey: ${TAKO_SSH_KEY}
environments:
  production:
    servers: [node-a]
    services:
      web:
        image: nginx:alpine
        port: 80
        proxy:
          domain: app.example.com
          cdn: cloudflare
`)
	if err := os.WriteFile(filepath.Join(root, "tako.yaml"), configData, 0600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
	resetConfigExplainGlobals(t)

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runConfigExplain(cmd, nil); err != nil {
		t.Fatalf("runConfigExplain returned error: %v", err)
	}

	for _, want := range []string{
		"Effective config: tako.yaml",
		"Runtime",
		"mode: takod (default)",
		"State",
		"backend: replicated (default)",
		"Mesh",
		"networkCIDR: 10.210.0.0/16 (default)",
		"natTraversal: true (default)",
		"Servers",
		"host: 203.0.113.10 (env)",
		"auth: sshKey (env)",
		"Services",
		"web:",
		"source: image \"nginx:alpine\" (config)",
		"replicas: 1 (default)",
		"deploy.strategy: recreate (default)",
		"proxy.domain: app.example.com (config)",
		"proxy.cdn: cloudflare (config)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output = %q, want %q", out.String(), want)
		}
	}
}

func resetConfigExplainGlobals(t *testing.T) {
	t.Helper()
	oldCfgFile := cfgFile
	oldEnvFlag := envFlag
	cfgFile = ""
	envFlag = ""
	t.Cleanup(func() {
		cfgFile = oldCfgFile
		envFlag = oldEnvFlag
	})
}

func TestRunConfigExplainShowsExtendsChainAndProvenance(t *testing.T) {
	root := switchToTempDir(t)
	t.Setenv("TAKO_EXPLAIN_SSH_PASSWORD", "super-secret-ssh-password")

	configData := []byte(`x-image: &image nginx:alpine
project:
  name: demo
  version: 1.0.0
servers:
  node-a:
    host: 203.0.113.10
    user: root
    password: ${TAKO_EXPLAIN_SSH_PASSWORD}
  node-b:
    host: 203.0.113.11
    user: root
    password: ${TAKO_EXPLAIN_SSH_PASSWORD}
environments:
  base:
    abstract: true
    services:
      web:
        image: *image
        port: 80
      api:
        image: *image
        replicas: 4
  production:
    extends: base
    servers: [node-a]
    services:
      web:
        replicas: 2
        proxy:
          domain: app.example.com
  staging:
    extends: production
    servers: [node-b]
    services:
      api:
        replicas: 1
`)
	if err := os.WriteFile(filepath.Join(root, "tako.yaml"), configData, 0600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
	resetConfigExplainGlobals(t)
	envFlag = "staging"

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runConfigExplain(cmd, nil); err != nil {
		t.Fatalf("runConfigExplain returned error: %v", err)
	}
	output := out.String()
	for _, want := range []string{
		"Environment: staging (extends production -> base)\n",
		"Servers (from staging)\n",
		"auth: password (env, redacted)",
		"  api:\n    type: worker\n    source: image \"nginx:alpine\" (config, from base)\n    replicas: 1 (config, from staging)\n",
		"  web:\n    type: public\n    source: image \"nginx:alpine\" (config, from base)\n    replicas: 2 (config, from production)\n    deploy.strategy: recreate (default)\n",
		"proxy.domain: app.example.com (config, from production)",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output = %q, want %q", output, want)
		}
	}
	if strings.Contains(output, "super-secret-ssh-password") {
		t.Fatalf("explain leaked the SSH password: %q", output)
	}

	envFlag = "base"
	err := runConfigExplain(&cobra.Command{}, nil)
	if err == nil || !strings.Contains(err.Error(), "environment 'base' is abstract and cannot be deployed; use one of: production, staging") {
		t.Fatalf("explain -e base err = %v", err)
	}
}
