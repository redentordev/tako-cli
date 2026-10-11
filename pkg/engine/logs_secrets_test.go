package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/secrets"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

const (
	sourcedSecretValue = "vault-sourced-value-913"
	aliasedSecretValue = "stripe-aliased-value-207"
	localSecretValue   = "local-file-value-555"
)

// useFakeSecretProvider stubs provider CLIs the same way
// pkg/secrets/provider_test.go does: the provider command re-executes this
// test binary, which prints a Doppler download (or fails).
func useFakeSecretProvider(t *testing.T, fail bool) {
	t.Helper()
	restore := secrets.SetProviderCommandForTesting(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=TestEngineProviderCommandHelper", "--", name}, args...)...)
	}, func(name string) (string, error) { return name, nil })
	t.Cleanup(restore)
	t.Setenv("GO_WANT_ENGINE_PROVIDER_HELPER", "1")
	if fail {
		t.Setenv("TAKO_ENGINE_PROVIDER_FAIL", "1")
	} else {
		t.Setenv("TAKO_ENGINE_PROVIDER_FAIL", "")
	}
}

func TestEngineProviderCommandHelper(t *testing.T) {
	if os.Getenv("GO_WANT_ENGINE_PROVIDER_HELPER") != "1" {
		return
	}
	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			args = args[i+1:]
			break
		}
	}
	if os.Getenv("TAKO_ENGINE_PROVIDER_FAIL") == "1" || len(args) < 3 || args[0] != "doppler" || args[1] != "secrets" || args[2] != "download" {
		_, _ = os.Stderr.WriteString("doppler: unauthorized")
		os.Exit(1)
	}
	_, _ = fmt.Fprintf(os.Stdout, `{"DATABASE_PASSWORD":%q,"STRIPE_KEY":%q}`, sourcedSecretValue, aliasedSecretValue)
	os.Exit(0)
}

// secretLogsTestConfig references one secretSources value, one aliased ref
// and one local .tako/secrets value from service web and job nightly.
func secretLogsTestConfig(nodes ...string) *config.Config {
	cfg := logsTestConfig(nodes...)
	cfg.SecretSources = []config.SecretSource{{Provider: "doppler"}}
	env := cfg.Environments["production"]
	web := env.Services["web"]
	web.Secrets = []string{"DATABASE_PASSWORD", "API_KEY:STRIPE_KEY", "LOCAL_TOKEN"}
	web.Deploy = config.DeployConfig{Strategy: config.DeployStrategyBlueGreen, Promotion: config.DeployPromotionManual}
	env.Services["web"] = web
	nightly := env.Services["nightly"]
	nightly.Secrets = []string{"NIGHTLY_KEY:STRIPE_KEY"}
	env.Services["nightly"] = nightly
	cfg.Environments["production"] = env
	return cfg
}

