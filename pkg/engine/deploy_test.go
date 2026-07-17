package engine

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/scheduler"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takodstate"
)

type recordingRemover struct {
	removed []string
	fail    string
}

func (r *recordingRemover) RemoveServiceTakod(serviceName string) error {
	if r.fail == serviceName {
		return fmt.Errorf("boom on %s", serviceName)
	}
	r.removed = append(r.removed, serviceName)
	return nil
}

func TestApplyRemovalsEmitsOrderedEvents(t *testing.T) {
	sink := &events.BufferSink{}
	eng := New(Options{Sink: sink})
	remover := &recordingRemover{}
	plan := &reconcile.ReconciliationPlan{
		Changes: []reconcile.ServiceChange{
			{Type: reconcile.ChangeUpdate, ServiceName: "web"},
			{Type: reconcile.ChangeRemove, ServiceName: "worker"},
			{Type: reconcile.ChangeRemove, ServiceName: "cron"},
		},
	}

	if err := eng.ApplyRemovals(remover, plan); err != nil {
		t.Fatalf("ApplyRemovals returned error: %v", err)
	}
	if got := strings.Join(remover.removed, ","); got != "worker,cron" {
		t.Fatalf("removed = %q, want worker,cron", got)
	}

	emitted := sink.Events()
	wantTypes := []string{
		events.TypeDeployServiceStarted,
		events.TypeDeployServiceRemoved,
		events.TypeDeployServiceStarted,
		events.TypeDeployServiceRemoved,
	}
	if len(emitted) != len(wantTypes) {
		t.Fatalf("events = %d, want %d", len(emitted), len(wantTypes))
	}
	for i, want := range wantTypes {
		if emitted[i].Type != want {
			t.Fatalf("event[%d].Type = %q, want %q", i, emitted[i].Type, want)
		}
		if emitted[i].Seq != int64(i+1) {
			t.Fatalf("event[%d].Seq = %d, want %d", i, emitted[i].Seq, i+1)
		}
	}
	if emitted[0].Message != "→ Removing service: worker\n" {
		t.Fatalf("message = %q", emitted[0].Message)
	}
}

func TestApplyRemovalsStopsOnFailure(t *testing.T) {
	eng := New(Options{})
	remover := &recordingRemover{fail: "worker"}
	plan := &reconcile.ReconciliationPlan{
		Changes: []reconcile.ServiceChange{
			{Type: reconcile.ChangeRemove, ServiceName: "worker"},
			{Type: reconcile.ChangeRemove, ServiceName: "cron"},
		},
	}
	err := eng.ApplyRemovals(remover, plan)
	if err == nil || !strings.Contains(err.Error(), "remove failed for worker") {
		t.Fatalf("err = %v", err)
	}
	if len(remover.removed) != 0 {
		t.Fatalf("removed = %v, want none", remover.removed)
	}
}

func TestRegisterRegistrySecretsRedactsCredentials(t *testing.T) {
	sink := &events.BufferSink{}
	eng := New(Options{Sink: sink})
	eng.RegisterRegistrySecrets(&config.Config{
		Registries: map[string]config.RegistryConfig{
			"ghcr.io": {Username: "octocat", Password: "gh-registry-token"},
		},
	})

	eng.info(events.TypeLogLine, events.PhaseDeploy, "pull failed: login with gh-registry-token rejected\n")

	emitted := sink.Events()
	if len(emitted) != 1 {
		t.Fatalf("events = %d, want 1", len(emitted))
	}
	if strings.Contains(emitted[0].Message, "gh-registry-token") {
		t.Fatalf("event leaked registry credential: %q", emitted[0].Message)
	}
}

func TestRegisterACMEDNSSecretsRedactsProviderTokens(t *testing.T) {
	sink := &events.BufferSink{}
	eng := New(Options{Sink: sink})
	eng.RegisterACMEDNSSecrets(&config.Config{Environments: map[string]config.EnvironmentConfig{
		"production": {Proxy: &config.EnvironmentProxyConfig{ACME: &config.EnvironmentACMEConfig{
			DNSProvider: "cloudflare", Credentials: map[string]string{"apiToken": "dns-zone-token"},
		}}},
	}})
	eng.info(events.TypeCertIssueFailed, events.PhaseDomains, "provider rejected dns-zone-token\n")
	emitted := sink.Events()
	if len(emitted) != 1 || strings.Contains(emitted[0].Message, "dns-zone-token") {
		t.Fatalf("event leaked DNS provider credential: %+v", emitted)
	}
}

func TestEngineEventsRedactRegisteredSecrets(t *testing.T) {
	sink := &events.BufferSink{}
	eng := New(Options{Sink: sink})
	eng.RegisterSecret("hunter2-super-secret")

	eng.info(events.TypeLogLine, events.PhaseDeploy, "env value hunter2-super-secret leaked?\n")

	emitted := sink.Events()
	if len(emitted) != 1 {
		t.Fatalf("events = %d, want 1", len(emitted))
	}
	if strings.Contains(emitted[0].Message, "hunter2-super-secret") {
		t.Fatalf("event leaked registered secret: %q", emitted[0].Message)
	}
}

