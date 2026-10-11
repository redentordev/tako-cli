package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/hooks"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/spf13/cobra"
)

// fakeRollbackSession stands in for *engine.RollbackSession.
type fakeRollbackSession struct {
	plan    engine.RollbackPlan
	request engine.RollbackRequest
	result  *engine.RollbackResult
	err     error
	applied int
	closed  int
}

func (f *fakeRollbackSession) Plan() engine.RollbackPlan { return f.plan }
func (f *fakeRollbackSession) Close()                    { f.closed++ }
func (f *fakeRollbackSession) Apply(ctx context.Context) (*engine.RollbackResult, error) {
	f.applied++
	if f.request.BeforeMutation != nil {
		if hookErr := f.request.BeforeMutation(ctx, engine.MutationInfo{
			Operation: "rollback", Services: f.plan.RolledBackServices(), Servers: f.plan.Servers, TargetDeploymentID: f.plan.Target.ID,
		}); hookErr != nil {
			return nil, hookErr
		}
	}
	return f.result, f.err
}

func sampleProjectRollbackPlan() engine.RollbackPlan {
	plan := engine.RollbackPlan{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindRollbackPlan,
		Project:     "demo",
		Environment: "production",
		Servers:     []string{"node-a"},
		Target:      engine.RollbackPlanTarget{ID: "1767225600000_1", DisplayID: "1767225600", Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Commit: "abc1234"},
		Services: []engine.RollbackServiceOutcome{
			{Name: "api", Action: engine.RollbackActionRolledBack, FromImage: "api:2", ToImage: "api:1", Replicas: 1, SourceDeploymentID: "1767225600000_1", ImageSource: engine.RollbackImageRetained},
			{Name: "nightly", Action: engine.RollbackActionSkipped, Reason: engine.RollbackReasonKindJob},
			{Name: "web", Action: engine.RollbackActionRolledBack, FromImage: "web:2", ToImage: "web:1", Replicas: 2, SourceDeploymentID: "1767225600000_1", ImageSource: engine.RollbackImagePull},
		},
		Warnings:        []string{"job nightly is not rolled back and keeps running the newer image and schedule"},
		ReleaseCommands: engine.RollbackReleaseSkip,
	}
	plan.Hash = plan.PlanHash()
	return plan
}

func setupProjectRollbackTest(t *testing.T, hookBlock string) (*fakeRollbackSession, *fakeHookRunner, *events.BufferSink) {
	t.Helper()
	_, runner := setupHookTest(t, hookBlock)
	sink := &events.BufferSink{}
	cliEngineInstance = engine.New(engine.Options{CLIVersion: Version, Sink: sink})
	oldPlan, oldAllowed, oldPrompt := planRollback, rollbackPromptAllowed, confirmRollbackPrompt
	oldFlags := []any{rollbackIncludePersistent, rollbackRunRelease, rollbackPlanOnly, rollbackPlanFile, rollbackYes}
	t.Cleanup(func() {
		planRollback, rollbackPromptAllowed, confirmRollbackPrompt = oldPlan, oldAllowed, oldPrompt
		rollbackIncludePersistent, rollbackRunRelease, rollbackPlanOnly, rollbackPlanFile, rollbackYes = oldFlags[0].(bool), oldFlags[1].(bool), oldFlags[2].(bool), oldFlags[3].(string), oldFlags[4].(bool)
	})
	rollbackService = ""
	rollbackIncludePersistent, rollbackRunRelease, rollbackPlanOnly, rollbackPlanFile, rollbackYes = false, false, false, "", false
	rollbackPromptAllowed = func(string) error { return nil }
	confirmRollbackPrompt = func() (bool, error) {
		t.Fatal("unexpected confirmation prompt")
		return false, nil
	}
	plan := sampleProjectRollbackPlan()
	session := &fakeRollbackSession{plan: plan}
	session.result = &engine.RollbackResult{
		APIVersion: takoapi.APIVersionCurrent, Kind: engine.KindRollbackResult, Project: "demo", Environment: "production",
		Scope: engine.RollbackScopeProject, DeploymentID: plan.Target.ID, Status: takoapi.StatusRolledBack, Services: plan.Services, Duration: 3,
	}
	planRollback = func(_ context.Context, request engine.RollbackRequest) (rollbackSessionAPI, error) {
		if request.Service != "" || request.Scope() != engine.RollbackScopeProject {
			t.Fatalf("project rollback request = %+v", request)
		}
		session.request = request
		return session, nil
	}
	return session, runner, sink
}

