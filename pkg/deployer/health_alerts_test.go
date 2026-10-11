package deployer

import (
	"encoding/json"
	"errors"
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

func healthTestDeployer(notifications *config.NotificationsConfig, services map[string]config.ServiceConfig) *Deployer {
	return &Deployer{
		config: &config.Config{
			Project:       config.ProjectConfig{Name: "shop"},
			Servers:       map[string]config.ServerConfig{"app-2": {}, "app-1": {}},
			Environments:  map[string]config.EnvironmentConfig{"production": {Servers: []string{"app-2", "app-1"}, Services: services}},
			Notifications: notifications,
		},
		environment: "production",
	}
}

func TestFullAlertRequestDefaultDerivedSections(t *testing.T) {
	services := map[string]config.ServiceConfig{
		"web": {
			Proxy:       &config.ProxyConfig{Domain: "Shop.Example.com"},
			HealthCheck: config.HealthCheckConfig{Path: "/healthz"},
			Monitoring:  &config.MonitoringConfig{Enabled: true, CheckType: "http", Interval: "30s"},
		},
		"worker":  {Monitoring: &config.MonitoringConfig{Enabled: true, CheckType: "container", Webhook: "http://10.0.0.5/hook"}},
		"nightly": {Kind: config.ServiceKindJob, Monitoring: &config.MonitoringConfig{Enabled: true}},
	}
	deploy := healthTestDeployer(&config.NotificationsConfig{
		Slack:  "https://hooks.slack.com/services/example",
		Alerts: &config.SchedulerAlertsConfig{Jobs: config.JobAlertsConfig{ConsecutiveFailures: 2}},
	}, services)
	request, plan, err := deploy.fullAlertRequest()
	if err != nil {
		t.Fatal(err)
	}
	if plan.Explicit {
		t.Fatalf("pre-existing keys must be default-derived")
	}
	if len(request.Monitors) != 2 {
		t.Fatalf("monitors = %+v", request.Monitors)
	}
	web := request.Monitors[0]
	if web.Service != "web" || web.CheckType != takod.MonitorCheckHTTP || web.Domain != "shop.example.com" || web.HealthPath != "/healthz" || web.CheckerNode != "app-1" || web.IntervalSeconds != 30 || web.TimeoutSeconds != 10 || web.AlertAfter != 3 || web.RecoverAfter != 2 {
		t.Fatalf("web monitor = %+v", web)
	}
	if request.Monitors[1].Service != "worker" || request.Monitors[1].CheckType != takod.MonitorCheckContainer {
		t.Fatalf("worker monitor = %+v", request.Monitors[1])
	}
	if request.ServiceTargets != nil {
		t.Fatalf("a private monitoring.webhook must never be sent: %v", request.ServiceTargets)
	}
	if len(plan.Warnings) == 0 {
		t.Fatalf("deprecated webhook not warned")
	}
	if request.Host == nil || request.Host.DiskPercent != 90 || request.Certificates == nil || request.Certificates.WarnBeforeSeconds != 14*86400 || request.Containers == nil || request.Containers.CrashLoopRestarts != 3 {
		t.Fatalf("default sections = %+v %+v %+v", request.Host, request.Containers, request.Certificates)
	}
	if !alertRequestEnabled(request) || !alertPolicyEnabled(request.Policy) {
		t.Fatalf("request should be enabled")
	}
}

func TestFullAlertRequestExplicitKeys(t *testing.T) {
	disk := 80
	services := map[string]config.ServiceConfig{
		"web": {Monitoring: &config.MonitoringConfig{Enabled: true, AlertAfter: 5, Webhook: "https://alerts.example.invalid/web"}},
	}
	deploy := healthTestDeployer(&config.NotificationsConfig{
		Webhook: "https://alerts.example.invalid/all",
		Alerts:  &config.SchedulerAlertsConfig{Host: &config.HostAlertsConfig{Disk: &disk, Memory: 85}},
	}, services)
	request, plan, err := deploy.fullAlertRequest()
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Explicit || request.Host.DiskPercent != 80 || request.Host.MemoryPercent != 85 || request.Monitors[0].AlertAfter != 5 {
		t.Fatalf("explicit plan = %+v request = %+v", plan, request)
	}
	if request.ServiceTargets["web"] != "https://alerts.example.invalid/web" {
		t.Fatalf("service target = %v", request.ServiceTargets)
	}
	if alertPolicyEnabled(request.Policy) || !alertRequestEnabled(request) {
		t.Fatalf("monitoring-only config must be pushed as enabled")
	}
	explicitService := healthTestDeployer(&config.NotificationsConfig{Webhook: "https://alerts.example.invalid/all"}, map[string]config.ServiceConfig{
		"web": {Monitoring: &config.MonitoringConfig{Enabled: true, Timeout: "5s"}},
	})
	if _, plan, err := explicitService.fullAlertRequest(); err != nil || !plan.Explicit {
		t.Fatalf("monitoring.timeout must be explicit: %v %+v", err, plan)
	}
}

func TestFullAlertRequestWithoutAlertsOrMonitoringSendsNothingNew(t *testing.T) {
	deploy := healthTestDeployer(&config.NotificationsConfig{Slack: "https://hooks.slack.com/services/example"}, map[string]config.ServiceConfig{"web": {}})
	request, plan, err := deploy.fullAlertRequest()
	if err != nil {
		t.Fatal(err)
	}
	if plan.enabled() || alertRequestEnabled(request) || request.Targets != (takod.AlertTargets{}) {
		t.Fatalf("config without alerts or monitoring produced %+v", request)
	}
}

func TestHealthAlertPreflightAndCapabilitySplit(t *testing.T) {
	if os.Geteuid() <= 0 {
		t.Skip("test requires a non-root peer UID")
	}
	dir, err := os.MkdirTemp("/tmp", "tako-health-policy-*")
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
	var supportsHealth atomic.Bool
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/status":
			capabilities := []string{nodeidentity.Capability, takod.CapabilitySchedulerAlertsV1}
			if supportsHealth.Load() {
				capabilities = append(capabilities, takod.CapabilityMonitoringHealthV1)
			}
			_ = json.NewEncoder(w).Encode(takodclient.AgentStatus{Runtime: "takod", Capabilities: capabilities, Identity: &installation.Identity})
		case "/v1/alerts":
			var request takod.AlertConfigRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			applied <- request
			_ = json.NewEncoder(w).Encode(takod.AlertStatusResponse{Project: request.Project, Environment: request.Environment})
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
		},
		Environments: map[string]config.EnvironmentConfig{"production": {Servers: []string{"node-a"}, Services: map[string]config.ServiceConfig{
			"web": {Monitoring: &config.MonitoringConfig{Enabled: true}},
		}}},
		Notifications: &config.NotificationsConfig{Webhook: "https://alerts.example.invalid/hook", Alerts: &config.SchedulerAlertsConfig{Jobs: config.JobAlertsConfig{ConsecutiveFailures: 2}}},
	}
	factory, err := nodeclient.NewFactoryWithLocalSocket(cfg, nil, takodclient.DefaultSocket, socket)
	if err != nil {
		t.Fatal(err)
	}
	deploy := &Deployer{config: cfg, environment: "production", runtimeFactory: factory, output: &discardWriter{}}

	// Default-derived sections never fail preflight; the old node gets the
	// job/backup request unchanged.
	if err := deploy.PreflightAlertPolicy(); err != nil {
		t.Fatalf("default-derived preflight failed: %v", err)
	}
	if err := deploy.ApplyAlertPolicy(); err != nil {
		t.Fatal(err)
	}
	if request := <-applied; len(request.Monitors) != 0 || request.Host != nil || request.Policy.JobConsecutiveFailures != 2 {
		t.Fatalf("old node received monitoring sections: %+v", request)
	}
	// Explicit keys fail preflight with a capability error (exit 2).
	cfg.Notifications.Alerts.Host = &config.HostAlertsConfig{}
	var capabilityErr *takodclient.CapabilityRequiredError
	if err := deploy.PreflightAlertPolicy(); !errors.As(err, &capabilityErr) {
		t.Fatalf("explicit preflight error = %v", err)
	}
	supportsHealth.Store(true)
	if err := deploy.PreflightAlertPolicy(); err != nil {
		t.Fatal(err)
	}
	if err := deploy.ApplyAlertPolicy(); err != nil {
		t.Fatal(err)
	}
	if request := <-applied; len(request.Monitors) != 1 || request.Host == nil {
		t.Fatalf("capable node did not receive monitoring sections: %+v", request)
	}
}

type discardWriter struct{}

func (*discardWriter) Write(p []byte) (int, error) { return len(p), nil }