func TestEngineBuildOutputRedactsRegisteredSecrets(t *testing.T) {
	var output bytes.Buffer
	eng := New(Options{BuildOutput: &output})
	eng.RegisterSecret("shared-build-secret")
	writer := eng.buildOutputWriter()
	if _, err := writer.Write([]byte("build failed with shared-")); err != nil {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatalf("partial line was emitted before it could be safely redacted: %q", output.String())
	}
	if _, err := writer.Write([]byte("build-secret\n")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "shared-build-secret") {
		t.Fatalf("build output leaked secret: %q", output.String())
	}
}

type recordingPruner struct {
	pruned bool
}

func (p *recordingPruner) PruneTakodServiceRevisions(services map[string]config.ServiceConfig, keepRevisions map[string]string) error {
	p.pruned = true
	return nil
}

func TestPruneRevisionsAfterGraceSkipsWithoutRevisions(t *testing.T) {
	eng := New(Options{})
	pruner := &recordingPruner{}
	if err := eng.PruneRevisionsAfterGrace(pruner, nil, nil, func(time.Duration) {}); err != nil {
		t.Fatalf("PruneRevisionsAfterGrace returned error: %v", err)
	}
	if pruner.pruned {
		t.Fatal("pruned without keep revisions")
	}
}

func TestDeployPlanHashIgnoresHumanText(t *testing.T) {
	plan := DeployPlan{
		Project:     "demo",
		Environment: "production",
		Revision:    "abc123",
		Services:    []string{"web"},
	}
	withText := plan
	withText.HumanText = "pretty table"
	if plan.Hash() == "" {
		t.Fatal("hash is empty")
	}
	if plan.Hash() != withText.Hash() {
		t.Fatal("hash changed with human text")
	}
	changed := plan
	changed.Revision = "def456"
	if plan.Hash() == changed.Hash() {
		t.Fatal("hash did not change with revision")
	}
}

func TestPlanDeployRejectsInvalidRequests(t *testing.T) {
	eng := New(Options{})
	if _, err := eng.PlanDeploy(t.Context(), DeployRequest{}); Classify(err) != ClassInvalid {
		t.Fatalf("missing config: Classify = %d, want ClassInvalid (%v)", Classify(err), err)
	}
	if _, err := eng.PlanDeploy(t.Context(), DeployRequest{Config: &config.Config{}}); Classify(err) != ClassInvalid {
		t.Fatalf("missing environment: Classify = %d, want ClassInvalid (%v)", Classify(err), err)
	}
}

func TestDeployStartedStateRecordedAfterDependencyResolutionBeforeMutations(t *testing.T) {
	source, err := os.ReadFile("deploy.go")
	if err != nil {
		t.Fatalf("read deploy.go: %v", err)
	}
	body := string(source)
	recordCall := "RecordStartedDeploymentStateContext(ctx, s.stateManager, deployment)"
	if count := strings.Count(body, recordCall); count != 1 {
		t.Fatalf("started-state record call count = %d, want 1", count)
	}
	resolveIndex := strings.Index(body, "deploymentOrder, err := resolver.ResolveOrder()")
	recordIndex := strings.Index(body, recordCall)
	loopIndex := strings.Index(body, "for _, serviceName := range deploymentOrder")
	if resolveIndex < 0 || recordIndex < 0 || loopIndex < 0 {
		t.Fatalf("missing expected deploy Apply ordering markers: resolve=%d record=%d loop=%d", resolveIndex, recordIndex, loopIndex)
	}
	if !(resolveIndex < recordIndex && recordIndex < loopIndex) {
		t.Fatalf("started-state record ordering invalid: resolve=%d record=%d loop=%d", resolveIndex, recordIndex, loopIndex)
	}
}

