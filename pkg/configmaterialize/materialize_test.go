package configmaterialize

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takoapi"
)

func TestDesiredServiceToConfigPreservesCommandEntrypointAndLabels(t *testing.T) {
	service, warnings, err := desiredServiceToConfig("consumer", takoapi.DesiredServiceDocument{
		Image:          "getsentry/sentry:26.6.0",
		CommandArgs:    []string{"sentry", "run", "consumer", "errors"},
		EntrypointArgs: []string{"/etc/sentry/entrypoint.sh", "run"},
		Labels:         map[string]string{"com.example.role": "consumer"},
	})
	if err != nil {
		t.Fatalf("desiredServiceToConfig returned error: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %#v", warnings)
	}
	if !service.Command.IsList() || strings.Join(service.Command.Arguments(), "|") != "sentry|run|consumer|errors" {
		t.Fatalf("command = %#v", service.Command.Arguments())
	}
	if !service.Entrypoint.IsList() || strings.Join(service.Entrypoint.Arguments(), "|") != "/etc/sentry/entrypoint.sh|run" {
		t.Fatalf("entrypoint = %#v", service.Entrypoint.Arguments())
	}
	if service.Labels["com.example.role"] != "consumer" {
		t.Fatalf("labels = %#v", service.Labels)
	}
}

func TestDesiredServiceToConfigPreservesRuntimeControlsAndRedactsBuildArgs(t *testing.T) {
	service, warnings, err := desiredServiceToConfig("web", takoapi.DesiredServiceDocument{
		Image:           "demo/web:latest",
		Build:           ".",
		BuildArgKeys:    []string{"BASE_IMAGE", "TOKEN"},
		BuildTarget:     "runtime",
		User:            "1000:1000",
		WorkingDir:      "/work",
		StopGracePeriod: "60s",
		Init:            true,
		ExtraHosts:      []string{"database:10.0.0.2"},
		Ulimits:         map[string]takoapi.UlimitDocument{"nofile": {Soft: 262144, Hard: 262144}},
		ShmSize:         "256m",
	})
	if err != nil {
		t.Fatalf("desiredServiceToConfig: %v", err)
	}
	if service.BuildTarget != "runtime" || service.User != "1000:1000" || service.WorkingDir != "/work" || !service.Init || service.ShmSize != "256m" {
		t.Fatalf("materialized runtime controls = %#v", service)
	}
	if service.Ulimits["nofile"].Hard != 262144 || len(service.BuildArgs) != 0 {
		t.Fatalf("materialized ulimits/build args = %#v / %#v", service.Ulimits, service.BuildArgs)
	}
	if len(warnings) != 1 || warnings[0].Code != "build_arg_values_redacted" {
		t.Fatalf("warnings = %#v", warnings)
	}
}

