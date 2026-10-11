package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/internal/providercli"
	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/registryauth"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
)

const (
	engineTestECRHost = "123456789012.dkr.ecr.us-west-1.amazonaws.com"
	engineFakeToken   = "fake-token-0123456789abcdef"
)

// TestEngineRegistryCLIHelper is the re-exec fake for provider CLIs.
func TestEngineRegistryCLIHelper(t *testing.T) {
	if os.Getenv("GO_WANT_ENGINE_REGISTRY_HELPER") != "1" {
		return
	}
	if logPath := os.Getenv("ENGINE_REGISTRY_LOG"); logPath != "" {
		if file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600); err == nil {
			_, _ = file.WriteString(strings.Join(os.Args[1:], " ") + "\n")
			_ = file.Close()
		}
	}
	fmt.Fprint(os.Stdout, os.Getenv("ENGINE_REGISTRY_OUT"))
	if os.Getenv("ENGINE_REGISTRY_FAIL") == "1" {
		os.Exit(1)
	}
	os.Exit(0)
}

func useEngineFakeRegistryCLI(t *testing.T, output string) string {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "cli.log")
	t.Setenv("GO_WANT_ENGINE_REGISTRY_HELPER", "1")
	t.Setenv("ENGINE_REGISTRY_LOG", logPath)
	t.Setenv("ENGINE_REGISTRY_OUT", output)
	t.Setenv("ENGINE_REGISTRY_FAIL", "0")
	restore := providercli.SetForTesting(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=TestEngineRegistryCLIHelper", "--", name}, args...)...)
	}, func(name string) (string, error) { return name, nil })
	t.Cleanup(restore)
	return logPath
}

func cliInvocations(t *testing.T, logPath string) int {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(strings.Split(strings.TrimSpace(string(data)), "\n"))
}

func providerRegistryConfig() *config.Config {
	return &config.Config{
		Project: config.ProjectConfig{Name: "demo"},
		Registries: map[string]config.RegistryConfig{
			engineTestECRHost: {Provider: "ecr"},
		},
	}
}

// TestRegistryPreflightPrecedesFirstMutation pins, for every operation that
// can attach registry credentials, that the provider mint preflight runs
// before runtime setup and before placement intent is persisted.
func TestRegistryPreflightPrecedesFirstMutation(t *testing.T) {
	firstMutations := map[string][]string{
		"deploy.go":           {"AcquireRemoteOperationLeasesContext(ctx, session.sshPool", "preflightAndSetupTakodRuntime(", "PersistTakodDesiredIntentWithPlacementBaseline("},
		"rollback.go":         {"preflightAndSetupTakodRuntime(", "PersistTakodDesiredIntentWithPlacementBaseline("},
		"rollback_project.go": {"s.deployer.SetupTakodRuntime()", "PersistTakodDesiredIntentWithPlacementBaseline("},
		"scale.go":            {"preflightAndSetupTakodRuntime(", "PersistTakodDesiredIntentWithPlacementBaseline("},
		"promote.go":          {"preflightAndSetupTakodRuntime(", "PersistTakodDesiredIntentWithPlacementBaseline("},
		"run.go":              {"preflightAndSetupTakodRuntime(", "PersistTakodDesiredIntentWithPlacementBaseline("},
		"placement_apply.go":  {"deploy.PreflightTakodProxyCapabilities(", "MovePreparedServiceTakod("},
	}
	for file, mutations := range firstMutations {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		body := string(source)
		if !strings.Contains(body, "e.attachRegistryMinter(deploy)") {
			t.Fatalf("%s does not attach a per-operation registry minter", file)
		}
		preflight := strings.Index(body, "preflightRegistryAuth(ctx,")
		if preflight < 0 {
			t.Fatalf("%s has no registry mint preflight", file)
		}
		for _, mutation := range mutations {
			index := strings.Index(body, mutation)
			if index < 0 {
				t.Fatalf("%s: marker %q not found", file, mutation)
			}
			if preflight > index {
				t.Fatalf("%s: registry preflight runs after %q", file, mutation)
			}
		}
	}
}

