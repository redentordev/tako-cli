package deployer

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

func boolPtr(value bool) *bool { return &value }

func TestBuildTakodHealthSpecAddsImplicitTCPReadinessForDefaultedRolling(t *testing.T) {
	deploy := &Deployer{}
	service := &config.ServiceConfig{
		Port:   8080,
		Deploy: config.DeployConfig{Strategy: config.DeployStrategyRolling, StrategyDefaulted: true},
	}
	spec := deploy.buildTakodHealthSpec(service)
	if spec == nil {
		t.Fatal("defaulted rolling service without health config must get an implicit readiness probe")
	}
	if !spec.Implicit || spec.Port != service.Port || spec.WaitAttempts != implicitTCPReadinessAttempts || implicitTCPReadinessAttempts != 60 {
		t.Fatalf("implicit spec = %+v", spec)
	}
	if spec.Path != "" || spec.Command != "" || spec.StartPeriod != "" {
		t.Fatalf("implicit spec must be a plain TCP probe without start period: %+v", spec)
	}
}

func TestBuildTakodHealthSpecSkipsImplicitReadinessWhenNotDefaultedRolling(t *testing.T) {
	deploy := &Deployer{}
	for name, service := range map[string]*config.ServiceConfig{
		"explicit rolling":  {Port: 8080, Deploy: config.DeployConfig{Strategy: config.DeployStrategyRolling}},
		"recreate default":  {Port: 8080, Deploy: config.DeployConfig{Strategy: config.DeployStrategyRecreate, StrategyDefaulted: true}},
		"explicit recreate": {Port: 8080, Deploy: config.DeployConfig{Strategy: config.DeployStrategyRecreate}},
		"no port":           {Deploy: config.DeployConfig{Strategy: config.DeployStrategyRolling, StrategyDefaulted: true}},
	} {
		if spec := deploy.buildTakodHealthSpec(service); spec != nil {
			t.Fatalf("%s: health spec = %+v, want nil", name, spec)
		}
	}
	// An operator healthCheck always wins over the implicit probe.
	spec := deploy.buildTakodHealthSpec(&config.ServiceConfig{
		Port:        8080,
		HealthCheck: config.HealthCheckConfig{Path: "/health"},
		Deploy:      config.DeployConfig{Strategy: config.DeployStrategyRolling, StrategyDefaulted: true},
	})
	if spec == nil || spec.Implicit || spec.Path != "/health" {
		t.Fatalf("operator health spec = %+v", spec)
	}
}

func TestDeriveTakodFailurePlan(t *testing.T) {
	tests := []struct {
		name       string
		strategy   string
		rollback   *bool
		policy     string
		capability bool
		explicit   bool
		mode       string
	}{
		{"recreate default", config.DeployStrategyRecreate, nil, takod.FailurePolicyRestore, true, false, RecoveryModeRestore},
		{"recreate true", config.DeployStrategyRecreate, boolPtr(true), takod.FailurePolicyRestore, true, true, RecoveryModeRestore},
		{"recreate false", config.DeployStrategyRecreate, boolPtr(false), takod.FailurePolicyRetainFailed, true, true, RecoveryModeRetainFailed},
		{"rolling default", config.DeployStrategyRolling, nil, "", false, false, RecoveryModeNotCutOver},
		{"rolling true", config.DeployStrategyRolling, boolPtr(true), "", false, true, RecoveryModeNotCutOver},
		{"rolling false", config.DeployStrategyRolling, boolPtr(false), takod.FailurePolicyRetainFailed, true, true, RecoveryModeRetainFailed},
		{"blue green default", config.DeployStrategyBlueGreen, nil, "", false, false, RecoveryModeNotCutOver},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := deriveTakodFailurePlan(&config.ServiceConfig{Port: 80, Deploy: config.DeployConfig{Strategy: tt.strategy, RollbackOnFailure: tt.rollback}})
			if plan.Policy != tt.policy || plan.NeedsCapability != tt.capability || plan.Explicit != tt.explicit || plan.Mode != tt.mode || plan.Strategy != tt.strategy {
				t.Fatalf("plan = %+v", plan)
			}
		})
	}
}

