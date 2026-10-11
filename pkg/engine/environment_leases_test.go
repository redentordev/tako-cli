package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/scheduler"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takodstate"
)

const testClusterID = "11111111-1111-4111-8111-111111111111"

// issue114Config is the reported #114 configuration: two environments with
// one role-less server each, a proxied build-backed service, and the default
// auto build strategy.
func issue114Config() *config.Config {
	web := config.ServiceConfig{Build: ".", Port: 3000, Proxy: &config.ProxyConfig{Domains: []string{"preview.example.test"}}}
	return &config.Config{
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
}

func previewDeployTargets(t *testing.T, cfg *config.Config, services map[string]config.ServiceConfig, prior *takodstate.DesiredRevision) ([]string, []string, error) {
	t.Helper()
	assignments := map[string][]scheduler.Assignment{}
	for name := range services {
		assignments[name] = []scheduler.Assignment{{Slot: 1, Node: "preview"}}
	}
	envServers, err := cfg.GetEnvironmentServers("preview")
	if err != nil {
		t.Fatal(err)
	}
	return resolveDeployTargets(cfg, "preview", envServers, envServers, "preview", services, assignments, prior)
}

func TestIssue114PreviewDeployLeasesOnlyPreview(t *testing.T) {
	cfg := issue114Config()
	if web := cfg.Environments["preview"].Services["web"]; !web.IsProxied() {
		t.Fatal("fixture service must be proxied")
	}
	services := cfg.Environments["preview"].Services
	setup, leases, err := previewDeployTargets(t, cfg, services, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(leases, []string{"preview"}) || !reflect.DeepEqual(setup, []string{"preview"}) {
		t.Fatalf("preview deploy leases = %v, setup = %v; want only preview", leases, setup)
	}

	// The deploy proceeds while production is unreachable: no lease (and
	// therefore no dial) is attempted on it.
	var contacted []string
	set, err := AcquireRemoteOperationLeasesWith(cfg.Servers, leases, "deploy", func(serverName string, _ config.ServerConfig) (RemoteLease, error) {
		contacted = append(contacted, serverName)
		if serverName == "production" {
			return RemoteLease{}, &ConnectivityError{Server: serverName, Err: fmt.Errorf("dial production: connection refused")}
		}
		return RemoteLease{ServerName: serverName, Manager: &recordingLeaseManager{}, Lease: &remotestate.LeaseInfo{ID: "lease-" + serverName}}, nil
	})
	if err != nil {
		t.Fatalf("preview deploy lease failed because of production: %v", err)
	}
	defer set.Release()
	if !reflect.DeepEqual(contacted, []string{"preview"}) {
		t.Fatalf("lease acquisition contacted %v", contacted)
	}

	// The deployer's builder and image-source candidates are the same set.
	builders, err := deployer.NewDeployer(nil, cfg, "preview", false).RemoteBuilderCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(builders, []string{"preview"}) {
		t.Fatalf("preview builders = %v", builders)
	}
}

func TestDeployLeaseTargetsAreEnvironmentScoped(t *testing.T) {
	enrolled := func(node string, roles ...string) config.ServerConfig {
		return config.ServerConfig{ClusterID: testClusterID, NodeID: node, Lifecycle: "schedulable", Roles: roles}
	}
	cases := []struct {
		name     string
		cfg      func() *config.Config
		services map[string]config.ServiceConfig
		env      string
		assigned string
		want     []string
	}{
		{
			name: "non-enrolled proxied", env: "preview", assigned: "preview", want: []string{"preview"},
			cfg:      issue114Config,
			services: map[string]config.ServiceConfig{"web": {Image: "nginx:alpine", Port: 80, Proxy: &config.ProxyConfig{Domains: []string{"a.example.test"}}}},
		},
		{
			name: "non-enrolled remote build", env: "preview", assigned: "preview", want: []string{"preview"},
			cfg: func() *config.Config {
				cfg := issue114Config()
				cfg.Deployment = &config.DeploymentConfig{Build: &config.BuildConfig{Strategy: config.BuildStrategyRemote}}
				return cfg
			},
			services: map[string]config.ServiceConfig{"web": {Build: "."}},
		},
		{
			name: "local build strategy", env: "preview", assigned: "preview", want: []string{"preview"},
			cfg: func() *config.Config {
				cfg := issue114Config()
				cfg.Deployment = &config.DeploymentConfig{Build: &config.BuildConfig{Strategy: config.BuildStrategyLocal}}
				return cfg
			},
			services: map[string]config.ServiceConfig{"web": {Build: "."}},
		},
		{
			name: "enrolled keeps cluster builders, edges and controller", env: "production", assigned: "worker",
			want: []string{"builder", "controller", "edge", "worker"},
			cfg: func() *config.Config {
				return &config.Config{
					Servers: map[string]config.ServerConfig{
						"controller": enrolled("22222222-2222-4222-8222-222222222222", "control-plane"),
						"builder":    enrolled("33333333-3333-4333-8333-333333333333", "builder"),
						"edge":       enrolled("44444444-4444-4444-8444-444444444444", "edge"),
						"worker":     enrolled("55555555-5555-4555-8555-555555555555", "worker"),
					},
					Environments: map[string]config.EnvironmentConfig{"production": {Servers: []string{"worker"}}},
				}
			},
			services: map[string]config.ServiceConfig{"web": {Build: ".", Port: 80, Proxy: &config.ProxyConfig{Domains: []string{"a.example.test"}}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg()
			assignments := map[string][]scheduler.Assignment{"web": {{Slot: 1, Node: tc.assigned}}}
			got := DeployMutationTargets(cfg, tc.env, tc.services, assignments)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("DeployMutationTargets = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPlacementApplyTargetsUseEnvironmentEdges(t *testing.T) {
	cfg := issue114Config()
	cfg.Servers["preview-2"] = config.ServerConfig{Host: "203.0.113.11", User: "deploy"}
	env := cfg.Environments["preview"]
	env.Servers = []string{"preview", "preview-2"}
	cfg.Environments["preview"] = env
	plan := scheduler.MovementPlan{Moves: []scheduler.Movement{{Service: "web", FromNode: "preview", ToNode: "preview-2"}}}
	got := placementApplyTargets(cfg, "preview", cfg.Environments["preview"].Services, plan)
	if !reflect.DeepEqual(got, []string{"preview", "preview-2"}) {
		t.Fatalf("placement-apply targets = %v", got)
	}
}

func writeScopedPriorOwnerConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	key := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(key, []byte("test-key"), 0600); err != nil {
		t.Fatal(err)
	}
	yaml := fmt.Sprintf(`project:
  name: app
  version: 1.0.0
servers:
  preview:
    host: 203.0.113.10
    user: deploy
    sshKey: %s
  old-1:
    host: ${TAKO_TEST_OLD_1_HOST}
    user: deploy
    sshKey: %s
environments:
  preview:
    servers: [preview]
    services:
      web:
        image: nginx:alpine
`, filepath.ToSlash(key), filepath.ToSlash(key))
	path := filepath.Join(dir, "tako.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func priorOwnedBy(nodes ...string) *takodstate.DesiredRevision {
	assignments := make([]scheduler.Assignment, 0, len(nodes))
	for i, node := range nodes {
		assignments = append(assignments, scheduler.Assignment{Slot: i + 1, Node: node})
	}
	return &takodstate.DesiredRevision{Services: map[string]takodstate.DesiredService{"web": {Name: "web", Assignments: assignments}}}
}

func TestPrunedPriorOwnerFailsBeforeAnyLease(t *testing.T) {
	t.Setenv("TAKO_TEST_OLD_1_HOST", "")
	os.Unsetenv("TAKO_TEST_OLD_1_HOST")
	cfg, err := config.LoadConfigForEnvironment(writeScopedPriorOwnerConfig(t), "preview")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Servers["old-1"]; ok {
		t.Fatal("old-1 should have been pruned by environment scoping")
	}
	_, _, err = previewDeployTargets(t, cfg, cfg.Environments["preview"].Services, priorOwnedBy("preview", "old-1"))
	var invalid *InvalidRequestError
	if err == nil || !errors.As(err, &invalid) || Classify(err) != ClassInvalid {
		t.Fatalf("pruned prior owner error = %v, want invalid request (exit 2)", err)
	}
	for _, want := range []string{"old-1", "TAKO_TEST_OLD_1_HOST", "preview"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}

	// With the variable exported, old-1 is loaded and leased.
	t.Setenv("TAKO_TEST_OLD_1_HOST", "203.0.113.30")
	cfg, err = config.LoadConfigForEnvironment(writeScopedPriorOwnerConfig(t), "preview")
	if err != nil {
		t.Fatal(err)
	}
	_, leases, err := previewDeployTargets(t, cfg, cfg.Environments["preview"].Services, priorOwnedBy("preview", "old-1"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(leases, []string{"old-1", "preview"}) {
		t.Fatalf("leases = %v", leases)
	}
}

func TestPriorOwnerAbsentFromServersIsSkipped(t *testing.T) {
	cfg := issue114Config()
	_, leases, err := previewDeployTargets(t, cfg, map[string]config.ServiceConfig{"web": {Image: "nginx:alpine"}}, priorOwnedBy("preview", "retired"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(leases, []string{"preview"}) {
		t.Fatalf("leases = %v", leases)
	}
}

func TestPriorOwnerLeaseErrorExplainsScope(t *testing.T) {
	cfg := issue114Config()
	cfg.Servers["old-1"] = config.ServerConfig{Host: "127.0.0.1", Port: 1, User: "deploy", Password: "x"}
	cfg.Servers["preview"] = config.ServerConfig{Host: "127.0.0.1", Port: 1, User: "deploy", Password: "x"}
	prior := priorOwnedBy("old-1")
	_, leases, err := previewDeployTargets(t, cfg, map[string]config.ServiceConfig{"web": {Image: "nginx:alpine"}}, prior)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(leases, []string{"old-1", "preview"}) {
		t.Fatalf("leases = %v", leases)
	}
	reasons := PriorOwnerScopeReasons("preview", []string{"preview"}, prior)
	if reasons["old-1"] != "node old-1 holds prior desired assignments for environment preview" || len(reasons) != 1 {
		t.Fatalf("reasons = %v", reasons)
	}
	pool := ssh.NewPool()
	defer pool.CloseAll()
	_, err = AcquireRemoteOperationLeasesContext(WithLeaseScopeReasons(context.Background(), reasons), pool, cfg, "preview", []string{"old-1"}, "deploy")
	var connectivity *ConnectivityError
	if err == nil || !errors.As(err, &connectivity) {
		t.Fatalf("lease error = %v, want connectivity error", err)
	}
	if !strings.Contains(err.Error(), "failed to connect to lease node old-1 (node old-1 holds prior desired assignments for environment preview)") {
		t.Fatalf("lease error does not explain why old-1 is in scope: %v", err)
	}
}

// TestMutatedNodesAreLeased is the "mutated implies leased" conformance
// harness. For every application operation it derives the lease set with the
// same helper the operation uses and the set of nodes the deployer may
// mutate (workload targets, builder and image-source candidates, and edge
// nodes) and fails when a mutated node is not leased. It fails if the
// deployer's builder candidates are widened back to every configured server.
func TestMutatedNodesAreLeased(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "app", Version: "1.0.0"},
		Servers: map[string]config.ServerConfig{
			"app-prod":  {Host: "203.0.113.20", User: "deploy"},
			"staging":   {Host: "203.0.113.30", User: "deploy"},
			"staging-2": {Host: "203.0.113.31", User: "deploy"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"staging": {Servers: []string{"staging-2", "staging"}, Services: map[string]config.ServiceConfig{
				"web": {Build: ".", Port: 3000, Replicas: 1, Proxy: &config.ProxyConfig{Domains: []string{"staging.example.test"}}},
			}},
			"production": {Servers: []string{"app-prod"}},
		},
	}
	const env = "staging"
	services := cfg.Environments[env].Services
	envServers, err := cfg.GetEnvironmentServers(env)
	if err != nil {
		t.Fatal(err)
	}
	members, err := config.ResolveSchedulableEnvironmentTargets(cfg.Servers, envServers, env)
	if err != nil {
		t.Fatal(err)
	}
	scaleTargets, err := ScaleTargetServers(cfg, env)
	if err != nil {
		t.Fatal(err)
	}
	deploySetup, deployLeases, err := resolveDeployTargets(cfg, env, members, envServers, "staging", services, map[string][]scheduler.Assignment{"web": {{Slot: 1, Node: "staging"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	type operation struct {
		name     string
		leases   []string
		workload []string
		builds   bool
	}
	operations := []operation{
		{name: "deploy", leases: deployLeases, workload: deploySetup, builds: true},
		{name: "run", leases: []string{"staging"}, workload: []string{"staging"}},
		{name: "rollback", leases: members, workload: members, builds: true},
		{name: "project rollback", leases: members, workload: members, builds: true},
		{name: "scale", leases: scaleTargets, workload: scaleTargets},
		{name: "promote", leases: members, workload: members},
	}
	edges, err := config.EnvironmentEdgeCandidates(cfg, env)
	if err != nil {
		t.Fatal(err)
	}
	builders, err := deployer.NewDeployer(nil, cfg, env, false).RemoteBuilderCandidates()
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range operations {
		leased := map[string]bool{}
		for _, name := range op.leases {
			leased[name] = true
		}
		mutated := append([]string(nil), op.workload...)
		if op.name != "run" {
			mutated = append(mutated, edges...)
		}
		if op.builds {
			mutated = append(mutated, builders...)
		}
		var unleased []string
		for _, name := range mutated {
			if !leased[name] {
				unleased = append(unleased, name)
			}
		}
		sort.Strings(unleased)
		if len(unleased) > 0 {
			t.Errorf("%s mutates unleased node(s) %v (leases %v)", op.name, unleased, op.leases)
		}
	}
}
