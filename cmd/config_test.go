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
