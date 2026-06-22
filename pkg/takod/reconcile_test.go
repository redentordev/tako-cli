package takod

import (
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
	"strings"
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

	validNoDowntime := valid
	validNoDowntime.DeployStrategy = "rolling"
	validNoDowntime.Revision = "rev-green"
	validNoDowntime.Containers = []ContainerSpec{{Name: "demo_production_web_r_rev_green_1", Labels: map[string]string{"tako.revision": "rev-green"}}}
	if err := validateReconcileServiceRequest(validNoDowntime); err != nil {
		t.Fatalf("valid no-downtime request returned error: %v", err)
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
	invalid.Labels = map[string]string{"tako.configHash": "abc\n--privileged"}
	if err := validateReconcileServiceRequest(invalid); err == nil {
		t.Fatalf("expected unsafe label value to be rejected")
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
			"tako.role": "frontend",
		},
		Mounts:  []string{"type=volume,source=demo_data,target=/data"},
		Command: "npm run worker",
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
		"--label", "tako.environment=production",
		"--label", "tako.project=demo",
		"--label", "tako.revision=rev1",
		"--label", "tako.role=frontend",
		"--label", "tako.runtime=takod",
		"--label", "tako.service=web",
		"--label", "tako.slot=1",
		"--env-file", "/tmp/web.env",
		"--mount", "type=volume,source=demo_data,target=/data",
		"--publish", "10.42.0.2:31001:3000",
		"registry.example.com/demo/web:abc",
		"sh", "-c", "npm run worker",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected docker args:\ngot:  %#v\nwant: %#v", got, want)
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
		if strings.HasPrefix(entry, "docker ps -aq --filter label=tako.project=demo") || strings.HasPrefix(entry, "docker rm -f") {
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
		if strings.HasPrefix(entry, "docker ps -aq --filter label=tako.project=frontend") || strings.HasPrefix(entry, "docker rm -f") {
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
			Command: "curl -sf http://127.0.0.1:3000/health || exit 1",
		},
	})
	if err == nil {
		t.Fatal("ReconcileService should fail for unhealthy containers")
	}

	entries := readCommandLog(t, logPath)
	if !slices.Contains(entries, "docker rm -f demo_production_web_1") {
		t.Fatalf("docker log missing cleanup of started container in %#v", entries)
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
	if !slices.Contains(entries, "docker rm -f demo_production_web_1") {
		t.Fatalf("docker log missing cleanup of started container in %#v", entries)
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
	if !slices.Contains(entries, "docker rm -f green456") {
		t.Fatalf("docker log missing target revision removal in %#v", entries)
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
	if !slices.Contains(entries, "docker rm -f green456") {
		t.Fatalf("docker log missing target revision removal in %#v", entries)
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
	if len(entries) != 2 {
		t.Fatalf("docker entries = %#v, want ps and rm", entries)
	}
	for _, want := range []string{
		"docker ps -aq --filter label=tako.project=demo --filter label=tako.environment=production --filter label=tako.service=old-api",
		"docker rm -f abc123 def456",
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
	if !slices.Contains(entries, "docker rm -f blue123 legacy789") {
		t.Fatalf("docker log missing stale revision removal in %#v", entries)
	}
	for _, entry := range entries {
		if strings.Contains(entry, "docker rm") && strings.Contains(entry, "green456") {
			t.Fatalf("kept revision container was removed: %#v", entries)
		}
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
		os.Exit(0)
	case "images":
		joined := strings.Join(commandArgs, " ")
		if strings.Contains(joined, "dangling=true") {
			if output := os.Getenv("TAKO_FAKE_DANGLING_IMAGE_IDS"); output != "" {
				_, _ = os.Stdout.WriteString(output)
			}
			os.Exit(0)
		}
		os.Exit(0)
	case "ps":
		if output := os.Getenv("TAKO_FAKE_PS_OUTPUT"); output != "" {
			_, _ = os.Stdout.WriteString(output)
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
			if missing != "" && commandArgs[2] == missing {
				_, _ = os.Stderr.WriteString("No such volume")
				os.Exit(1)
			}
			os.Exit(0)
		}
		if len(commandArgs) > 1 && commandArgs[1] == "ls" {
			if output := os.Getenv("TAKO_FAKE_VOLUME_LS_OUTPUT"); output != "" {
				_, _ = os.Stdout.WriteString(output)
			}
		}
		if len(commandArgs) > 1 && commandArgs[1] == "rm" {
			failVolume := os.Getenv("TAKO_FAKE_FAIL_VOLUME_RM")
			if failVolume != "" && strings.Contains(strings.Join(commandArgs, " "), failVolume) {
				_, _ = os.Stderr.WriteString("volume is in use")
				os.Exit(1)
			}
		}
		os.Exit(0)
	case "pull":
		_, _ = os.Stdout.WriteString("pulled\n")
		os.Exit(0)
	case "run":
		_, _ = os.Stdout.WriteString("container-id\n")
		os.Exit(0)
	case "exec":
		if output := os.Getenv("TAKO_FAKE_DOCKER_EXEC_ERROR"); output != "" {
			_, _ = os.Stderr.WriteString(output)
			os.Exit(1)
		}
		os.Exit(0)
	case "inspect":
		joined := strings.Join(commandArgs, " ")
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
			_, _ = os.Stdout.WriteString("true\n")
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

func readCommandLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read command log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}
