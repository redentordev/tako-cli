package deployer

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

func TestBuildAlertConfigRequest(t *testing.T) {
	deploy := &Deployer{
		config: &config.Config{
			Project: config.ProjectConfig{Name: "demo"},
			Notifications: &config.NotificationsConfig{
				Webhook: "https://alerts.example/token",
				Alerts: &config.SchedulerAlertsConfig{
					Jobs:    config.JobAlertsConfig{ConsecutiveFailures: 3, ConsecutiveTimeouts: 2, ConsecutiveSkipped: 4, NoSuccessFor: "90m"},
					Backups: config.BackupAlertsConfig{MaxAge: "25h"},
				},
			},
		},
		environment: "production",
	}
	request, err := deploy.buildAlertConfigRequest()
	if err != nil {
		t.Fatal(err)
	}
	if request.Project != "demo" || request.Environment != "production" || request.Policy.JobConsecutiveFailures != 3 || request.Policy.JobConsecutiveTimeouts != 2 || request.Policy.JobConsecutiveSkipped != 4 || request.Policy.JobNoSuccessSeconds != 5400 || request.Policy.BackupMaxAgeSeconds != 90000 || request.Targets.Webhook != "https://alerts.example/token" {
		t.Fatalf("request = %#v", request)
	}
}

func TestBuildAlertConfigRequestAbsentPolicyDisablesAlerts(t *testing.T) {
	deploy := &Deployer{config: &config.Config{Project: config.ProjectConfig{Name: "demo"}}, environment: "production"}
	request, err := deploy.buildAlertConfigRequest()
	if err != nil {
		t.Fatal(err)
	}
	if request.Project != "demo" || request.Environment != "production" || request.Policy.JobConsecutiveFailures != 0 || request.Policy.JobConsecutiveTimeouts != 0 || request.Policy.JobConsecutiveSkipped != 0 || request.Policy.JobNoSuccessSeconds != 0 || request.Policy.BackupMaxAgeSeconds != 0 {
		t.Fatalf("request = %#v", request)
	}
}

func TestAlertPolicyPreflightPrecedesApplyOnLocalRuntime(t *testing.T) {
	if os.Geteuid() <= 0 {
		t.Skip("test requires a non-root peer UID")
	}
	dir, err := os.MkdirTemp("/tmp", "tako-alert-policy-*")
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
	applied := make(chan takod.AlertConfigRequest, 8)
	var supportsAlerts atomic.Bool
	var failEnabled atomic.Bool
	supportsAlerts.Store(true)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/status":
			capabilities := []string{nodeidentity.Capability}
			if supportsAlerts.Load() {
				capabilities = append(capabilities, takod.CapabilitySchedulerAlertsV1)
			}
			_ = json.NewEncoder(w).Encode(takodclient.AgentStatus{Runtime: "takod", Capabilities: capabilities, Identity: &installation.Identity})
		case "/v1/alerts":
			var request takod.AlertConfigRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if failEnabled.Load() && alertRequestEnabled(request) {
				http.Error(w, "owner unavailable", http.StatusServiceUnavailable)
				return
			}
			applied <- request
			_ = json.NewEncoder(w).Encode(takod.AlertStatusResponse{Project: request.Project, Environment: request.Environment, Configured: true})
		default:
			http.NotFound(w, r)
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "demo"},
		Servers: map[string]config.ServerConfig{
			"node-a": {Transport: "local", ClusterID: installation.ClusterID, NodeID: installation.NodeID, WorkerUID: os.Geteuid()},
			"node-b": {Transport: "local", ClusterID: installation.ClusterID, NodeID: installation.NodeID, WorkerUID: os.Geteuid(), Lifecycle: nodeidentity.NodeLifecycleCordoned},
		},
		Environments:  map[string]config.EnvironmentConfig{"production": {Servers: []string{"node-a", "node-b"}}},
		Notifications: &config.NotificationsConfig{Webhook: "https://alerts.example/token", Alerts: &config.SchedulerAlertsConfig{Jobs: config.JobAlertsConfig{ConsecutiveFailures: 2}}},
	}
	factory, err := nodeclient.NewFactoryWithLocalSocket(cfg, nil, takodclient.DefaultSocket, socket)
	if err != nil {
		t.Fatal(err)
	}
	deploy := &Deployer{config: cfg, environment: "production", runtimeFactory: factory}
	if err := deploy.PreflightAlertPolicy(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-applied:
		t.Fatal("preflight mutated alert policy")
	default:
	}
	if err := deploy.ApplyAlertPolicy(); err != nil {
		t.Fatal(err)
	}
	request := <-applied
	staleDisable := <-applied
	if !alertRequestEnabled(request) {
		request, staleDisable = staleDisable, request
	}
	if request.Project != "demo" || request.Environment != "production" || request.Policy.JobConsecutiveFailures != 2 || request.Targets.Webhook != "https://alerts.example/token" {
		t.Fatalf("applied request = %#v", request)
	}
	if alertRequestEnabled(staleDisable) || staleDisable.Targets != (takod.AlertTargets{}) {
		t.Fatalf("nonschedulable node was not disabled: %#v", staleDisable)
	}
	failEnabled.Store(true)
	if err := deploy.ApplyAlertPolicy(); err == nil {
		t.Fatal("enabled owner failure was not reported")
	}
	select {
	case disableAfterFailure := <-applied:
		t.Fatalf("stale node was disabled before owner handoff succeeded: %#v", disableAfterFailure)
	default:
	}
	failEnabled.Store(false)
	cfg.Notifications = nil
	if err := deploy.ApplyAlertPolicy(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		disabled := <-applied
		if disabled.Project != "demo" || disabled.Environment != "production" || disabled.Policy.JobConsecutiveFailures != 0 || disabled.Targets.Webhook != "" {
			t.Fatalf("disable request = %#v", disabled)
		}
	}
	supportsAlerts.Store(false)
	if err := deploy.PreflightAlertPolicy(); err != nil {
		t.Fatalf("disabled policy preflight rejected old agent: %v", err)
	}
	if err := deploy.ApplyAlertPolicy(); err != nil {
		t.Fatalf("disabled policy apply rejected old agent: %v", err)
	}
	select {
	case request := <-applied:
		t.Fatalf("disable request sent to incapable agent: %#v", request)
	default:
	}
}

func TestAlertTargetServersIgnoreNarrowWorkloadTargets(t *testing.T) {
	cfg := &config.Config{
		Servers: map[string]config.ServerConfig{
			"node-a": {}, "node-b": {}, "node-c": {Lifecycle: nodeidentity.NodeLifecycleCordoned},
		},
		Environments: map[string]config.EnvironmentConfig{"production": {Servers: []string{"node-a", "node-b", "node-c"}}},
	}
	deploy := &Deployer{config: cfg, environment: "production", targetServers: []string{"node-a"}}
	targets, err := deploy.alertTargetServers(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 || targets[0] != "node-a" || targets[1] != "node-b" {
		t.Fatalf("targets = %#v", targets)
	}
	targets, err = deploy.alertTargetServers(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 3 || targets[0] != "node-a" || targets[1] != "node-b" || targets[2] != "node-c" {
		t.Fatalf("disable targets = %#v", targets)
	}
}
