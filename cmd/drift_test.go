package cmd

import (
	"testing"

	"github.com/redentordev/tako-cli/pkg/reconcile"
)

func TestDriftServicesFromReconcilePrefixesServiceNames(t *testing.T) {
	got := driftServicesFromReconcile("demo", "production", map[string]*reconcile.ActualService{
		"web": {
			Name:     "web",
			Image:    "demo:web",
			Replicas: 3,
		},
	})

	service, ok := got["demo_production_web"]
	if !ok {
		t.Fatalf("converted services = %#v, want demo_production_web", got)
	}
	if service.Name != "demo_production_web" {
		t.Fatalf("service name = %q, want full detector name", service.Name)
	}
	if service.Replicas != 3 || service.Running != 3 {
		t.Fatalf("replicas/running = %d/%d, want 3/3", service.Replicas, service.Running)
	}
}
