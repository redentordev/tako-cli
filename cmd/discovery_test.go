package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodstate"
)

func TestValidateDiscoveryEndpointAcceptsPrivateHealthyEndpoint(t *testing.T) {
	endpoint, err := validateDiscoveryEndpoint(validDiscoveryEndpoint(), "web", 3000)
	if err != nil {
		t.Fatalf("validateDiscoveryEndpoint returned error: %v", err)
	}
	if endpoint.Address != "172.20.0.20:3000" {
		t.Fatalf("address = %q, want 172.20.0.20:3000", endpoint.Address)
	}
}

func TestValidateDiscoveryEndpointAcceptsMeshEndpoint(t *testing.T) {
	endpoint := validDiscoveryEndpoint()
	endpoint.Host = "10.210.0.10"
	endpoint.Address = "10.210.0.10:24567"
	endpoint.Scope = "mesh"

	got, err := validateDiscoveryEndpoint(endpoint, "web", 3000)
	if err != nil {
		t.Fatalf("validateDiscoveryEndpoint returned error: %v", err)
	}
	if got.Address != "10.210.0.10:24567" {
		t.Fatalf("address = %q, want mesh published address", got.Address)
	}
}

func TestValidateDiscoveryEndpointRejectsUnsafeResponses(t *testing.T) {
	tests := []struct {
		name   string
		update func(*takod.DiscoveryEndpoint)
	}{
		{
			name: "public host",
			update: func(endpoint *takod.DiscoveryEndpoint) {
				endpoint.Host = "203.0.113.10"
				endpoint.Address = "203.0.113.10:3000"
			},
		},
		{
			name: "wrong scope",
			update: func(endpoint *takod.DiscoveryEndpoint) {
				endpoint.Scope = "remote"
			},
		},
		{
			name: "unhealthy",
			update: func(endpoint *takod.DiscoveryEndpoint) {
				endpoint.Healthy = false
			},
		},
		{
			name: "address mismatch",
			update: func(endpoint *takod.DiscoveryEndpoint) {
				endpoint.Address = "172.20.0.21:3000"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			endpoint := validDiscoveryEndpoint()
			tt.update(&endpoint)
			if _, err := validateDiscoveryEndpoint(endpoint, "web", 3000); err == nil {
				t.Fatal("expected unsafe response to be rejected")
			}
		})
	}
}

func TestDiscoveryRowsFromResponseUsesServerNameWhenNodeMissing(t *testing.T) {
	response := takod.DiscoveryResponse{
		Project:     "demo",
		Environment: "production",
		Endpoints:   []takod.DiscoveryEndpoint{validDiscoveryEndpoint()},
	}

	rows, err := discoveryRowsFromResponse("node-a", response, "demo", "production", "web", discoveryPortSpec{Name: "http", Target: 3000, Protocol: "http"})
	if err != nil {
		t.Fatalf("discoveryRowsFromResponse returned error: %v", err)
	}
	if len(rows) != 1 || rows[0].Node != "node-a" || rows[0].Address != "172.20.0.20:3000" {
		t.Fatalf("unexpected rows: %#v", rows)
	}
}

func TestGatherDiscoveryRowsWithWarnsOnOneFailedNode(t *testing.T) {
	servers := map[string]config.ServerConfig{
		"node-a": {Host: "node-a"},
		"node-b": {Host: "node-b"},
	}
	rows, warnings, err := gatherDiscoveryRowsWith(
		servers,
		[]string{"web"},
		[]string{"node-a", "node-b"},
		func(serverName string, _ config.ServerConfig, serviceName string, port discoveryPortSpec) ([]discoveryRow, error) {
			if serverName == "node-b" {
				return nil, fmt.Errorf("unreachable")
			}
			return []discoveryRow{{
				Node:      serverName,
				Service:   serviceName,
				Port:      port,
				Container: "web-1",
				Address:   "172.20.0.20:3000",
			}}, nil
		},
		func(string) []discoveryPortSpec {
			return []discoveryPortSpec{{Name: "http", Target: 3000, Protocol: "http"}}
		},
	)
	if err != nil {
		t.Fatalf("gatherDiscoveryRowsWith returned error: %v", err)
	}
	if len(rows) != 1 || rows[0].Node != "node-a" {
		t.Fatalf("unexpected rows: %#v", rows)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "node-b: unreachable") {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}
}

func TestGatherDiscoveryRowsWithFailsWhenEveryNodeFails(t *testing.T) {
	servers := map[string]config.ServerConfig{"node-a": {Host: "node-a"}}
	_, _, err := gatherDiscoveryRowsWith(
		servers,
		[]string{"web"},
		[]string{"node-a"},
		func(string, config.ServerConfig, string, discoveryPortSpec) ([]discoveryRow, error) {
			return nil, fmt.Errorf("unreachable")
		},
		func(string) []discoveryPortSpec {
			return []discoveryPortSpec{{Name: "http", Target: 3000, Protocol: "http"}}
		},
	)
	if err == nil {
		t.Fatal("expected all-node failure to return an error")
	}
}