func TestDesiredServiceToConfigRestoresRunImageFromWithoutResolvedImageConflict(t *testing.T) {
	service, _, err := desiredServiceToConfig("migrate", takoapi.DesiredServiceDocument{
		Kind: takoapi.KindDesiredServiceDocument, WorkloadKind: config.ServiceKindRun, Image: "demo/app:rev", ImageFrom: "app",
		CommandArgs: []string{"bin/app", "migrate"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !service.IsRun() || service.Image != "" || service.ImageFrom != "app" || !service.Command.IsList() {
		t.Fatalf("materialized run = %#v", service)
	}
}

func TestDesiredServiceToConfigDoesNotTreatDocumentKindAsWorkloadKind(t *testing.T) {
	service, _, err := desiredServiceToConfig("web", takoapi.DesiredServiceDocument{
		Kind: takoapi.KindDesiredServiceDocument, Type: "public", Image: "nginx:alpine", Replicas: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if service.Kind != "" || service.IsRun() || service.IsJob() {
		t.Fatalf("materialized document identity as workload kind: %#v", service)
	}
}

func TestDesiredServiceToConfigReadsLegacyScalarCommand(t *testing.T) {
	service, _, err := desiredServiceToConfig("worker", takoapi.DesiredServiceDocument{
		Image: "busybox:latest", Command: "echo legacy",
	})
	if err != nil {
		t.Fatalf("desiredServiceToConfig returned error: %v", err)
	}
	command, ok := service.Command.Scalar()
	if !ok || command != "echo legacy" {
		t.Fatalf("legacy command = %q scalar=%v", command, ok)
	}
}

func TestBuildConfigDesiredOverActualPrecedence(t *testing.T) {
	desired := baseDesired()
	desired.Services["web"] = takoapi.DesiredServiceDocument{Image: "desired:image", Replicas: 2, Port: 8080}
	actual := baseActual()
	actual.Services["web"] = takoapi.ActualServiceDocument{Image: "actual:image", Replicas: 9}

	cfg, warnings, err := BuildConfig(Options{Desired: desired, Actual: actual, Servers: baseServers(t)})
	if err != nil {
		t.Fatalf("BuildConfig() error = %v", err)
	}
	if !hasWarning(warnings, "default_project_version", "") {
		t.Fatalf("warnings = %#v, want default_project_version", warnings)
	}

	service := cfg.Environments["production"].Services["web"]
	if service.Image != "desired:image" || service.Replicas != 2 || service.Port != 8080 {
		t.Fatalf("service = %#v, want desired state values", service)
	}
}

func TestBuildConfigEnvKeysAreRedacted(t *testing.T) {
	desired := baseDesired()
	desired.Services["web"] = takoapi.DesiredServiceDocument{Image: "nginx:latest", EnvKeys: []string{"DATABASE_URL", "API_KEY"}}

	cfg, warnings, err := BuildConfig(Options{Desired: desired, Servers: baseServers(t)})
	if err != nil {
		t.Fatalf("BuildConfig() error = %v", err)
	}
	if !hasWarning(warnings, "env_values_redacted", "web") {
		t.Fatalf("warnings = %#v, want env_values_redacted", warnings)
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

func TestBuildConfigActualOnlyPersistentPreservedWithWarning(t *testing.T) {
	desired := baseDesired()
	actual := baseActual()
	actual.Services["db"] = takoapi.ActualServiceDocument{Image: "postgres:16", Replicas: 1, Persistent: true, DeployStrategy: config.DeployStrategyBlueGreen}

	cfg, warnings, err := BuildConfig(Options{Desired: desired, Actual: actual, Servers: baseServers(t)})
	if err != nil {
		t.Fatalf("BuildConfig() error = %v", err)
	}

	service := cfg.Environments["production"].Services["db"]
	if !service.Persistent || service.Deploy.Strategy != config.DeployStrategyBlueGreen {
		t.Fatalf("service = %#v, want persistent blue-green", service)
	}
	if !hasWarning(warnings, "actual_only_persistent_service", "db") {
		t.Fatalf("warnings = %#v, want actual_only_persistent_service", warnings)
	}
}

func TestBuildConfigActualOnlyPersistentValidationErrorReturnsWarnings(t *testing.T) {
	actual := baseActual()
	actual.Services["db"] = takoapi.ActualServiceDocument{Image: "postgres:16", Replicas: 1, Persistent: true}

	cfg, warnings, err := BuildConfig(Options{Actual: actual, Servers: baseServers(t), Validate: true})
	if err == nil {
		t.Fatal("BuildConfig() error = nil, want persistent volume validation error")
	}
	if !strings.Contains(err.Error(), "persistent services must declare at least one volume") {
		t.Fatalf("error = %q, want persistent volume requirement", err)
	}
	if !hasWarning(warnings, "actual_only_persistent_service", "db") {
		t.Fatalf("warnings = %#v, want actual_only_persistent_service", warnings)
	}
	if cfg != nil {
		t.Fatalf("cfg = %#v, want nil on validation failure", cfg)
	}
}

func TestBuildConfigDesiredEnvFileWarning(t *testing.T) {
	desired := baseDesired()
	desired.Services["web"] = takoapi.DesiredServiceDocument{Image: "nginx:latest", EnvFile: true}

	_, warnings, err := BuildConfig(Options{Desired: desired, Servers: baseServers(t)})
	if err != nil {
		t.Fatalf("BuildConfig() error = %v", err)
	}
	if !hasWarning(warnings, "env_file_not_recovered", "web") {
		t.Fatalf("warnings = %#v, want env_file_not_recovered", warnings)
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
	if !hasWarning(warnings, "default_project_version", "") {
		t.Fatalf("warnings = %#v, want default_project_version", warnings)
	}
	if cfg.Runtime == nil || cfg.Runtime.Mode != config.RuntimeModeTakod {
		t.Fatalf("runtime defaults were not applied: %#v", cfg.Runtime)
	}
	if cfg.Environments["production"].Servers[0] != "node1" {
		t.Fatalf("environment servers = %#v, want node1", cfg.Environments["production"].Servers)
	}
}

func TestBuildConfigValidationRoundTripsRunWithoutContainerDefaults(t *testing.T) {
	desired := baseDesired()
	desired.Services["migrate"] = takoapi.DesiredServiceDocument{
		Kind: takoapi.KindDesiredServiceDocument, WorkloadKind: config.ServiceKindRun, Type: config.ServiceKindRun,
		Image: "busybox:1.36", CommandArgs: []string{"true"}, Replicas: 1,
		// Compatibility with desired documents written before run defaults were suppressed.
		Restart: "unless-stopped", DeployStrategy: config.DeployStrategyRecreate,
	}
	cfg, _, err := BuildConfig(Options{Desired: desired, Servers: baseServers(t), Validate: true})
	if err != nil {
		t.Fatalf("BuildConfig() run roundtrip error = %v", err)
	}
	run := cfg.Environments["production"].Services["migrate"]
	if !run.IsRun() || run.Restart != "" || run.Deploy.Strategy != "" {
		t.Fatalf("round-tripped run defaults = %#v", run)
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
