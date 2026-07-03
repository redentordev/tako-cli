package cmd

import (
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestCollectInternalHostEntriesPrefersPrivateHostAndFallsBackToMesh(t *testing.T) {
	cfg := testInternalHostsConfig()

	entries, err := collectInternalHostEntries(cfg, "production", cfg.Environments["production"].Services, "", "auto")
	if err != nil {
		t.Fatalf("collectInternalHostEntries returned error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %#v, want two proxy-node entries", entries)
	}
	if entries[0].Host != "web.production.demo.tako.internal" {
		t.Fatalf("host = %q", entries[0].Host)
	}
	got := []string{entries[0].Address + "/" + entries[0].Source, entries[1].Address + "/" + entries[1].Source}
	if strings.Join(got, ",") != "10.0.1.10/privateHost,10.210.0.2/mesh" {
		t.Fatalf("addresses = %#v", got)
	}
}

func TestCollectInternalHostEntriesRequiresPrivateHostWhenRequested(t *testing.T) {
	cfg := testInternalHostsConfig()

	_, err := collectInternalHostEntries(cfg, "production", cfg.Environments["production"].Services, "", "private")
	if err == nil {
		t.Fatal("collectInternalHostEntries should require privateHost in private mode")
	}
	if !strings.Contains(err.Error(), "node-b has no privateHost") {
		t.Fatalf("error = %q, want missing privateHost", err)
	}
}

func testInternalHostsConfig() *config.Config {
	return &config.Config{
		Project: config.ProjectConfig{Name: "demo", Version: "1.0.0"},
		Mesh: &config.MeshConfig{
			NetworkCIDR: "10.210.0.0/16",
		},
		Servers: map[string]config.ServerConfig{
			"node-a": {Host: "203.0.113.10", PrivateHost: "10.0.1.10"},
			"node-b": {Host: "203.0.113.11"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Servers: []string{"node-a", "node-b"},
				Services: map[string]config.ServiceConfig{
					"api": {Port: 4000},
					"web": {
						Port: 3000,
						Proxy: &config.ProxyConfig{
							Host:       "web.production.demo.tako.internal",
							Visibility: config.ProxyVisibilityInternal,
						},
					},
				},
			},
		},
	}
}
