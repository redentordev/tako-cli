package takod

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
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
	validRuntimeLabels := valid
	validRuntimeLabels.Labels = map[string]string{
		runtimeid.ServiceIdentityLabel: runtimeid.ServiceIdentity("demo", "production", "web"),
		"tako.persistent":              "false",
		"tako.configHash":              strings.Repeat("a", 64),
		"com.example.role":             "frontend",
	}
	if err := validateReconcileServiceRequest(validRuntimeLabels); err != nil {
		t.Fatalf("valid runtime labels returned error: %v", err)
	}

	validNoDowntime := valid
	validNoDowntime.DeployStrategy = "rolling"
	validNoDowntime.Revision = "rev-green"
	validNoDowntime.Containers = []ContainerSpec{{Name: "demo_production_web_r_rev_green_1", Labels: map[string]string{"tako.revision": "rev-green"}}}
	if err := validateReconcileServiceRequest(validNoDowntime); err != nil {
		t.Fatalf("valid no-downtime request returned error: %v", err)
	}

	for _, policy := range []string{"", FailurePolicyRemove, FailurePolicyRestore, FailurePolicyRetainFailed} {
		withPolicy := valid
		withPolicy.FailurePolicy = policy
		if err := validateReconcileServiceRequest(withPolicy); err != nil {
			t.Fatalf("failure policy %q returned error: %v", policy, err)
		}
	}
	for _, policy := range []string{"rollback", "Restore", "retain"} {
		withPolicy := valid
		withPolicy.FailurePolicy = policy
		if err := validateReconcileServiceRequest(withPolicy); err == nil || !strings.Contains(err.Error(), "invalid failure policy") {
			t.Fatalf("failure policy %q error = %v, want invalid failure policy", policy, err)
		}
	}

	invalid := valid
	invalid.EnvFile = "/tmp/demo.env"
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected external env file path to be rejected")
	}

	invalid = valid
	invalid.Containers = []ContainerSpec{{}}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected empty container name to be rejected")
	}

	invalid = valid
	invalid.EnvFileContent = string(make([]byte, (1<<20)+1))
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected oversized envFileContent to be rejected")
	}

	invalid = valid
	invalid.Project = "../demo"
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe project name to be rejected")
	}

	invalid = valid
	invalid.Service = "Web"
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe service name to be rejected")
	}

	invalid = valid
	invalid.Image = "demo\nweb"
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe image name to be rejected")
	}

	invalid = valid
	invalid.Restart = "always;reboot"
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe restart policy to be rejected")
	}

	invalid = valid
	invalid.Containers = []ContainerSpec{{Name: "bad/name"}}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe container name to be rejected")
	}

	invalid = valid
	invalid.Containers = []ContainerSpec{{Name: "demo_production_web_1", NetworkAliases: []string{"bad/alias"}}}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe container network alias to be rejected")
	}

	invalid = valid
	invalid.NetworkAttachments = []NetworkAttachmentSpec{{Network: "bad/network"}}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe network attachment to be rejected")
	}

	invalid = valid
	invalid.NetworkAttachments = []NetworkAttachmentSpec{{Network: "tako_backend_api_production_api_export", Aliases: []string{"bad/alias"}}}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe network attachment alias to be rejected")
	}

	invalid = valid
	invalid.NetworkAttachments = []NetworkAttachmentSpec{{Network: "tako_backend_api_production_api_export", Labels: map[string]string{"tako.discovery": "export\nbad"}}}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe network attachment label to be rejected")
	}

	invalid = valid
	invalid.Containers = []ContainerSpec{{Name: "demo_production_web_1", Publishes: []string{"80:80\n--privileged"}}}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe publish value to be rejected")
	}

	invalid = valid
	invalid.Mounts = []string{"type=bind,source=/data,target=/data\n--privileged"}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe mount value to be rejected")
	}

	invalid = valid
	invalid.ExternalVolumes = []string{"bad/name"}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe external volume name to be rejected")
	}

	invalid = valid
	invalid.Mounts = []string{"type=volume,source=bad:name,target=/data"}
	invalid.VolumeOwnership = []ManagedVolumeOwnership{{Name: "bad:name", UID: 1000, GID: 1000}}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe ownership volume name to be rejected")
	}

	invalid = valid
	invalid.Labels = map[string]string{"tako.configHash": "abc\n--privileged"}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe label value to be rejected")
	}

	invalid = valid
	invalid.Labels = map[string]string{"tako.role": "spoofed"}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected arbitrary reserved request label to be rejected")
	}

	invalid = valid
	invalid.Containers = []ContainerSpec{{Name: "demo_production_web_1", Labels: map[string]string{"tako.active": "not-bool"}}}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected invalid reserved container label to be rejected")
	}

	invalid = valid
	invalid.MemoryLimit = "512m --privileged"
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe memory limit to be rejected")
	}

	invalid = valid
	invalid.CPULimit = "1.5 --privileged"
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe cpu limit to be rejected")
	}

	invalid = valid
	invalid.CPULimit = "0.0"
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected zero cpu limit to be rejected")
	}

	invalid = valid
	invalid.CPULimit = "1.5"
	if err := validateReconcileServiceRequest(invalid); err != nil {
		t.Fatalf("expected fractional cpu limit to be accepted, got %v", err)
	}

	invalid = valid
	invalid.Health = &HealthSpec{Command: "curl -sf /health\n--privileged"}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe health command to be rejected")
	}

	invalid = valid
	invalid.Health = &HealthSpec{Path: "health", Port: 3000}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected health path without slash to be rejected")
	}

	invalid = valid
	invalid.Health = &HealthSpec{Path: "/health"}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected health path without port to be rejected")
	}

	invalid = valid
	invalid.Health = &HealthSpec{SmokePath: "smoke", SmokePort: 3000}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected smoke path without slash to be rejected")
	}

	invalid = valid
	invalid.Health = &HealthSpec{SmokePath: "/smoke"}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected smoke path without port to be rejected")
	}

	invalid = valid
	invalid.Health = &HealthSpec{SmokePath: "/smoke", SmokePort: 3000, SmokeExpectedStatus: 99}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected invalid smoke expected status to be rejected")
	}

	invalid = valid
	invalid.Health = &HealthSpec{Interval: "not-a-duration"}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected invalid health interval to be rejected")
	}

	invalid = valid
	invalid.Health = &HealthSpec{Retries: maxHealthRetries + 1}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected oversized health retries to be rejected")
	}

	invalid = valid
	invalid.Health = &HealthSpec{WaitAttempts: maxHealthWaitAttempts + 1}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected oversized health waitAttempts to be rejected")
	}

	invalid = valid
	invalid.DeployStrategy = "canary"
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected invalid deploy strategy to be rejected")
	}

	invalid = valid
	invalid.DeployStrategy = "rolling"
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected rolling request without revision to be rejected")
	}

	invalid = valid
	invalid.Revision = "../rev"
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe revision to be rejected")
	}

	invalid = valid
	invalid.DeployStrategy = "blue_green"
	invalid.Revision = "rev-green"
	invalid.Containers = []ContainerSpec{{Name: "demo_production_web_r_rev_blue_1", Labels: map[string]string{"tako.revision": "rev-blue"}}}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected mismatched container revision label to be rejected")
	}
}

func TestReconcileServiceRequestCommandWireAcceptsLegacyStringAndExecList(t *testing.T) {
	for _, tt := range []struct {
		name string
		json string
		want []string
	}{
		{name: "legacy string", json: `{"command":"echo legacy"}`, want: []string{"sh", "-c", "echo legacy"}},
		{name: "exec list", json: `{"command":["echo","raw"]}`, want: []string{"echo", "raw"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var request ReconcileServiceRequest
			if err := json.Unmarshal([]byte(tt.json), &request); err != nil {
				t.Fatalf("json.Unmarshal returned error: %v", err)
			}
			if !reflect.DeepEqual(request.Command.ContainerCommand(), tt.want) {
				t.Fatalf("command = %#v, want %#v", request.Command.ContainerCommand(), tt.want)
			}
		})
	}
}

