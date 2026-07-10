package deployer

import (
	"fmt"
	"strings"
	"sync"
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
		Command:  config.StringValue("generate-report"),
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

func TestBuildJobSpecPreservesExecCommandEntrypointAndLabels(t *testing.T) {
	d, service := jobDeployerFixture()
	service.Command = config.ListValue("report", "--format", "json")
	service.Entrypoint = config.ListValue("/usr/bin/env", "python3")
	service.Labels = map[string]string{"com.example.role": "report"}
	spec, err := d.buildJobSpec("report", service)
	if err != nil {
		t.Fatalf("buildJobSpec: %v", err)
	}
	if strings.Join(spec.Command, "|") != "report|--format|json" {
		t.Fatalf("command = %#v", spec.Command)
	}
	if strings.Join(spec.Entrypoint, "|") != "/usr/bin/env|python3" {
		t.Fatalf("entrypoint = %#v", spec.Entrypoint)
	}
	if spec.Labels["com.example.role"] != "report" {
		t.Fatalf("labels = %#v", spec.Labels)
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

func TestTakodJobApplyPreflightsAllOwnersBeforeAnyApply(t *testing.T) {
	tests := []struct {
		name       string
		statusJSON string
		statusErr  error
	}{
		{name: "stale", statusJSON: `{"version":"dev"}`},
		{name: "malformed", statusJSON: `{`},
		{name: "unreachable", statusErr: fmt.Errorf("takod unavailable")},
	}
	targetServers := []string{"node-a", "node-b", "node-c"}
	entrypointOwners := []string{"node-a", "node-c"}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deploy := &Deployer{config: &config.Config{}}
			var mu sync.Mutex
			checked := make(map[string]bool)
			var applied []string
			err := runTakodJobApplyPhases(targetServers, entrypointOwners, func(serverNames []string) error {
				return preflightTakodContainerArgvWithCheck(serverNames, func(serverName string) error {
					mu.Lock()
					checked[serverName] = true
					mu.Unlock()
					return deploy.ensureTakodContainerArgvCapability(fakeTakodStatusExecutor{
						output: tt.statusJSON,
						err:    tt.statusErr,
					}, serverName)
				})
			}, func(serverName string) error {
				mu.Lock()
				defer mu.Unlock()
				applied = append(applied, serverName)
				return nil
			})
			if err == nil {
				t.Fatal("runTakodJobApplyPhases() succeeded with incompatible owner")
			}
			if len(checked) != len(entrypointOwners) {
				t.Fatalf("preflight checked %v, want every owner %v", checked, entrypointOwners)
			}
			if len(applied) != 0 {
				t.Fatalf("job apply ran after failed preflight: %v", applied)
			}
		})
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
