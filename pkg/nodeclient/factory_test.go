package nodeclient

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

func TestFactoryUsesPeerAuthenticatedWorkerIngressForEnrolledLocalNode(t *testing.T) {
	if os.Geteuid() <= 0 {
		t.Skip("test requires a non-root peer UID")
	}
	dir, err := os.MkdirTemp("/tmp", "tako-factory-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "worker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	installation, err := nodeidentity.New("", "", "node-1", []string{nodeidentity.RoleWorker}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/status" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(takodclient.AgentStatus{Runtime: "takod", Capabilities: []string{nodeidentity.Capability}, Identity: &installation.Identity})
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	cfg := &config.Config{Servers: map[string]config.ServerConfig{"node-1": {
		Transport: "local", ClusterID: installation.ClusterID, NodeID: installation.NodeID, WorkerUID: os.Geteuid(),
	}}}
	factory, err := NewFactoryWithLocalSocket(cfg, nil, takodclient.DefaultSocket, socket)
	if err != nil {
		t.Fatal(err)
	}
	client, decision, err := factory.Client(context.Background(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if client == nil || decision.Transport != TransportLocal || decision.Evidence != EvidenceInstallationMatch {
		t.Fatalf("local resolution = %#v, client=%v", decision, client)
	}
}

func TestFactoryLegacySSHDoesNotProbeLocalSocket(t *testing.T) {
	cfg := &config.Config{Servers: map[string]config.ServerConfig{"legacy": {Host: "legacy.example.test", User: "deploy"}}}
	factory, err := NewFactory(cfg, nil, takodclient.DefaultSocket)
	if err != nil {
		t.Fatal(err)
	}
	probed := false
	factory.newLocal = func(string, uint32) (*takodclient.AgentClient, error) {
		probed = true
		return nil, nil
	}
	_, decision, err := factory.Client(context.Background(), "legacy")
	if err == nil || decision.Transport != TransportSSH || decision.Evidence != EvidenceLegacySSHDefault {
		t.Fatalf("legacy resolution = %#v, %v", decision, err)
	}
	if probed {
		t.Fatal("legacy SSH policy probed local runtime")
	}
}