func TestValidateRemoveServiceRequest(t *testing.T) {
	valid := RemoveServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
	}
	if err := validateRemoveServiceRequest(valid); err != nil {
		t.Fatalf("valid remove request returned error: %v", err)
	}

	valid.KeepRevision = "rev-green"
	if err := validateRemoveServiceRequest(valid); err != nil {
		t.Fatalf("valid keep revision returned error: %v", err)
	}

	invalid := valid
	invalid.Project = "../demo"
	if err := validateRemoveServiceRequest(invalid); err == nil {
		t.Fatal("expected unsafe project name to be rejected")
	}

	invalid = valid
	invalid.Environment = "prod\n"
	if err := validateRemoveServiceRequest(invalid); err == nil {
		t.Fatal("expected unsafe environment name to be rejected")
	}

	invalid = valid
	invalid.Service = "Web"
	if err := validateRemoveServiceRequest(invalid); err == nil {
		t.Fatal("expected unsafe service name to be rejected")
	}

	invalid = valid
	invalid.KeepRevision = "../rev"
	if err := validateRemoveServiceRequest(invalid); err == nil {
		t.Fatal("expected unsafe keep revision to be rejected")
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
			"com.example.role": "frontend",
		},
		Mounts:      []string{"type=volume,source=demo_data,target=/data"},
		Command:     config.StringValue("npm run worker"),
		MemoryLimit: "512m",
		CPULimit:    "1.5",
		Health: &HealthSpec{
			Path:        "/health",
			Port:        3000,
			Scheme:      "http",
			Interval:    "10s",
			Timeout:     "5s",
			Retries:     3,
			StartPeriod: "10s",
		},
	}

	got := buildServiceContainerArgs(req, ContainerSpec{
		Name:           "demo_production_web_1",
		NetworkAliases: []string{"tako-demo-production-web-1"},
		Publishes:      []string{"10.42.0.2:31001:3000"},
		Labels: map[string]string{
			"tako.revision": "rev1",
			"tako.slot":     "1",
		},
	})

	want := []string{
		"run", "-d",
		"--name", "demo_production_web_1",
		"--restart", "unless-stopped",
		"--network", "tako_demo_production",
		"--network-alias", "web",
		"--network-alias", "tako-demo-production-web-1",
		"--label", "com.example.role=frontend",
		"--label", "tako.environment=production",
		"--label", "tako.project=demo",
		"--label", "tako.revision=rev1",
		"--label", "tako.runtime=takod",
		"--label", "tako.service=web",
		"--label", "tako.slot=1",
		"--env-file", "/tmp/web.env",
		"--mount", "type=volume,source=demo_data,target=/data",
		"--publish", "10.42.0.2:31001:3000",
		"--memory", "512m",
		"--cpus", "1.5",
		"registry.example.com/demo/web:abc",
		"sh", "-c", "npm run worker",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected docker args:\ngot:  %#v\nwant: %#v", got, want)
	}
}

func TestBuildServiceContainerArgsUsesEntrypointAndRawExecCommand(t *testing.T) {
	req := ReconcileServiceRequest{
		Project: "demo", Environment: "production", Service: "consumer",
		Image: "getsentry/sentry:26.6.0", Restart: "unless-stopped",
		Network: "tako_demo_production", NetworkAlias: "consumer",
		Entrypoint: config.ListValue("/etc/sentry/entrypoint.sh", "run"),
		Command:    config.ListValue("sentry", "run", "consumer", "errors"),
		Labels:     map[string]string{"com.example.role": "consumer"},
	}
	got := buildServiceContainerArgs(req, ContainerSpec{Name: "demo_production_consumer_1"})
	wantTail := []string{
		"--entrypoint", "/etc/sentry/entrypoint.sh",
		"getsentry/sentry:26.6.0",
		"run", "sentry", "run", "consumer", "errors",
	}
	if len(got) < len(wantTail) || !reflect.DeepEqual(got[len(got)-len(wantTail):], wantTail) {
		t.Fatalf("docker args tail = %#v, want %#v; all args %#v", got[len(got)-len(wantTail):], wantTail, got)
	}
	if !slices.Contains(got, "com.example.role=consumer") {
		t.Fatalf("docker args missing custom label: %#v", got)
	}
}

func TestBuildServiceContainerArgsUsesRuntimeControls(t *testing.T) {
	req := ReconcileServiceRequest{
		Project: "demo", Environment: "production", Service: "web",
		Image: "demo/web:latest", Restart: "unless-stopped",
		Network: "tako_demo_production", NetworkAlias: "web",
		User:               "1000:1000",
		WorkingDir:         "/work",
		StopTimeoutSeconds: 60,
		Init:               true,
		ExtraHosts:         []string{"database:10.0.0.2", "host.docker.internal:host-gateway"},
		Ulimits: map[string]config.UlimitConfig{
			"nofile": {Soft: 262144, Hard: 262144},
			"nproc":  {Soft: 4096, Hard: 8192},
		},
		ShmSize: "256m",
	}
	got := buildServiceContainerArgs(req, ContainerSpec{Name: "demo_production_web_1"})
	want := []string{
		"run", "-d", "--name", "demo_production_web_1", "--restart", "unless-stopped",
		"--network", "tako_demo_production", "--network-alias", "web",
		"--label", "tako.environment=production", "--label", "tako.project=demo",
		"--label", "tako.runtime=takod", "--label", "tako.service=web",
		"--user", "1000:1000", "--workdir", "/work", "--stop-timeout", "60", "--init",
		"--add-host", "database:10.0.0.2", "--add-host", "host.docker.internal:host-gateway",
		"--ulimit", "nofile=262144:262144", "--ulimit", "nproc=4096:8192",
		"--shm-size", "256m", "demo/web:latest",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("docker args = %#v, want %#v", got, want)
	}
}

func TestBuildServiceContainerArgsUsesBoundedLogging(t *testing.T) {
	req := ReconcileServiceRequest{
		Project: "demo", Environment: "production", Service: "web", Image: "demo/web:1",
		Restart: "unless-stopped", Network: "tako_demo_production", NetworkAlias: "web",
		Logging: &config.ServiceLoggingConfig{Driver: "local", MaxSize: "25m", MaxFiles: 5},
	}
	args := strings.Join(buildServiceContainerArgs(req, ContainerSpec{Name: "tako_demo_production_web_1"}), " ")
	for _, expected := range []string{"--log-driver local", "--log-opt max-size=25m", "--log-opt max-file=5"} {
		if !strings.Contains(args, expected) {
			t.Fatalf("args missing %q: %s", expected, args)
		}
	}
	plain := req
	plain.Logging = nil
	if args := strings.Join(buildServiceContainerArgs(plain, ContainerSpec{Name: "tako_demo_production_web_1"}), " "); strings.Contains(args, "--log-") {
		t.Fatalf("omitted logging emitted args: %s", args)
	}
}

func TestValidateReconcileServiceRejectsUnsafeLogging(t *testing.T) {
	for _, logging := range []*config.ServiceLoggingConfig{
		{Driver: "journald", MaxSize: "10m", MaxFiles: 3},
		{Driver: "local", MaxSize: "10m --label=x", MaxFiles: 3},
		{Driver: "json-file", MaxSize: "10m", MaxFiles: 0},
	} {
		if err := validateDockerLogging(logging); err == nil {
			t.Fatalf("logging accepted: %#v", logging)
		}
	}
}

func TestValidateContainerRuntimeControlsRejectsUnsafeDirectPayloads(t *testing.T) {
	tests := []struct {
		name       string
		user       string
		workingDir string
		stop       int
		hosts      []string
		ulimits    map[string]config.UlimitConfig
		shm        string
	}{
		{name: "flag user", user: "--privileged"},
		{name: "relative workdir", workingDir: "work"},
		{name: "oversized timeout", stop: 86401},
		{name: "host flag", hosts: []string{"--privileged:host-gateway"}},
		{name: "invalid address", hosts: []string{"db:not-an-ip"}},
		{name: "invalid ulimit", ulimits: map[string]config.UlimitConfig{"nofile": {Soft: 10, Hard: 1}}},
		{name: "invalid ulimit name", ulimits: map[string]config.UlimitConfig{"nofile=value": {Soft: 1, Hard: 1}}},
		{name: "shm flag", shm: "--privileged"},
		{name: "oversized shm number", shm: "12345678901234567890m"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateContainerRuntimeControls(tt.user, tt.workingDir, tt.stop, tt.hosts, tt.ulimits, tt.shm); err == nil {
				t.Fatal("unsafe runtime controls accepted")
			}
		})
	}
}

func TestBuildServiceContainerArgsAppliesRequestRevisionLabels(t *testing.T) {
	req := ReconcileServiceRequest{
		Project:        "demo",
		Environment:    "production",
		Service:        "web",
		Image:          "registry.example.com/demo/web:abc",
		Restart:        "unless-stopped",
		Network:        "tako_demo_production",
		NetworkAlias:   "web",
		Revision:       "rev-green",
		DeployStrategy: "blue_green",
	}

	got := buildServiceContainerArgs(req, ContainerSpec{
		Name: "demo_production_web_r_rev_green_1",
		Labels: map[string]string{
			"tako.active": "false",
			"tako.slot":   "1",
		},
	})

	for _, want := range []string{
		"--label",
		"tako.deployStrategy=blue_green",
		"tako.revision=rev-green",
		"tako.active=false",
		"tako.slot=1",
	} {
		if !slices.Contains(got, want) {
			t.Fatalf("docker args missing %q in %#v", want, got)
		}
	}
}

