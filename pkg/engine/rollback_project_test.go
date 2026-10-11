package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

func TestSingleServiceRollbackResultJSONAddsOnlyScope(t *testing.T) {
	result := RollbackResult{
		APIVersion:   takoapi.APIVersionCurrent,
		Kind:         KindRollbackResult,
		Project:      "demo",
		Environment:  "production",
		Scope:        RollbackScopeService,
		Service:      "web",
		DeploymentID: "1767225600000_1",
		Version:      "1.2.3",
		Status:       takoapi.StatusRolledBack,
		StartedAt:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Duration:     1.5,
		Message:      "rolled back web to deployment 1767225600000_1",
	}
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"apiVersion":"` + takoapi.APIVersionCurrent + `","kind":"RollbackResult","project":"demo","environment":"production","scope":"service","service":"web","deploymentId":"1767225600000_1","version":"1.2.3","status":"rolled_back","startedAt":"2026-01-01T00:00:00Z","durationSeconds":1.5,"message":"rolled back web to deployment 1767225600000_1"}`
	if string(payload) != want {
		t.Fatalf("single-service result JSON =\n%s\nwant\n%s", payload, want)
	}

	project := result
	project.Scope = RollbackScopeProject
	project.Service = ""
	project.Services = []RollbackServiceOutcome{{Name: "api", Action: RollbackActionFailed, Recovery: &RecoveryOutcome{Status: "restored"}}}
	payload, err = json.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["service"]; ok {
		t.Fatalf("project result must omit service: %s", payload)
	}
	services := decoded["services"].([]any)
	if recovery := services[0].(map[string]any)["recovery"].(map[string]any); recovery["status"] != "restored" {
		t.Fatalf("recovery not carried: %s", payload)
	}
}

func sampleRollbackPlan() RollbackPlan {
	plan := RollbackPlan{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindRollbackPlan,
		Project:     "demo",
		Environment: "production",
		Target:      RollbackPlanTarget{ID: "D2", DisplayID: "D2", Timestamp: rollbackTestBase},
		Services: []RollbackServiceOutcome{
			{Name: "api", Action: RollbackActionRolledBack, FromImage: "api:D3", ToImage: "api:D2", Replicas: 2, SourceDeploymentID: "D2", ImageSource: RollbackImageRetained},
			{Name: "worker", Action: RollbackActionUnchanged, Reason: "already running the target state"},
		},
		Warnings:        []string{"job nightly is not rolled back and keeps running the newer image and schedule"},
		ReleaseCommands: RollbackReleaseSkip,
	}
	plan.Hash = plan.PlanHash()
	return plan
}

func TestRollbackPlanHash(t *testing.T) {
	base := sampleRollbackPlan()
	if base.Hash == "" {
		t.Fatal("plan hash is empty")
	}
	same := sampleRollbackPlan()
	same.Warnings = []string{"different wording"}
	same.Target.Timestamp = time.Now()
	same.Services[0].FromImage = "api:other"
	same.Services[1].Reason = "other reason"
	if same.PlanHash() != base.Hash {
		t.Fatal("display-only fields changed the plan hash")
	}
	mutations := map[string]func(*RollbackPlan){
		"toImage":     func(p *RollbackPlan) { p.Services[0].ToImage = "api:D1" },
		"target":      func(p *RollbackPlan) { p.Target.ID = "D1" },
		"imageSource": func(p *RollbackPlan) { p.Services[0].ImageSource = RollbackImageRebuild },
		"replicas":    func(p *RollbackPlan) { p.Services[0].Replicas = 3 },
		"action":      func(p *RollbackPlan) { p.Services[1].Action = RollbackActionRolledBack },
		"release":     func(p *RollbackPlan) { p.ReleaseCommands = RollbackReleaseRun },
		"persistent":  func(p *RollbackPlan) { p.IncludePersistent = true },
		"environment": func(p *RollbackPlan) { p.Environment = "staging" },
	}
	for name, mutate := range mutations {
		changed := sampleRollbackPlan()
		mutate(&changed)
		if changed.PlanHash() == base.Hash {
			t.Fatalf("changing %s did not change the plan hash", name)
		}
	}
}

func TestNewRollbackPlanDocumentWarnsAboutJobsAndPersistent(t *testing.T) {
	cfg := &config.Config{Project: config.ProjectConfig{Name: "demo"}}
	services := map[string]config.ServiceConfig{
		"web":      {Image: "web"},
		"nightly":  {Kind: config.ServiceKindJob, Image: "nightly"},
		"postgres": {Image: "postgres", Persistent: true},
	}
	target := rollbackRecord("1767225600000_1", remotestate.StatusSuccess, 0, "web")
	target.GitCommitShort = "abc1234"
	outcomes := []RollbackServiceOutcome{
		{Name: "nightly", Action: RollbackActionSkipped, Reason: RollbackReasonKindJob},
		{Name: "postgres", Action: RollbackActionRolledBack, ToImage: "postgres:15"},
		{Name: "web", Action: RollbackActionRolledBack, ToImage: "web:1"},
	}
	plan := newRollbackPlanDocument(cfg, "production", []string{"node-a"}, target, outcomes, services, RollbackRequest{IncludePersistent: true, RunRelease: true})
	if plan.Kind != KindRollbackPlan || plan.Target.DisplayID != "1767225600" || plan.Target.Commit != "abc1234" {
		t.Fatalf("plan identity = %#v", plan)
	}
	if plan.ReleaseCommands != RollbackReleaseRun || !plan.IncludePersistent {
		t.Fatalf("plan flags = %#v", plan)
	}
	joined := strings.Join(plan.Warnings, "\n")
	if !strings.Contains(joined, "job nightly is not rolled back") || !strings.Contains(joined, "persistent service postgres") {
		t.Fatalf("warnings = %q", plan.Warnings)
	}
	if plan.Hash == "" || plan.Hash != plan.PlanHash() {
		t.Fatalf("plan hash = %q", plan.Hash)
	}
	if got := plan.RolledBackServices(); strings.Join(got, ",") != "postgres,web" {
		t.Fatalf("rolled back services = %v", got)
	}
}

func TestPreflightRollbackImagesChoosesSources(t *testing.T) {
	records := map[string]*remotestate.DeploymentState{
		"D1": {ID: "D1", GitCommit: "c0ffee0000000000000000000000000000000001"},
		"D2": {ID: "D2"},
	}
	services := map[string]config.ServiceConfig{
		"retained": {Build: "."},
		"rebuild":  {Build: "."},
		"registry": {Image: "nginx:1"},
		"shared":   {ImageFrom: "app"},
		"skipped":  {Image: "x"},
	}
	targets := map[string]remotestate.ServiceState{
		"retained": {Image: "demo/retained:D1"},
		"rebuild":  {Image: "demo/rebuild:D1"},
		"registry": {Image: "nginx:1"},
		"shared":   {Image: "demo/shared/app:D1", SharedBuildHash: "h", SharedBuild: "app"},
	}
	outcomes := []RollbackServiceOutcome{
		{Name: "rebuild", Action: RollbackActionRolledBack, SourceDeploymentID: "D1"},
		{Name: "registry", Action: RollbackActionRolledBack, SourceDeploymentID: "D2"},
		{Name: "retained", Action: RollbackActionRolledBack, SourceDeploymentID: "D2"},
		{Name: "shared", Action: RollbackActionRolledBack, SourceDeploymentID: "D1"},
		{Name: "skipped", Action: RollbackActionSkipped},
	}
	var located []string
	probe := rollbackImageProbe{
		LocateRetained: func(name string, state remotestate.ServiceState) (bool, error) {
			located = append(located, name)
			return name == "retained" || name == "shared", nil
		},
		CommitAvailable: func(commit string) bool { return commit == records["D1"].GitCommit },
	}
	unavailable, err := preflightRollbackImages(outcomes, services, targets, records, probe)
	if err != nil || len(unavailable) != 0 {
		t.Fatalf("preflight = %v, %v", unavailable, err)
	}
	want := map[string]string{"rebuild": RollbackImageRebuild, "registry": RollbackImagePull, "retained": RollbackImageRetained, "shared": RollbackImageRetained, "skipped": ""}
	for _, outcome := range outcomes {
		if outcome.ImageSource != want[outcome.Name] {
			t.Fatalf("%s image source = %q, want %q", outcome.Name, outcome.ImageSource, want[outcome.Name])
		}
	}
	if strings.Join(located, ",") != "rebuild,registry,retained,shared" {
		t.Fatalf("located = %v (skipped services must not be probed)", located)
	}
}

func TestPreflightRollbackImagesReportsUnavailableImages(t *testing.T) {
	records := map[string]*remotestate.DeploymentState{"D1": {ID: "D1", GitCommit: "deadbeef00000000000000000000000000000001"}}
	services := map[string]config.ServiceConfig{"web": {Image: "web"}, "api": {Build: "."}, "shared": {ImageFrom: "app"}}
	targets := map[string]remotestate.ServiceState{
		"web":    {Image: "web:1"},
		"api":    {Image: "demo/api:D1"},
		"shared": {Image: "demo/shared/app:D1", SharedBuildHash: "h"},
	}
	outcomes := []RollbackServiceOutcome{
		{Name: "api", Action: RollbackActionRolledBack, SourceDeploymentID: "D1"},
		{Name: "shared", Action: RollbackActionRolledBack, SourceDeploymentID: "D1"},
		{Name: "web", Action: RollbackActionRolledBack, SourceDeploymentID: "D1"},
	}
	unavailable, err := preflightRollbackImages(outcomes, services, targets, records, rollbackImageProbe{
		LocateRetained:  func(string, remotestate.ServiceState) (bool, error) { return false, nil },
		CommitAvailable: func(string) bool { return false },
	})
	if err != nil {
		t.Fatalf("preflight error = %v", err)
	}
	joined := strings.Join(unavailable, "; ")
	if len(unavailable) != 2 || !strings.Contains(joined, "api (demo/api:D1") || !strings.Contains(joined, "not in the local repository") || !strings.Contains(joined, "shared (demo/shared/app:D1") {
		t.Fatalf("unavailable = %q", unavailable)
	}

	// A node without image descriptor support fails closed (exit 2).
	capabilityErr := &takodclient.CapabilityRequiredError{Server: "node-a", Capability: "image.descriptor-v1", Feature: "exact image reuse"}
	_, err = preflightRollbackImages(outcomes, services, targets, records, rollbackImageProbe{
		LocateRetained:  func(string, remotestate.ServiceState) (bool, error) { return false, capabilityErr },
		CommitAvailable: func(string) bool { return true },
	})
	if !errors.Is(err, capabilityErr) || Classify(err) != ClassInvalid {
		t.Fatalf("capability error = %v (class %d)", err, Classify(err))
	}
}

func TestUnavailableResultMarksPlannedServicesNotAttempted(t *testing.T) {
	session := &RollbackSession{
		engine:  New(Options{}),
		cfg:     &config.Config{Project: config.ProjectConfig{Name: "demo"}},
		envName: "production",
		target:  &remotestate.DeploymentState{ID: "D2"},
		planDoc: sampleRollbackPlan(),
	}
	result := session.unavailableResult(errors.New("no image source for api"))
	if result.Status != takoapi.StatusFailed || result.Scope != RollbackScopeProject || result.Error == "" {
		t.Fatalf("result = %#v", result)
	}
	if result.Services[0].Action != RollbackActionNotAttempted || result.Services[1].Action != RollbackActionUnchanged {
		t.Fatalf("services = %#v", result.Services)
	}
}

func TestRunRollbackSequenceStopsAtFirstFailure(t *testing.T) {
	sink := &events.BufferSink{}
	e := New(Options{Sink: sink})
	defer e.stream.SetContextData(map[string]any{"operation": "rollback", "targetDeploymentId": "D2"})()
	outcomes := []RollbackServiceOutcome{
		{Name: "api", Action: RollbackActionRolledBack, ToImage: "api:D2"},
		{Name: "web", Action: RollbackActionRolledBack, ToImage: "web:D2"},
		{Name: "worker", Action: RollbackActionRolledBack, ToImage: "worker:D2"},
		{Name: "cache", Action: RollbackActionUnchanged},
	}
	var called []string
	rolled, err := e.runRollbackSequence(context.Background(), []string{"cache", "api", "web", "worker"}, outcomes, func(_ context.Context, outcome RollbackServiceOutcome) (*RecoveryOutcome, error) {
		called = append(called, outcome.Name)
		if outcome.Name == "web" {
			return &RecoveryOutcome{Status: "restored", Mode: "restore"}, errors.New("health check failed")
		}
		return nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), "rollback of web failed") || Classify(err) != ClassFailed {
		t.Fatalf("sequence error = %v", err)
	}
	if strings.Join(called, ",") != "api,web" || strings.Join(rolled, ",") != "api" {
		t.Fatalf("called = %v, rolled = %v", called, rolled)
	}
	want := map[string]string{"api": RollbackActionRolledBack, "web": RollbackActionFailed, "worker": RollbackActionNotAttempted, "cache": RollbackActionUnchanged}
	for _, outcome := range outcomes {
		if outcome.Action != want[outcome.Name] {
			t.Fatalf("%s action = %s, want %s", outcome.Name, outcome.Action, want[outcome.Name])
		}
	}
	if outcomes[1].Error == "" || outcomes[1].Recovery == nil || outcomes[1].Recovery.Status != "restored" {
		t.Fatalf("failed outcome = %#v", outcomes[1])
	}
	var types []string
	for _, event := range sink.Events() {
		types = append(types, event.Type)
		if event.Data["operation"] != "rollback" || event.Data["targetDeploymentId"] != "D2" {
			t.Fatalf("event %s data = %#v", event.Type, event.Data)
		}
		if strings.HasPrefix(event.Type, "deploy.release.") {
			t.Fatalf("unexpected release event %s", event.Type)
		}
	}
	got := strings.Join(types, ",")
	wantTypes := "deploy.service.started,deploy.service.reconciled,deploy.service.started,deploy.service.failed,deploy.service.restore.started,deploy.service.restored"
	if got != wantTypes {
		t.Fatalf("event types = %s, want %s", got, wantTypes)
	}
}

func TestRollbackApplyUpToDateMutatesNothing(t *testing.T) {
	sink := &events.BufferSink{}
	e := New(Options{Sink: sink})
	plan := RollbackPlan{
		Kind:     KindRollbackPlan,
		Target:   RollbackPlanTarget{ID: "D2"},
		Services: []RollbackServiceOutcome{{Name: "web", Action: RollbackActionUnchanged, Reason: "already running the target state"}, {Name: "nightly", Action: RollbackActionSkipped, Reason: RollbackReasonKindJob}},
	}
	var hookInfo *MutationInfo
	session := &RollbackSession{
		engine:  e,
		cfg:     &config.Config{Project: config.ProjectConfig{Name: "demo"}},
		envName: "production",
		target:  &remotestate.DeploymentState{ID: "D2"},
		planDoc: plan,
		req: RollbackRequest{BeforeMutation: func(_ context.Context, info MutationInfo) error {
			hookInfo = &info
			return nil
		}},
	}
	restore := e.stream.SetContextData(map[string]any{"operation": "rollback", "targetDeploymentId": "D2"})
	defer restore()
	// stateManager, deployer and sshPool are nil: any history write or
	// runtime mutation would panic.
	result, err := session.Apply(context.Background())
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if result.Status != RollbackStatusUpToDate || result.Scope != RollbackScopeProject || result.DeploymentID != "D2" {
		t.Fatalf("result = %#v", result)
	}
	if hookInfo == nil || len(hookInfo.Services) != 0 || hookInfo.TargetDeploymentID != "D2" {
		t.Fatalf("BeforeMutation info = %#v", hookInfo)
	}
	skipped := 0
	for _, event := range sink.Events() {
		if event.Type == events.TypeDeployServiceSkipped {
			skipped++
			if event.Data["action"] == "" || event.Data["reason"] == "" || event.Data["operation"] != "rollback" {
				t.Fatalf("skipped event data = %#v", event.Data)
			}
		}
	}
	if skipped != 2 {
		t.Fatalf("skipped events = %d, want 2", skipped)
	}
	if _, err := session.Apply(context.Background()); err == nil {
		t.Fatal("second Apply must fail")
	}
}

func TestRollbackApplyBeforeMutationErrorAborts(t *testing.T) {
	session := &RollbackSession{
		engine:  New(Options{}),
		cfg:     &config.Config{Project: config.ProjectConfig{Name: "demo"}},
		envName: "production",
		target:  &remotestate.DeploymentState{ID: "D2"},
		planDoc: sampleRollbackPlan(),
		req: RollbackRequest{BeforeMutation: func(context.Context, MutationInfo) error {
			return errors.New("pre-rollback hook failed")
		}},
	}
	if _, err := session.Apply(context.Background()); err == nil || err.Error() != "pre-rollback hook failed" {
		t.Fatalf("Apply() error = %v", err)
	}
}

func TestBuildRollbackDeploymentForServicesRecordsOnlyRolledBackServices(t *testing.T) {
	cfg := &config.Config{Project: config.ProjectConfig{Name: "demo"}}
	target := rollbackRecord("D2", remotestate.StatusSuccess, 0, "api", "web")
	target.GitCommit = "abc"
	rolled := map[string]remotestate.ServiceState{"api": target.Services["api"], "web": target.Services["web"]}
	record := BuildRollbackDeploymentForServices(cfg, "production", "node-a", rollbackTestBase, time.Second, target, rolled, projectRollbackMessage("D2", 2), "v1", "c1")
	if record.Status != remotestate.StatusRolledBack || len(record.Services) != 2 || record.GitCommit != "abc" {
		t.Fatalf("record = %#v", record)
	}
	if record.Message != "rolled back project to deployment D2 (2 services)" {
		t.Fatalf("message = %q", record.Message)
	}
	for name, state := range record.Services {
		if state.Kind == config.ServiceKindRun || state.Run != nil {
			t.Fatalf("service %s recorded as a run entry", name)
		}
	}
	rolled["extra"] = remotestate.ServiceState{}
	if _, ok := record.Services["extra"]; ok {
		t.Fatal("record aliases the caller's services map")
	}
}

func TestRollbackProxyInputsForServicesRollsBackEveryService(t *testing.T) {
	cfg := &config.Config{Project: config.ProjectConfig{Name: "demo"}}
	services := map[string]config.ServiceConfig{
		"api":    {Image: "api:current", Port: 4000, Replicas: 3},
		"web":    {Build: ".", Port: 3000, Replicas: 2},
		"worker": {Image: "worker:current"},
	}
	rolled := map[string]remotestate.ServiceState{
		"api": {Image: "api:old", Replicas: 1},
		"web": {Image: "demo/web:old", Replicas: 1, Port: 8080},
	}
	desired, refs, _ := RollbackProxyInputsForServices(cfg, "production", services, rolled, nil)
	if desired["api"].Image != "api:old" || desired["api"].Replicas != 1 || desired["web"].Image != "demo/web:old" || desired["web"].Port != 8080 {
		t.Fatalf("desired = %#v", desired)
	}
	if desired["worker"].Image != "worker:current" || len(refs) != 2 {
		t.Fatalf("worker or refs changed: %#v %#v", desired["worker"], refs)
	}
	if services["api"].Image != "api:current" {
		t.Fatal("input services were mutated")
	}
}
