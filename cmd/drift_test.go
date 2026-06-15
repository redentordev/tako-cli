package cmd

import (
	"testing"

	"github.com/redentordev/tako-cli/pkg/reconcile"
)

func TestDriftServicesFromReconcileUsesLogicalServiceNames(t *testing.T) {
	got := driftServicesFromReconcile(map[string]*reconcile.ActualService{
		"web": {
			Name:     "web",
			Image:    "demo:web",
			Replicas: 3,
		},
	})

	service, ok := got["web"]
	if !ok {
		t.Fatalf("converted services = %#v, want web", got)
	}
	if service.Name != "web" {
		t.Fatalf("service name = %q, want logical detector name", service.Name)
	}
	if service.Replicas != 3 || service.Running != 3 {
		t.Fatalf("replicas/running = %d/%d, want 3/3", service.Replicas, service.Running)
	}
}