func TestRollbackProjectFlagsRejectedWithService(t *testing.T) {
	oldService := rollbackService
	t.Cleanup(func() { rollbackService = oldService })
	rollbackService = "web"
	for _, name := range rollbackProjectOnlyFlags {
		t.Run(name, func(t *testing.T) {
			command := &cobra.Command{}
			command.Flags().Bool("include-persistent", false, "")
			command.Flags().Bool("run-release", false, "")
			command.Flags().Bool("plan-only", false, "")
			command.Flags().String("plan", "", "")
			command.Flags().BoolP("yes", "y", false, "")
			value := "true"
			if name == "plan" {
				value = "plan.json"
			}
			if err := command.Flags().Set(name, value); err != nil {
				t.Fatal(err)
			}
			err := validateRollbackFlags(command)
			if err == nil || exitCodeForError(err) != 2 || !strings.Contains(err.Error(), "--"+name) {
				t.Fatalf("--service with --%s: err = %v", name, err)
			}
		})
	}
	if err := validateRollbackFlags(&cobra.Command{}); err != nil {
		t.Fatalf("plain --service rejected: %v", err)
	}
}

func TestRollbackCommandNoLongerRequiresService(t *testing.T) {
	flag := rollbackCmd.Flags().Lookup("service")
	if flag == nil {
		t.Fatal("--service flag missing")
	}
	if _, required := flag.Annotations[cobra.BashCompOneRequiredFlag]; required {
		t.Fatal("--service must not be required")
	}
	for _, name := range []string{"include-persistent", "run-release", "plan-only", "plan", "yes"} {
		if rollbackCmd.Flags().Lookup(name) == nil {
			t.Fatalf("--%s flag missing", name)
		}
	}
	if rollbackCmd.Flags().ShorthandLookup("y") == nil {
		t.Fatal("-y shorthand missing")
	}
}

func TestProjectRollbackMachineModeRequiresConfirmation(t *testing.T) {
	session, _, _ := setupProjectRollbackTest(t, "")
	outputFormatFlag = outputFormatJSON
	var runErr error
	stdout := captureStdout(t, func() { runErr = runRollback(&cobra.Command{}, nil) })
	if exitCodeForError(runErr) != 2 || session.applied != 0 {
		t.Fatalf("err = %v applied = %d", runErr, session.applied)
	}
	var document map[string]any
	if err := json.Unmarshal([]byte(stdout), &document); err != nil {
		t.Fatalf("stdout: %v\n%s", err, stdout)
	}
	if document["kind"] != "ConfirmationRequired" || document["operation"] != "rollback" {
		t.Fatalf("document = %v", document)
	}
	plan, ok := document["plan"].(map[string]any)
	if !ok || plan["kind"] != engine.KindRollbackPlan || plan["hash"] == "" {
		t.Fatalf("plan missing from confirmation document: %s", stdout)
	}
	if session.closed == 0 {
		t.Fatal("session not closed")
	}
}

func TestProjectRollbackNonInteractiveTextModeExitsTwo(t *testing.T) {
	session, _, _ := setupProjectRollbackTest(t, "")
	rollbackPromptAllowed = requireDeployPromptAllowed
	t.Setenv("CI", "true")
	var runErr error
	captureStdout(t, func() { runErr = runRollback(&cobra.Command{}, nil) })
	var confirmation *engine.ConfirmationRequiredError
	if !errors.As(runErr, &confirmation) || exitCodeForError(runErr) != 2 || session.applied != 0 {
		t.Fatalf("err = %v applied = %d", runErr, session.applied)
	}
}

func TestProjectRollbackHumanDeclineChangesNothing(t *testing.T) {
	session, _, _ := setupProjectRollbackTest(t, "")
	prompted := false
	confirmRollbackPrompt = func() (bool, error) {
		prompted = true
		return false, nil
	}
	var runErr error
	stdout := captureStdout(t, func() { runErr = runRollback(&cobra.Command{}, nil) })
	if runErr != nil || !prompted || session.applied != 0 {
		t.Fatalf("err = %v prompted = %v applied = %d", runErr, prompted, session.applied)
	}
	for _, want := range []string{"Rollback plan for demo (production)", "1767225600", "api", "retained", "job nightly is not rolled back", "Rollback cancelled"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("output missing %q:\n%s", want, stdout)
		}
	}
}

