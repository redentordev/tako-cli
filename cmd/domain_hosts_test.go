package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takod"
)

func TestCollectInternalHostEntriesPrefersPrivateHostAndFallsBackToMesh(t *testing.T) {
	cfg := testInternalHostsConfig()
	stubDomainsHostsStatus(t, map[string]string{"node-b": "10.210.0.2/24"})

	entries, err := collectInternalHostEntries(cfg, "production", cfg.Environments["production"].Services, "", "auto")
	if err != nil {
		t.Fatalf("collectInternalHostEntries returned error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %#v, want two proxy-node entries", entries)
	}
	if entries[0].Host != "web.production.demo.tako.internal" {
		t.Fatalf("host = %q", entries[0].Host)
	}
	got := []string{entries[0].Address + "/" + entries[0].Source, entries[1].Address + "/" + entries[1].Source}
	if strings.Join(got, ",") != "10.0.1.10/privateHost,10.210.0.2/mesh" {
		t.Fatalf("addresses = %#v", got)
	}
}

func TestCollectInternalHostEntriesRequiresPrivateHostWhenRequested(t *testing.T) {
	cfg := testInternalHostsConfig()

	_, err := collectInternalHostEntries(cfg, "production", cfg.Environments["production"].Services, "", "private")
	if err == nil {
		t.Fatal("collectInternalHostEntries should require privateHost in private mode")
	}
	if !strings.Contains(err.Error(), "node-b has no privateHost") {
		t.Fatalf("error = %q, want missing privateHost", err)
	}
}

func testInternalHostsConfig() *config.Config {
	return &config.Config{
		Project: config.ProjectConfig{Name: "demo", Version: "1.0.0"},
		Mesh: &config.MeshConfig{
			NetworkCIDR: "10.210.0.0/16",
		},
		Servers: map[string]config.ServerConfig{
			"node-a": {Host: "203.0.113.10", PrivateHost: "10.0.1.10"},
			"node-b": {Host: "203.0.113.11"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Servers: []string{"node-a", "node-b"},
				Services: map[string]config.ServiceConfig{
					"api": {Port: 4000},
					"web": {
						Port: 3000,
						Proxy: &config.ProxyConfig{
							Host:       "web.production.demo.tako.internal",
							Visibility: config.ProxyVisibilityInternal,
						},
					},
				},
			},
		},
	}
}

// stubDomainsHostsStatus fakes GET /v1/status: servers in recorded report
// that node.mesh.address; any other server is unreachable.
func stubDomainsHostsStatus(t *testing.T, recorded map[string]string) *[]string {
	t.Helper()
	var reads []string
	previous := readDomainsHostsNodeStatus
	readDomainsHostsNodeStatus = func(_ *config.Config, serverName string) (takod.Status, error) {
		reads = append(reads, serverName)
		address, ok := recorded[serverName]
		if !ok {
			return takod.Status{}, errors.New("dial tcp: connection refused")
		}
		status := takod.Status{Runtime: "takod"}
		if address != "" {
			status.Node = map[string]any{"mesh": map[string]any{"address": address}}
		}
		return status, nil
	}
	t.Cleanup(func() { readDomainsHostsNodeStatus = previous })
	return &reads
}

// The mesh address comes from the node's record, not from the server's
// position in the environment: a node recorded at .7 is listed at .7.
func TestDomainsHostsMeshUsesRecordedAddress(t *testing.T) {
	cfg := testInternalHostsConfig()
	reads := stubDomainsHostsStatus(t, map[string]string{"node-a": "10.210.0.7/24", "node-b": "10.210.0.3/24"})

	entries, err := collectInternalHostEntries(cfg, "production", cfg.Environments["production"].Services, "", "mesh")
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, entry := range entries {
		got = append(got, entry.Server+"="+entry.Address)
	}
	if strings.Join(got, ",") != "node-b=10.210.0.3,node-a=10.210.0.7" {
		t.Fatalf("entries = %v", got)
	}
	if len(*reads) != 2 {
		t.Fatalf("status reads = %v", *reads)
	}
}

func TestDomainsHostsMeshErrorsWhenUnreachableOrUnrecorded(t *testing.T) {
	cfg := testInternalHostsConfig()
	stubDomainsHostsStatus(t, map[string]string{"node-a": "10.210.0.1/24"})
	_, err := collectInternalHostEntries(cfg, "production", cfg.Environments["production"].Services, "", "mesh")
	if err == nil || !strings.Contains(err.Error(), "could not read the recorded mesh address of server node-b") {
		t.Fatalf("unreachable err = %v", err)
	}

	stubDomainsHostsStatus(t, map[string]string{"node-a": "10.210.0.1/24", "node-b": ""})
	_, err = collectInternalHostEntries(cfg, "production", cfg.Environments["production"].Services, "", "mesh")
	if err == nil || !strings.Contains(err.Error(), "server node-b has no recorded mesh address") {
		t.Fatalf("unrecorded err = %v", err)
	}
}

// Private and SSH modes never contact nodes.
func TestDomainsHostsPrivateAndSSHModesSkipStatus(t *testing.T) {
	cfg := testInternalHostsConfig()
	reads := stubDomainsHostsStatus(t, nil)
	if _, err := collectInternalHostEntries(cfg, "production", cfg.Environments["production"].Services, "", "ssh"); err != nil {
		t.Fatal(err)
	}
	if len(*reads) != 0 {
		t.Fatalf("ssh mode read status from %v", *reads)
	}
}