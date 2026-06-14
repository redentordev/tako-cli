package cmd

import (
	"fmt"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takod"
)

func TestInspectRowsFromResponseBuildsDisplayRows(t *testing.T) {
	rows, err := inspectRowsFromResponse("node-a", takod.InspectResponse{
		Project:     "demo",
		Environment: "production",
		Containers: []takod.InspectContainer{
			{
				ID:      "container-id-123456",
				ShortID: "container-id",
				Name:    "web-1",
				Service: "web",
				Slot:    1,
				Image:   "demo/web:1",
				State:   "running",
				Running: true,
				Health:  "healthy",
				Ports: []takod.InspectPort{
					{PrivatePort: 3000, Protocol: "tcp", HostIP: "127.0.0.1", HostPort: "31000"},
				},
				Mounts: []takod.InspectMount{
					{Name: "data", Destination: "/data", RW: true},
				},
			},
		},
	}, "demo", "production", "web")
	if err != nil {
		t.Fatalf("inspectRowsFromResponse returned error: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %#v, want one row", rows)
	}
	row := rows[0]
	if row.Node != "node-a" || row.Service != "web" || row.Slot != 1 || row.Container != "web-1@container-id" {
		t.Fatalf("unexpected row identity: %#v", row)
	}
	if row.State != "running" || row.Health != "healthy" || row.Ports != "127.0.0.1:31000->3000/tcp" || row.Mounts != "data:/data:rw" {
		t.Fatalf("unexpected row details: %#v", row)
	}
}

func TestInspectRowsFromResponseRejectsMismatchedService(t *testing.T) {
	_, err := inspectRowsFromResponse("node-a", takod.InspectResponse{
		Project:     "demo",
		Environment: "production",
		Containers: []takod.InspectContainer{
			{ID: "container-id", Name: "api-1", Service: "api"},
		},
	}, "demo", "production", "web")
	if err == nil {
		t.Fatal("expected mismatched service to be rejected")
	}
}

func TestGatherInspectRowsWithWarnsOnOneFailedNode(t *testing.T) {
	servers := map[string]config.ServerConfig{
		"node-a": {Host: "node-a"},
		"node-b": {Host: "node-b"},
	}
	rows, warnings, err := gatherInspectRowsWith(servers, []string{"node-a", "node-b"}, func(serverName string, _ config.ServerConfig) ([]inspectRow, error) {
		if serverName == "node-b" {
			return nil, fmt.Errorf("unreachable")
		}
		return []inspectRow{{Node: serverName, Service: "web", Container: "web-1@abc", State: "running"}}, nil
	})
	if err != nil {
		t.Fatalf("gatherInspectRowsWith returned error: %v", err)
	}
	if len(rows) != 1 || rows[0].Node != "node-a" {
		t.Fatalf("unexpected rows: %#v", rows)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "node-b: unreachable") {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}
}

func TestGatherInspectRowsWithFailsWhenEveryNodeFails(t *testing.T) {
	servers := map[string]config.ServerConfig{"node-a": {Host: "node-a"}}
	_, _, err := gatherInspectRowsWith(servers, []string{"node-a"}, func(string, config.ServerConfig) ([]inspectRow, error) {
		return nil, fmt.Errorf("unreachable")
	})
	if err == nil {
		t.Fatal("expected all-node failure to return an error")
	}
}

func TestInspectFormatters(t *testing.T) {
	if got := displayContainerState(takod.InspectContainer{State: "exited", ExitCode: 7}); got != "exited(7)" {
		t.Fatalf("state = %q, want exited(7)", got)
	}
	if got := displayContainerHealth(takod.InspectContainer{Running: false}); got != "n/a" {
		t.Fatalf("health = %q, want n/a", got)
	}
	ports := formatInspectPorts([]takod.InspectPort{
		{PrivatePort: 80, Protocol: "tcp"},
		{PrivatePort: 53, Protocol: "udp", HostIP: "10.0.0.1", HostPort: "5353"},
	})
	if ports != "10.0.0.1:5353->53/udp,80/tcp" {
		t.Fatalf("ports = %q", ports)
	}
	mounts := formatInspectMounts([]takod.InspectMount{
		{Type: "bind", Source: "/host", Destination: "/data", RW: false},
	})
	if mounts != "/host:/data:ro" {
		t.Fatalf("mounts = %q", mounts)
	}
}
