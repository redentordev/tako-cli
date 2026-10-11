package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/hooks"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/spf13/cobra"
)

// fakeHookRunner records hook runs instead of starting processes.
type fakeHookRunner struct {
	mu      sync.Mutex
	calls   []fakeHookCall
	results map[hooks.Name]hooks.Outcome
	trace   *[]string
}

type fakeHookCall struct {
	name hooks.Name
	env  map[string]string
}

func (f *fakeHookRunner) Run(_ context.Context, invocation hooks.Invocation, env []string) hooks.Outcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	values := map[string]string{}
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		values[name] = value
	}
	f.calls = append(f.calls, fakeHookCall{name: invocation.Name, env: values})
	if f.trace != nil {
		*f.trace = append(*f.trace, "hook:"+string(invocation.Name))
	}
	if outcome, ok := f.results[invocation.Name]; ok {
		outcome.Name = invocation.Name
		return outcome
	}
	code := 0
	return hooks.Outcome{Name: invocation.Name, Status: hooks.StatusCompleted, ExitCode: &code, DurationMs: 5}
}

func (f *fakeHookRunner) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, 0, len(f.calls))
	for _, call := range f.calls {
		names = append(names, string(call.name))
	}
	return names
}

func (f *fakeHookRunner) call(t *testing.T, name hooks.Name) fakeHookCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.calls {
		if call.name == name {
			return call
		}
	}
	t.Fatalf("hook %s did not run; runs: %+v", name, f.calls)
	return fakeHookCall{}
}

func failedOutcome(code int, err error) hooks.Outcome {
	return hooks.Outcome{Status: hooks.StatusFailed, ExitCode: &code, DurationMs: 3, Err: err}
}

