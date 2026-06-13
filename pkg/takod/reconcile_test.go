package takod

import (
	"reflect"
	"testing"
)

func TestValidateReconcileServiceRequest(t *testing.T) {
	valid := ReconcileServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Image:       "registry.example.com/demo/web:abc",
		Network:     "tako_demo_production",
		Containers: []ContainerSpec{
			{Name: "demo_production_web_1"},
		},
	}
	if err := validateReconcileServiceRequest(valid); err != nil {
		t.Fatalf("valid request returned error: %v", err)
	}

	invalid := valid
	invalid.EnvFile = "relative.env"
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected relative env file to be rejected")
	}

	invalid = valid
	invalid.Containers = []ContainerSpec{{}}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected empty container name to be rejected")
	}
}

func TestBuildServiceContainerArgs(t *testing.T) {
	req := ReconcileServiceRequest{
		Project:      "demo",
		Environment:  "production",
		Service:      "web",
		Image:        "registry.example.com/demo/web:abc",
		Restart:      "unless-stopped",
		Network:      "tako_demo_production",
		NetworkAlias: "web",
		EnvFile:      "/tmp/web.env",
		Labels: map[string]string{
			"tako.role": "frontend",
		},
		Mounts:  []string{"type=volume,source=demo_data,target=/data"},
		Command: "npm run worker",
		Health: &HealthSpec{
			Command:     "curl -sf http://localhost:3000/health || exit 1",
			Interval:    "10s",
			Timeout:     "5s",
			Retries:     3,
			StartPeriod: "10s",
		},
	}

	got := buildServiceContainerArgs(req, ContainerSpec{
		Name:      "demo_production_web_1",
		Publishes: []string{"10.42.0.2:31001:3000"},
	})

	want := []string{
		"run", "-d",
		"--name", "demo_production_web_1",
		"--restart", "unless-stopped",
		"--network", "tako_demo_production",
		"--network-alias", "web",
		"--label", "tako.environment=production",
		"--label", "tako.project=demo",
		"--label", "tako.role=frontend",
		"--label", "tako.runtime=takod",
		"--label", "tako.service=web",
		"--env-file", "/tmp/web.env",
		"--mount", "type=volume,source=demo_data,target=/data",
		"--publish", "10.42.0.2:31001:3000",
		"--health-cmd", "curl -sf http://localhost:3000/health || exit 1",
		"--health-interval", "10s",
		"--health-timeout", "5s",
		"--health-retries", "3",
		"--health-start-period", "10s",
		"registry.example.com/demo/web:abc",
		"sh", "-c", "npm run worker",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected docker args:\ngot:  %#v\nwant: %#v", got, want)
	}
}