func missingCapabilityCheck(missing ...string) func(string) error {
	set := map[string]bool{}
	for _, server := range missing {
		set[server] = true
	}
	return func(server string) error {
		if set[server] {
			return &takodclient.CapabilityRequiredError{Server: server, Capability: takod.CapabilityDeployRestoreOnFailureV1, Feature: restoreOnFailureFeature}
		}
		return nil
	}
}

func TestResolveTakodFailurePlanAllNodesSupportCapability(t *testing.T) {
	plan := deriveTakodFailurePlan(&config.ServiceConfig{Deploy: config.DeployConfig{Strategy: config.DeployStrategyRecreate}})
	resolved, missing, err := resolveTakodFailurePlan("web", plan, []string{"node-a", "node-b"}, missingCapabilityCheck())
	if err != nil || len(missing) != 0 || resolved.Policy != takod.FailurePolicyRestore || resolved.Unsupported {
		t.Fatalf("resolved = %+v missing=%v err=%v", resolved, missing, err)
	}
}

func TestResolveTakodFailurePlanDefaultedDowngradesAllNodesWhenOneLacksCapability(t *testing.T) {
	plan := deriveTakodFailurePlan(&config.ServiceConfig{Deploy: config.DeployConfig{Strategy: config.DeployStrategyRecreate}})
	resolved, missing, err := resolveTakodFailurePlan("web", plan, []string{"node-a", "node-b"}, missingCapabilityCheck("node-b"))
	if err != nil {
		t.Fatalf("defaulted rollbackOnFailure must downgrade, got %v", err)
	}
	if resolved.Policy != "" || resolved.NeedsCapability || !resolved.Unsupported || resolved.Mode != RecoveryModeLegacy {
		t.Fatalf("resolved = %+v", resolved)
	}
	if strings.Join(missing, ",") != "node-b" {
		t.Fatalf("missing = %v", missing)
	}
	warning := restoreOnFailureDowngradeWarning("web", missing)
	for _, want := range []string{"node-b", takod.CapabilityDeployRestoreOnFailureV1, "tako upgrade servers"} {
		if !strings.Contains(warning, want) {
			t.Fatalf("warning %q missing %q", warning, want)
		}
	}
}

func TestResolveTakodFailurePlanExplicitFailsClosed(t *testing.T) {
	for _, rollback := range []bool{true, false} {
		plan := deriveTakodFailurePlan(&config.ServiceConfig{Deploy: config.DeployConfig{Strategy: config.DeployStrategyRecreate, RollbackOnFailure: boolPtr(rollback)}})
		_, _, err := resolveTakodFailurePlan("web", plan, []string{"node-a", "node-b"}, missingCapabilityCheck("node-a"))
		var capabilityErr *takodclient.CapabilityRequiredError
		if err == nil || !errors.As(err, &capabilityErr) {
			t.Fatalf("rollbackOnFailure=%v: err = %v, want capability error", rollback, err)
		}
		for _, want := range []string{"deploy.rollbackOnFailure", takod.CapabilityDeployRestoreOnFailureV1, "tako upgrade servers", "node-a"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q missing %q", err, want)
			}
		}
	}
}