func TestImportDiscoveryServerNamesUsesImportServers(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "edge"},
		Servers: map[string]config.ServerConfig{
			"edge-node": {Host: "10.0.0.1"},
			"app-node":  {Host: "10.0.0.2"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {Servers: []string{"edge-node"}},
		},
	}
	importConfig := config.ImportConfig{
		Project:     "jardin-cms",
		Environment: "production",
		Service:     "site-renderer",
		Port:        "web",
		Servers:     []string{"app-node"},
	}

	servers, err := importDiscoveryServerNames(cfg, "production", importConfig, "")
	if err != nil {
		t.Fatalf("importDiscoveryServerNames returned error: %v", err)
	}
	if !slices.Equal(servers, []string{"app-node"}) {
		t.Fatalf("servers = %#v, want app-node", servers)
	}
	_, err = importDiscoveryServerNames(cfg, "production", importConfig, "edge-node")
	if err == nil {
		t.Fatal("expected requested server outside import server set to be rejected")
	}
}

func TestResolveImportExportReadsDesiredExportRecord(t *testing.T) {
	desired := takodstate.DesiredRevision{
		Project:     "jardin-cms",
		Environment: "production",
		Exports: []takodstate.DesiredExportRecord{{
			Project:     "jardin-cms",
			Environment: "production",
			Service:     "site-renderer",
			Port:        "web",
			Target:      3000,
			Protocol:    "http",
		}},
	}
	content, err := json.Marshal(desired)
	if err != nil {
		t.Fatalf("failed to marshal desired state: %v", err)
	}
	response, err := json.Marshal(takod.StateDocumentResponse{Found: true, Content: string(content)})
	if err != nil {
		t.Fatalf("failed to marshal state response: %v", err)
	}
	resolved, err := resolveImportExport(staticRequestExecutor{output: string(response)}, "/run/tako/takod.sock", "jardin_renderer", config.ImportConfig{
		Project:     "jardin-cms",
		Environment: "production",
		Service:     "site-renderer",
		Port:        "web",
	})
	if err != nil {
		t.Fatalf("resolveImportExport returned error: %v", err)
	}
	if resolved.Target != 3000 || resolved.Protocol != "http" {
		t.Fatalf("resolved = %#v, want web:3000/http", resolved)
	}
}

func TestDiscoveryPortsForServiceUsesConfiguredPorts(t *testing.T) {
	ports := discoveryPortsForService(config.ServiceConfig{
		Ports: []config.PortConfig{
			{Name: "metrics", Target: 9090, Protocol: "tcp", Mode: "internal"},
			{Name: "http", Target: 3000, Protocol: "http", Mode: "proxy"},
		},
	}, 0)

	got := []string{discoveryPortLabel(ports[0]), discoveryPortLabel(ports[1])}
	if !slices.Equal(got, []string{"http:3000", "metrics:9090"}) {
		t.Fatalf("port labels = %#v, want configured sorted ports", got)
	}
}

func TestDiscoveryPortsForServiceHonorsOverride(t *testing.T) {
	ports := discoveryPortsForService(config.ServiceConfig{Port: 3000}, 5432)
	if len(ports) != 1 || discoveryPortLabel(ports[0]) != "custom:5432" {
		t.Fatalf("unexpected override ports: %#v", ports)
	}
}

func TestRenderDiscoveryRowsAsUpstreams(t *testing.T) {
	rows := []discoveryRow{
		{
			Node:    "node-a",
			Service: "admin",
			Port:    discoveryPortSpec{Name: "web", Target: 3000, Protocol: "http"},
			Address: "10.210.0.1:24567",
		},
		{
			Node:    "node-b",
			Service: "admin",
			Port:    discoveryPortSpec{Name: "web", Target: 3000, Protocol: "https"},
			Address: "10.210.0.2:24568",
		},
		{
			Node:    "node-a",
			Service: "admin",
			Port:    discoveryPortSpec{Name: "web", Target: 3000, Protocol: "http"},
			Address: "10.210.0.1:24567",
		},
	}

	got, err := renderDiscoveryRows(rows, "upstreams")
	if err != nil {
		t.Fatalf("renderDiscoveryRows returned error: %v", err)
	}
	want := "http://10.210.0.1:24567 https://10.210.0.2:24568\n"
	if got != want {
		t.Fatalf("upstreams = %q, want %q", got, want)
	}
}

func TestRenderDiscoveryRowsRejectsUnknownFormat(t *testing.T) {
	_, err := renderDiscoveryRows(nil, "xml")
	if err == nil {
		t.Fatal("expected unsupported format to be rejected")
	}
}

type staticRequestExecutor struct {
	output string
}

func (executor staticRequestExecutor) ExecuteWithContext(context.Context, string) (string, error) {
	return executor.output, nil
}

func (executor staticRequestExecutor) ExecuteWithInput(context.Context, string, io.Reader) (string, error) {
	return executor.output, nil
}

func validDiscoveryEndpoint() takod.DiscoveryEndpoint {
	return takod.DiscoveryEndpoint{
		Service:     "web",
		Container:   "web-1",
		ContainerID: "container-1",
		Host:        "172.20.0.20",
		Port:        3000,
		Address:     "172.20.0.20:3000",
		Scope:       "local",
		Healthy:     true,
	}
}