func TestRegistryMintFailureIsOperationFailure(t *testing.T) {
	err := fmt.Errorf("deploy: %w", &registryauth.MintError{Host: "gcr.io", Provider: "gcr", Err: errors.New(`gcr provider requires the "gcloud" CLI on PATH`)})
	if Classify(err) != ClassFailed {
		t.Fatalf("Classify = %v, want ClassFailed (exit 1)", Classify(err))
	}
}
func TestOperationWithoutProviderCredentialRunsNoCLI(t *testing.T) {
	logPath := useEngineFakeRegistryCLI(t, engineFakeToken)
	eng := New(Options{})
	cfg := providerRegistryConfig()
	deploy := deployer.NewDeployer(nil, cfg, "production", false)
	eng.attachRegistryMinter(deploy)

	services := map[string]config.ServiceConfig{"web": {Image: "nginx:1.27"}, "api": {Build: "."}}
	deploy.SetSkipBuild(true)
	if err := preflightRegistryAuth(context.Background(), deploy, deployRegistryAuthNeeds(deploy, services, services, "")); err != nil {
		t.Fatal(err)
	}
	// Promote and placement apply attach nothing.
	if err := preflightRegistryAuth(context.Background(), deploy, deployer.RegistryAuthNeeds{}); err != nil {
		t.Fatal(err)
	}
	if n := cliInvocations(t, logPath); n != 0 {
		t.Fatalf("provider CLI ran %d time(s) for an operation attaching no provider credential", n)
	}
}

func TestMintedTokenIsRedactedInEventsAndErrors(t *testing.T) {
	logPath := useEngineFakeRegistryCLI(t, engineFakeToken+"\n")
	sink := &events.BufferSink{}
	eng := New(Options{Sink: sink})
	deploy := deployer.NewDeployer(nil, providerRegistryConfig(), "production", false)
	deploy.SetEventSink(eng.stream)
	eng.attachRegistryMinter(deploy)

	needs := deploy.DeployRegistryAuthNeeds(map[string]config.ServiceConfig{
		"web":    {Image: engineTestECRHost + "/web:v1"},
		"worker": {Image: engineTestECRHost + "/worker:v1"},
	}, nil)
	if err := preflightRegistryAuth(context.Background(), deploy, needs); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if n := cliInvocations(t, logPath); n != 1 {
		t.Fatalf("aws ran %d time(s), want once for one ECR host", n)
	}

	dockerOutput := "Error response from daemon: Get https://" + engineTestECRHost + "/v2/: denied: token " + engineFakeToken + " expired"
	eng.emit(events.Event{Type: events.TypeImagePullAuthFailed, Phase: events.PhaseDeploy, Level: events.LevelError, Message: dockerOutput + "\n", Data: map[string]any{"output": dockerOutput}})
	for _, event := range sink.Events() {
		if strings.Contains(event.Message, engineFakeToken) || strings.Contains(fmt.Sprint(event.Data), engineFakeToken) {
			t.Fatalf("event leaked minted token: %+v", event)
		}
	}
	if redacted := eng.RedactSensitive(fmt.Errorf("pull failed: %s", dockerOutput).Error()); strings.Contains(redacted, engineFakeToken) {
		t.Fatalf("result error leaked minted token: %s", redacted)
	}
}

func TestOperationRegistryNeedsHelpers(t *testing.T) {
	deploy := deployer.NewDeployer(nil, providerRegistryConfig(), "production", false)

	scaled := scaleRolloutServices(map[string]config.ServiceConfig{
		"web":    {Image: engineTestECRHost + "/web:v1"},
		"shared": {ImageFrom: "base", SharedBuildHash: "abc"},
		"idle":   {Image: "redis:7"},
	}, map[string]int{"web": 3, "shared": 2})
	if len(scaled) != 1 || scaled["web"].Replicas != 3 {
		t.Fatalf("scale rollout services = %+v", scaled)
	}

	all := map[string]config.ServiceConfig{
		"web":     {Image: "nginx:1.27", DependsOn: []string{"migrate"}},
		"migrate": {Image: engineTestECRHost + "/migrate:v1", Kind: config.ServiceKindRun},
	}
	needs := deployRegistryAuthNeeds(deploy, map[string]config.ServiceConfig{"web": all["web"]}, all, "web")
	joined := strings.Join(needs.PullImages, ",")
	if !strings.Contains(joined, engineTestECRHost+"/migrate:v1") {
		t.Fatalf("targeted deploy needs = %+v, want run prerequisite image", needs)
	}

	outcomes := []RollbackServiceOutcome{
		{Name: "web", Action: RollbackActionRolledBack, ImageSource: RollbackImagePull},
		{Name: "api", Action: RollbackActionRolledBack, ImageSource: RollbackImageRetained},
		{Name: "cache", Action: RollbackActionUnchanged},
	}
	targets := map[string]remotestate.ServiceState{
		"web":   {Image: engineTestECRHost + "/web:v0"},
		"api":   {Image: "demo/api:v0"},
		"cache": {Image: "redis:7"},
	}
	services := map[string]config.ServiceConfig{
		"web":   {Image: engineTestECRHost + "/web:v1"},
		"api":   {Build: "."},
		"cache": {Image: "redis:7"},
	}
	rollback := projectRollbackRegistryAuthNeeds(deploy, services, targets, outcomes)
	if rollback.Build || len(rollback.PullImages) != 1 || rollback.PullImages[0] != engineTestECRHost+"/web:v0" {
		t.Fatalf("project rollback needs = %+v", rollback)
	}
}