func TestResolveTakodFailurePlanTransportErrorFails(t *testing.T) {
	plan := deriveTakodFailurePlan(&config.ServiceConfig{Deploy: config.DeployConfig{Strategy: config.DeployStrategyRecreate}})
	_, _, err := resolveTakodFailurePlan("web", plan, []string{"node-a"}, func(string) error { return fmt.Errorf("connection refused") })
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveTakodFailurePlanSkipsCheckForLegacyPlans(t *testing.T) {
	plan := deriveTakodFailurePlan(&config.ServiceConfig{Port: 80, Deploy: config.DeployConfig{Strategy: config.DeployStrategyRolling}})
	called := false
	_, _, err := resolveTakodFailurePlan("web", plan, []string{"node-a"}, func(string) error { called = true; return nil })
	if err != nil || called {
		t.Fatalf("rolling default plan must not check capability: called=%v err=%v", called, err)
	}
}

type fakeRecoveryNodes struct {
	mu         sync.Mutex
	reconcile  map[string]error
	finalized  []string
	restored   []string
	restoreErr map[string]error
	finalErr   map[string]error
	warnings   []string
	noToken    map[string]bool
}

func (f *fakeRecoveryNodes) operations() takodRecoveryOperations {
	return takodRecoveryOperations{
		Reconcile: func(server string) (*takod.ReconcileServiceResponse, error) {
			if err := f.reconcile[server]; err != nil {
				return nil, err
			}
			response := &takod.ReconcileServiceResponse{Service: "web"}
			if !f.noToken[server] {
				response.RestoreToken = "rev." + server
				response.RetainedContainers = []string{"tako_demo_production_web_prev_1"}
			}
			return response, nil
		},
		Finalize: func(server string, response *takod.ReconcileServiceResponse) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if response.RestoreToken != "rev."+server {
				return fmt.Errorf("unexpected token %q", response.RestoreToken)
			}
			f.finalized = append(f.finalized, server)
			return f.finalErr[server]
		},
		Restore: func(server string, response *takod.ReconcileServiceResponse) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.restored = append(f.restored, server)
			return f.restoreErr[server]
		},
		Warn: func(message string) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.warnings = append(f.warnings, message)
		},
	}
}

func typedReconcileFailure(body takod.ReconcileFailureBody) error {
	raw, _ := json.Marshal(body)
	return parseTakodReconcileFailure(&takodclient.HTTPError{Method: "POST", Endpoint: "/v1/reconcile-service", Status: 502, Body: string(raw)})
}

func restorePlan() takodFailurePlan {
	return deriveTakodFailurePlan(&config.ServiceConfig{Deploy: config.DeployConfig{Strategy: config.DeployStrategyRecreate}})
}

