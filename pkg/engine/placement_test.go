package engine

import (
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

func TestPreferredPlacementStateSourceSkipsCordonedLocalAndUsesController(t *testing.T) {
	cfg := &config.Config{Servers: map[string]config.ServerConfig{
		"local":      {Transport: "local", Lifecycle: nodeidentity.NodeLifecycleCordoned, Roles: []string{nodeidentity.RoleWorker}},
		"controller": {Lifecycle: nodeidentity.NodeLifecycleSchedulable, Roles: []string{nodeidentity.RoleControlPlane, nodeidentity.RoleWorker}},
	}}
	candidates, err := config.ResolveSchedulableEnvironmentTargets(cfg.Servers, []string{"local", "controller"}, "production")
	if err != nil {
		t.Fatal(err)
	}
	source, err := preferredPlacementStateSource(cfg, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if source != "controller" {
		t.Fatalf("placement state source = %s, want controller", source)
	}
}
