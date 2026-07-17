package monitoring

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
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

func TestInternalMonitorUsesAuthenticatedLocalRuntimeWithoutSSH(t *testing.T) {
	if os.Geteuid() <= 0 {
		t.Skip("test requires a non-root peer UID")
	}
	dir, err := os.MkdirTemp("/tmp", "tako-monitor-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "worker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	installation, err := nodeidentity.New("", "", "node-a", []string{nodeidentity.RoleWorker}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/status":
			_ = json.NewEncoder(w).Encode(takodclient.AgentStatus{Runtime: "takod", Capabilities: []string{nodeidentity.Capability}, Identity: &installation.Identity})
		case "/v1/actual":
			_ = json.NewEncoder(w).Encode(takod.ActualStateResponse{Project: "demo", Environment: "production", Services: map[string]*takod.ActualService{"web": {Name: "web", Replicas: 1}}})
		default:
			http.NotFound(w, r)
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	cfg := &config.Config{
		Project:      config.ProjectConfig{Name: "demo"},
		Servers:      map[string]config.ServerConfig{"node-a": {Transport: "local", ClusterID: installation.ClusterID, NodeID: installation.NodeID, WorkerUID: os.Geteuid()}},
		Environments: map[string]config.EnvironmentConfig{"production": {Servers: []string{"node-a"}}},
	}
	monitor := NewMonitor(cfg, nil, false)
	factory, err := nodeclient.NewFactoryWithLocalSocket(cfg, nil, takodclient.DefaultSocket, socket)
	if err != nil {
		t.Fatal(err)
	}
	monitor.runtimeFactory = factory
	monitor.envName = "production"
	defer monitor.Close()
	if err := monitor.checkInternalService("web", &config.ServiceConfig{Replicas: 1}); err != nil {
		t.Fatal(err)
	}
}
