package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/spf13/cobra"
)

func TestDiscoveryTargetServersAllEnvironmentsUsesAllConfiguredServers(t *testing.T) {
	cfg := &config.Config{
		Servers: map[string]config.ServerConfig{
			"node-b": {Host: "10.0.0.2"},
			"node-a": {Host: "10.0.0.1"},
		},
	}

	got, err := discoveryTargetServers(cfg, "", "", true)
	if err != nil {
		t.Fatalf("discoveryTargetServers returned error: %v", err)
	}
	want := []string{"node-a", "node-b"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("servers = %#v, want %#v", got, want)
	}
}

func TestCollectDiscoveryExportsWithSeparatesConnectAndReadErrors(t *testing.T) {
	servers := map[string]config.ServerConfig{
		"node-a": {Host: "10.0.0.1"},
		"node-b": {Host: "10.0.0.2"},
		"node-c": {Host: "10.0.0.3"},
	}

	results := collectDiscoveryExportsWith(servers, []string{"node-a", "node-b", "node-c"}, "production", func(serverName string, server config.ServerConfig, environment string) (*takod.ExportDiscoveryResponse, error) {
		switch serverName {
		case "node-a":
			return &takod.ExportDiscoveryResponse{Exports: []takod.ExportDiscoveryRecord{{
				Network:     "tako_backend_api_production_api_export",
				Project:     "backend-api",
				Environment: environment,
				Service:     "api",
				Alias:       "backend-api-production-api",
				Runtime:     "takod",
			}}}, nil
		case "node-b":
			return nil, errors.New("connect: refused")
		default:
			return nil, errors.New("discovery: bad json")
		}
	})

	if len(results) != 3 {
		t.Fatalf("results = %#v, want three", results)
	}
	if len(results[0].Exports) != 1 || results[0].ConnectErr != nil || results[0].ReadErr != nil {
		t.Fatalf("node-a result = %#v", results[0])
	}
	if results[1].ConnectErr == nil || results[1].ReadErr != nil {
		t.Fatalf("node-b result = %#v, want connect error", results[1])
	}
	if results[2].ReadErr == nil || results[2].ConnectErr != nil {
		t.Fatalf("node-c result = %#v, want read error", results[2])
	}
}

func TestPrintDiscoveryExportsTextIncludesRecordsAndWarnings(t *testing.T) {
	results := []discoveryNodeResult{
		{
			ServerName: "node-a",
			Exports: []takod.ExportDiscoveryRecord{{
				Network:     "tako_backend_api_production_api_export",
				Project:     "backend-api",
				Environment: "production",
				Service:     "api",
				Alias:       "backend-api-production-api",
			}},
		},
		{ServerName: "node-b", ConnectErr: errors.New("refused")},
	}
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)

	if err := printDiscoveryExportsText(cmd, "production", false, results); err != nil {
		t.Fatalf("printDiscoveryExportsText returned error: %v", err)
	}
	output := buf.String()
	for _, want := range []string{
		"Export discovery records",
		"Environment: production",
		"backend-api-production-api",
		"tako_backend_api_production_api_export",
		"Warnings:",
		"node-b: connect failed - refused",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
}

func TestPrintDiscoveryExportsJSONShape(t *testing.T) {
	results := []discoveryNodeResult{{
		ServerName: "node-a",
		Exports: []takod.ExportDiscoveryRecord{{
			Network:     "tako_backend_api_production_api_export",
			Project:     "backend-api",
			Environment: "production",
			Service:     "api",
			Alias:       "backend-api-production-api",
		}},
	}}
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)

	if err := printDiscoveryExportsJSON(cmd, "production", false, results); err != nil {
		t.Fatalf("printDiscoveryExportsJSON returned error: %v", err)
	}
	var decoded discoveryExportsJSONOutput
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid json output: %v\n%s", err, buf.String())
	}
	if decoded.Environment != "production" || len(decoded.Nodes) != 1 || decoded.Nodes[0].Node != "node-a" || len(decoded.Nodes[0].Exports) != 1 {
		t.Fatalf("decoded output = %#v", decoded)
	}
}

func TestPrintDiscoveryExportsJSONFailsWhenNoNodeReadable(t *testing.T) {
	results := []discoveryNodeResult{{
		ServerName: "node-a",
		ConnectErr: errors.New("refused"),
	}}
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)

	err := printDiscoveryExportsJSON(cmd, "production", false, results)
	if err == nil {
		t.Fatal("expected no reachable nodes error")
	}
	if !strings.Contains(err.Error(), "no reachable nodes") {
		t.Fatalf("error = %q", err)
	}
	var decoded discoveryExportsJSONOutput
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid json output: %v\n%s", err, buf.String())
	}
	if len(decoded.Nodes) != 1 || decoded.Nodes[0].Error == "" {
		t.Fatalf("decoded output = %#v, want node error", decoded)
	}
}
