package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/reconcile"
)

func TestPrepareRunPlanServicesFingerprintsResolvedImageFromRevision(t *testing.T) {
	all := map[string]config.ServiceConfig{
		"web":     {Build: "."},
		"migrate": {Kind: config.ServiceKindRun, ImageFrom: "web", Command: config.ListValue("migrate")},
	}
	first, err := prepareRunPlanServices(all, all, map[string]string{"web": "demo/web:rev-a"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := prepareRunPlanServices(all, all, map[string]string{"web": "demo/web:rev-b"})
	if err != nil {
		t.Fatal(err)
	}
	firstHash, _ := reconcile.SafeServiceConfigHash(first["migrate"])
	secondHash, _ := reconcile.SafeServiceConfigHash(second["migrate"])
	if first["migrate"].Image != "demo/web:rev-a" || firstHash == secondHash {
		t.Fatalf("resolved run images/hashes = %#v %s %s", first["migrate"], firstHash, secondHash)
	}
}

func TestDeployPlanAndResultExposeRunMachineContract(t *testing.T) {
	service := config.ServiceConfig{Kind: config.ServiceKindRun, Image: "busybox", Command: config.ListValue("echo", "ok")}
	plan := reconcile.ComputePlan("demo", "production", map[string]config.ServiceConfig{"bootstrap": service}, nil)
	doc := newDeployPlanDocument("demo", "production", plan, map[string]config.ServiceConfig{"bootstrap": service})
	if len(doc.Changes) != 1 || strings.Join(doc.Changes[0].RunCommand, "|") != "echo|ok" {
		t.Fatalf("plan run contract = %#v", doc.Changes)
	}
	result := DeployResult{Services: []ServiceOutcome{{
		Name: "bootstrap", Action: OutcomeRan,
		Run: &RunOutcome{Command: []string{"echo", "ok"}, Server: "node-a", Image: "busybox", ExitCode: 0, DurationMs: 42},
	}}}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"action":"ran"`, `"exitCode":0`, `"durationMs":42`, `"server":"node-a"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("result JSON missing %s: %s", want, data)
		}
	}
}

func TestAddRunHistoryActualDoesNotHideNewestFailedAttempt(t *testing.T) {
	desired := config.ServiceConfig{Kind: config.ServiceKindRun, Image: "busybox:1.36", Command: config.ListValue("true")}
	successHash := runServiceFingerprint(desired, desired.Image)
	history := &remotestate.DeploymentHistory{Deployments: []*remotestate.DeploymentState{
		{Timestamp: time.Now().Add(-time.Hour), Services: map[string]remotestate.ServiceState{
			"migrate": {Kind: config.ServiceKindRun, Name: "migrate", ConfigHash: successHash, Run: &remotestate.RunState{ExitCode: 0}},
		}},
		{Timestamp: time.Now(), Services: map[string]remotestate.ServiceState{
			"migrate": {Kind: config.ServiceKindRun, Name: "migrate", ConfigHash: "failed-newer", Run: &remotestate.RunState{ExitCode: 2}},
		}},
	}}
	actual := map[string]*reconcile.ActualService{}
	addRunHistoryActual(actual, map[string]config.ServiceConfig{"migrate": desired}, history)
	if actual["migrate"] != nil {
		t.Fatalf("newest failed attempt should require rerun, got %#v", actual["migrate"])
	}
}

func TestAddRunHistoryActualDoesNotHideNewestPreflightFailure(t *testing.T) {
	desired := config.ServiceConfig{Kind: config.ServiceKindRun, Image: "busybox:1.36", Command: config.ListValue("true")}
	history := &remotestate.DeploymentHistory{Deployments: []*remotestate.DeploymentState{
		{Timestamp: time.Now().Add(-time.Hour), Services: map[string]remotestate.ServiceState{
			"migrate": runHistoryServiceState("migrate", desired, desired.Image, &deployer.DeployRunResult{ExitCode: 0}),
		}},
		{Timestamp: time.Now(), Services: map[string]remotestate.ServiceState{
			"migrate": runHistoryServiceState("migrate", desired, desired.Image, nil),
		}},
	}}
	actual := map[string]*reconcile.ActualService{}
	addRunHistoryActual(actual, map[string]config.ServiceConfig{"migrate": desired}, history)
	if actual["migrate"] != nil {
		t.Fatalf("newest preflight failure should require rerun, got %#v", actual["migrate"])
	}
}