// prepareSecretsDir lays out .tako/ in the current (temporary) directory the
// way `tako secrets init` leaves it, writing files directly so the setup also
// works where atomic directory syncs are unsupported (Windows). Values are
// stored as plaintext placeholders, which the manager reads as-is.
func prepareSecretsDir(t *testing.T, values map[string]string) {
	t.Helper()
	if err := os.MkdirAll(".tako", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(".tako", ".gitignore"), []byte(secrets.GitignoreContent), 0o644); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	if err := os.WriteFile(filepath.Join(".tako", "encryption.key"), []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	var content strings.Builder
	for name, value := range values {
		fmt.Fprintf(&content, "%s=%s\n", name, value)
	}
	if err := os.WriteFile(filepath.Join(".tako", "secrets.production"), []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeLocalSecret(t *testing.T) {
	t.Helper()
	prepareSecretsDir(t, map[string]string{"LOCAL_TOKEN": localSecretValue})
}

func assertNoSecretValues(t *testing.T, label string, text string) {
	t.Helper()
	for _, value := range []string{sourcedSecretValue, aliasedSecretValue, localSecretValue} {
		if strings.Contains(text, value) {
			t.Fatalf("%s leaked secret %q: %q", label, value, text)
		}
	}
}

func TestStreamLogsRedactsSourcedAliasedAndLocalSecrets(t *testing.T) {
	useFakeSecretProvider(t, false)
	line := fmt.Sprintf("db=%s stripe=%s local=%s", sourcedSecretValue, aliasedSecretValue, localSecretValue)
	node := &fakeLogsNode{supports: true, frames: []takodclient.LogsStreamFrame{attachedFrame("web", "1"), lineFrame("web", "1", line)}}
	eng, sink := newLogsTestEngine(t, map[string]*fakeLogsNode{"node-a": node})
	writeLocalSecret(t)

	if _, err := eng.StreamLogs(context.Background(), LogsRequest{Config: secretLogsTestConfig("node-a"), Environment: "production", Service: "web"}); err != nil {
		t.Fatal(err)
	}
	lines := logLineEvents(sink)
	if len(lines) != 1 {
		t.Fatalf("lines = %#v", lines)
	}
	assertNoSecretValues(t, "data.data", fmt.Sprint(lines[0].Data["data"]))
	assertNoSecretValues(t, "message", lines[0].Message)
	if !strings.Contains(lines[0].Message, "db=va***13 stripe=st***07 local=lo***55") {
		t.Fatalf("message = %q", lines[0].Message)
	}
	if warnings := eventsOfType(sink, events.TypeWarning); len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}
}

func TestStreamLogsRegistersJobSecretsForRecordedRuns(t *testing.T) {
	useFakeSecretProvider(t, false)
	eng, sink := newLogsTestEngine(t, nil)
	writeLocalSecret(t)
	eng.logsHooks.jobRuns = func(context.Context, JobRunsRequest) (*JobRunsResult, error) {
		return &JobRunsResult{Runs: []JobRunInfo{{Job: "nightly", Server: "node-a", Status: "failed", Output: "charging " + aliasedSecretValue + "\n"}}}, nil
	}
	if _, err := eng.StreamLogs(context.Background(), LogsRequest{Config: secretLogsTestConfig("node-a"), Environment: "production", Service: "nightly"}); err != nil {
		t.Fatal(err)
	}
	lines := logLineEvents(sink)
	if len(lines) != 1 {
		t.Fatalf("lines = %#v", lines)
	}
	assertNoSecretValues(t, "job line", lines[0].Message+fmt.Sprint(lines[0].Data["data"]))
}

func TestStreamLogsWarnsOnceAndContinuesWhenSecretsCannotResolve(t *testing.T) {
	useFakeSecretProvider(t, true)
	node := &fakeLogsNode{supports: true, frames: []takodclient.LogsStreamFrame{attachedFrame("web", "1"), lineFrame("web", "1", "still streaming")}}
	eng, sink := newLogsTestEngine(t, map[string]*fakeLogsNode{"node-a": node})
	writeLocalSecret(t)
	result, err := eng.StreamLogs(context.Background(), LogsRequest{Config: secretLogsTestConfig("node-a"), Environment: "production", Service: "web"})
	if err != nil {
		t.Fatalf("logs must not fail when secrets cannot be resolved: %v", err)
	}
	warnings := eventsOfType(sink, events.TypeWarning)
	if len(warnings) != 1 || !strings.Contains(warnings[0].Message, "will not be redacted") || !strings.Contains(warnings[0].Message, "could not resolve secrets for production") {
		t.Fatalf("warnings = %#v", warnings)
	}
	if len(logLineEvents(sink)) != 1 || result.Status != logsStatusSuccess {
		t.Fatalf("streaming did not proceed: %#v", result)
	}
}

func TestBuildExecEnvFileContentRegistersAliasedSecret(t *testing.T) {
	t.Chdir(t.TempDir())
	prepareSecretsDir(t, map[string]string{"STRIPE_KEY": aliasedSecretValue})
	eng := New(Options{})
	service := config.ServiceConfig{Image: "web", Secrets: []string{"API_KEY:STRIPE_KEY"}}
	content, err := buildExecEnvFileContent(eng, &config.Config{Project: config.ProjectConfig{Name: "demo"}}, "production", &service)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "API_KEY=") {
		t.Fatalf("env file = %q", content)
	}
	if got := eng.RedactSensitive("env: API_KEY=" + aliasedSecretValue); strings.Contains(got, aliasedSecretValue) {
		t.Fatalf("aliased exec secret not registered: %q", got)
	}
}

func TestRegisterServiceSecretValuesResolvesAliasesAndReportsMissing(t *testing.T) {
	t.Chdir(t.TempDir())
	prepareSecretsDir(t, map[string]string{"STRIPE_KEY": aliasedSecretValue})
	eng := New(Options{})
	err := eng.registerServiceSecretValues(&config.Config{}, "production", map[string]config.ServiceConfig{
		"web": {Secrets: []string{"API_KEY:STRIPE_KEY", "MISSING_ONE"}},
	})
	if err == nil || !strings.Contains(err.Error(), "MISSING_ONE") {
		t.Fatalf("err = %v, want missing secret reported", err)
	}
	if got := eng.RedactSensitive(aliasedSecretValue); got == aliasedSecretValue {
		t.Fatal("resolvable aliased secret was not registered alongside the missing one")
	}
}

// Rollback, promote and scale embed recent container log lines in health
// failure errors. Each registers the same secret values before contacting
// any node; a cancelled context stops them right after registration.
func TestRollbackPromoteScaleRegisterSourcedAndAliasedSecrets(t *testing.T) {
	healthErr := fmt.Sprintf("container web failed health check; last log lines:\nconnect db=%s\ncharge with %s\nlocal %s", sourcedSecretValue, aliasedSecretValue, localSecretValue)
	operations := map[string]func(context.Context, *Engine, *config.Config) error{
		"rollback": func(ctx context.Context, eng *Engine, cfg *config.Config) error {
			_, err := eng.Rollback(ctx, RollbackRequest{
				Config: cfg, Environment: "production", Service: "web",
				HistorySource: func() (string, *remotestate.DeploymentHistory, error) { return "", nil, fmt.Errorf("no history") },
				ListDeployments: func(*remotestate.DeploymentHistory, *remotestate.HistoryOptions) []*remotestate.DeploymentState {
					return nil
				},
			})
			return err
		},
		"promote": func(ctx context.Context, eng *Engine, cfg *config.Config) error {
			_, err := eng.Promote(ctx, PromoteRequest{Config: cfg, Environment: "production", ServiceName: "web"})
			return err
		},
		"scale": func(ctx context.Context, eng *Engine, cfg *config.Config) error {
			_, err := eng.Scale(ctx, ScaleRequest{Config: cfg, Environment: "production", Targets: map[string]int{"web": 3}})
			return err
		},
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			useFakeSecretProvider(t, false)
			t.Chdir(t.TempDir())
			writeLocalSecret(t)
			sink := &events.BufferSink{}
			eng := New(Options{Sink: sink})
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := operation(ctx, eng, secretLogsTestConfig("node-a")); err == nil {
				t.Fatal("expected the cancelled operation to fail")
			}
			assertNoSecretValues(t, name+" result error", eng.RedactSensitive(healthErr))
			for _, event := range sink.Events() {
				if event.Type == events.TypeWarning && strings.Contains(event.Message, "will not be redacted") {
					t.Fatalf("unexpected unresolved-secrets warning: %q", event.Message)
				}
			}
		})
	}
}

func TestRollbackWarnsWhenSecretsCannotResolve(t *testing.T) {
	useFakeSecretProvider(t, true)
	t.Chdir(t.TempDir())
	writeLocalSecret(t)
	sink := &events.BufferSink{}
	eng := New(Options{Sink: sink})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = eng.Scale(ctx, ScaleRequest{Config: secretLogsTestConfig("node-a"), Environment: "production", Targets: map[string]int{"web": 2}})
	warnings := eventsOfType(sink, events.TypeWarning)
	if len(warnings) != 1 || !strings.Contains(warnings[0].Message, "will not be redacted") {
		t.Fatalf("warnings = %#v", warnings)
	}
}
