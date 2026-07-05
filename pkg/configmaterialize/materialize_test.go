package configmaterialize

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takoapi"
)

func TestBuildConfigDesiredOverActualPrecedence(t *testing.T) {
	desired := baseDesired()
	desired.Services["web"] = takoapi.DesiredServiceDocument{Image: "desired:image", Replicas: 2, Port: 8080}
	actual := baseActual()
	actual.Services["web"] = takoapi.ActualServiceDocument{Image: "actual:image", Replicas: 9}

	cfg, warnings, err := BuildConfig(Options{Desired: desired, Actual: actual, Servers: baseServers(t)})
	if err != nil {
		t.Fatalf("BuildConfig() error = %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %#v, want none", warnings)
	}

	service := cfg.Environments["production"].Services["web"]
	if service.Image != "desired:image" || service.Replicas != 2 || service.Port != 8080 {
		t.Fatalf("service = %#v, want desired state values", service)
	}
}

func TestBuildConfigEnvKeysAreRedacted(t *testing.T) {
	desired := baseDesired()
	desired.Services["web"] = takoapi.DesiredServiceDocument{Image: "nginx:latest", EnvKeys: []string{"DATABASE_URL", "API_KEY"}}

	cfg, _, err := BuildConfig(Options{Desired: desired, Servers: baseServers(t)})
	if err != nil {
		t.Fatalf("BuildConfig() error = %v", err)
	}

	env := cfg.Environments["production"].Services["web"].Env
	if got := env["DATABASE_URL"]; got != "" {
		t.Fatalf("DATABASE_URL = %q, want redacted empty string", got)
	}
	if got := env["API_KEY"]; got != "" {
		t.Fatalf("API_KEY = %q, want redacted empty string", got)
	}
	if len(env) != 2 {
		t.Fatalf("env = %#v, want two redacted keys", env)
	}
}

func TestBuildConfigDomainProxyReconstruction(t *testing.T) {
	desired := baseDesired()
	desired.Services["web"] = takoapi.DesiredServiceDocument{Image: "nginx:latest", Port: 80, Domains: []string{"www.example.com", "example.com"}}

	cfg, warnings, err := BuildConfig(Options{Desired: desired, Servers: baseServers(t)})
	if err != nil {
		t.Fatalf("BuildConfig() error = %v", err)
	}

	proxy := cfg.Environments["production"].Services["web"].Proxy
	if proxy == nil {
		t.Fatal("proxy is nil")
	}
	if proxy.Domain != "www.example.com" {
		t.Fatalf("proxy.Domain = %q, want first domain www.example.com", proxy.Domain)
	}
	if len(proxy.RedirectFrom) != 1 || proxy.RedirectFrom[0] != "example.com" {
		t.Fatalf("proxy.RedirectFrom = %#v, want example.com", proxy.RedirectFrom)
	}
	if !hasWarning(warnings, "extra_domains_as_redirects", "web") {
		t.Fatalf("warnings = %#v, want extra_domains_as_redirects", warnings)
	}
}

func TestBuildConfigPlacementAndHealthCheckRawJSON(t *testing.T) {
	placement := mustRaw(t, config.PlacementConfig{Strategy: "pinned", Servers: []string{"node1"}})
	healthCheck := mustRaw(t, config.HealthCheckConfig{Path: "/health", Interval: "10s", Timeout: "5s", Retries: 3})
	desired := baseDesired()
	desired.Services["web"] = takoapi.DesiredServiceDocument{
		Image:       "nginx:latest",
		Port:        80,
		Placement:   placement,
		HealthCheck: healthCheck,
	}

	cfg, _, err := BuildConfig(Options{Desired: desired, Servers: baseServers(t)})
	if err != nil {
		t.Fatalf("BuildConfig() error = %v", err)
	}

	service := cfg.Environments["production"].Services["web"]
	if service.Placement == nil || service.Placement.Strategy != "pinned" || len(service.Placement.Servers) != 1 || service.Placement.Servers[0] != "node1" {
		t.Fatalf("placement = %#v, want pinned node1", service.Placement)
	}
	if service.HealthCheck.Path != "/health" || service.HealthCheck.Interval != "10s" || service.HealthCheck.Timeout != "5s" || service.HealthCheck.Retries != 3 {
		t.Fatalf("healthCheck = %#v, want decoded raw JSON", service.HealthCheck)
	}
}