func TestAddRunHistoryActualUsesNewestSuccessfulRunRecord(t *testing.T) {
	desired := config.ServiceConfig{Kind: config.ServiceKindRun, Image: "busybox:1.36", Command: config.ListValue("true")}
	hash := runServiceFingerprint(desired, desired.Image)
	history := &remotestate.DeploymentHistory{Deployments: []*remotestate.DeploymentState{{
		Timestamp: time.Now(), Services: map[string]remotestate.ServiceState{
			"migrate": {Kind: config.ServiceKindRun, ConfigHash: hash, Run: &remotestate.RunState{ExitCode: 0}},
		},
	}}}
	actual := map[string]*reconcile.ActualService{}
	addRunHistoryActual(actual, map[string]config.ServiceConfig{"migrate": desired}, history)
	if actual["migrate"] == nil || actual["migrate"].ConfigHash != hash || !actual["migrate"].ConfigSnapshot.IsRun() {
		t.Fatalf("synthetic actual = %#v", actual["migrate"])
	}
}

func TestRunHistoryServiceStateCarriesMachineContract(t *testing.T) {
	service := config.ServiceConfig{Kind: config.ServiceKindRun, Image: "busybox", Command: config.ListValue("echo", "ok")}
	result := runHistoryServiceState("bootstrap", service, "busybox", &deployer.DeployRunResult{
		Service: "bootstrap", Server: "node-a", Image: "busybox", Command: []string{"echo", "ok"}, ExitCode: 0, DurationMs: 42,
	})
	if result.Kind != config.ServiceKindRun || result.ConfigHash == "" || result.Run == nil || result.Run.Server != "node-a" || result.Run.DurationMs != 42 {
		t.Fatalf("history result = %#v", result)
	}
}

func TestTargetRunPrerequisitesRequireCurrentSuccessfulFingerprint(t *testing.T) {
	run := config.ServiceConfig{Kind: config.ServiceKindRun, Image: "busybox:1.36", Command: config.ListValue("migrate")}
	services := map[string]config.ServiceConfig{
		"migrate": run,
		"api":     {Image: "demo/api:1", DependsOn: []string{"migrate"}},
		"web":     {Image: "demo/web:1", DependsOn: []string{"api"}},
	}
	required := targetRunPrerequisites(services, "web")
	requiredRun, ok := required["migrate"]
	if len(required) != 1 || !ok || !requiredRun.IsRun() {
		t.Fatalf("required runs = %#v", required)
	}
	if err := ensureRunPrerequisitesCompleted("web", required, nil); err == nil {
		t.Fatal("missing run history accepted")
	}
	history := &remotestate.DeploymentHistory{Deployments: []*remotestate.DeploymentState{{
		Timestamp: time.Now(), Services: map[string]remotestate.ServiceState{
			"migrate": runHistoryServiceState("migrate", run, run.Image, &deployer.DeployRunResult{ExitCode: 0}),
		},
	}}}
	if err := ensureRunPrerequisitesCompleted("web", required, history); err != nil {
		t.Fatalf("successful prerequisite rejected: %v", err)
	}
}

func TestRunHistoryDoesNotOverwriteExistingLongRunningService(t *testing.T) {
	run := config.ServiceConfig{Kind: config.ServiceKindRun, Image: "busybox", Command: config.ListValue("true")}
	old := config.ServiceConfig{Image: "busybox", Command: config.ListValue("sleep", "infinity")}
	actual := map[string]*reconcile.ActualService{"task": {ConfigSnapshot: &old}}
	history := &remotestate.DeploymentHistory{Deployments: []*remotestate.DeploymentState{{
		Timestamp: time.Now(), Services: map[string]remotestate.ServiceState{
			"task": runHistoryServiceState("task", run, run.Image, &deployer.DeployRunResult{ExitCode: 0}),
		},
	}}}
	addRunHistoryActual(actual, map[string]config.ServiceConfig{"task": run}, history)
	if actual["task"].ConfigSnapshot.IsRun() {
		t.Fatalf("run history overwrote live service: %#v", actual["task"])
	}
	if err := validateRunKindTransitions(map[string]config.ServiceConfig{"task": run}, actual); err == nil {
		t.Fatal("long-running to run transition accepted")
	}
}

func TestApplyImageOverrideClearsRunImageFrom(t *testing.T) {
	run := config.ServiceConfig{Kind: config.ServiceKindRun, ImageFrom: "app", Command: config.ListValue("migrate")}
	overridden := ApplyImageOverride(run, "registry.example.com/migrate:1")
	if overridden.Image != "registry.example.com/migrate:1" || overridden.ImageFrom != "" || overridden.Build != "" {
		t.Fatalf("image override = %#v", overridden)
	}
}
