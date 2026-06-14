package config

import (
	"slices"
	"strings"
	"testing"
)

func TestResolvePlacementTargetsFiltersNodeLabelConstraints(t *testing.T) {
	servers := map[string]ServerConfig{
		"node-a": {Labels: map[string]string{"role": "web", "region": "iad"}},
		"node-b": {Labels: map[string]string{"role": "worker", "region": "iad"}},
		"node-c": {Labels: map[string]string{"role": "web", "region": "sfo"}},
	}

	got, err := ResolvePlacementTargets(&PlacementConfig{
		Strategy: "spread",
		Constraints: []string{
			"node.labels.role==web",
			"node.labels.region == 'iad'",
		},
	}, servers, []string{"node-a", "node-b", "node-c"}, "production")
	if err != nil {
		t.Fatalf("ResolvePlacementTargets returned error: %v", err)
	}
	if !slices.Equal(got, []string{"node-a"}) {
		t.Fatalf("targets = %#v, want node-a", got)
	}
}

func TestResolvePlacementTargetsRejectsUnsupportedConstraint(t *testing.T) {
	_, err := ResolvePlacementTargets(&PlacementConfig{
		Constraints: []string{"node.cpu>=4"},
	}, map[string]ServerConfig{"node-a": {}}, []string{"node-a"}, "production")
	if err == nil {
		t.Fatal("ResolvePlacementTargets should reject unsupported constraints")
	}
	if !strings.Contains(err.Error(), "supported form") {
		t.Fatalf("error = %q, want supported form guidance", err)
	}
}

func TestResolvePlacementTargetsRejectsPreferencesUntilImplemented(t *testing.T) {
	_, err := ResolvePlacementTargets(&PlacementConfig{
		Preferences: []string{"spread=node.labels.region"},
	}, map[string]ServerConfig{"node-a": {}}, []string{"node-a"}, "production")
	if err == nil {
		t.Fatal("ResolvePlacementTargets should reject unsupported preferences")
	}
	if !strings.Contains(err.Error(), "preferences are not supported") {
		t.Fatalf("error = %q, want preferences guidance", err)
	}
}

func TestValidatePlacementConfigRejectsBadConstraint(t *testing.T) {
	err := ValidatePlacementConfig(&PlacementConfig{Constraints: []string{"node.labels.role!=web"}})
	if err == nil {
		t.Fatal("ValidatePlacementConfig should reject unsupported operator")
	}
}

func TestValidatePlacementConfigRejectsPinnedWithoutServers(t *testing.T) {
	err := ValidatePlacementConfig(&PlacementConfig{Strategy: "pinned"})
	if err == nil {
		t.Fatal("ValidatePlacementConfig should reject pinned placement without servers")
	}
	if !strings.Contains(err.Error(), "pinned placement requires servers") {
		t.Fatalf("error = %q, want pinned guidance", err)
	}
}

func TestValidatePlacementConfigRejectsServersOutsidePinned(t *testing.T) {
	err := ValidatePlacementConfig(&PlacementConfig{
		Strategy: "spread",
		Servers:  []string{"node-a"},
	})
	if err == nil {
		t.Fatal("ValidatePlacementConfig should reject placement servers outside pinned")
	}
	if !strings.Contains(err.Error(), "servers require strategy pinned") {
		t.Fatalf("error = %q, want pinned strategy guidance", err)
	}
}
