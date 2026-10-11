package cmd

import (
	"reflect"
	"sort"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

// TestEnvironmentCommandTargetListsStayInsideEnvironment audits the target
// lists of backup, cleanup, env push, maintenance, live and certs: each one
// selects only servers of the requested environment, so an unreachable node
// of another environment never blocks them.
func TestEnvironmentCommandTargetListsStayInsideEnvironment(t *testing.T) {
	web := config.ServiceConfig{Image: "nginx:alpine", Port: 80, Proxy: &config.ProxyConfig{Domain: "preview.example.test"}}
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "app", Version: "1.0.0"},
		Servers: map[string]config.ServerConfig{
			"preview":    {Host: "203.0.113.10", User: "deploy"},
			"production": {Host: "203.0.113.20", User: "deploy"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"preview":    {Servers: []string{"preview"}, Services: map[string]config.ServiceConfig{"web": web}},
			"production": {Servers: []string{"production"}, Services: map[string]config.ServiceConfig{"web": web}},
		},
	}
	want := []string{"preview"}
	check := func(name string, got []string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s targets = %v, want %v", name, got, want)
		}
	}

	// env push, backup and cleanup resolve through statePullServerNames.
	names, err := statePullServerNames(cfg, "preview", "")
	check("env push", names, err)
	servers, err := resolveEnvironmentServerSet(cfg, "preview", "")
	if err != nil {
		t.Fatal(err)
	}
	check("backup", sortedBackupServerNames(servers), nil)
	_, cleanupTargets, err := schedulableMutationServerSet(cfg, "preview", servers, true)
	check("cleanup", cleanupTargets, err)
	// maintenance and live lease the environment's servers directly.
	envServers, err := cfg.GetEnvironmentServers("preview")
	check("maintenance/live", envServers, err)
	// certs targets the environment's proxy nodes.
	certTargets, err := certificateTargetServers(cfg, "preview", "")
	check("certs", certTargets, err)
	if _, err := statePullServerNames(cfg, "preview", "production"); err == nil {
		t.Fatal("a server of another environment was accepted as a preview target")
	}
}