func TestNamedVolumeSourcesFromMountsReturnsUniqueSortedVolumes(t *testing.T) {
	got := namedVolumeSourcesFromMounts([]string{
		"type=volume,source=tako_demo_production_cache,target=/cache",
		"type=bind,source=/srv/data,target=/data",
		"type=volume,src=tako_demo_production_uploads,target=/uploads",
		"type=volume,source=tako_demo_production_cache,target=/cache2",
	})
	want := []string{"tako_demo_production_cache", "tako_demo_production_uploads"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("volume sources = %#v, want %#v", got, want)
	}
}

func TestReconcileServiceCreatesNamedVolumesWithRuntimeLabels(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	_, err := ReconcileService(context.Background(), ReconcileServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Image:       "registry.example.com/demo/web:abc",
		Network:     "tako_demo_production",
		Mounts: []string{
			"type=volume,source=tako_demo_production_data,target=/data",
			"type=bind,source=/srv/demo,target=/srv/demo",
		},
		Containers: []ContainerSpec{{Name: "demo_production_web_1"}},
	})
	if err != nil {
		t.Fatalf("ReconcileService returned error: %v", err)
	}

	entries := readCommandLog(t, logPath)
	want := "docker volume create --label tako.project=demo --label tako.environment=production --label tako.runtime=takod --label tako.service=web tako_demo_production_data"
	if !slices.Contains(entries, want) {
		t.Fatalf("docker log missing labeled volume create %q in %#v", want, entries)
	}
	for _, entry := range entries {
		if strings.Contains(entry, "volume create") && strings.Contains(entry, "/srv/demo") {
			t.Fatalf("bind mount should not create Docker volume: %#v", entries)
		}
	}
}

func TestReconcileServiceInspectsExternalVolumesWithoutCreatingThem(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	_, err := ReconcileService(context.Background(), ReconcileServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Image:       "registry.example.com/demo/web:abc",
		Network:     "tako_demo_production",
		Mounts: []string{
			"type=volume,source=captain--n8n-data,target=/home/node/.n8n",
		},
		ExternalVolumes: []string{"captain--n8n-data"},
		Containers:      []ContainerSpec{{Name: "demo_production_web_1"}},
	})
	if err != nil {
		t.Fatalf("ReconcileService returned error: %v", err)
	}

	entries := readCommandLog(t, logPath)
	if !slices.Contains(entries, "docker volume inspect captain--n8n-data") {
		t.Fatalf("docker log missing external volume inspect in %#v", entries)
	}
	for _, entry := range entries {
		if strings.Contains(entry, "volume create") && strings.Contains(entry, "captain--n8n-data") {
			t.Fatalf("external volume should not be created: %#v", entries)
		}
	}
}

func TestReconcileServiceFailsMissingExternalVolumeBeforeRemovingContainers(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_MISSING_VOLUME_INSPECT", "captain--missing-data")

	_, err := ReconcileService(context.Background(), ReconcileServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Image:       "registry.example.com/demo/web:abc",
		Network:     "tako_demo_production",
		Mounts: []string{
			"type=volume,source=captain--missing-data,target=/data",
		},
		ExternalVolumes: []string{"captain--missing-data"},
		Containers:      []ContainerSpec{{Name: "demo_production_web_1"}},
	})
	if err == nil {
		t.Fatal("ReconcileService should fail for missing external volume")
	}
	if !strings.Contains(err.Error(), "external docker volume captain--missing-data does not exist") {
		t.Fatalf("error = %q, want missing external volume context", err)
	}

	entries := readCommandLog(t, logPath)
	for _, entry := range entries {
		if strings.HasPrefix(entry, "docker ps -aq --filter label=tako.project=demo") || strings.HasPrefix(entry, "docker stop ") || strings.HasPrefix(entry, "docker rm ") {
			t.Fatalf("missing external volume should fail before removing old containers; log %#v", entries)
		}
	}
}

func TestReconcileServiceCreatesAndConnectsExportNetworkAttachments(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_MISSING_NETWORK_INSPECT", "tako_backend_api_production_api_export")

	_, err := ReconcileService(context.Background(), ReconcileServiceRequest{
		Project:     "backend-api",
		Environment: "production",
		Service:     "api",
		Image:       "registry.example.com/backend-api/api:abc",
		Network:     "tako_backend_api_production",
		NetworkAttachments: []NetworkAttachmentSpec{{
			Network: "tako_backend_api_production_api_export",
			Aliases: []string{
				"backend-api-production-api",
			},
			Create: true,
			Labels: map[string]string{
				"tako.discovery":    "export",
				"tako.environment":  "production",
				"tako.export.alias": "backend-api-production-api",
				"tako.project":      "backend-api",
				"tako.runtime":      "takod",
				"tako.service":      "api",
			},
		}},
		Containers: []ContainerSpec{{Name: "backend_api_production_api_1"}},
	})
	if err != nil {
		t.Fatalf("ReconcileService returned error: %v", err)
	}

	entries := readCommandLog(t, logPath)
	for _, want := range []string{
		"docker network inspect tako_backend_api_production_api_export",
		"docker network create --label tako.discovery=export --label tako.environment=production --label tako.export.alias=backend-api-production-api --label tako.project=backend-api --label tako.runtime=takod --label tako.service=api tako_backend_api_production_api_export",
		"docker network connect --alias backend-api-production-api tako_backend_api_production_api_export backend_api_production_api_1",
	} {
		if !slices.Contains(entries, want) {
			t.Fatalf("docker log missing %q in %#v", want, entries)
		}
	}
}

func TestReconcileServiceFailsMissingImportNetworkBeforeRemovingContainers(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_MISSING_NETWORK_INSPECT", "tako_backend_api_production_api_export")

	_, err := ReconcileService(context.Background(), ReconcileServiceRequest{
		Project:     "frontend",
		Environment: "production",
		Service:     "web",
		Image:       "registry.example.com/frontend/web:abc",
		Network:     "tako_frontend_production",
		NetworkAttachments: []NetworkAttachmentSpec{{
			Network: "tako_backend_api_production_api_export",
		}},
		Containers: []ContainerSpec{{Name: "frontend_production_web_1"}},
	})
	if err == nil {
		t.Fatal("ReconcileService should fail for missing import network")
	}
	if !strings.Contains(err.Error(), "import network tako_backend_api_production_api_export does not exist") {
		t.Fatalf("error = %q, want missing import network context", err)
	}

	entries := readCommandLog(t, logPath)
	for _, entry := range entries {
		if strings.HasPrefix(entry, "docker ps -aq --filter label=tako.project=frontend") || strings.HasPrefix(entry, "docker stop ") || strings.HasPrefix(entry, "docker rm ") {
			t.Fatalf("missing import network should fail before removing old containers; log %#v", entries)
		}
	}
}

func TestPrepareServiceEnvFileWritesAndCleansUpTempFile(t *testing.T) {
	req := ReconcileServiceRequest{
		Project:        "demo",
		Environment:    "production",
		Service:        "web",
		EnvFileContent: "TOKEN=value\n",
	}

	cleanup, err := prepareServiceEnvFile(&req)
	if err != nil {
		t.Fatalf("prepareServiceEnvFile returned error: %v", err)
	}
	if cleanup == nil {
		t.Fatal("expected cleanup function")
	}
	if req.EnvFileContent != "" {
		t.Fatal("expected env file content to be cleared after writing")
	}
	if req.EnvFile == "" || !filepath.IsAbs(req.EnvFile) {
		t.Fatalf("expected absolute env file path, got %q", req.EnvFile)
	}

	data, err := os.ReadFile(req.EnvFile)
	if err != nil {
		t.Fatalf("failed to read temp env file: %v", err)
	}
	if string(data) != "TOKEN=value\n" {
		t.Fatalf("temp env file content = %q", string(data))
	}

	cleanup()
	if _, err := os.Stat(req.EnvFile); !os.IsNotExist(err) {
		t.Fatalf("expected cleanup to remove temp env file, stat err=%v", err)
	}
}

