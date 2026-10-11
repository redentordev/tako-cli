package deployplan

import (
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/reconcile"
)

// selfHealing is a node-local policy, not container identity: enabling,
// tuning or disabling it must never recreate a container.
func TestSelfHealingDoesNotChangeContainerIdentity(t *testing.T) {
	enabled, disabled := true, false
	base := config.ServiceConfig{Image: "web:1", Port: 3000, Replicas: 2, HealthCheck: config.HealthCheckConfig{Path: "/health"}}
	variants := []*config.SelfHealingConfig{
		{Enabled: &enabled},
		{Enabled: &disabled},
		{Enabled: &enabled, RestartUnhealthy: &disabled, StartExited: &enabled, UnhealthyThreshold: 7, MaxRestarts: 9, Window: "2h"},
	}
	wantHash, ok := reconcile.SafeServiceConfigHash(base)
	if !ok {
		t.Fatal("base hash unavailable")
	}
	wantRevision := ServiceRevisionID("demo", "production", "web", "web:1", base)
	for _, block := range variants {
		service := base
		service.SelfHealing = block
		hash, ok := reconcile.SafeServiceConfigHash(service)
		if !ok || hash != wantHash {
			t.Fatalf("SafeServiceConfigHash changed with selfHealing %+v: %q != %q", block, hash, wantHash)
		}
		if got := ServiceRevisionID("demo", "production", "web", "web:1", service); got != wantRevision {
			t.Fatalf("ServiceRevisionID changed with selfHealing %+v: %q != %q", block, got, wantRevision)
		}
	}
}