// setupHookTest writes a project with the given extra tako.yaml content into
// a temp dir, resets CLI globals, and installs a fake hook runner. A hook
// command of "@self" is replaced with the test binary (a path that always
// resolves).
func setupHookTest(t *testing.T, extra string, servers ...string) (string, *fakeHookRunner) {
	t.Helper()
	root := switchToTempDir(t)
	if len(servers) == 0 {
		servers = []string{"node-a"}
	}
	sshKey := filepath.Join(root, "id_ed25519")
	if err := os.WriteFile(sshKey, []byte("test-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	var serverBlock strings.Builder
	for i, name := range servers {
		fmt.Fprintf(&serverBlock, "  %s:\n    host: 203.0.113.%d\n    user: deploy\n    sshKey: '%s'\n", name, 10+i, sshKey)
	}
	content := "project:\n  name: demo\n  version: 1.0.0\nservers:\n" + serverBlock.String() +
		"environments:\n  production:\n    servers: [" + strings.Join(servers, ", ") + "]\n    services:\n      web:\n        image: nginx:alpine\n" +
		strings.ReplaceAll(extra, "@self", "'"+os.Args[0]+"'")
	if err := os.WriteFile(filepath.Join(root, "tako.yaml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	oldCfg, oldEnv, oldVerbose := cfgFile, envFlag, verbose
	oldOutput, oldEvents, oldEngine := outputFormatFlag, eventsFormatFlag, cliEngineInstance
	oldRunner, oldPlan := newHookRunner, planDeploy
	oldRollback, oldPromote, oldSetupNode := runRollbackEngine, runPromoteEngine, setupNode
	oldDeploy := []any{deployPlanOnly, deployPlanFile, deployYes, deploySkipHooks, deployAcceptCluster}
	oldRollbackService, oldRollbackSkip, oldPromoteSkip, oldSetupSkip, oldSetupServer := rollbackService, rollbackSkipHooks, promoteSkipHooks, setupSkipHooks, setupServer
	t.Cleanup(func() {
		cfgFile, envFlag, verbose = oldCfg, oldEnv, oldVerbose
		outputFormatFlag, eventsFormatFlag, cliEngineInstance = oldOutput, oldEvents, oldEngine
		newHookRunner, planDeploy = oldRunner, oldPlan
		runRollbackEngine, runPromoteEngine, setupNode = oldRollback, oldPromote, oldSetupNode
		deployPlanOnly, deployPlanFile, deployYes, deploySkipHooks, deployAcceptCluster = oldDeploy[0].(bool), oldDeploy[1].(string), oldDeploy[2].(bool), oldDeploy[3].(bool), oldDeploy[4].(string)
		rollbackService, rollbackSkipHooks, promoteSkipHooks, setupSkipHooks, setupServer = oldRollbackService, oldRollbackSkip, oldPromoteSkip, oldSetupSkip, oldSetupServer
	})
	cfgFile, envFlag, verbose = "", "", false
	outputFormatFlag, eventsFormatFlag = "", ""
	deployPlanOnly, deployPlanFile, deployYes, deploySkipHooks, deployAcceptCluster = false, "", true, false, ""
	rollbackSkipHooks, promoteSkipHooks, setupSkipHooks, setupServer = false, false, false, ""

	sink := &events.BufferSink{}
	cliEngineInstance = engine.New(engine.Options{CLIVersion: Version, Sink: sink})
	runner := &fakeHookRunner{results: map[hooks.Name]hooks.Outcome{}}
	newHookRunner = func(events.Sink, *config.HooksConfig) hookRunner { return runner }
	return root, runner
}

func loadHookTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := loadDeployConfig("")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

// fakeDeploySession stands in for *engine.DeploySession.
type fakeDeploySession struct {
	plan         engine.DeployPlan
	needsConfirm bool
	result       *engine.DeployResult
	err          error
	applied      int
	closed       int
	trace        *[]string
}

func (f *fakeDeploySession) Plan() engine.DeployPlan  { return f.plan }
func (f *fakeDeploySession) NeedsConfirmation() bool { return f.needsConfirm }
func (f *fakeDeploySession) Close()                  { f.closed++ }
func (f *fakeDeploySession) Apply(context.Context) (*engine.DeployResult, error) {
	f.applied++
	if f.trace != nil {
		*f.trace = append(*f.trace, "apply")
	}
	return f.result, f.err
}

func installFakeDeploy(t *testing.T, empty bool) *fakeDeploySession {
	t.Helper()
	plan := engine.DeployPlan{
		APIVersion: takoapi.APIVersionCurrent, Kind: engine.KindDeployPlan, Project: "demo", Environment: "production",
		Revision: "abc1234", Servers: []string{"node-a"}, Services: []string{"web", "api"}, Empty: empty,
		Git: &engine.GitInfo{Commit: "abc1234def5678", CommitShort: "abc1234", Branch: "main"},
	}
	session := &fakeDeploySession{plan: plan}
	session.result = &engine.DeployResult{
		APIVersion: takoapi.APIVersionCurrent, Kind: engine.KindDeployResult, Project: "demo", Environment: "production",
		Status: takoapi.DeploymentStatus("success"), Services: []engine.ServiceOutcome{{Name: "web", Action: engine.OutcomeDeployed}},
		PlanHash: plan.Hash(), DeploymentID: "1700000000_7", Duration: 4.2,
	}
	planDeploy = func(context.Context, engine.DeployRequest) (deploySessionAPI, error) { return session, nil }
	return session
}

const allDeployHooks = `hooks:
  commands:
    pre-deploy: [@self]
    post-deploy: [@self]
    on-failure: [@self]
`

func TestDeployRunsPreAndPostHooksAroundApply(t *testing.T) {
	_, runner := setupHookTest(t, allDeployHooks)
	session := installFakeDeploy(t, true)
	trace := []string{}
	runner.trace, session.trace = &trace, &trace
	if err := os.WriteFile(".env", []byte("HOOKTEST_DB_PASSWORD=dotenv-secret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Unsetenv("HOOKTEST_DB_PASSWORD") })
	outputFormatFlag = outputFormatJSON

	var runErr error
	stdout := captureStdout(t, func() { runErr = runDeploy(&cobra.Command{}, nil) })
	if runErr != nil {
		t.Fatalf("runDeploy: %v", runErr)
	}
	if strings.Join(trace, ",") != "hook:pre-deploy,apply,hook:post-deploy" {
		t.Fatalf("order = %v", trace)
	}
	if session.closed == 0 {
		t.Fatal("session not closed")
	}
	pre := runner.call(t, hooks.PreDeploy)
	plan := session.plan
	for name, want := range map[string]string{
		"TAKO_HOOK": "pre-deploy", "TAKO_COMMAND": "deploy", "TAKO_PROJECT": "demo", "TAKO_ENVIRONMENT": "production",
		"TAKO_SERVICES": "api,web", "TAKO_SERVERS": "node-a", "TAKO_HOSTS": "203.0.113.10", "TAKO_PLAN_HASH": plan.Hash(),
		"TAKO_PLAN_EMPTY": "true", "TAKO_GIT_COMMIT": "abc1234def5678", "TAKO_REVISION": "abc1234",
	} {
		if pre.env[name] != want {
			t.Fatalf("pre-deploy %s = %q, want %q", name, pre.env[name], want)
		}
	}
	if os.Getenv("HOOKTEST_DB_PASSWORD") == "" {
		t.Fatal("test precondition: config loading should export .env values")
	}
	for name, value := range pre.env {
		if name == "HOOKTEST_DB_PASSWORD" || value == "dotenv-secret-value" {
			t.Fatalf(".env value reached the hook environment as %s", name)
		}
	}
	post := runner.call(t, hooks.PostDeploy)
	if post.env["TAKO_RESULT"] != "success" || post.env["TAKO_DEPLOYMENT_ID"] != "1700000000_7" || post.env["TAKO_DURATION_SECONDS"] != "4.200" {
		t.Fatalf("post-deploy env = %v", post.env)
	}
	var result engine.DeployResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout is not one DeployResult: %v\n%s", err, stdout)
	}
	if len(result.Hooks) != 2 || result.Hooks[0].Name != "pre-deploy" || result.Hooks[1].Name != "post-deploy" || result.Hooks[1].Status != engine.HookCompleted || *result.Hooks[0].ExitCode != 0 {
		t.Fatalf("hooks = %+v", result.Hooks)
	}
	if result.DeploymentID != "1700000000_7" || result.PlanHash != pre.env["TAKO_PLAN_HASH"] {
		t.Fatalf("result = %+v", result)
	}
}

func TestDeployWithoutApprovalRunsNoHooks(t *testing.T) {
	cases := map[string]func(t *testing.T, session *fakeDeploySession){
		"plan only": func(t *testing.T, _ *fakeDeploySession) { deployPlanOnly = true },
		"plan drift": func(t *testing.T, _ *fakeDeploySession) {
			reviewed := engine.DeployPlan{Project: "demo", Revision: "other"}
			payload, _ := json.Marshal(reviewed)
			if err := os.WriteFile("reviewed.json", payload, 0o600); err != nil {
				t.Fatal(err)
			}
			deployPlanFile = "reviewed.json"
		},
		"confirmation required": func(t *testing.T, session *fakeDeploySession) {
			session.needsConfirm = true
			deployYes = false
		},
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			_, runner := setupHookTest(t, allDeployHooks)
			session := installFakeDeploy(t, false)
			outputFormatFlag = outputFormatJSON
			prepare(t, session)
			var runErr error
			captureStdout(t, func() { runErr = runDeploy(&cobra.Command{}, nil) })
			if name != "plan only" && engine.Classify(runErr) != engine.ClassInvalid {
				t.Fatalf("err = %v (class %d), want invalid", runErr, engine.Classify(runErr))
			}
			if len(runner.names()) != 0 || session.applied != 0 {
				t.Fatalf("hooks ran %v, applied %d", runner.names(), session.applied)
			}
		})
	}
}

func TestDeployFailingPostHookIsAttention(t *testing.T) {
	_, runner := setupHookTest(t, allDeployHooks)
	installFakeDeploy(t, false)
	runner.results[hooks.PostDeploy] = failedOutcome(1, errors.New("hook post-deploy exited with status 1"))
	outputFormatFlag = outputFormatJSON

	var runErr error
	stdout := captureStdout(t, func() { runErr = runDeploy(&cobra.Command{}, nil) })
	if engine.Classify(runErr) != engine.ClassAttention || exitCodeForError(runErr) != 6 {
		t.Fatalf("err = %v, want attention (exit 6)", runErr)
	}
	var result engine.DeployResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout: %v\n%s", err, stdout)
	}
	if result.Status != "success" || len(result.Hooks) != 2 || result.Hooks[1].Status != engine.HookFailed || *result.Hooks[1].ExitCode != 1 {
		t.Fatalf("result = %+v hooks = %+v", result, result.Hooks)
	}
	if strings.Join(runner.names(), ",") != "pre-deploy,post-deploy" {
		t.Fatalf("on-failure must not run after a successful deploy: %v", runner.names())
	}
}

func TestDeployFailingPreHookAbortsBeforeApply(t *testing.T) {
	_, runner := setupHookTest(t, allDeployHooks)
	session := installFakeDeploy(t, false)
	runner.results[hooks.PreDeploy] = failedOutcome(3, errors.New("hook pre-deploy exited with status 3"))
	outputFormatFlag = outputFormatJSON

	var runErr error
	stdout := captureStdout(t, func() { runErr = runDeploy(&cobra.Command{}, nil) })
	if exitCodeForError(runErr) != 1 {
		t.Fatalf("err = %v, want exit 1", runErr)
	}
	var hookErr *engine.HookFailedError
	if !errors.As(runErr, &hookErr) || hookErr.Hook != "pre-deploy" {
		t.Fatalf("err = %T %v", runErr, runErr)
	}
	if session.applied != 0 || session.closed == 0 {
		t.Fatalf("applied = %d closed = %d", session.applied, session.closed)
	}
	var result engine.DeployResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout: %v\n%s", err, stdout)
	}
	if result.Status != "failed" || !strings.Contains(result.Error, "pre-deploy") || !strings.Contains(result.Error, "status 3") || result.PlanHash != session.plan.Hash() {
		t.Fatalf("result = %+v", result)
	}
	if len(result.Hooks) != 2 || result.Hooks[0].Status != engine.HookFailed || result.Hooks[1].Name != "on-failure" {
		t.Fatalf("hooks = %+v", result.Hooks)
	}
	failure := runner.call(t, hooks.OnFailure)
	for name, want := range map[string]string{"TAKO_HOOK": "on-failure", "TAKO_COMMAND": "deploy", "TAKO_RESULT": "failed", "TAKO_FAILED_HOOK": "pre-deploy", "TAKO_ERROR_CLASS": "failed"} {
		if failure.env[name] != want {
			t.Fatalf("on-failure %s = %q, want %q", name, failure.env[name], want)
		}
	}
}

func TestDeployFailureRunsOnFailureWithRedactedError(t *testing.T) {
	_, runner := setupHookTest(t, allDeployHooks)
	session := installFakeDeploy(t, false)
	secret := "sup3r-s3cret-registry-token"
	cliEngine().RegisterSecret(secret)
	session.result.Status = "failed"
	session.err = fmt.Errorf("reconcile web: pull failed with token %s", secret)
	runner.results[hooks.OnFailure] = failedOutcome(9, errors.New("hook on-failure exited with status 9"))

	runErr := runDeploy(&cobra.Command{}, nil)
	if runErr != session.err {
		t.Fatalf("err = %v, want the deploy error unchanged", runErr)
	}
	failure := runner.call(t, hooks.OnFailure)
	if strings.Contains(failure.env["TAKO_ERROR"], secret) || !strings.Contains(failure.env["TAKO_ERROR"], "reconcile web") {
		t.Fatalf("TAKO_ERROR = %q", failure.env["TAKO_ERROR"])
	}
	if strings.Join(runner.names(), ",") != "pre-deploy,on-failure" {
		t.Fatalf("runs = %v", runner.names())
	}
}

func TestDeploySkipHooksSkipsBrokenHooks(t *testing.T) {
	root, runner := setupHookTest(t, "hooks:\n  commands:\n    pre-deploy: [@self]\n")
	// A file hook for the same name makes the definition ambiguous.
	if err := os.MkdirAll(filepath.Join(root, ".tako-hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".tako-hooks", "pre-deploy"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	session := installFakeDeploy(t, false)

	err := runDeploy(&cobra.Command{}, nil)
	var invalid *engine.InvalidRequestError
	if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "defined more than once") || session.applied != 0 {
		t.Fatalf("err = %v applied = %d", err, session.applied)
	}

	sink := &events.BufferSink{}
	cliEngineInstance = engine.New(engine.Options{Sink: sink})
	deploySkipHooks = true
	if err := runDeploy(&cobra.Command{}, nil); err != nil {
		t.Fatalf("--skip-hooks deploy: %v", err)
	}
	if session.applied != 1 || len(runner.names()) != 0 {
		t.Fatalf("applied = %d runs = %v", session.applied, runner.names())
	}
	var skipped []events.Event
	for _, event := range sink.Events() {
		if event.Type == events.TypeHookSkipped {
			skipped = append(skipped, event)
		}
	}
	if len(skipped) != 1 || skipped[0].Data["hook"] != "pre-deploy" || skipped[0].Data["reason"] != "--skip-hooks" {
		t.Fatalf("skipped events = %+v", skipped)
	}
	if hooks := session.result.Hooks; len(hooks) != 1 || hooks[0].Status != engine.HookSkipped || hooks[0].ExitCode != nil {
		t.Fatalf("result hooks = %+v", hooks)
	}
}

func TestDeployWithoutHooksIsUnchanged(t *testing.T) {
	_, runner := setupHookTest(t, "")
	session := installFakeDeploy(t, false)
	sink := &events.BufferSink{}
	cliEngineInstance = engine.New(engine.Options{Sink: sink})
	if err := runDeploy(&cobra.Command{}, nil); err != nil {
		t.Fatal(err)
	}
	if session.result.Hooks != nil || len(runner.names()) != 0 {
		t.Fatalf("hooks = %+v runs = %v", session.result.Hooks, runner.names())
	}
	for _, event := range sink.Events() {
		if event.Phase == events.PhaseHooks {
			t.Fatalf("hook event without hooks: %+v", event)
		}
	}
}

func TestHookSessionCancellationClassifiesCancelled(t *testing.T) {
	_, runner := setupHookTest(t, allDeployHooks)
	session := installFakeDeploy(t, false)
	runner.results[hooks.PreDeploy] = hooks.Outcome{Status: hooks.StatusFailed, Err: fmt.Errorf("hook pre-deploy interrupted: %w", context.Canceled)}
	err := runDeploy(&cobra.Command{}, nil)
	if exitCodeForError(err) != 5 || session.applied != 0 {
		t.Fatalf("err = %v (exit %d), applied = %d", err, exitCodeForError(err), session.applied)
	}
	if runner.call(t, hooks.OnFailure).env["TAKO_ERROR_CLASS"] != "cancelled" {
		t.Fatal("on-failure must run after a cancelled pre hook")
	}
}

func TestHookConfigErrorsAreInvalidRequests(t *testing.T) {
	for name, block := range map[string]string{
		"path inside .tako":  "hooks:\n  path: .tako/hooks\n",
		"path escaping":      "hooks:\n  path: ../shared-hooks\n",
		"timeout too long":   "hooks:\n  timeout: 2h\n",
		"unknown hook name":  "hooks:\n  commands:\n    pre-build: [make]\n",
		"env ref in command": "hooks:\n  commands:\n    post-deploy: [notify, \"${SLACK_TOKEN}\"]\n",
		"reserved passEnv":   "hooks:\n  passEnv: [TAKO_X]\n",
	} {
		t.Run(name, func(t *testing.T) {
			setupHookTest(t, block)
			_, err := loadDeployConfig("")
			if err == nil || exitCodeForError(err) != 2 {
				t.Fatalf("err = %v, want exit 2", err)
			}
		})
	}
}

func TestRollbackHooksReceiveTarget(t *testing.T) {
	_, runner := setupHookTest(t, "hooks:\n  commands:\n    pre-rollback: [@self]\n    post-rollback: [@self]\n")
	rollbackService = "web"
	mutated := false
	runRollbackEngine = func(ctx context.Context, request engine.RollbackRequest) (*engine.RollbackResult, error) {
		if request.BeforeMutation == nil {
			t.Fatal("BeforeMutation not wired")
		}
		if err := request.BeforeMutation(ctx, engine.MutationInfo{Operation: "rollback", Services: []string{"web"}, Servers: []string{"node-a"}, TargetDeploymentID: "deploy-42", Revision: "abc1234"}); err != nil {
			return nil, err
		}
		mutated = true
		return &engine.RollbackResult{APIVersion: takoapi.APIVersionCurrent, Kind: engine.KindRollbackResult, Service: "web", DeploymentID: "deploy-42", Status: "rolled_back", Duration: 2}, nil
	}
	outputFormatFlag = outputFormatJSON
	var runErr error
	stdout := captureStdout(t, func() { runErr = runRollback(&cobra.Command{}, nil) })
	if runErr != nil || !mutated {
		t.Fatalf("err = %v mutated = %v", runErr, mutated)
	}
	pre := runner.call(t, hooks.PreRollback)
	if pre.env["TAKO_ROLLBACK_TARGET"] != "deploy-42" || pre.env["TAKO_SERVICES"] != "web" || pre.env["TAKO_COMMAND"] != "rollback" || pre.env["TAKO_REVISION"] != "abc1234" {
		t.Fatalf("pre-rollback env = %v", pre.env)
	}
	post := runner.call(t, hooks.PostRollback)
	if post.env["TAKO_ROLLBACK_TARGET"] != "deploy-42" || post.env["TAKO_RESULT"] != "rolled_back" {
		t.Fatalf("post-rollback env = %v", post.env)
	}
	var result engine.RollbackResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil || len(result.Hooks) != 2 {
		t.Fatalf("result = %+v err = %v\n%s", result, err, stdout)
	}
}

func TestRollbackFailingPreHookBlocksMutation(t *testing.T) {
	_, runner := setupHookTest(t, "hooks:\n  commands:\n    pre-rollback: [@self]\n    on-failure: [@self]\n")
	rollbackService = "web"
	runner.results[hooks.PreRollback] = failedOutcome(2, errors.New("hook pre-rollback exited with status 2"))
	mutated := false
	runRollbackEngine = func(ctx context.Context, request engine.RollbackRequest) (*engine.RollbackResult, error) {
		if err := request.BeforeMutation(ctx, engine.MutationInfo{Operation: "rollback", Services: []string{"web"}, TargetDeploymentID: "deploy-42"}); err != nil {
			return nil, err
		}
		mutated = true
		return nil, nil
	}
	outputFormatFlag = outputFormatJSON
	var runErr error
	stdout := captureStdout(t, func() { runErr = runRollback(&cobra.Command{}, nil) })
	if mutated || exitCodeForError(runErr) != 1 {
		t.Fatalf("mutated = %v err = %v", mutated, runErr)
	}
	var result engine.RollbackResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout: %v\n%s", err, stdout)
	}
	if result.Status != "failed" || result.DeploymentID != "deploy-42" || !strings.Contains(result.Error, "pre-rollback") || len(result.Hooks) != 2 {
		t.Fatalf("result = %+v", result)
	}
	if runner.call(t, hooks.OnFailure).env["TAKO_FAILED_HOOK"] != "pre-rollback" {
		t.Fatal("on-failure missing TAKO_FAILED_HOOK")
	}
}

func TestPromoteHooksReceiveRevision(t *testing.T) {
	_, runner := setupHookTest(t, "hooks:\n  commands:\n    pre-promote: [@self]\n    post-promote: [@self]\n    on-failure: [@self]\n")
	runner.results[hooks.PrePromote] = failedOutcome(1, errors.New("hook pre-promote exited with status 1"))
	switched := false
	runPromoteEngine = func(ctx context.Context, request engine.PromoteRequest) (*engine.PromoteResult, error) {
		if err := request.BeforeMutation(ctx, engine.MutationInfo{Operation: "promote", Services: []string{"web"}, Servers: []string{"node-a"}, Revision: "rev-1", Image: "registry.test/web:abc"}); err != nil {
			return nil, err
		}
		switched = true
		return &engine.PromoteResult{Status: "success"}, nil
	}
	err := runPromote(&cobra.Command{}, []string{"web"})
	if switched || exitCodeForError(err) != 1 {
		t.Fatalf("switched = %v err = %v", switched, err)
	}
	pre := runner.call(t, hooks.PrePromote)
	if pre.env["TAKO_REVISION"] != "rev-1" || pre.env["TAKO_IMAGE"] != "registry.test/web:abc" || pre.env["TAKO_COMMAND"] != "promote" {
		t.Fatalf("pre-promote env = %v", pre.env)
	}
	failure := runner.call(t, hooks.OnFailure)
	if failure.env["TAKO_HOOK"] != "on-failure" || failure.env["TAKO_FAILED_HOOK"] != "pre-promote" {
		t.Fatalf("on-failure env = %v", failure.env)
	}
}

func TestSetupHooksWrapTheNodeLoop(t *testing.T) {
	_, runner := setupHookTest(t, "hooks:\n  commands:\n    pre-setup: [@self]\n    post-setup: [@self]\n", "node-a", "node-b")
	trace := []string{}
	runner.trace = &trace
	setupNode = func(_ *config.Config, _ *ssh.Pool, name string, server config.ServerConfig, _ string) (engine.SetupNodeResult, error) {
		trace = append(trace, "node:"+name)
		return engine.SetupNodeResult{Server: name, Host: server.Host}, nil
	}
	outputFormatFlag = outputFormatJSON
	var runErr error
	stdout := captureStdout(t, func() { runErr = runSetup(&cobra.Command{}, nil) })
	if runErr != nil {
		t.Fatal(runErr)
	}
	if got := strings.Join(trace, ","); got != "hook:pre-setup,node:node-a,node:node-b,hook:post-setup" {
		t.Fatalf("order = %s", got)
	}
	pre := runner.call(t, hooks.PreSetup)
	if pre.env["TAKO_SERVERS"] != "node-a,node-b" || pre.env["TAKO_HOSTS"] != "203.0.113.10,203.0.113.11" || pre.env["TAKO_COMMAND"] != "setup" {
		t.Fatalf("pre-setup env = %v", pre.env)
	}
	var result engine.SetupResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil || len(result.Hooks) != 2 || len(result.Nodes) != 2 {
		t.Fatalf("result = %+v err = %v", result, err)
	}
}

func TestSetupFailingPreHookSetsUpNoNode(t *testing.T) {
	_, runner := setupHookTest(t, "hooks:\n  commands:\n    pre-setup: [@self]\n")
	runner.results[hooks.PreSetup] = failedOutcome(1, errors.New("hook pre-setup exited with status 1"))
	setupNode = func(*config.Config, *ssh.Pool, string, config.ServerConfig, string) (engine.SetupNodeResult, error) {
		t.Fatal("node set up despite failing pre-setup")
		return engine.SetupNodeResult{}, nil
	}
	err := runSetup(&cobra.Command{}, nil)
	if exitCodeForError(err) != 1 {
		t.Fatalf("err = %v", err)
	}
}

func TestHookSessionIgnoresProblemsInOtherCommandsHooks(t *testing.T) {
	setupHookTest(t, "hooks:\n  commands:\n    pre-setup: [definitely-not-a-real-command-xyz]\n")
	cfg := loadHookTestConfig(t)
	if _, err := newHookSession(hookCommandDeploy, cfg, "tako.yaml", false); err != nil {
		t.Fatalf("deploy must not fail on a broken pre-setup hook: %v", err)
	}
	_, err := newHookSession(hookCommandSetup, cfg, "tako.yaml", false)
	if exitCodeForError(err) != 2 || !strings.Contains(err.Error(), "definitely-not-a-real-command-xyz") {
		t.Fatalf("setup err = %v", err)
	}
}

// TestHookHelperProcess is the real hook process for the machine-output
// tests; it prints to stdout and exits before the test framework writes.
func TestHookHelperProcess(t *testing.T) {
	if os.Getenv("HOOK_HELPER_MODE") != "hello" {
		return
	}
	fmt.Println("hello")
	fmt.Fprintln(os.Stderr, "to-stderr")
	os.Exit(0)
}

const realHookConfig = `hooks:
  passEnv: [HOOK_HELPER_MODE]
  commands:
    pre-deploy: [@self, "-test.run=^TestHookHelperProcess$"]
`

func TestHookOutputUnderJSONOutputStaysOnStderr(t *testing.T) {
	setupHookTest(t, realHookConfig)
	newHookRunner = func(sink events.Sink, cfg *config.HooksConfig) hookRunner {
		return &hooks.Runner{Sink: sink, Timeout: cfg.TimeoutDuration()}
	}
	t.Setenv("HOOK_HELPER_MODE", "hello")
	installFakeDeploy(t, false)
	outputFormatFlag = outputFormatJSON

	var runErr error
	stdout, stderr := captureStdoutAndStderr(t, func() {
		cliEngineInstance = nil
		runErr = runDeploy(&cobra.Command{}, nil)
	})
	if runErr != nil {
		t.Fatalf("runDeploy: %v", runErr)
	}
	var result engine.DeployResult
	decoder := json.NewDecoder(strings.NewReader(stdout))
	if err := decoder.Decode(&result); err != nil || decoder.More() {
		t.Fatalf("stdout is not a single JSON document (%v):\n%s", err, stdout)
	}
	if strings.Contains(stdout, "hello") || !strings.Contains(stderr, "hello") || !strings.Contains(stderr, "to-stderr") {
		t.Fatalf("hook output misrouted\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	if len(result.Hooks) != 1 || result.Hooks[0].Status != engine.HookCompleted {
		t.Fatalf("hooks = %+v", result.Hooks)
	}
}

func TestHookOutputUnderNDJSONIsEvents(t *testing.T) {
	setupHookTest(t, realHookConfig)
	newHookRunner = func(sink events.Sink, cfg *config.HooksConfig) hookRunner {
		return &hooks.Runner{Sink: sink, Timeout: cfg.TimeoutDuration()}
	}
	t.Setenv("HOOK_HELPER_MODE", "hello")
	installFakeDeploy(t, false)
	eventsFormatFlag = eventsFormatNDJSON

	var runErr error
	stdout, _ := captureStdoutAndStderr(t, func() {
		cliEngineInstance = nil
		runErr = runDeploy(&cobra.Command{}, nil)
	})
	if runErr != nil {
		t.Fatalf("runDeploy: %v", runErr)
	}
	type wireEvent struct {
		Type  string         `json:"type"`
		Phase string         `json:"phase"`
		Data  map[string]any `json:"data"`
	}
	var stream []wireEvent
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	for scanner.Scan() {
		var event wireEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("stdout line is not an event: %q", scanner.Text())
		}
		stream = append(stream, event)
	}
	outputIndex, resultIndex := -1, -1
	for i, event := range stream {
		if event.Type == events.TypeHookOutput && event.Data["data"] == "hello" {
			if event.Data["hook"] != "pre-deploy" || event.Data["stream"] != "stdout" || event.Phase != events.PhaseHooks {
				t.Fatalf("hook.output = %+v", event)
			}
			outputIndex = i
		}
		if event.Type == events.TypeResult {
			resultIndex = i
			result, _ := event.Data["result"].(map[string]any)
			if hooksField, ok := result["hooks"].([]any); !ok || len(hooksField) != 1 {
				t.Fatalf("result event without hooks: %+v", event.Data)
			}
		}
	}
	if outputIndex < 0 || resultIndex < 0 || outputIndex > resultIndex || resultIndex != len(stream)-1 {
		t.Fatalf("hook.output index %d, result index %d of %d events", outputIndex, resultIndex, len(stream))
	}
}

func runValidateJSON(t *testing.T) (engine.ValidateResult, error) {
	t.Helper()
	validateQuiet = true
	t.Cleanup(func() { validateQuiet = false })
	outputFormatFlag = outputFormatJSON
	var runErr error
	stdout := captureStdout(t, func() { runErr = runValidate(&cobra.Command{}, nil) })
	var result engine.ValidateResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout is not one ValidateResult: %v\n%s", err, stdout)
	}
	return result, runErr
}