func TestReconcileServiceRunsDockerMutation(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	_, err := ReconcileService(context.Background(), ReconcileServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Image:       "registry.example.com/demo/web:abc",
		Network:     "tako_demo_production",
		Containers:  []ContainerSpec{{Name: "demo_production_web_1"}},
	})
	if err != nil {
		t.Fatalf("ReconcileService returned error: %v", err)
	}

	entries := readCommandLog(t, logPath)
	if len(entries) == 0 {
		t.Fatalf("expected docker commands, got %#v", entries)
	}
	if !strings.HasPrefix(entries[0], "docker network inspect ") {
		t.Fatalf("expected first Docker mutation to preflight network, got %#v", entries)
	}
	if !slices.Contains(entries, "docker ps -aq --filter label=tako.project=demo --filter label=tako.environment=production --filter label=tako.service=web") {
		t.Fatalf("expected Docker mutation to list old containers after preflight, got %#v", entries)
	}
}

func TestReconcileServiceCleansStartedContainersOnHealthFailure(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_HEALTH_STATUS", "unhealthy")

	_, err := ReconcileService(context.Background(), ReconcileServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Image:       "registry.example.com/demo/web:abc",
		Network:     "tako_demo_production",
		Containers:  []ContainerSpec{{Name: "demo_production_web_1"}},
		Health: &HealthSpec{
			Command:      "curl -sf http://127.0.0.1:3000/health || exit 1",
			WaitAttempts: 1,
		},
	})
	if err == nil {
		t.Fatal("ReconcileService should fail for unhealthy containers")
	}

	entries := readCommandLog(t, logPath)
	if !slices.Contains(entries, "docker stop demo_production_web_1") || !slices.Contains(entries, "docker rm demo_production_web_1") {
		t.Fatalf("docker log missing graceful cleanup of started container in %#v", entries)
	}
}

func TestReconcileServiceUsesHostSideHTTPHealthWithoutDockerHealthCommand(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("failed to parse test server URL: %v", err)
	}
	host, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatalf("failed to split test server host: %v", err)
	}

	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_CONTAINER_IP", host)

	_, err = ReconcileService(context.Background(), ReconcileServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Image:       "registry.example.com/demo/web:abc",
		Network:     "tako_demo_production",
		Containers:  []ContainerSpec{{Name: "demo_production_web_1"}},
		Health: &HealthSpec{
			Path:         "/health",
			Port:         mustAtoi(t, port),
			Timeout:      "2s",
			WaitAttempts: 1,
		},
	})
	if err != nil {
		t.Fatalf("ReconcileService returned error: %v", err)
	}

	entries := readCommandLog(t, logPath)
	for _, entry := range entries {
		if strings.Contains(entry, "--health-cmd") || strings.Contains(entry, "curl -sf") {
			t.Fatalf("docker args should not include in-container health command: %#v", entries)
		}
	}
	wantInspect := fmt.Sprintf("docker inspect demo_production_web_1 --format {{with index .NetworkSettings.Networks %q}}{{.IPAddress}}{{end}}", "tako_demo_production")
	if !slices.Contains(entries, wantInspect) {
		t.Fatalf("docker log missing network IP inspect %q in %#v", wantInspect, entries)
	}
}

func TestReconcileServiceRunsSmokeTestAfterHTTPReadiness(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ready":
			_, _ = w.Write([]byte("ready"))
		case "/smoke":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("failed to parse test server URL: %v", err)
	}
	host, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatalf("failed to split test server host: %v", err)
	}

	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_CONTAINER_IP", host)

	_, err = ReconcileService(context.Background(), ReconcileServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Image:       "registry.example.com/demo/web:abc",
		Network:     "tako_demo_production",
		Containers:  []ContainerSpec{{Name: "demo_production_web_1"}},
		Health: &HealthSpec{
			Path:                "/ready",
			Port:                mustAtoi(t, port),
			SmokePath:           "/smoke",
			SmokePort:           mustAtoi(t, port),
			SmokeExpectedStatus: http.StatusNoContent,
			Timeout:             "2s",
			WaitAttempts:        1,
		},
	})
	if err != nil {
		t.Fatalf("ReconcileService returned error: %v", err)
	}
}

func TestReconcileServiceSupportsSmokeOnlyHealthSpec(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/smoke" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("failed to parse test server URL: %v", err)
	}
	host, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatalf("failed to split test server host: %v", err)
	}

	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_CONTAINER_IP", host)

	_, err = ReconcileService(context.Background(), ReconcileServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Image:       "registry.example.com/demo/web:abc",
		Network:     "tako_demo_production",
		Containers:  []ContainerSpec{{Name: "demo_production_web_1"}},
		Health: &HealthSpec{
			SmokePath:           "/smoke",
			SmokePort:           mustAtoi(t, port),
			SmokeExpectedStatus: http.StatusNoContent,
			Timeout:             "2s",
			WaitAttempts:        1,
		},
	})
	if err != nil {
		t.Fatalf("ReconcileService returned error: %v", err)
	}
}

func TestReconcileServiceCleansStartedContainersOnSmokeFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ready":
			_, _ = w.Write([]byte("ready"))
		case "/smoke":
			http.Error(w, "bad", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("failed to parse test server URL: %v", err)
	}
	host, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatalf("failed to split test server host: %v", err)
	}

	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_CONTAINER_IP", host)

	_, err = ReconcileService(context.Background(), ReconcileServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Image:       "registry.example.com/demo/web:abc",
		Network:     "tako_demo_production",
		Containers:  []ContainerSpec{{Name: "demo_production_web_1"}},
		Health: &HealthSpec{
			Path:                "/ready",
			Port:                mustAtoi(t, port),
			SmokePath:           "/smoke",
			SmokePort:           mustAtoi(t, port),
			SmokeExpectedStatus: http.StatusNoContent,
			Timeout:             "2s",
			WaitAttempts:        1,
		},
	})
	if err == nil {
		t.Fatal("ReconcileService should fail when smoke test status is wrong")
	}
	if !strings.Contains(err.Error(), "HTTP smoke returned status") {
		t.Fatalf("error = %v, want smoke status failure", err)
	}

	entries := readCommandLog(t, logPath)
	if !slices.Contains(entries, "docker stop demo_production_web_1") || !slices.Contains(entries, "docker rm demo_production_web_1") {
		t.Fatalf("docker log missing graceful cleanup of started container in %#v", entries)
	}
}

func TestReconcileServiceUsesHostSideTCPHealthWithoutDockerHealthCommand(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("failed to split listener address: %v", err)
	}

	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_CONTAINER_IP", host)

	_, err = ReconcileService(context.Background(), ReconcileServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "postgres",
		Image:       "postgres:16-alpine",
		Network:     "tako_demo_production",
		Containers:  []ContainerSpec{{Name: "demo_production_postgres_1"}},
		Health: &HealthSpec{
			Port:         mustAtoi(t, port),
			Timeout:      "2s",
			WaitAttempts: 1,
		},
	})
	if err != nil {
		t.Fatalf("ReconcileService returned error: %v", err)
	}

	entries := readCommandLog(t, logPath)
	for _, entry := range entries {
		if strings.Contains(entry, "--health-cmd") || strings.Contains(entry, "curl -sf") {
			t.Fatalf("docker args should not include in-container health command: %#v", entries)
		}
	}
	wantInspect := fmt.Sprintf("docker inspect demo_production_postgres_1 --format {{with index .NetworkSettings.Networks %q}}{{.IPAddress}}{{end}}", "tako_demo_production")
	if !slices.Contains(entries, wantInspect) {
		t.Fatalf("docker log missing network IP inspect %q in %#v", wantInspect, entries)
	}
}

func TestContainerHealthWaitAttemptsDoesNotUseDockerRetryCount(t *testing.T) {
	got := containerHealthWaitAttempts(&HealthSpec{
		Command: "curl -sf http://127.0.0.1:3000/health || exit 1",
		Retries: 1,
	})
	if got != 30 {
		t.Fatalf("wait attempts = %d, want default 30", got)
	}
	if got := containerHealthWaitAttempts(&HealthSpec{Retries: 1, WaitAttempts: 45}); got != 45 {
		t.Fatalf("explicit wait attempts = %d, want 45", got)
	}
}