func TestProjectRollbackPlanOnlyDoesNotApply(t *testing.T) {
	session, _, _ := setupProjectRollbackTest(t, "")
	rollbackPlanOnly = true
	outputFormatFlag = outputFormatJSON
	var runErr error
	stdout := captureStdout(t, func() { runErr = runRollback(&cobra.Command{}, nil) })
	if runErr != nil || session.applied != 0 {
		t.Fatalf("err = %v applied = %d", runErr, session.applied)
	}
	var plan engine.RollbackPlan
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil {
		t.Fatalf("stdout: %v\n%s", err, stdout)
	}
	if plan.Kind != engine.KindRollbackPlan || plan.Hash == "" || plan.Hash != plan.PlanHash() || len(plan.Warnings) != 1 {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestProjectRollbackYesAppliesAndRunsHooks(t *testing.T) {
	session, runner, sink := setupProjectRollbackTest(t, "hooks:\n  commands:\n    pre-rollback: [@self]\n    post-rollback: [@self]\n")
	rollbackYes = true
	eventsFormatFlag = eventsFormatNDJSON
	var runErr error
	captureStdout(t, func() { runErr = runRollback(&cobra.Command{}, nil) })
	if runErr != nil || session.applied != 1 {
		t.Fatalf("err = %v applied = %d", runErr, session.applied)
	}
	pre := runner.call(t, hooks.PreRollback)
	if pre.env["TAKO_SERVICES"] != "api,web" || pre.env["TAKO_ROLLBACK_TARGET"] != "1767225600000_1" {
		t.Fatalf("pre-rollback env = %v", pre.env)
	}
	var result *engine.RollbackResult
	for _, event := range sink.Events() {
		if event.Data["operation"] != "rollback" {
			t.Fatalf("event %s lacks data.operation: %#v", event.Type, event.Data)
		}
		if event.Type == events.TypeResult {
			if event.Data["targetDeploymentId"] != "1767225600000_1" {
				t.Fatalf("result event data = %#v", event.Data)
			}
			result, _ = event.Data["result"].(*engine.RollbackResult)
		}
	}
	if result == nil || result.Scope != engine.RollbackScopeProject || len(result.Hooks) != 2 {
		t.Fatalf("result = %+v", result)
	}
}

func TestProjectRollbackPlanFileVerification(t *testing.T) {
	writePlan := func(t *testing.T, document any) string {
		t.Helper()
		payload, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "plan.json")
		if err := os.WriteFile(path, payload, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("matching plan applies", func(t *testing.T) {
		session, _, _ := setupProjectRollbackTest(t, "")
		rollbackPlanFile, rollbackYes = writePlan(t, sampleProjectRollbackPlan()), true
		outputFormatFlag = outputFormatJSON
		var runErr error
		captureStdout(t, func() { runErr = runRollback(&cobra.Command{}, nil) })
		if runErr != nil || session.applied != 1 {
			t.Fatalf("err = %v applied = %d", runErr, session.applied)
		}
	})
	t.Run("drift is refused", func(t *testing.T) {
		session, _, _ := setupProjectRollbackTest(t, "")
		reviewed := sampleProjectRollbackPlan()
		reviewed.Target.ID = "1767220000000_0"
		rollbackPlanFile, rollbackYes = writePlan(t, reviewed), true
		var runErr error
		captureStdout(t, func() { runErr = runRollback(&cobra.Command{}, nil) })
		if exitCodeForError(runErr) != 2 || !strings.Contains(errorText(runErr), "plan drift detected") || session.applied != 0 {
			t.Fatalf("err = %v applied = %d", runErr, session.applied)
		}
	})
	t.Run("deploy plan is not a rollback plan", func(t *testing.T) {
		session, _, _ := setupProjectRollbackTest(t, "")
		rollbackPlanFile, rollbackYes = writePlan(t, engine.DeployPlan{APIVersion: takoapi.APIVersionCurrent, Kind: engine.KindDeployPlan, Project: "demo"}), true
		var runErr error
		captureStdout(t, func() { runErr = runRollback(&cobra.Command{}, nil) })
		if exitCodeForError(runErr) != 2 || !strings.Contains(errorText(runErr), "not a RollbackPlan document") || session.applied != 0 {
			t.Fatalf("err = %v applied = %d", runErr, session.applied)
		}
	})
}

func TestProjectRollbackUnavailableImageEmitsFailedResult(t *testing.T) {
	setupProjectRollbackTest(t, "")
	failed := &engine.RollbackResult{
		APIVersion: takoapi.APIVersionCurrent, Kind: engine.KindRollbackResult, Scope: engine.RollbackScopeProject,
		DeploymentID: "1767225600000_1", Status: takoapi.StatusFailed, Error: "no image source for api",
		Services: []engine.RollbackServiceOutcome{{Name: "api", Action: engine.RollbackActionNotAttempted}},
	}
	planRollback = func(context.Context, engine.RollbackRequest) (rollbackSessionAPI, error) {
		return nil, &engine.RollbackUnavailableError{Result: failed, Err: errors.New("no image source for api")}
	}
	outputFormatFlag = outputFormatJSON
	var runErr error
	stdout := captureStdout(t, func() { runErr = runRollback(&cobra.Command{}, nil) })
	if exitCodeForError(runErr) != 1 {
		t.Fatalf("err = %v, want exit 1", runErr)
	}
	var result engine.RollbackResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil || result.Status != takoapi.StatusFailed || result.Services[0].Action != engine.RollbackActionNotAttempted {
		t.Fatalf("result = %+v err = %v\n%s", result, err, stdout)
	}
}