func hookFindings(result engine.ValidateResult) []engine.ValidateFinding {
	var findings []engine.ValidateFinding
	for _, finding := range result.Findings {
		if strings.HasPrefix(finding.Field, "hooks") {
			findings = append(findings, finding)
		}
	}
	return findings
}

func noHookRunnerAllowed(t *testing.T) {
	newHookRunner = func(events.Sink, *config.HooksConfig) hookRunner {
		t.Fatal("validate must not create a hook runner")
		return nil
	}
}

func TestValidateReportsNonExecutableHook(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable bit is a Unix concept")
	}
	root, _ := setupHookTest(t, "")
	noHookRunnerAllowed(t)
	writeTestHook(t, root, "pre-deploy", 0o644)
	result, err := runValidateJSON(t)
	if exitCodeForError(err) != 2 || result.Valid {
		t.Fatalf("err = %v result = %+v", err, result)
	}
	findings := hookFindings(result)
	if len(findings) != 1 || findings[0].Severity != engine.ValidateSeverityError || findings[0].Field != "hooks.pre-deploy" || !strings.Contains(findings[0].Message, "chmod +x") {
		t.Fatalf("findings = %+v", findings)
	}
}

func TestValidateReportsAmbiguousHook(t *testing.T) {
	root, _ := setupHookTest(t, "hooks:\n  commands:\n    post-deploy: [@self]\n")
	noHookRunnerAllowed(t)
	writeTestHook(t, root, "post-deploy", 0o755)
	result, err := runValidateJSON(t)
	if exitCodeForError(err) != 2 || result.Valid {
		t.Fatalf("err = %v result = %+v", err, result)
	}
	findings := hookFindings(result)
	if len(findings) != 1 || findings[0].Field != "hooks.post-deploy" || !strings.Contains(findings[0].Message, "defined more than once") {
		t.Fatalf("findings = %+v", findings)
	}
}

