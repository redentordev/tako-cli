package engine

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/scheduler"
)

type recordingRuntimeVerifier struct {
	calls []string
}

func (r *recordingRuntimeVerifier) PreflightTakodProxyCapabilities(map[string]config.ServiceConfig) error {
	r.calls = append(r.calls, "proxy-preflight")
	return nil
}

func (r *recordingRuntimeVerifier) PreflightGracefulStopCapabilities() error {
	r.calls = append(r.calls, "graceful-stop-preflight")
	return nil
}

func (r *recordingRuntimeVerifier) PreflightServiceMutationCapabilities(map[string]config.ServiceConfig) error {
	r.calls = append(r.calls, "mutation-preflight")
	return nil
}

func (r *recordingRuntimeVerifier) VerifyTakodRuntime() error {
	r.calls = append(r.calls, "verify")
	return nil
}

func (r *recordingRuntimeVerifier) SetupTakodRuntime() error {
	r.calls = append(r.calls, "setup")
	return nil
}

// #114 2.4: tako run keeps capability admission but only verifies agents. It
// never prepares the mesh (no PUT /v1/metadata, no POST /v1/mesh/apply), so a
// run on one node cannot renumber it or drop its peers.
func TestRunVerifiesTakodRuntimeWithoutMeshSetup(t *testing.T) {
	runtime := &recordingRuntimeVerifier{}
	if err := preflightAndVerifyTakodRuntime(runtime, nil, nil); err != nil {
		t.Fatal(err)
	}
	want := []string{"proxy-preflight", "graceful-stop-preflight", "mutation-preflight", "verify"}
	if !reflect.DeepEqual(runtime.calls, want) {
		t.Fatalf("calls = %v, want %v", runtime.calls, want)
	}

	source, err := os.ReadFile("run.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(source), "SetupTakodRuntime") || strings.Contains(string(source), "preflightAndSetupTakodRuntime") {
		t.Fatal("run.go must not prepare the takod mesh")
	}
}

// Mesh apply reaches every schedulable member (D4), so a deploy whose
// workloads touch only one node must still lease every member: the mesh
// write is never an unfenced mutation of a node.
func TestSubsetDeployLeasesEveryMeshMember(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "app", Version: "1.0.0"},
		Servers: map[string]config.ServerConfig{
			"a": {Host: "203.0.113.1", User: "deploy"},
			"b": {Host: "203.0.113.2", User: "deploy"},
			"c": {Host: "203.0.113.3", User: "deploy"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {Servers: []string{"a", "b", "c"}, Services: map[string]config.ServiceConfig{"worker": {Image: "busybox:1.36"}}},
		},
	}
	services := cfg.Environments["production"].Services
	assignments := map[string][]scheduler.Assignment{"worker": {{Slot: 1, Node: "c"}}}
	envServers, err := cfg.GetEnvironmentServers("production")
	if err != nil {
		t.Fatal(err)
	}
	setup, leases, err := resolveDeployTargets(cfg, "production", envServers, envServers, "c", services, assignments, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(setup, []string{"c"}) {
		t.Fatalf("workload setup targets = %v, want [c]", setup)
	}
	if !reflect.DeepEqual(leases, []string{"a", "b", "c"}) {
		t.Fatalf("deploy leases = %v, want every mesh member", leases)
	}
	for _, scaleTargets := range [][]string{mustScaleTargets(t, cfg)} {
		if !reflect.DeepEqual(scaleTargets, []string{"a", "b", "c"}) {
			t.Fatalf("scale leases = %v, want every mesh member", scaleTargets)
		}
	}
}

func mustScaleTargets(t *testing.T, cfg *config.Config) []string {
	t.Helper()
	targets, err := ScaleTargetServers(cfg, "production")
	if err != nil {
		t.Fatal(err)
	}
	return targets
}