func TestDeployPersistsPlacementIntentAndPreflightsBeforeWorkloadMutation(t *testing.T) {
	source, err := os.ReadFile("deploy.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(source)
	preflight := strings.Index(body, "s.deployer.PreflightAssignmentMutations(servicesToDeploy)")
	intent := strings.Index(body, "recorded stable placement before deploy mutation")
	build := strings.Index(body, "buildSharedImages(s.deployer")
	mutationLoop := strings.Index(body, "for _, serviceName := range deploymentOrder")
	if preflight < 0 || intent < 0 || build < 0 || mutationLoop < 0 || !(preflight < intent && intent < build && build < mutationLoop) {
		t.Fatalf("deploy placement ordering invalid: preflight=%d intent=%d build=%d loop=%d", preflight, intent, build, mutationLoop)
	}
}

func TestDeployRemovalIntentPrecedesCleanupAndFinalStateDropsPendingMarker(t *testing.T) {
	source, err := os.ReadFile("deploy.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(source)
	intent := strings.Index(body, "PersistTakodDesiredIntentWithPlacementBaseline")
	cleanup := strings.Index(body, "s.applyRemovals(plan)")
	finalState := strings.Index(body, "PersistTakodRuntimeStateWithPlacementBaseline(")
	if intent < 0 || cleanup < 0 || finalState < 0 || !(intent < cleanup && cleanup < finalState) {
		t.Fatalf("removal state ordering invalid: intent=%d cleanup=%d final=%d", intent, cleanup, finalState)
	}
}

func TestScalePersistsPlacementIntentBeforePartialMutationLoop(t *testing.T) {
	source, err := os.ReadFile("scale.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(source)
	intent := strings.Index(body, "recorded stable placement before scale mutation")
	prepare := strings.Index(body, "deploy.EnsurePreparedServiceImage")
	mutationLoop := strings.Index(body, "for serviceName, desiredReplicas := range scaleTargets")
	// The first loop creates desired config before persistence; require intent
	// before image transfer and the later loop that calls DeployServiceTakod.
	deployCall := strings.Index(body, "deploy.DeployServiceTakod")
	if intent < 0 || prepare < 0 || mutationLoop < 0 || deployCall < 0 || !(intent < prepare && intent < deployCall) {
		t.Fatalf("scale placement ordering invalid: intent=%d prepare=%d loop=%d deploy=%d", intent, prepare, mutationLoop, deployCall)
	}
}

func TestRejectPersistentConfigRemovalPreservesAuthoritativePlacement(t *testing.T) {
	plan := &reconcile.ReconciliationPlan{Changes: []reconcile.ServiceChange{{
		Type: reconcile.ChangeNone, ServiceName: "database", OldConfig: &config.ServiceConfig{Persistent: true}, NewConfig: nil,
	}}}
	err := rejectPersistentConfigRemovals(plan)
	if err == nil || !strings.Contains(err.Error(), "restore it to tako.yaml") {
		t.Fatalf("persistent config removal error = %v", err)
	}
}

func TestRemovalServiceConfigsRetainsPriorConfiguration(t *testing.T) {
	old := config.ServiceConfig{Image: "nginx:1.27", Replicas: 2}
	plan := &reconcile.ReconciliationPlan{Changes: []reconcile.ServiceChange{
		{Type: reconcile.ChangeNone, ServiceName: "kept"},
		{Type: reconcile.ChangeRemove, ServiceName: "old-web", OldConfig: &old},
	}}
	pending, err := removalServiceConfigs(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending["old-web"].Image != old.Image || pending["old-web"].Replicas != 2 {
		t.Fatalf("pending removals = %#v", pending)
	}
}

func TestRemovalServiceConfigsFailsClosedWithoutPriorConfiguration(t *testing.T) {
	plan := &reconcile.ReconciliationPlan{Changes: []reconcile.ServiceChange{{
		Type: reconcile.ChangeRemove, ServiceName: "orphan",
	}}}
	_, err := removalServiceConfigs(plan)
	if err == nil || !strings.Contains(err.Error(), "prior configuration is unavailable") {
		t.Fatalf("missing prior configuration error = %v", err)
	}
}

func TestPreservedDesiredServicesCarriesUnrelatedRemovalPendingRecord(t *testing.T) {
	prior := &takodstate.DesiredRevision{Services: map[string]takodstate.DesiredService{
		"api": {Name: "api"},
		"old-worker": {
			Name:           "old-worker",
			RemovalPending: true,
			Assignments:    []scheduler.Assignment{{Slot: 1, Node: "node-a", NodeID: "id-a"}},
		},
	}}
	preserved := preservedDesiredServices(prior, map[string]config.ServiceConfig{"api": {Image: "api:2"}}, nil)
	if len(preserved) != 1 || !preserved["old-worker"].RemovalPending || len(preserved["old-worker"].Assignments) != 1 {
		t.Fatalf("preserved desired services = %#v", preserved)
	}
}

func TestValidatePriorDesiredServicesRejectsUnrelatedPersistentConfigRemoval(t *testing.T) {
	prior := &takodstate.DesiredRevision{Services: map[string]takodstate.DesiredService{
		"database": {Name: "database", Persistent: true},
	}}
	err := ValidatePriorDesiredServices(prior, map[string]config.ServiceConfig{"api": {Image: "api:2"}})
	if err == nil || !strings.Contains(err.Error(), "restore it before running any workload operation") {
		t.Fatalf("persistent baseline validation error = %v", err)
	}
}

func TestNarrowWorkflowsPreservePlacementBaselineBeforeAndAfterMutation(t *testing.T) {
	for _, file := range []string{"scale.go", "promote.go", "rollback.go", "run.go"} {
		t.Run(file, func(t *testing.T) {
			source, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			body := string(source)
			if !strings.Contains(body, "PersistTakodDesiredIntentWithPlacementBaseline") {
				t.Fatalf("%s does not preserve prior desired state in pre-mutation intent", file)
			}
			if !strings.Contains(body, "PersistTakodRuntimeStateWithPlacementBaseline") {
				t.Fatalf("%s does not preserve prior desired state in final runtime state", file)
			}
		})
	}
}
