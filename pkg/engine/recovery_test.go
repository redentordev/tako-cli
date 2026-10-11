package engine

import (
	"encoding/json"
	"strings"
	"testing"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
)

func TestDeployPlanDocumentReportsStrategyAndSource(t *testing.T) {
	web := config.ServiceConfig{Image: "nginx", Port: 80, Deploy: config.DeployConfig{Strategy: config.DeployStrategyRolling, StrategyDefaulted: true}}
	db := config.ServiceConfig{Image: "postgres", Persistent: true, Deploy: config.DeployConfig{Strategy: config.DeployStrategyRecreate}}
	run := config.ServiceConfig{Kind: config.ServiceKindRun, Image: "busybox", Command: config.ListValue("echo", "ok")}
	plan := &reconcile.ReconciliationPlan{Changes: []reconcile.ServiceChange{
		{Type: reconcile.ChangeUpdate, ServiceName: "web", Reasons: []string{"Deploy strategy default changed: recreate → rolling"}},
		{Type: reconcile.ChangeAdd, ServiceName: "db"},
		{Type: reconcile.ChangeAdd, ServiceName: "migrate"},
	}}
	doc := newDeployPlanDocument("demo", "production", plan, map[string]config.ServiceConfig{"web": web, "db": db, "migrate": run})
	got := map[string]PlanChange{}
	for _, change := range doc.Changes {
		got[change.Service] = change
	}
	if got["web"].Strategy != "rolling" || got["web"].StrategySource != "default" {
		t.Fatalf("web change = %#v", got["web"])
	}
	if got["db"].Strategy != "recreate" || got["db"].StrategySource != "explicit" {
		t.Fatalf("db change = %#v", got["db"])
	}
	if got["migrate"].Strategy != "" || got["migrate"].StrategySource != "" {
		t.Fatalf("run change must not report a strategy: %#v", got["migrate"])
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"strategy":"rolling","strategySource":"default"`) {
		t.Fatalf("plan JSON = %s", data)
	}
}

func TestFailedServiceOutcomeForEachRecoveryStatus(t *testing.T) {
	tests := []struct {
		report *deployer.RecoveryReport
		action string
	}{
		{&deployer.RecoveryReport{Status: deployer.RecoveryRestored, Mode: deployer.RecoveryModeRestore, Revision: "rev-new", Nodes: []deployer.RecoveryNodeReport{{Server: "node-a", Status: deployer.RecoveryNodeRestored}}}, OutcomeRestored},
		{&deployer.RecoveryReport{Status: deployer.RecoveryFailed, Mode: deployer.RecoveryModeRestore, Nodes: []deployer.RecoveryNodeReport{{Server: "node-a", Status: deployer.RecoveryNodeFailed, Error: "start failed"}}}, OutcomeFailed},
		{&deployer.RecoveryReport{Status: deployer.RecoveryDisabled, Mode: deployer.RecoveryModeRetainFailed}, OutcomeFailed},
		{&deployer.RecoveryReport{Status: deployer.RecoveryUnsupported, Mode: deployer.RecoveryModeLegacy}, OutcomeFailed},
		{nil, OutcomeFailed},
	}
	for _, tt := range tests {
		recovery := recoveryOutcomeFromReport(tt.report, "rev-old")
		if action := failedServiceOutcomeAction(recovery); action != tt.action {
			t.Fatalf("report %+v action = %q, want %q", tt.report, action, tt.action)
		}
		if tt.report == nil {
			continue
		}
		if recovery.Status != tt.report.Status || recovery.Revision != "rev-old" || recovery.FailedRevision != tt.report.Revision {
			t.Fatalf("recovery = %+v", recovery)
		}
		// A failed deploy keeps status failed whatever the recovery.
		result := DeployResult{Status: takoapi.DeploymentStatus(remotestate.StatusFailed), Services: []ServiceOutcome{{Name: "web", Action: failedServiceOutcomeAction(recovery), Strategy: "recreate", Recovery: recovery}}}
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `"status":"failed"`) || !strings.Contains(string(data), `"recovery":{"status":"`+tt.report.Status+`"`) {
			t.Fatalf("result JSON = %s", data)
		}
	}
}

func TestEmitRecoveryEventsOrderAndPayload(t *testing.T) {
	sink := &events.BufferSink{}
	eng := New(Options{Sink: sink})
	recovery := recoveryOutcomeFromReport(&deployer.RecoveryReport{
		Status: deployer.RecoveryRestored, Mode: deployer.RecoveryModeRestore, Revision: "rev-new",
		Nodes: []deployer.RecoveryNodeReport{{Server: "node-a", Status: deployer.RecoveryNodeRestored}, {Server: "node-b", Status: deployer.RecoveryNodeRestored}},
	}, "rev-old")
	eng.emit(events.Event{Type: events.TypeDeployServiceFailed, Service: "web"})
	eng.emitRecoveryEvents("web", recovery)

	var types []string
	for _, event := range sink.Events() {
		types = append(types, event.Type)
	}
	want := []string{events.TypeDeployServiceFailed, events.TypeDeployServiceRestoreStarted, events.TypeDeployServiceRestored}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("event order = %v, want %v", types, want)
	}
	payload, err := json.Marshal(sink.Events()[2].Data)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"status":"restored"`, `"revision":"rev-old"`, `"server":"node-a"`} {
		if !strings.Contains(string(payload), want) {
			t.Fatalf("payload %s missing %s", payload, want)
		}
	}
	for _, forbidden := range []string{"env", "secret", "password"} {
		if strings.Contains(strings.ToLower(string(payload)), forbidden) {
			t.Fatalf("payload %s contains %q", payload, forbidden)
		}
	}
}

func TestEmitRecoveryEventsRestoreFailedAndNoEventsWithoutRestore(t *testing.T) {
	sink := &events.BufferSink{}
	eng := New(Options{Sink: sink})
	eng.emitRecoveryEvents("web", recoveryOutcomeFromReport(&deployer.RecoveryReport{
		Status: deployer.RecoveryFailed, Mode: deployer.RecoveryModeRestore,
		Nodes: []deployer.RecoveryNodeReport{{Server: "node-b", Status: deployer.RecoveryNodeFailed, Error: "start failed"}},
	}, "rev-old"))
	got := sink.Events()
	if len(got) != 2 || got[1].Type != events.TypeDeployServiceRestoreFailed || !strings.Contains(got[1].Message, "node-b") {
		t.Fatalf("events = %+v", got)
	}

	sink = &events.BufferSink{}
	eng = New(Options{Sink: sink})
	eng.emitRecoveryEvents("web", recoveryOutcomeFromReport(&deployer.RecoveryReport{Status: deployer.RecoveryDisabled}, ""))
	eng.emitRecoveryEvents("web", recoveryOutcomeFromReport(&deployer.RecoveryReport{Status: deployer.RecoveryUnsupported}, ""))
	if len(sink.Events()) != 0 {
		t.Fatalf("disabled/unsupported must not emit restore events: %+v", sink.Events())
	}
}

func TestFailureNotificationContentIncludesRecovery(t *testing.T) {
	recovery := recoveryOutcomeFromReport(&deployer.RecoveryReport{Status: deployer.RecoveryRestored, Mode: deployer.RecoveryModeRestore}, "rev-old")
	message, details := failureNotificationContent("Deployment of `demo` to `production` failed after 3s", map[string]string{"version": "1"}, "web", recovery)
	if details["recovery"] != "restored" || details["service"] != "web" || details["version"] != "1" {
		t.Fatalf("details = %#v", details)
	}
	if !strings.Contains(message, "Previous revision of web restored") {
		t.Fatalf("message = %q", message)
	}
	unchanged, same := failureNotificationContent("failed", map[string]string{"version": "1"}, "", nil)
	if unchanged != "failed" || len(same) != 1 {
		t.Fatalf("no recovery must leave the notification unchanged: %q %#v", unchanged, same)
	}
	_, unsupported := failureNotificationContent("failed", nil, "web", recoveryOutcomeFromReport(&deployer.RecoveryReport{Status: deployer.RecoveryUnsupported}, ""))
	if unsupported["recovery"] != "unsupported" {
		t.Fatalf("details = %#v", unsupported)
	}
}

func TestFailedServiceHistoryRoundTripsStrategyAndRecovery(t *testing.T) {
	recovery := recoveryOutcomeFromReport(&deployer.RecoveryReport{
		Status: deployer.RecoveryRestored, Mode: deployer.RecoveryModeRestore,
		Nodes: []deployer.RecoveryNodeReport{{Server: "node-a", Status: deployer.RecoveryNodeRestored}},
	}, "rev-old")
	internal := remotestate.ServiceState{Name: "web", Image: "nginx", DeployStrategy: "recreate", Recovery: recoveryHistoryState(recovery)}
	data, err := json.Marshal(internal)
	if err != nil {
		t.Fatal(err)
	}
	var document takoapi.ServiceStateDocument
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if document.DeployStrategy != "recreate" || document.Recovery == nil || document.Recovery.Status != "restored" || document.Recovery.Revision != "rev-old" || len(document.Recovery.Nodes) != 1 || document.Recovery.Nodes[0].Server != "node-a" {
		t.Fatalf("document = %+v recovery=%+v", document, document.Recovery)
	}
	back, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	var again remotestate.ServiceState
	if err := json.Unmarshal(back, &again); err != nil {
		t.Fatal(err)
	}
	if again.DeployStrategy != "recreate" || again.Recovery == nil || again.Recovery.Mode != deployer.RecoveryModeRestore {
		t.Fatalf("round trip = %+v", again)
	}
}