func TestReconcileServiceNoDowntimeStrategyReplacesOnlyTargetRevision(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "blue123\ngreen456\nlegacy789\n")
	t.Setenv("TAKO_FAKE_INSPECT_LABELS_BY_ID", `{
		"blue123":"{\"tako.revision\":\"rev-blue\"}",
		"green456":"{\"tako.revision\":\"rev-green\"}",
		"legacy789":"{}"
	}`)

	_, err := ReconcileService(context.Background(), ReconcileServiceRequest{
		Project:        "demo",
		Environment:    "production",
		Service:        "web",
		Revision:       "rev-green",
		DeployStrategy: "blue_green",
		Image:          "registry.example.com/demo/web:abc",
		Network:        "tako_demo_production",
		Containers: []ContainerSpec{{
			Name:           "demo_production_web_r_rev_green_1",
			NetworkAliases: []string{"tako-demo-production-web-r-rev-green-1"},
			Labels: map[string]string{
				"tako.active":   "false",
				"tako.revision": "rev-green",
				"tako.slot":     "1",
			},
		}},
	})
	if err != nil {
		t.Fatalf("ReconcileService returned error: %v", err)
	}

	entries := readCommandLog(t, logPath)
	if !slices.Contains(entries, "docker stop green456") || !slices.Contains(entries, "docker rm green456") {
		t.Fatalf("docker log missing graceful target revision removal in %#v", entries)
	}
	for _, entry := range entries {
		if strings.Contains(entry, "docker rm") && (strings.Contains(entry, "blue123") || strings.Contains(entry, "legacy789")) {
			t.Fatalf("side-by-side reconcile removed non-target revision: %#v", entries)
		}
	}
	runEntry := ""
	for _, entry := range entries {
		if strings.HasPrefix(entry, "docker run ") {
			runEntry = entry
			break
		}
	}
	for _, want := range []string{
		"--label tako.deployStrategy=blue_green",
		"--label tako.revision=rev-green",
		"--label tako.active=false",
	} {
		if !strings.Contains(runEntry, want) {
			t.Fatalf("docker run entry missing %q in %q", want, runEntry)
		}
	}
}

func TestReconcileServiceNoDowntimeEmptyRequestRemovesOnlyTargetRevision(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "blue123\ngreen456\nlegacy789\n")
	t.Setenv("TAKO_FAKE_INSPECT_LABELS_BY_ID", `{
		"blue123":"{\"tako.revision\":\"rev-blue\"}",
		"green456":"{\"tako.revision\":\"rev-green\"}",
		"legacy789":"{}"
	}`)

	response, err := ReconcileService(context.Background(), ReconcileServiceRequest{
		Project:        "demo",
		Environment:    "production",
		Service:        "web",
		Revision:       "rev-green",
		DeployStrategy: "rolling",
		Image:          "registry.example.com/demo/web:abc",
		Network:        "tako_demo_production",
	})
	if err != nil {
		t.Fatalf("ReconcileService returned error: %v", err)
	}
	if response.RemovedContainers != 1 {
		t.Fatalf("removed containers = %d, want only target revision", response.RemovedContainers)
	}

	entries := readCommandLog(t, logPath)
	if !slices.Contains(entries, "docker stop green456") || !slices.Contains(entries, "docker rm green456") {
		t.Fatalf("docker log missing graceful target revision removal in %#v", entries)
	}
	for _, entry := range entries {
		if strings.Contains(entry, "docker rm") && (strings.Contains(entry, "blue123") || strings.Contains(entry, "legacy789")) {
			t.Fatalf("cleanup-only reconcile removed non-target revision: %#v", entries)
		}
	}
}

func mustAtoi(t *testing.T, value string) int {
	t.Helper()
	var out int
	if _, err := fmt.Sscanf(value, "%d", &out); err != nil {
		t.Fatalf("failed to parse integer %q: %v", value, err)
	}
	return out
}

func TestRemoveServiceRemovesMatchingContainers(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "abc123\ndef456\n")

	response, err := RemoveService(context.Background(), RemoveServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "old-api",
	})
	if err != nil {
		t.Fatalf("RemoveService returned error: %v", err)
	}
	if response.RemovedContainers != 2 {
		t.Fatalf("removed containers = %d, want 2", response.RemovedContainers)
	}

	entries := readCommandLog(t, logPath)
	if len(entries) != 4 {
		t.Fatalf("docker entries = %#v, want ps, one concurrent stop request, and per-container rm", entries)
	}
	for _, want := range []string{
		"docker ps -aq --filter label=tako.project=demo --filter label=tako.environment=production --filter label=tako.service=old-api",
		"docker stop abc123 def456",
		"docker rm abc123",
		"docker rm def456",
	} {
		found := false
		for _, entry := range entries {
			if entry == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("docker log missing %q in %#v", want, entries)
		}
	}
	stopIndex := slices.Index(entries, "docker stop abc123 def456")
	firstRemoveIndex := slices.Index(entries, "docker rm abc123")
	if stopIndex < 0 || firstRemoveIndex <= stopIndex {
		t.Fatalf("graceful stop must precede removal: %#v", entries)
	}
}

func TestRemoveServiceKeepsRequestedRevisionContainers(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "blue123\ngreen456\nlegacy789\n")
	t.Setenv("TAKO_FAKE_INSPECT_LABELS_BY_ID", `{
		"blue123":"{\"tako.revision\":\"rev-blue\"}",
		"green456":"{\"tako.revision\":\"rev-green\"}",
		"legacy789":"{}"
	}`)

	response, err := RemoveService(context.Background(), RemoveServiceRequest{
		Project:      "demo",
		Environment:  "production",
		Service:      "web",
		KeepRevision: "rev-green",
	})
	if err != nil {
		t.Fatalf("RemoveService returned error: %v", err)
	}
	if response.RemovedContainers != 2 {
		t.Fatalf("removed containers = %d, want 2 stale containers", response.RemovedContainers)
	}

	entries := readCommandLog(t, logPath)
	if !slices.Contains(entries, "docker stop blue123 legacy789") || !slices.Contains(entries, "docker rm blue123") || !slices.Contains(entries, "docker rm legacy789") {
		t.Fatalf("docker log missing graceful stale revision removal in %#v", entries)
	}
	for _, entry := range entries {
		if strings.Contains(entry, "docker rm") && strings.Contains(entry, "green456") {
			t.Fatalf("kept revision container was removed: %#v", entries)
		}
	}
}

func TestRemoveContainerIDsDoesNotRemoveContainersWhenGracefulStopFails(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_DOCKER_STOP_ERROR", "stop failed")

	removed, err := removeContainerIDs(context.Background(), []string{"worker123"})
	if err == nil || !strings.Contains(err.Error(), "gracefully stop") {
		t.Fatalf("removeContainerIDs error = %v, want graceful stop failure", err)
	}
	if removed != 0 {
		t.Fatalf("removed containers = %d, want 0", removed)
	}
	entries := readCommandLog(t, logPath)
	if !slices.Equal(entries, []string{"docker stop worker123"}) {
		t.Fatalf("docker entries = %#v, want stop only", entries)
	}
}

func TestRemoveContainerIDsAcceptsDockerAutoRemovalAfterStop(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_DOCKER_RM_NOT_FOUND", "job123")

	removed, err := removeContainerIDs(context.Background(), []string{"job123"})
	if err != nil {
		t.Fatalf("removeContainerIDs returned error for auto-removed container: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed containers = %d, want 1", removed)
	}
}

func TestRemoveContainerIDsStopsMoreThanSixteenContainersInOneDrainWave(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	ids := make([]string, 17)
	for index := range ids {
		ids[index] = fmt.Sprintf("worker%02d", index+1)
	}
	if _, err := removeContainerIDs(context.Background(), ids); err != nil {
		t.Fatalf("removeContainerIDs returned error: %v", err)
	}
	entries := readCommandLog(t, logPath)
	wantStop := "docker stop " + strings.Join(ids, " ")
	if len(entries) != 1+len(ids) || entries[0] != wantStop {
		t.Fatalf("containers were not stopped in one daemon drain wave: %#v", entries)
	}
}

func TestCleanupStartedContainersSurvivesCanceledRequestContext(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := cleanupStartedContainers(ctx, []string{"worker123"}); err != nil {
		t.Fatalf("cleanupStartedContainers returned error: %v", err)
	}
	entries := readCommandLog(t, logPath)
	if !slices.Equal(entries, []string{"docker stop worker123", "docker rm worker123"}) {
		t.Fatalf("docker entries = %#v, want detached graceful cleanup", entries)
	}
}

func useFakeCommands(t *testing.T, logPath string) func() {
	t.Helper()
	oldDocker := dockerCommandContext
	dockerCommandContext = fakeCommandContext
	t.Setenv("GO_WANT_TAKOD_COMMAND_HELPER", "1")
	t.Setenv("TAKO_COMMAND_LOG", logPath)
	return func() {
		dockerCommandContext = oldDocker
	}
}

func fakeCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	commandArgs := append([]string{"-test.run=TestTakodCommandHelper", "--", name}, args...)
	return exec.CommandContext(ctx, os.Args[0], commandArgs...)
}