func TestValidateWarnsAboutMisspelledHooks(t *testing.T) {
	root, _ := setupHookTest(t, "")
	noHookRunnerAllowed(t)
	writeTestHook(t, root, "pre_deploy", 0o755)
	writeTestHook(t, root, "post-deploi", 0o755)
	result, err := runValidateJSON(t)
	if err != nil || !result.Valid {
		t.Fatalf("err = %v result = %+v", err, result)
	}
	findings := hookFindings(result)
	if len(findings) != 2 {
		t.Fatalf("findings = %+v", findings)
	}
	for _, finding := range findings {
		if finding.Severity != engine.ValidateSeverityWarn || !strings.Contains(finding.Message, "Known hooks: pre-deploy") {
			t.Fatalf("finding = %+v", finding)
		}
	}
}

func TestValidateWithoutHooksHasNoHookFindings(t *testing.T) {
	setupHookTest(t, "")
	noHookRunnerAllowed(t)
	result, err := runValidateJSON(t)
	if err != nil || !result.Valid || len(hookFindings(result)) != 0 {
		t.Fatalf("err = %v result = %+v", err, result)
	}
}

func TestValidateNeverRunsHooks(t *testing.T) {
	setupHookTest(t, "hooks:\n  passEnv: [HOOK_HELPER_MODE]\n  commands:\n    pre-deploy: [@self, \"-test.run=^TestHookHelperProcess$\"]\n    on-failure: [@self, \"-test.run=^TestHookHelperProcess$\"]\n")
	t.Setenv("HOOK_HELPER_MODE", "hello")
	noHookRunnerAllowed(t)
	validateQuiet = true
	t.Cleanup(func() { validateQuiet = false })
	eventsFormatFlag = eventsFormatNDJSON
	var runErr error
	stdout, stderr := captureStdoutAndStderr(t, func() {
		cliEngineInstance = nil
		runErr = runValidate(&cobra.Command{}, nil)
	})
	if runErr != nil {
		t.Fatalf("validate: %v", runErr)
	}
	if strings.Contains(stdout+stderr, "hello") || strings.Contains(stdout, `"type":"hook.`) {
		t.Fatalf("validate emitted hook events:\n%s", stdout)
	}
}

// writeTestHook writes an executable-or-not shell hook into .tako-hooks.
func writeTestHook(t *testing.T, root, name string, mode os.FileMode) {
	t.Helper()
	dir := filepath.Join(root, ".tako-hooks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\ntouch ran\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
