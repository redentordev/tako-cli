package config

import (
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

func TestPlatformPlacementSeparatesConnectivityFromSchedulability(t *testing.T) {
	servers := map[string]ServerConfig{
		"ready":  {Lifecycle: nodeidentity.NodeLifecycleReady, Roles: []string{nodeidentity.RoleWorker}},
		"active": {Lifecycle: nodeidentity.NodeLifecycleSchedulable, Roles: []string{nodeidentity.RoleWorker}},
	}
	connectivity, err := ResolvePlacementTargets(nil, servers, []string{"ready", "active"}, "production")
	if err != nil || len(connectivity) != 2 {
		t.Fatalf("connectivity targets = %v, %v", connectivity, err)
	}
	placement, err := ResolveSchedulablePlacementTargets(nil, servers, connectivity, "production")
	if err != nil || len(placement) != 1 || placement[0] != "active" {
		t.Fatalf("schedulable targets = %v, %v", placement, err)
	}
	_, err = ResolveSchedulablePlacementTargets(&PlacementConfig{Strategy: "pinned", Servers: []string{"ready"}}, servers, connectivity, "production")
	if err == nil || !strings.Contains(err.Error(), "cannot receive new assignments") {
		t.Fatalf("pinned unschedulable target error = %v", err)
	}
}

func TestExplicitMutationTargetRejectsUnschedulableNode(t *testing.T) {
	servers := map[string]ServerConfig{
		"active": {Lifecycle: nodeidentity.NodeLifecycleSchedulable},
		"ready":  {Lifecycle: nodeidentity.NodeLifecycleReady},
	}
	targets, err := ResolveSchedulableMutationTargets(servers, []string{"active", "ready"}, "production", false)
	if err != nil || len(targets) != 1 || targets[0] != "active" {
		t.Fatalf("implicit mutation targets = %v, %v", targets, err)
	}
	if _, err := ResolveSchedulableMutationTargets(servers, []string{"ready"}, "production", true); err == nil || !strings.Contains(err.Error(), "cannot receive application mutations") {
		t.Fatalf("explicit ready mutation error = %v", err)
	}
}

func TestPlatformProxyPlacementRequiresEdgeRole(t *testing.T) {
	servers := map[string]ServerConfig{
		"edge":   {Roles: []string{nodeidentity.RoleEdge, nodeidentity.RoleWorker}},
		"worker": {Roles: []string{nodeidentity.RoleWorker}},
	}
	targets, err := ResolveEnvironmentProxyTargets(nil, servers, []string{"edge", "worker"}, "production")
	if err != nil || len(targets) != 1 || targets[0] != "edge" {
		t.Fatalf("default proxy targets = %v, %v", targets, err)
	}
	_, err = ResolveEnvironmentProxyTargets(&EnvironmentProxyConfig{Placement: &PlacementConfig{Strategy: "pinned", Servers: []string{"worker"}}}, servers, []string{"edge", "worker"}, "production")
	if err == nil || !strings.Contains(err.Error(), "edge role") {
		t.Fatalf("worker-only proxy placement error = %v", err)
	}
}

func TestPlatformProxyPlacementExcludesUnschedulableEdges(t *testing.T) {
	servers := map[string]ServerConfig{
		"active":   {Lifecycle: nodeidentity.NodeLifecycleSchedulable, Roles: []string{nodeidentity.RoleEdge, nodeidentity.RoleWorker}},
		"cordoned": {Lifecycle: nodeidentity.NodeLifecycleCordoned, Roles: []string{nodeidentity.RoleEdge, nodeidentity.RoleWorker}},
	}
	targets, err := ResolveEnvironmentProxyTargets(nil, servers, []string{"active", "cordoned"}, "production")
	if err != nil || len(targets) != 1 || targets[0] != "active" {
		t.Fatalf("proxy mutation targets = %v, %v", targets, err)
	}
	_, err = ResolveEnvironmentProxyTargets(&EnvironmentProxyConfig{Placement: &PlacementConfig{Strategy: "pinned", Servers: []string{"cordoned"}}}, servers, []string{"active", "cordoned"}, "production")
	if err == nil || !strings.Contains(err.Error(), "cannot receive proxy mutations") {
		t.Fatalf("pinned cordoned proxy error = %v", err)
	}
}
