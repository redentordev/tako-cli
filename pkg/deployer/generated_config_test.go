package deployer

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestRenderCaddyfileFromImportsIncludesOnDemandTLS(t *testing.T) {
	caddy := generatedCaddyConfigForTest()
	got, err := renderCaddyfileFromImports(caddy, []string{"http://10.210.0.1:31000"}, []string{"http://10.210.0.1:31001", "http://10.210.0.2:31001"}, "http://10.210.0.1:31000/api/platform/domains/ask")
	if err != nil {
		t.Fatalf("renderCaddyfileFromImports returned error: %v", err)
	}
	for _, expected := range []string{
		"email ops@example.com",
		"ask http://10.210.0.1:31000/api/platform/domains/ask",
		"admin.example.com {",
		"sites.example.com {",
		"https:// {",
		"tls {\n\t\ton_demand\n\t}",
		"reverse_proxy http://10.210.0.1:31001 http://10.210.0.2:31001",
		"header_up Host {host}",
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("rendered Caddyfile missing %q:\n%s", expected, got)
		}
	}
}

func TestPrepareGeneratedConfigArtifactsHashesAndUploadsGeneratedCaddyfile(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "edge", Version: "1.0.0"},
		Configs: map[string]config.ConfigFileConfig{
			"caddyfile": {
				Generate: &config.GeneratedConfigConfig{
					Caddy: generatedCaddyConfigForTest(),
				},
			},
		},
	}
	deploy := &Deployer{
		config:      cfg,
		environment: "production",
		importResolver: func(alias string) ([]string, error) {
			switch alias {
			case "jardin_admin":
				return []string{"http://10.210.0.1:31000"}, nil
			case "jardin_renderer":
				return []string{"http://10.210.0.2:31001"}, nil
			default:
				t.Fatalf("unexpected import alias %s", alias)
				return nil, nil
			}
		},
	}
	services := map[string]config.ServiceConfig{
		"edge": {
			Image: "caddy:2.9-alpine",
			Configs: []config.ServiceConfigFileMount{{
				Source: "caddyfile",
				Target: "/etc/caddy/Caddyfile",
				Mode:   "0444",
			}},
		},
	}

	prepared, err := deploy.PrepareGeneratedConfigArtifacts(services)
	if err != nil {
		t.Fatalf("PrepareGeneratedConfigArtifacts returned error: %v", err)
	}
	mount := prepared["edge"].Configs[0]
	if mount.ContentHash == "" {
		t.Fatal("generated config content hash was not populated")
	}

	edge := prepared["edge"]
	files, err := deploy.buildTakodConfigFiles("edge", &edge)
	if err != nil {
		t.Fatalf("buildTakodConfigFiles returned error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("files = %#v, want one generated file", files)
	}
	data, err := base64.StdEncoding.DecodeString(files[0].ContentBase64)
	if err != nil {
		t.Fatalf("invalid generated content base64: %v", err)
	}
	if !strings.Contains(string(data), "reverse_proxy http://10.210.0.2:31001") {
		t.Fatalf("generated Caddyfile did not include renderer upstream:\n%s", string(data))
	}
	sum := sha256.Sum256(data)
	wantHash := hex.EncodeToString(sum[:])
	if files[0].ContentHash != wantHash || mount.ContentHash != wantHash {
		t.Fatalf("hash mismatch: file=%q mount=%q want=%q", files[0].ContentHash, mount.ContentHash, wantHash)
	}
}

func TestRenderCaddyfileFromImportsRejectsUnsafeTokens(t *testing.T) {
	caddy := generatedCaddyConfigForTest()
	caddy.Email = "ops@example.com }"
	if _, err := renderCaddyfileFromImports(caddy, []string{"http://10.210.0.1:31000"}, []string{"http://10.210.0.2:31001"}, "http://10.210.0.1:31000/api/platform/domains/ask"); err == nil {
		t.Fatal("renderCaddyfileFromImports should reject unsafe Caddy token")
	}
}

func generatedCaddyConfigForTest() *config.GeneratedCaddyConfig {
	return &config.GeneratedCaddyConfig{
		Email:          "ops@example.com",
		AdminHost:      "admin.example.com",
		SiteHost:       "sites.example.com",
		AdminImport:    "jardin_admin",
		RendererImport: "jardin_renderer",
		AskImport:      "jardin_admin",
		AskPath:        "/api/platform/domains/ask",
		OnDemandTLS:    true,
	}
}
