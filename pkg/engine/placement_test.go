package engine

import (
	"os"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

func TestPlacementApplyPreflightsCapabilitiesBeforeIntentAndMovement(t *testing.T) {
	source, err := os.ReadFile("placement_apply.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(source)
	proxyPreflight := strings.Index(body, "deploy.PreflightTakodProxyCapabilities(services)")
	loggingPreflight := strings.Index(body, "deploy.PreflightServiceMutationCapabilities(movementServices)")
	intent := strings.Index(body, "manager.WriteDesired(next)")
	move := strings.Index(body, "deploy.MovePreparedServiceTakod")
	if proxyPreflight < 0 || loggingPreflight < 0 || intent < 0 || move < 0 || !(proxyPreflight < loggingPreflight && loggingPreflight < intent && intent < move) {
		t.Fatalf("placement apply ordering invalid: proxy=%d logging=%d intent=%d move=%d", proxyPreflight, loggingPreflight, intent, move)
	}
}

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
