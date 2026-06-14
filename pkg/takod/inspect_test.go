package takod

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestInspectProjectReturnsScopedSafeContainerDetails(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	t.Setenv("TAKO_FAKE_PS_OUTPUT", "web-2\nweb-1\nother-1\n")
	t.Setenv("TAKO_FAKE_INSPECT_OUTPUT", `[
  {
    "Id": "web-container-2-abcdef",
    "Name": "/web-2",
    "Image": "sha256:image2",
    "Config": {
      "Image": "demo/web:2",
      "Env": ["SECRET_VALUE=must-not-leak"],
      "Labels": {
        "tako.project": "demo",
        "tako.environment": "production",
        "tako.service": "web",
        "tako.slot": "2",
        "tako.configHash": "hash-web",
        "tako.runtimeId": "runtime-web"
      }
    },
    "State": {"Status": "running", "Running": true, "ExitCode": 0, "StartedAt": "2026-06-15T00:00:00Z", "Health": {"Status": "healthy"}},
    "NetworkSettings": {
      "Ports": {"3000/tcp": [{"HostIp": "127.0.0.1", "HostPort": "32000"}], "9090/tcp": null},
      "Networks": {"tako_demo_production": {"IPAddress": "172.20.0.3"}}
    },
    "Mounts": [{"Type": "volume", "Name": "tako_demo_production_data", "Source": "/var/lib/docker/volumes/tako_demo_production_data/_data", "Destination": "/data", "RW": true}]
  },
  {
    "Id": "web-container-1-abcdef",
    "Name": "/web-1",
    "Image": "sha256:image1",
    "Config": {
      "Image": "demo/web:1",
      "Labels": {
        "tako.project": "demo",
        "tako.environment": "production",
        "tako.service": "web",
        "tako.slot": "1"
      }
    },
    "State": {"Status": "exited", "Running": false, "ExitCode": 7, "FinishedAt": "2026-06-15T00:01:00Z"},
    "NetworkSettings": {"Ports": {}, "Networks": {}},
    "Mounts": []
  },
  {
    "Id": "other-container",
    "Name": "/other-1",
    "Config": {"Image": "other/web:1", "Labels": {"tako.project": "other", "tako.environment": "production", "tako.service": "web"}},
    "State": {"Status": "running", "Running": true}
  }
]`)

	response, err := InspectProject(context.Background(), InspectRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
	}, "node-a")
	if err != nil {
		t.Fatalf("InspectProject returned error: %v", err)
	}
	if response.Node != "node-a" {
		t.Fatalf("node = %q, want node-a", response.Node)
	}
	if len(response.Services) != 1 || response.Services[0].Service != "web" {
		t.Fatalf("unexpected services: %#v", response.Services)
	}
	containers := response.Services[0].Containers
	if len(containers) != 2 {
		t.Fatalf("container count = %d, want 2: %#v", len(containers), containers)
	}
	if containers[0].Name != "web-1" || containers[0].Slot != 1 || containers[0].State != "exited" || containers[0].ExitCode != 7 {
		t.Fatalf("unexpected first container: %#v", containers[0])
	}
	if containers[1].Name != "web-2" || containers[1].Health != "healthy" || containers[1].ShortID != "web-containe" {
		t.Fatalf("unexpected second container: %#v", containers[1])
	}
	if len(containers[1].Ports) != 2 || containers[1].Ports[0].PrivatePort != 3000 || containers[1].Ports[0].HostPort != "32000" {
		t.Fatalf("unexpected ports: %#v", containers[1].Ports)
	}
	if len(containers[1].Mounts) != 1 || containers[1].Mounts[0].Destination != "/data" || !containers[1].Mounts[0].RW {
		t.Fatalf("unexpected mounts: %#v", containers[1].Mounts)
	}
	if len(containers[1].Networks) != 1 || containers[1].Networks[0].IPAddress != "172.20.0.3" {
		t.Fatalf("unexpected networks: %#v", containers[1].Networks)
	}
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("failed to marshal response: %v", err)
	}
	if strings.Contains(string(data), "SECRET_VALUE") {
		t.Fatalf("inspect response leaked container env: %s", data)
	}

	entries := readCommandLog(t, logPath)
	want := "docker ps -a --filter label=tako.project=demo --filter label=tako.environment=production --filter label=tako.service=web --format {{.Names}}"
	if !slices.Contains(entries, want) {
		t.Fatalf("docker log missing service-scoped inspect ps %q in %#v", want, entries)
	}
}

func TestInspectProjectRejectsInvalidRequest(t *testing.T) {
	_, err := InspectProject(context.Background(), InspectRequest{
		Project:     "../demo",
		Environment: "production",
	}, "")
	if err == nil {
		t.Fatal("expected invalid project to be rejected")
	}

	_, err = InspectProject(context.Background(), InspectRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "../web",
	}, "")
	if err == nil {
		t.Fatal("expected invalid service to be rejected")
	}
}

func TestHandleInspectReturnsScopedResponse(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "web-1\n")
	t.Setenv("TAKO_FAKE_INSPECT_OUTPUT", `[
  {
    "Id": "container-1",
    "Name": "/web-1",
    "Config": {"Image": "demo/web:1", "Labels": {"tako.project": "demo", "tako.environment": "production", "tako.service": "web"}},
    "State": {"Status": "running", "Running": true}
  }
]`)

	server := NewServerWithOptions("", "", "dev", ServerOptions{NodeName: "node-a"})
	req := httptest.NewRequest(http.MethodGet, "/v1/inspect?project=demo&environment=production&service=web", nil)
	recorder := httptest.NewRecorder()
	server.handleInspect(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var response InspectResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if response.Node != "node-a" || len(response.Containers) != 1 || response.Containers[0].Service != "web" {
		t.Fatalf("unexpected inspect response: %#v", response)
	}
}