func TestCoordinateTakodServiceReconcileFinalizesAllNodesOnSuccess(t *testing.T) {
	nodes := &fakeRecoveryNodes{}
	report, err := coordinateTakodServiceReconcile([]string{"node-a", "node-b"}, restorePlan(), nodes.operations())
	if err != nil || report != nil {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if len(nodes.finalized) != 2 || len(nodes.restored) != 0 {
		t.Fatalf("finalized=%v restored=%v", nodes.finalized, nodes.restored)
	}
}

func TestCoordinateTakodServiceReconcileFinalizeFailureIsWarning(t *testing.T) {
	nodes := &fakeRecoveryNodes{finalErr: map[string]error{"node-b": fmt.Errorf("docker rm failed")}}
	report, err := coordinateTakodServiceReconcile([]string{"node-a", "node-b"}, restorePlan(), nodes.operations())
	if err != nil || report != nil {
		t.Fatalf("finalize failure must not fail the deploy: report=%+v err=%v", report, err)
	}
	if len(nodes.warnings) != 1 || !strings.Contains(nodes.warnings[0], "docker rm failed") {
		t.Fatalf("warnings = %v", nodes.warnings)
	}
}

func TestCoordinateTakodServiceReconcileRestoresSucceededNodesWhenOneFails(t *testing.T) {
	nodes := &fakeRecoveryNodes{reconcile: map[string]error{
		"node-b": typedReconcileFailure(takod.ReconcileFailureBody{Error: "health check failed", FailurePolicy: takod.FailurePolicyRestore, Restored: true}),
	}}
	report, err := coordinateTakodServiceReconcile([]string{"node-a", "node-b"}, restorePlan(), nodes.operations())
	if err == nil || !strings.Contains(err.Error(), "health check failed") {
		t.Fatalf("err = %v", err)
	}
	if strings.Join(nodes.restored, ",") != "node-a" || len(nodes.finalized) != 0 {
		t.Fatalf("restored=%v finalized=%v", nodes.restored, nodes.finalized)
	}
	if report == nil || report.Status != RecoveryRestored || report.Mode != RecoveryModeRestore || len(report.Nodes) != 2 {
		t.Fatalf("report = %+v", report)
	}
	for _, node := range report.Nodes {
		if node.Status != RecoveryNodeRestored {
			t.Fatalf("node report = %+v", node)
		}
	}
}

func TestCoordinateTakodServiceReconcileReportsRestoreFailure(t *testing.T) {
	nodes := &fakeRecoveryNodes{
		reconcile:  map[string]error{"node-b": typedReconcileFailure(takod.ReconcileFailureBody{Error: "boom", FailurePolicy: takod.FailurePolicyRestore, RestoreError: "start failed"})},
		restoreErr: map[string]error{},
	}
	report, err := coordinateTakodServiceReconcile([]string{"node-a", "node-b"}, restorePlan(), nodes.operations())
	if err == nil || report == nil || report.Status != RecoveryFailed {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	var failed *RecoveryNodeReport
	for i := range report.Nodes {
		if report.Nodes[i].Server == "node-b" {
			failed = &report.Nodes[i]
		}
	}
	if failed == nil || failed.Status != RecoveryNodeFailed || failed.Error != "start failed" {
		t.Fatalf("node-b report = %+v", failed)
	}
}

func TestCoordinateTakodServiceReconcileReportsCrossNodeRestoreFailure(t *testing.T) {
	nodes := &fakeRecoveryNodes{
		reconcile:  map[string]error{"node-b": typedReconcileFailure(takod.ReconcileFailureBody{Error: "boom", FailurePolicy: takod.FailurePolicyRestore, Restored: true})},
		restoreErr: map[string]error{"node-a": fmt.Errorf("restore endpoint unreachable")},
	}
	report, _ := coordinateTakodServiceReconcile([]string{"node-a", "node-b"}, restorePlan(), nodes.operations())
	if report == nil || report.Status != RecoveryFailed || report.Nodes[0].Server != "node-a" || report.Nodes[0].Status != RecoveryNodeFailed {
		t.Fatalf("report = %+v", report)
	}
}

func TestCoordinateTakodServiceReconcileRejectedRequestLeavesNodeUnchanged(t *testing.T) {
	nodes := &fakeRecoveryNodes{reconcile: map[string]error{
		"node-b": fmt.Errorf("takod service reconciliation failed: %w", &takodclient.HTTPError{Method: "POST", Endpoint: "/v1/reconcile-service", Status: 400, Body: "invalid service name"}),
	}}
	report, _ := coordinateTakodServiceReconcile([]string{"node-a", "node-b"}, restorePlan(), nodes.operations())
	if report == nil || report.Status != RecoveryRestored || report.Nodes[1].Status != RecoveryNodeUnchanged {
		t.Fatalf("report = %+v", report)
	}
}

func TestCoordinateTakodServiceReconcileRestoresNodeThatFailedAfterReplacement(t *testing.T) {
	nodes := &fakeRecoveryNodes{}
	ops := nodes.operations()
	reconcile := ops.Reconcile
	ops.Reconcile = func(server string) (*takod.ReconcileServiceResponse, error) {
		response, _ := reconcile(server)
		if server == "node-a" {
			return response, fmt.Errorf("backup schedule sync failed")
		}
		return response, nil
	}
	report, err := coordinateTakodServiceReconcile([]string{"node-a", "node-b"}, restorePlan(), ops)
	if err == nil || report == nil || report.Status != RecoveryRestored {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if strings.Join(nodes.restored, ",") != "node-a,node-b" && strings.Join(nodes.restored, ",") != "node-b,node-a" {
		t.Fatalf("restored = %v", nodes.restored)
	}
}

func TestCoordinateTakodServiceReconcileUnsupportedAndRetainFailedReports(t *testing.T) {
	failure := fmt.Errorf("health check failed")
	nodes := &fakeRecoveryNodes{reconcile: map[string]error{"node-a": failure}, noToken: map[string]bool{"node-b": true}}
	report, _ := coordinateTakodServiceReconcile([]string{"node-a", "node-b"}, restorePlan().downgradeToLegacy(), nodes.operations())
	if report == nil || report.Status != RecoveryUnsupported || report.Mode != RecoveryModeLegacy || len(nodes.restored) != 0 {
		t.Fatalf("unsupported report = %+v restored=%v", report, nodes.restored)
	}

	retain := deriveTakodFailurePlan(&config.ServiceConfig{Deploy: config.DeployConfig{Strategy: config.DeployStrategyRecreate, RollbackOnFailure: boolPtr(false)}})
	nodes = &fakeRecoveryNodes{reconcile: map[string]error{
		"node-a": typedReconcileFailure(takod.ReconcileFailureBody{Error: "boom", FailurePolicy: takod.FailurePolicyRetainFailed, KeptContainers: []string{"tako_demo_production_web_1"}}),
	}, noToken: map[string]bool{"node-b": true}}
	report, err := coordinateTakodServiceReconcile([]string{"node-a", "node-b"}, retain, nodes.operations())
	if report == nil || report.Status != RecoveryDisabled || len(report.Nodes) != 1 || report.Nodes[0].Status != RecoveryNodeKept || report.Nodes[0].Containers[0] != "tako_demo_production_web_1" {
		t.Fatalf("retain-failed report = %+v", report)
	}
	if !strings.Contains(err.Error(), "kept stopped for debugging") {
		t.Fatalf("err = %v", err)
	}
}

func TestCoordinateTakodServiceReconcileRollingFailureNotCutOver(t *testing.T) {
	plan := deriveTakodFailurePlan(&config.ServiceConfig{Port: 80, Deploy: config.DeployConfig{Strategy: config.DeployStrategyRolling}})
	nodes := &fakeRecoveryNodes{reconcile: map[string]error{"node-a": fmt.Errorf("health check failed")}, noToken: map[string]bool{"node-b": true}}
	report, err := coordinateTakodServiceReconcile([]string{"node-a", "node-b"}, plan, nodes.operations())
	if err == nil || report == nil || report.Status != RecoveryRestored || report.Mode != RecoveryModeNotCutOver || len(nodes.restored) != 0 || len(nodes.finalized) != 0 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestParseTakodReconcileFailureLeavesUntypedErrorsAlone(t *testing.T) {
	plain := &takodclient.HTTPError{Status: 500, Body: "docker exploded"}
	if got := parseTakodReconcileFailure(plain); got != error(plain) {
		t.Fatalf("parse = %v", got)
	}
	other := fmt.Errorf("ssh closed")
	if got := parseTakodReconcileFailure(other); got != other {
		t.Fatalf("parse = %v", got)
	}
}

func TestDecodeReconcileServiceResponseToleratesLegacyBodies(t *testing.T) {
	if response := decodeReconcileServiceResponse(`{"project":"demo","containers":["a"]}`); response == nil || response.RestoreToken != "" {
		t.Fatalf("legacy response = %+v", response)
	}
	if response := decodeReconcileServiceResponse(`not json`); response == nil || response.RestoreToken != "" {
		t.Fatalf("garbage response = %+v", response)
	}
	if response := decodeReconcileServiceResponse(`{"restoreToken":"rev.0123456789abcdef"}`); response.RestoreToken != "rev.0123456789abcdef" {
		t.Fatalf("response = %+v", response)
	}
}

func TestRecoveryReportForRecordsPerService(t *testing.T) {
	d := &Deployer{}
	if d.RecoveryReportFor("web") != nil {
		t.Fatal("expected no report")
	}
	d.recordRecoveryReport("web", &RecoveryReport{Status: RecoveryRestored})
	if got := d.RecoveryReportFor("web"); got == nil || got.Status != RecoveryRestored {
		t.Fatalf("report = %+v", got)
	}
	d.recordRecoveryReport("web", nil)
	if d.RecoveryReportFor("web") != nil {
		t.Fatal("expected cleared report")
	}
}
