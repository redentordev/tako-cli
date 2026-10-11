package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

func TestScopedOutServersReportsPrunedServersAndVariables(t *testing.T) {
	dir := t.TempDir()
	sshKey := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(sshKey, []byte("test-key"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCOPE_SSH_KEY", filepath.ToSlash(sshKey))
	t.Setenv("SCOPE_PREVIEW_HOST", "203.0.113.10")
	t.Setenv("SCOPE_PREVIEW_URL", "https://preview.example.test")
	for _, name := range []string{"SCOPE_PRODUCTION_HOST", "SCOPE_PRODUCTION_URL", "SCOPE_PRODUCTION_REPLICAS"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	path := filepath.Join(dir, "tako.yaml")
	if err := os.WriteFile(path, []byte(scopedTwoEnvironmentsYAML), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigForEnvironment(path, "preview")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"production": {"SCOPE_PRODUCTION_HOST"}}
	if got := cfg.ScopedOutServers(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ScopedOutServers() = %v, want %v", got, want)
	}

	// A fully expanded config prunes nothing.
	t.Setenv("SCOPE_PRODUCTION_HOST", "203.0.113.20")
	t.Setenv("SCOPE_PRODUCTION_URL", "https://example.test")
	t.Setenv("SCOPE_PRODUCTION_REPLICAS", "2")
	cfg, err = LoadConfigForEnvironment(path, "preview")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ScopedOutServers(); got != nil {
		t.Fatalf("ScopedOutServers() = %v, want nil", got)
	}
}

func TestEnvironmentBuilderAndEdgeCandidates(t *testing.T) {
	legacy := &Config{
		Servers: map[string]ServerConfig{
			"preview":  {Host: "203.0.113.10"},
			"app-prod": {Host: "203.0.113.20"},
			"staging":  {Host: "203.0.113.30"},
		},
		Environments: map[string]EnvironmentConfig{
			"preview":    {Servers: []string{"preview"}},
			"staging":    {Servers: []string{"staging"}},
			"production": {Servers: []string{"app-prod"}},
		},
	}
	enrolledServer := func(node string, roles ...string) ServerConfig {
		return ServerConfig{ClusterID: "11111111-1111-4111-8111-111111111111", NodeID: node, Lifecycle: nodeidentity.NodeLifecycleSchedulable, Roles: roles}
	}
	enrolled := &Config{
		Servers: map[string]ServerConfig{
			"controller": enrolledServer("22222222-2222-4222-8222-222222222222", nodeidentity.RoleControlPlane, nodeidentity.RoleBuilder, nodeidentity.RoleEdge),
			"worker":     enrolledServer("33333333-3333-4333-8333-333333333333", nodeidentity.RoleWorker),
		},
		Environments: map[string]EnvironmentConfig{"production": {Servers: []string{"worker"}}},
	}
	cases := []struct {
		name         string
		cfg          *Config
		environment  string
		wantBuilders []string
		wantEdges    []string
	}{
		{name: "legacy staging never selects app-prod", cfg: legacy, environment: "staging", wantBuilders: []string{"staging"}, wantEdges: []string{"staging"}},
		{name: "legacy preview", cfg: legacy, environment: "preview", wantBuilders: []string{"preview"}, wantEdges: []string{"preview"}},
		{name: "enrolled keeps inventory roles", cfg: enrolled, environment: "production", wantBuilders: []string{"controller"}, wantEdges: []string{"controller"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			builders, err := EnvironmentBuilderCandidates(tc.cfg, tc.environment)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(builders, tc.wantBuilders) {
				t.Fatalf("builders = %v, want %v", builders, tc.wantBuilders)
			}
			edges, err := EnvironmentEdgeCandidates(tc.cfg, tc.environment)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(edges, tc.wantEdges) {
				t.Fatalf("edges = %v, want %v", edges, tc.wantEdges)
			}
		})
	}
}
