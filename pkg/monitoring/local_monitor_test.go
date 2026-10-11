package monitoring

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
)

type sentAlert struct {
	provider, endpoint string
	payload            map[string]any
}

func newTestLocalMonitor(t *testing.T, server *httptest.Server, monitoring *config.MonitoringConfig) (*Monitor, *[]sentAlert, *time.Time) {
	t.Helper()
	domain := strings.TrimPrefix(server.URL, "https://")
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "shop"},
		Notifications: &config.NotificationsConfig{
			Webhook: "https://alerts.example.invalid/all",
			Alerts:  &config.SchedulerAlertsConfig{Services: &config.ServiceAlertsConfig{AlertAfter: 2, RecoverAfter: 1}},
		},
		Environments: map[string]config.EnvironmentConfig{"production": {Services: map[string]config.ServiceConfig{
			"web": {Proxy: &config.ProxyConfig{Domain: domain}, HealthCheck: config.HealthCheckConfig{Path: "/health"}, Monitoring: monitoring},
		}}},
	}
	monitor := NewMonitor(cfg, nil, false)
	monitor.SetOutput(&bytes.Buffer{})
	monitor.httpClient = server.Client()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	monitor.now = func() time.Time { return now }
	var sent []sentAlert
	monitor.deliver = func(_ context.Context, provider, endpoint string, payload map[string]any) (bool, string) {
		sent = append(sent, sentAlert{provider, endpoint, payload})
		return true, ""
	}
	if err := monitor.load("production"); err != nil {
		t.Fatal(err)
	}
	return monitor, &sent, &now
}

func TestLocalMonitorDampingIntervalsAndPayload(t *testing.T) {
	status := http.StatusServiceUnavailable
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte("stack trace with secret"))
	}))
	defer server.Close()
	monitor, sent, now := newTestLocalMonitor(t, server, &config.MonitoringConfig{Enabled: true, CheckType: "http", Interval: "30s", Webhook: "http://10.0.0.5/hook"})
	ctx := context.Background()

	monitor.runDue(ctx, true)
	if len(*sent) != 0 {
		t.Fatalf("alerted before alertAfter")
	}
	*now = now.Add(10 * time.Second)
	monitor.runDue(ctx, true) // not due yet: per-service interval is honored
	if monitor.state["web"].failing != 1 {
		t.Fatalf("interval not honored: failing=%d", monitor.state["web"].failing)
	}
	*now = now.Add(25 * time.Second)
	monitor.runDue(ctx, true)
	if len(*sent) != 1 {
		t.Fatalf("want one opening, got %d", len(*sent))
	}
	alert := (*sent)[0]
	if alert.endpoint != "https://alerts.example.invalid/all" || alert.provider != "webhook" {
		t.Fatalf("private monitoring.webhook must never be used: %+v", alert)
	}
	payload := alert.payload
	if payload["schemaVersion"] != 1 || payload["node"] != ExternalNode || payload["environment"] != "production" || payload["transition"] != "opening" || payload["event"] != "service_down" {
		t.Fatalf("payload = %v", payload)
	}
	if details := payload["details"].(map[string]string); details["statusCode"] != "503" {
		t.Fatalf("details = %v", details)
	}
	for _, value := range payload {
		if s, ok := value.(string); ok && strings.Contains(s, "secret") {
			t.Fatalf("payload carries response text: %v", payload)
		}
	}
	status = http.StatusOK
	*now = now.Add(31 * time.Second)
	monitor.runDue(ctx, true)
	if len(*sent) != 2 || (*sent)[1].payload["transition"] != "recovery" || (*sent)[1].payload["episodeId"] != payload["episodeId"] {
		t.Fatalf("recovery = %+v", *sent)
	}
}

func TestLocalMonitorOnceAlertsEachFailure(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	monitor, sent, _ := newTestLocalMonitor(t, server, &config.MonitoringConfig{Enabled: true, CheckType: "http", Webhook: "https://service.example.invalid/web"})
	if err := monitor.CheckOnce(context.Background(), "production"); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 1 || (*sent)[0].provider != "service-webhook" || (*sent)[0].endpoint != "https://service.example.invalid/web" {
		t.Fatalf("--once deliveries = %+v", *sent)
	}
}