func TestTakodCommandHelper(t *testing.T) {
	if os.Getenv("GO_WANT_TAKOD_COMMAND_HELPER") != "1" {
		return
	}
	args := os.Args
	separator := -1
	for i, arg := range args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator == -1 || separator+1 >= len(args) {
		os.Exit(2)
	}
	command := args[separator+1]
	commandArgs := args[separator+2:]
	entry := strings.Join(append([]string{command}, commandArgs...), " ")
	if logPath := os.Getenv("TAKO_COMMAND_LOG"); logPath != "" {
		file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			os.Exit(2)
		}
		_, _ = file.WriteString(entry + "\n")
		_ = file.Close()
	}

	if command == "sh" {
		if os.Getenv("TAKO_FAKE_FAIL_SHELL") != "" && strings.Contains(strings.Join(commandArgs, " "), os.Getenv("TAKO_FAKE_FAIL_SHELL")) {
			_, _ = os.Stderr.WriteString("shell failed")
			os.Exit(7)
		}
		os.Exit(0)
	}

	if command != "docker" || len(commandArgs) == 0 {
		os.Exit(0)
	}
	switch commandArgs[0] {
	case "build":
		if output := os.Getenv("TAKO_FAKE_DOCKER_BUILD_ERROR"); output != "" {
			_, _ = os.Stderr.WriteString(output)
			os.Exit(1)
		}
		os.Exit(0)
	case "image":
		if len(commandArgs) > 1 && commandArgs[1] == "prune" {
			if output := os.Getenv("TAKO_FAKE_DOCKER_IMAGE_PRUNE_ERROR"); output != "" {
				_, _ = os.Stderr.WriteString(output)
				os.Exit(1)
			}
			_, _ = os.Stdout.WriteString("Deleted Images:\n")
			for _, id := range uniqueFields(os.Getenv("TAKO_FAKE_DANGLING_IMAGE_IDS")) {
				_, _ = os.Stdout.WriteString("deleted: " + id + "\n")
			}
			os.Exit(0)
		}
		if len(commandArgs) > 1 && commandArgs[1] == "inspect" {
			if os.Getenv("TAKO_FAKE_IMAGE_INSPECT_ERROR") != "" {
				_, _ = os.Stderr.WriteString("inspect failed")
				os.Exit(1)
			}
			_, _ = os.Stdout.WriteString(os.Getenv("TAKO_FAKE_IMAGE_INSPECT_OUTPUT"))
			os.Exit(0)
		}
		os.Exit(0)
	case "images":
		joined := strings.Join(commandArgs, " ")
		if strings.Contains(joined, "dangling=true") {
			if output := os.Getenv("TAKO_FAKE_DANGLING_IMAGE_IDS"); output != "" {
				_, _ = os.Stdout.WriteString(output)
			}
			os.Exit(0)
		}
		if output := os.Getenv("TAKO_FAKE_IMAGE_LIST_ERROR"); output != "" {
			_, _ = os.Stderr.WriteString(output)
			os.Exit(1)
		}
		if output := os.Getenv("TAKO_FAKE_IMAGE_LIST_OUTPUT"); output != "" {
			_, _ = os.Stdout.WriteString(output)
		}
		os.Exit(0)
	case "ps":
		if output := os.Getenv("TAKO_FAKE_DOCKER_PS_ERROR"); output != "" {
			_, _ = os.Stderr.WriteString(output)
			os.Exit(1)
		}
		if marker := os.Getenv("TAKO_FAKE_PROXY_RUNNING_MARKER"); marker != "" {
			if _, err := os.Stat(marker); err == nil {
				_, _ = os.Stdout.WriteString("tako-proxy\n")
			}
			os.Exit(0)
		}
		if strings.Contains(strings.Join(commandArgs, " "), "label=tako.project=") {
			if output := os.Getenv("TAKO_FAKE_PROJECT_PS_OUTPUT"); output != "" {
				_, _ = os.Stdout.WriteString(output)
				os.Exit(0)
			}
		}
		if output := os.Getenv("TAKO_FAKE_PS_OUTPUT"); output != "" {
			_, _ = os.Stdout.WriteString(output)
		}
		os.Exit(0)
	case "rm":
		if missing := os.Getenv("TAKO_FAKE_DOCKER_RM_NOT_FOUND"); missing != "" && slices.Contains(commandArgs[1:], missing) {
			_, _ = os.Stderr.WriteString("Error: No such container: " + missing)
			os.Exit(1)
		}
		if output := os.Getenv("TAKO_FAKE_DOCKER_RM_ERROR"); output != "" {
			_, _ = os.Stderr.WriteString(output)
			os.Exit(1)
		}
		if marker := os.Getenv("TAKO_FAKE_PROXY_RUNNING_MARKER"); marker != "" && slices.Contains(commandArgs[1:], "tako-proxy") {
			_ = os.Remove(marker)
		}
		os.Exit(0)
	case "create":
		helperStateFile := os.Getenv("TAKO_COMMAND_LOG") + ".helper"
		if stateFile := os.Getenv("TAKO_FAKE_VOLUME_OWNERSHIP_STATE_FILE"); stateFile != "" {
			if replacement := os.Getenv("TAKO_FAKE_VOLUME_REPLACE_ON_HELPER_CREATE"); replacement != "" {
				_ = os.WriteFile(stateFile, []byte("local|"+replacement+"\n"), 0600)
			}
		}
		for index, arg := range commandArgs {
			if arg == "-v" && index+1 < len(commandArgs) && (strings.HasSuffix(commandArgs[index+1], ":/volume") || strings.HasSuffix(commandArgs[index+1], ":/target")) {
				volume := strings.TrimSuffix(strings.TrimSuffix(commandArgs[index+1], ":/volume"), ":/target")
				_ = os.WriteFile(helperStateFile, []byte(volume), 0600)
			}
		}
		os.Exit(0)
	case "stop":
		if exact := os.Getenv("TAKO_FAKE_DOCKER_STOP_FAIL_EXACT"); exact != "" && len(commandArgs) == 2 && commandArgs[1] == exact {
			_, _ = os.Stderr.WriteString("selective stop failure")
			os.Exit(1)
		}
		if output := os.Getenv("TAKO_FAKE_DOCKER_STOP_ERROR"); output != "" {
			_, _ = os.Stderr.WriteString(output)
			os.Exit(1)
		}
		os.Exit(0)
	case "network":
		if len(commandArgs) > 2 && commandArgs[1] == "inspect" {
			missing := os.Getenv("TAKO_FAKE_MISSING_NETWORK_INSPECT")
			if missing != "" && commandArgs[2] == missing {
				_, _ = os.Stderr.WriteString("No such network")
				os.Exit(1)
			}
			if byNameRaw := os.Getenv("TAKO_FAKE_NETWORK_INSPECT_LABELS_BY_NAME"); byNameRaw != "" {
				byName := make(map[string]string)
				if err := json.Unmarshal([]byte(byNameRaw), &byName); err != nil {
					os.Exit(2)
				}
				_, _ = os.Stdout.WriteString(byName[commandArgs[2]])
				os.Exit(0)
			}
			if output := os.Getenv("TAKO_FAKE_NETWORK_INSPECT_LABELS"); output != "" {
				_, _ = os.Stdout.WriteString(output)
				os.Exit(0)
			}
		}
		if len(commandArgs) > 1 && commandArgs[1] == "ls" {
			if output := os.Getenv("TAKO_FAKE_NETWORK_LS_OUTPUT"); output != "" {
				_, _ = os.Stdout.WriteString(output)
			}
			os.Exit(0)
		}
		if len(commandArgs) > 2 && commandArgs[1] == "connect" {
			if marker := os.Getenv("TAKO_FAKE_FAIL_NETWORK_CONNECT_ONCE_FILE"); marker != "" {
				if _, err := os.Stat(marker); os.IsNotExist(err) {
					_ = os.WriteFile(marker, []byte("failed"), 0600)
					_, _ = os.Stderr.WriteString("could not find a network matching network mode stale")
					os.Exit(1)
				}
			}
		}
		_, _ = os.Stdout.WriteString("network-ok\n")
		os.Exit(0)
	case "volume":
		if len(commandArgs) > 2 && commandArgs[1] == "inspect" {
			missing := os.Getenv("TAKO_FAKE_MISSING_VOLUME_INSPECT")
			if missing != "" && commandArgs[len(commandArgs)-1] == missing {
				if _, err := os.Stat(os.Getenv("TAKO_COMMAND_LOG") + ".restore-target"); os.IsNotExist(err) {
					_, _ = os.Stderr.WriteString("No such volume")
					os.Exit(1)
				}
			}
			if strings.Contains(strings.Join(commandArgs, " "), ".CreatedAt") {
				identity := os.Getenv("TAKO_FAKE_VOLUME_IDENTITY")
				if identity == "" {
					identity = "local|2026-01-01T00:00:00Z|{}|{}|local"
				}
				_, _ = os.Stdout.WriteString(identity + "\n")
				os.Exit(0)
			}
			if strings.Contains(strings.Join(commandArgs, " "), "tako.restore-target-operation") {
				if data, err := os.ReadFile(os.Getenv("TAKO_COMMAND_LOG") + ".restore-target"); err == nil {
					_, _ = os.Stdout.Write(data)
					os.Exit(0)
				}
				_, _ = os.Stdout.WriteString("local|<no value>\n")
				os.Exit(0)
			}
			if strings.Contains(strings.Join(commandArgs, " "), "tako.runtime") && strings.Contains(strings.Join(commandArgs, " "), ".Options") {
				evidence := os.Getenv("TAKO_FAKE_VOLUME_CLEANUP_EVIDENCE")
				if evidence == "" {
					evidence = "local|demo|production|takod|<no value>|null"
				}
				_, _ = os.Stdout.WriteString(evidence + "\n")
				os.Exit(0)
			}
			if strings.Contains(strings.Join(commandArgs, " "), "tako.volume-ownership-operation") {
				if stateFile := os.Getenv("TAKO_FAKE_VOLUME_OWNERSHIP_STATE_FILE"); stateFile != "" {
					data, err := os.ReadFile(stateFile)
					if os.IsNotExist(err) {
						_, _ = os.Stderr.WriteString("No such volume")
						os.Exit(1)
					}
					_, _ = os.Stdout.Write(data)
					os.Exit(0)
				}
				if output := os.Getenv("TAKO_FAKE_VOLUME_OWNERSHIP_INSPECT"); output != "" {
					_, _ = os.Stdout.WriteString(output)
					os.Exit(0)
				}
			}
			os.Exit(0)
		}
		if len(commandArgs) > 1 && commandArgs[1] == "create" {
			restoreToken := ""
			for _, arg := range commandArgs {
				if strings.HasPrefix(arg, "tako.restore-target-operation=") {
					restoreToken = strings.TrimPrefix(arg, "tako.restore-target-operation=")
				}
			}
			if restoreToken != "" {
				if replacement := os.Getenv("TAKO_FAKE_RESTORE_TARGET_REPLACEMENT_TOKEN"); replacement != "" {
					restoreToken = replacement
				}
				_ = os.WriteFile(os.Getenv("TAKO_COMMAND_LOG")+".restore-target", []byte("local|"+restoreToken+"\n"), 0600)
				_ = os.WriteFile(os.Getenv("TAKO_COMMAND_LOG")+".restore-target-name", []byte(commandArgs[len(commandArgs)-1]), 0600)
			}
			if stateFile := os.Getenv("TAKO_FAKE_VOLUME_OWNERSHIP_STATE_FILE"); stateFile != "" {
				token := ""
				for _, arg := range commandArgs {
					if strings.HasPrefix(arg, "tako.volume-ownership-operation=") {
						token = strings.TrimPrefix(arg, "tako.volume-ownership-operation=")
					}
				}
				if token != "" {
					_ = os.WriteFile(stateFile, []byte("local|"+token+"\n"), 0600)
				}
			}
		}
		if len(commandArgs) > 1 && commandArgs[1] == "ls" {
			if output := os.Getenv("TAKO_FAKE_VOLUME_LS_OUTPUT"); output != "" {
				_, _ = os.Stdout.WriteString(output)
			}
		}
		if len(commandArgs) > 1 && commandArgs[1] == "rm" {
			if target, err := os.ReadFile(os.Getenv("TAKO_COMMAND_LOG") + ".restore-target-name"); err == nil && len(commandArgs) > 2 && commandArgs[2] == string(target) {
				_ = os.Remove(os.Getenv("TAKO_COMMAND_LOG") + ".restore-target")
				_ = os.Remove(os.Getenv("TAKO_COMMAND_LOG") + ".restore-target-name")
			}
			if ambiguous := os.Getenv("TAKO_FAKE_VOLUME_RM_AMBIGUOUS"); ambiguous != "" && strings.Contains(strings.Join(commandArgs, " "), ambiguous) {
				if stateFile := os.Getenv("TAKO_FAKE_VOLUME_OWNERSHIP_STATE_FILE"); stateFile != "" {
					_ = os.Remove(stateFile)
				}
				_, _ = os.Stderr.WriteString("daemon disconnected after volume removal")
				os.Exit(1)
			}
			failVolume := os.Getenv("TAKO_FAKE_FAIL_VOLUME_RM")
			if failVolume != "" && strings.Contains(strings.Join(commandArgs, " "), failVolume) {
				_, _ = os.Stderr.WriteString("volume is in use")
				os.Exit(1)
			}
			if stateFile := os.Getenv("TAKO_FAKE_VOLUME_OWNERSHIP_STATE_FILE"); stateFile != "" {
				_ = os.Remove(stateFile)
			}
			if markerFile := os.Getenv("TAKO_FAKE_VOLUME_OWNERSHIP_MARKER_FILE"); markerFile != "" {
				_ = os.Remove(markerFile)
			}
		}
		os.Exit(0)
	case "pull":
		_, _ = os.Stdout.WriteString("pulled\n")
		os.Exit(0)
	case "run":
		if fragment := os.Getenv("TAKO_FAKE_DOCKER_RUN_FAIL_CONTAINS"); fragment != "" && strings.Contains(strings.Join(commandArgs, " "), fragment) {
			marker := os.Getenv("TAKO_FAKE_DOCKER_RUN_FAIL_ONCE_FILE")
			if marker == "" {
				_, _ = os.Stderr.WriteString("selective docker run failure")
				os.Exit(1)
			}
			if _, err := os.Stat(marker); os.IsNotExist(err) {
				_ = os.WriteFile(marker, []byte("failed"), 0600)
				_, _ = os.Stderr.WriteString("selective docker run failure")
				os.Exit(1)
			}
		}
		if strings.Contains(strings.Join(commandArgs, " "), "/quota-probe") {
			_, _ = os.Stdout.WriteString("dd: writing '/quota-probe': No space left on device\n")
			os.Exit(0)
		}
		joined := strings.Join(commandArgs, " ")
		if strings.Contains(joined, "stat -c %u:%g /volume") {
			owner := os.Getenv("TAKO_FAKE_VOLUME_ROOT_OWNER")
			if owner == "" {
				owner = "0:0"
			}
			_, _ = os.Stdout.WriteString(owner + "\n")
			os.Exit(0)
		}
		if strings.Contains(joined, "cat /volume/.tako-volume-ownership-v1") {
			if marker := os.Getenv("TAKO_FAKE_VOLUME_OWNERSHIP_MARKER_FILE"); marker != "" {
				if _, err := os.Stat(marker); os.IsNotExist(err) {
					os.Exit(1)
				}
			}
		}
		if strings.Contains(joined, "printf") && strings.Contains(joined, ".tako-volume-ownership-v1") {
			if marker := os.Getenv("TAKO_FAKE_VOLUME_OWNERSHIP_MARKER_FILE"); marker != "" {
				_ = os.WriteFile(marker, []byte("initialized"), 0600)
			}
		}
		if strings.Contains(strings.Join(commandArgs, " "), "du -sk /source") {
			_, _ = os.Stdout.WriteString("7\t/source\n")
			os.Exit(0)
		}
		if os.Getenv("TAKO_FAKE_DOCKER_RUN_BLOCK_UNTIL_CANCEL") == "1" {
			if marker := os.Getenv("TAKO_FAKE_DOCKER_RUN_STARTED_FILE"); marker != "" {
				_ = os.WriteFile(marker, []byte("started"), 0600)
			}
			time.Sleep(time.Hour)
		}
		if marker := os.Getenv("TAKO_FAKE_PROXY_RUNNING_MARKER"); marker != "" {
			for index, arg := range commandArgs {
				if arg == "--name" && index+1 < len(commandArgs) && commandArgs[index+1] == "tako-proxy" {
					_ = os.WriteFile(marker, []byte("running"), 0600)
				}
			}
		}
		createFakeBackupArchive(commandArgs)
		if id := os.Getenv("TAKO_FAKE_DOCKER_RUN_ID"); id != "" {
			_, _ = os.Stdout.WriteString(id + "\n")
			os.Exit(0)
		}
		_, _ = os.Stdout.WriteString("container-id\n")
		os.Exit(0)
	case "rename":
		if fragment := os.Getenv("TAKO_FAKE_DOCKER_RENAME_FAIL_CONTAINS"); fragment != "" && strings.Contains(strings.Join(commandArgs, " "), fragment) {
			_, _ = os.Stderr.WriteString("selective rename failure")
			os.Exit(1)
		}
		if marker := os.Getenv("TAKO_FAKE_PROXY_RUNNING_MARKER"); marker != "" && len(commandArgs) >= 3 {
			if commandArgs[1] == "tako-proxy" {
				_ = os.Remove(marker)
			}
			if commandArgs[2] == "tako-proxy" {
				_ = os.WriteFile(marker, []byte("running"), 0600)
			}
		}
		os.Exit(0)
	case "start":
		if output := os.Getenv("TAKO_FAKE_DOCKER_START_ERROR"); output != "" {
			_, _ = os.Stderr.WriteString(output)
			os.Exit(1)
		}
		if marker := os.Getenv("TAKO_FAKE_PROXY_RUNNING_MARKER"); marker != "" && slices.Contains(commandArgs[1:], "tako-proxy") {
			_ = os.WriteFile(marker, []byte("running"), 0600)
		}
		os.Exit(0)
	case "exec":
		joined := strings.Join(commandArgs, " ")
		if fragment := os.Getenv("TAKO_FAKE_DOCKER_EXEC_FAIL_CONTAINS"); fragment != "" && strings.Contains(joined, fragment) {
			if marker := os.Getenv("TAKO_FAKE_DOCKER_EXEC_FAIL_ONCE_FILE"); marker == "" {
				_, _ = os.Stderr.WriteString("selective exec failure")
				os.Exit(1)
			} else if _, err := os.Stat(marker); os.IsNotExist(err) {
				_ = os.WriteFile(marker, []byte("failed"), 0600)
				_, _ = os.Stderr.WriteString("selective exec failure")
				os.Exit(1)
			}
		}
		if output := os.Getenv("TAKO_FAKE_DOCKER_EXEC_ERROR"); output != "" {
			_, _ = os.Stderr.WriteString(output)
			os.Exit(1)
		}
		if os.Getenv("TAKO_FAKE_DOCKER_EXEC_INTERACTIVE") == "echo" {
			scanner := bufio.NewScanner(os.Stdin)
			for scanner.Scan() {
				line := strings.TrimRight(scanner.Text(), "\r")
				if line == "exit" {
					break
				}
				_, _ = os.Stdout.WriteString("echo:" + line + "\n")
			}
			code := 0
			if v := os.Getenv("TAKO_FAKE_DOCKER_EXEC_EXIT"); v != "" {
				code, _ = strconv.Atoi(v)
			}
			os.Exit(code)
		}
		if strings.Contains(joined, " psql ") {
			if strings.Contains(joined, "SHOW server_version_num") {
				version := os.Getenv("TAKO_FAKE_POSTGRES_VERSION")
				if version == "" {
					version = "160000"
				}
				_, _ = os.Stdout.WriteString(version + "\n")
			} else {
				_, _ = os.Stdout.WriteString(os.Getenv("TAKO_FAKE_PSQL_OUTPUT"))
			}
		}
		os.Exit(0)
	case "inspect":
		joined := strings.Join(commandArgs, " ")
		if strings.Contains(joined, ".HostConfig.RestartPolicy.Name") {
			// Retention inspect: TAKO_FAKE_RETAIN_INSPECT_BY_ID maps a container
			// ID to its "/name|running|restartPolicy|labelsJSON" line.
			if byIDRaw := os.Getenv("TAKO_FAKE_RETAIN_INSPECT_BY_ID"); byIDRaw != "" && len(commandArgs) > 1 {
				byID := make(map[string]string)
				if err := json.Unmarshal([]byte(byIDRaw), &byID); err != nil {
					os.Exit(2)
				}
				_, _ = os.Stdout.WriteString(byID[commandArgs[1]] + "\n")
			}
			os.Exit(0)
		}
		if strings.Contains(joined, `eq .Destination "/volume"`) || strings.Contains(joined, `eq .Destination "/target"`) {
			if volume, err := os.ReadFile(os.Getenv("TAKO_COMMAND_LOG") + ".helper"); err == nil {
				_, _ = os.Stdout.WriteString("volume|" + string(volume) + "\n")
			}
			os.Exit(0)
		}
		if strings.Contains(joined, ".Args") {
			if output := os.Getenv("TAKO_FAKE_INSPECT_ARGS"); output != "" {
				_, _ = os.Stdout.WriteString(output)
			}
			os.Exit(0)
		}
		if strings.Contains(joined, ".NetworkSettings.Ports") {
			if output := os.Getenv("TAKO_FAKE_INSPECT_PORTS"); output != "" {
				_, _ = os.Stdout.WriteString(output)
			}
			os.Exit(0)
		}
		if strings.Contains(joined, ".Config.Image") {
			if output := os.Getenv("TAKO_FAKE_INSPECT_IMAGE"); output != "" {
				_, _ = os.Stdout.WriteString(output)
			}
			os.Exit(0)
		}
		if strings.Contains(joined, ".Mounts") {
			if output := os.Getenv("TAKO_FAKE_INSPECT_MOUNTS"); output != "" {
				_, _ = os.Stdout.WriteString(output)
			}
			os.Exit(0)
		}
		if strings.Contains(joined, ".Config.Env") {
			if output := os.Getenv("TAKO_FAKE_INSPECT_ENV"); output != "" {
				_, _ = os.Stdout.WriteString(output)
			}
			os.Exit(0)
		}
		if strings.Contains(joined, ".State.Health.Status") {
			if output := os.Getenv("TAKO_FAKE_HEALTH_STATUS"); output != "" {
				_, _ = os.Stdout.WriteString(output + "\n")
			}
			os.Exit(0)
		}
		if strings.Contains(joined, ".NetworkSettings.Networks") {
			if output := os.Getenv("TAKO_FAKE_CONTAINER_IP"); output != "" {
				_, _ = os.Stdout.WriteString(output + "\n")
			}
			os.Exit(0)
		}
		if strings.Contains(joined, ".HostConfig.PortBindings") {
			if output := os.Getenv("TAKO_FAKE_INSPECT_PORT_BINDINGS"); output != "" {
				_, _ = os.Stdout.WriteString(output)
			}
			os.Exit(0)
		}
		if strings.Contains(joined, ".Config.Labels") {
			if byIDRaw := os.Getenv("TAKO_FAKE_INSPECT_LABELS_BY_ID"); byIDRaw != "" {
				byID := make(map[string]string)
				if err := json.Unmarshal([]byte(byIDRaw), &byID); err != nil {
					os.Exit(2)
				}
				if len(commandArgs) > 1 {
					_, _ = os.Stdout.WriteString(byID[commandArgs[1]])
				}
				os.Exit(0)
			}
			if output := os.Getenv("TAKO_FAKE_INSPECT_LABELS"); output != "" {
				_, _ = os.Stdout.WriteString(output)
			}
			os.Exit(0)
		}
		if strings.Contains(joined, ".State.Running") {
			if marker := os.Getenv("TAKO_FAKE_PROXY_RUNNING_FALSE_ONCE_FILE"); marker != "" {
				if _, err := os.Stat(marker); os.IsNotExist(err) {
					_ = os.WriteFile(marker, []byte("failed"), 0600)
					_, _ = os.Stdout.WriteString("false\n")
					os.Exit(0)
				}
			}
			_, _ = os.Stdout.WriteString("true\n")
		}
		os.Exit(0)
	case "info":
		if output := os.Getenv("TAKO_FAKE_DOCKER_INFO_OUTPUT"); output != "" {
			_, _ = os.Stdout.WriteString(output)
		}
		os.Exit(0)
	case "logs":
		_, _ = os.Stdout.WriteString("logs\n")
		os.Exit(0)
	case "stats":
		if output := os.Getenv("TAKO_FAKE_STATS_OUTPUT"); output != "" {
			_, _ = os.Stdout.WriteString(output)
		}
		os.Exit(0)
	default:
		os.Exit(0)
	}
}

func createFakeBackupArchive(commandArgs []string) {
	backupPath := ""
	for i := 0; i+1 < len(commandArgs); i++ {
		if commandArgs[i] != "-v" {
			continue
		}
		hostPath, containerPath, ok := strings.Cut(commandArgs[i+1], ":")
		if ok && containerPath == "/backup" {
			backupPath = hostPath
			break
		}
	}
	if backupPath == "" {
		return
	}
	for i := 0; i+1 < len(commandArgs); i++ {
		if commandArgs[i] != "-czf" || !strings.HasPrefix(commandArgs[i+1], "/backup/") {
			continue
		}
		name := strings.TrimPrefix(commandArgs[i+1], "/backup/")
		_ = os.MkdirAll(backupPath, 0750)
		file, err := os.OpenFile(filepath.Join(backupPath, name), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			return
		}
		compressed := gzip.NewWriter(file)
		archive := tar.NewWriter(compressed)
		content := []byte("backup")
		_ = archive.WriteHeader(&tar.Header{Name: "data", Mode: 0600, Size: int64(len(content))})
		_, _ = archive.Write(content)
		_ = archive.Close()
		_ = compressed.Close()
		_ = file.Close()
		return
	}
}

func readCommandLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("failed to read command log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}
