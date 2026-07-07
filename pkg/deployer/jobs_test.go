package deployer

import (
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

func jobDeployerFixture() (*Deployer, *config.ServiceConfig) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "demo"},
		Servers: map[string]config.ServerConfig{
			"node-a": {Host: "10.0.0.1"},
			"node-b": {Host: "10.0.0.2"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {Servers: []string{"node-a", "node-b"}},
		},
	}
	service := &config.ServiceConfig{
		Kind:     config.ServiceKindJob,
		Schedule: "*/5 * * * *",
		Timezone: "Europe/Berlin",
		Build:    "./report",
		Command:  "generate-report",
		Timeout:  "30m",
	}
	return NewDeployer(nil, cfg, "production", false), service
}

func TestBuildJobSpecRendersServiceConfig(t *testing.T) {
	d, service := jobDeployerFixture()
	d.recordJobImage("report", "demo-production-report:abc123")

	spec, err := d.buildJobSpec("report", service)
	if err != nil {
		t.Fatalf("buildJobSpec: %v", err)
	}
	if spec.Name != "report" || spec.Schedule != "*/5 * * * *" || spec.Timezone != "Europe/Berlin" {
		t.Fatalf("spec = %+v", spec)
	}
	if spec.Image != "demo-production-report:abc123" {
		t.Fatalf("image = %q", spec.Image)
	}
	if strings.Join(spec.Command, " ") != "sh -c generate-report" {
		t.Fatalf("command = %v", spec.Command)
	}
	if spec.Network != runtimeid.NetworkName("demo", "production") {
		t.Fatalf("network = %q", spec.Network)
	}
	if spec.TimeoutSeconds != 1800 {
		t.Fatalf("timeoutSeconds = %d", spec.TimeoutSeconds)
	}
	wantHash, _ := reconcile.SafeServiceConfigHash(*service)
	if spec.ConfigHash != wantHash {
		t.Fatalf("configHash = %q, want %q", spec.ConfigHash, wantHash)
	}
}

func TestBuildJobSpecWithoutBuiltImageLeavesImageEmpty(t *testing.T) {
	d, service := jobDeployerFixture()
	spec, err := d.buildJobSpec("report", service)
	if err != nil {
		t.Fatalf("buildJobSpec: %v", err)
	}
	if spec.Image != "" {
		t.Fatalf("image = %q, want empty (inherit on node)", spec.Image)
	}
}

func TestBuildJobSpecUsesRegistryImage(t *testing.T) {
	d, service := jobDeployerFixture()
	service.Build = ""
	service.Image = "registry.example.com/report:v1"
	spec, err := d.buildJobSpec("report", service)
	if err != nil {
		t.Fatalf("buildJobSpec: %v", err)
	}
	if spec.Image != "registry.example.com/report:v1" {
		t.Fatalf("image = %q", spec.Image)
	}
}

func TestBuildJobSpecRejectsInvalidTimeout(t *testing.T) {
	d, service := jobDeployerFixture()
	service.Timeout = "soon"
	if _, err := d.buildJobSpec("report", service); err == nil {
		t.Fatal("invalid timeout accepted")
	}
}

func TestJobOwnerServerIsDeterministic(t *testing.T) {
	d, service := jobDeployerFixture()
	first, err := d.JobOwnerServer(service)
	if err != nil {
		t.Fatalf("owner: %v", err)
	}
	for i := 0; i < 5; i++ {
		owner, err := d.JobOwnerServer(service)
		if err != nil || owner != first {
			t.Fatalf("owner flapped: %q vs %q (err %v)", owner, first, err)
		}
	}
}