func TestBuildConfigActualOnlyFallbackWithWarning(t *testing.T) {
	desired := baseDesired()
	actual := baseActual()
	actual.Services["worker"] = takoapi.ActualServiceDocument{Image: "busybox:latest", Replicas: 3}

	cfg, warnings, err := BuildConfig(Options{Desired: desired, Actual: actual, Servers: baseServers(t)})
	if err != nil {
		t.Fatalf("BuildConfig() error = %v", err)
	}

	service := cfg.Environments["production"].Services["worker"]
	if service.Image != "busybox:latest" || service.Replicas != 3 {
		t.Fatalf("service = %#v, want actual image/replicas", service)
	}
	if !hasWarning(warnings, "actual_only_service", "worker") {
		t.Fatalf("warnings = %#v, want actual_only_service", warnings)
	}
}

func TestBuildConfigProjectVersionFromLatestDeploymentHistory(t *testing.T) {
	desired := baseDesired()
	desired.Services["web"] = takoapi.DesiredServiceDocument{Image: "nginx:latest"}
	history := &takoapi.DeploymentHistoryDocument{Deployments: []*takoapi.DeploymentStateDocument{
		{Version: "old", Timestamp: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)},
		{Version: "new", Timestamp: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)},
	}}

	cfg, _, err := BuildConfig(Options{Desired: desired, History: history, Servers: baseServers(t)})
	if err != nil {
		t.Fatalf("BuildConfig() error = %v", err)
	}
	if cfg.Project.Version != "new" {
		t.Fatalf("project version = %q, want new", cfg.Project.Version)
	}
}

func TestBuildConfigValidationSucceedsRepresentativeSingleServer(t *testing.T) {
	desired := baseDesired()
	desired.Services["web"] = takoapi.DesiredServiceDocument{
		Image:          "nginx:latest",
		Port:           80,
		Replicas:       1,
		Restart:        "unless-stopped",
		Domains:        []string{"example.com"},
		HealthCheck:    mustRaw(t, config.HealthCheckConfig{Path: "/health", Interval: "10s", Timeout: "5s", Retries: 3}),
		DeployStrategy: config.DeployStrategyRecreate,
	}

	cfg, warnings, err := BuildConfig(Options{Desired: desired, Servers: baseServers(t), Validate: true})
	if err != nil {
		t.Fatalf("BuildConfig() error = %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %#v, want none", warnings)
	}
	if cfg.Runtime == nil || cfg.Runtime.Mode != config.RuntimeModeTakod {
		t.Fatalf("runtime defaults were not applied: %#v", cfg.Runtime)
	}
	if cfg.Environments["production"].Servers[0] != "node1" {
		t.Fatalf("environment servers = %#v, want node1", cfg.Environments["production"].Servers)
	}
}

func baseDesired() *takoapi.DesiredStateDocument {
	return &takoapi.DesiredStateDocument{
		Project:     "demo",
		Environment: "production",
		TargetNodes: []string{"node1"},
		Services:    map[string]takoapi.DesiredServiceDocument{},
	}
}

func baseActual() *takoapi.ActualStateDocument {
	return &takoapi.ActualStateDocument{
		Project:     "demo",
		Environment: "production",
		TargetNodes: []string{"node1"},
		Services:    map[string]takoapi.ActualServiceDocument{},
	}
}

func baseServers(t *testing.T) map[string]config.ServerConfig {
	t.Helper()
	keyPath := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(keyPath, []byte("test key"), 0600); err != nil {
		t.Fatalf("write temp ssh key: %v", err)
	}
	return map[string]config.ServerConfig{
		"node1": {Host: "127.0.0.1", User: "deploy", SSHKey: keyPath},
	}
}

func mustRaw(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return data
}

func hasWarning(warnings []Warning, code string, service string) bool {
	for _, warning := range warnings {
		if warning.Code == code && warning.Service == service {
			return true
		}
	}
	return false
}
