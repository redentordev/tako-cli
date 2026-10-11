package engine

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/reconcile"
)

var rollbackTestBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// testListDeployments mirrors cmd's lister: newest first, failed included.
func testListDeployments(history *remotestate.DeploymentHistory, opts *remotestate.HistoryOptions) []*remotestate.DeploymentState {
	if history == nil {
		return nil
	}
	var out []*remotestate.DeploymentState
	for _, deployment := range history.Deployments {
		if deployment == nil {
			continue
		}
		if opts != nil && !opts.IncludeFailed && deployment.Status == remotestate.StatusFailed {
			continue
		}
		out = append(out, deployment)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.After(out[j].Timestamp) })
	return out
}

func rollbackRecord(id string, status remotestate.DeploymentStatus, offset time.Duration, services ...string) *remotestate.DeploymentState {
	states := make(map[string]remotestate.ServiceState, len(services))
	for _, name := range services {
		states[name] = remotestate.ServiceState{Name: name, Image: name + ":" + id, Replicas: 1}
	}
	return &remotestate.DeploymentState{ID: id, Status: status, Timestamp: rollbackTestBase.Add(offset), Services: states}
}

func rollbackHistory(records ...*remotestate.DeploymentState) *remotestate.DeploymentHistory {
	return &remotestate.DeploymentHistory{Deployments: records}
}

func TestSelectProjectRollbackTarget(t *testing.T) {
	commitRecord := rollbackRecord("1767225900_11", remotestate.StatusSuccess, 3*time.Hour, "web")
	commitRecord.GitCommit = "abc1234def5678900000000000000000000000ab"
	commitRecord.GitCommitShort = "abc1234"
	olderCommit := rollbackRecord("1767225800_10", remotestate.StatusSuccess, 2*time.Hour, "web")
	olderCommit.GitCommit = commitRecord.GitCommit
	tests := []struct {
		name     string
		history  *remotestate.DeploymentHistory
		argument string
		want     string
		wantErr  string
	}{
		{
			name:    "success then success targets the previous deployment",
			history: rollbackHistory(rollbackRecord("D1", remotestate.StatusSuccess, 0, "web"), rollbackRecord("D2", remotestate.StatusSuccess, time.Hour, "web")),
			want:    "D1",
		},
		{
			name:    "failed head targets the newest stable record before it",
			history: rollbackHistory(rollbackRecord("D1", remotestate.StatusSuccess, 0, "web"), rollbackRecord("D2", remotestate.StatusSuccess, time.Hour, "web"), rollbackRecord("D3", remotestate.StatusFailed, 2*time.Hour, "web")),
			want:    "D2",
		},
		{
			name:    "rolled_back head toggles back to the deployment rolled away from",
			history: rollbackHistory(rollbackRecord("D1", remotestate.StatusSuccess, 0, "web"), rollbackRecord("D2", remotestate.StatusSuccess, time.Hour, "web"), rollbackRecord("R", remotestate.StatusRolledBack, 2*time.Hour, "web")),
			want:    "D2",
		},
		{
			name:    "single record has no previous stable deployment",
			history: rollbackHistory(rollbackRecord("D1", remotestate.StatusSuccess, 0, "web")),
			wantErr: "no previous stable deployment",
		},
		{
			name:     "display ID prefix resolves a unique deployment",
			history:  rollbackHistory(rollbackRecord("1767225600123456789_42", remotestate.StatusSuccess, 0, "web"), rollbackRecord("1767229200123456789_42", remotestate.StatusSuccess, time.Hour, "web")),
			argument: "1767225600",
			want:     "1767225600123456789_42",
		},
		{
			name:     "ambiguous prefix lists the candidates",
			history:  rollbackHistory(rollbackRecord("1767225600111_1", remotestate.StatusSuccess, 0, "web"), rollbackRecord("1767225600222_2", remotestate.StatusSuccess, time.Hour, "web")),
			argument: "1767225600",
			wantErr:  "1767225600222_2, 1767225600111_1",
		},
		{
			name:     "git commit prefix picks the newest stable match",
			history:  rollbackHistory(olderCommit, commitRecord, rollbackRecord("1767226000_12", remotestate.StatusSuccess, 4*time.Hour, "web")),
			argument: "abc1234",
			want:     "1767225900_11",
		},
		{
			name:     "failed explicit target is rejected",
			history:  rollbackHistory(rollbackRecord("D1", remotestate.StatusSuccess, 0, "web"), rollbackRecord("D2", remotestate.StatusFailed, time.Hour, "web")),
			argument: "D2",
			wantErr:  "not a stable rollback target",
		},
		{
			name:     "unknown target is rejected",
			history:  rollbackHistory(rollbackRecord("D1", remotestate.StatusSuccess, 0, "web")),
			argument: "zzz",
			wantErr:  "not found",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := SelectProjectRollbackTarget(test.history, test.argument, testListDeployments)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want %q", err, test.wantErr)
				}
				if Classify(err) != ClassInvalid {
					t.Fatalf("error class = %d, want ClassInvalid", Classify(err))
				}
				return
			}
			if err != nil {
				t.Fatalf("SelectProjectRollbackTarget() error = %v", err)
			}
			if got.ID != test.want {
				t.Fatalf("target = %s, want %s", got.ID, test.want)
			}
		})
	}
}

func TestSelectRollbackTargetFromHistoryAcceptsPrefixAndCommit(t *testing.T) {
	webOld := rollbackRecord("1767225600000_1", remotestate.StatusSuccess, 0, "web")
	webOld.GitCommit = "feedface00000000000000000000000000000001"
	apiOnly := rollbackRecord("1767229200000_2", remotestate.StatusSuccess, time.Hour, "api")
	apiOnly.GitCommit = webOld.GitCommit
	history := rollbackHistory(webOld, apiOnly, rollbackRecord("1767232800000_3", remotestate.StatusSuccess, 2*time.Hour, "web"))

	got, err := SelectRollbackTargetFromHistory(history, "1767225600", "web", testListDeployments)
	if err != nil || got.ID != webOld.ID {
		t.Fatalf("prefix target = %v, %v; want %s", got, err, webOld.ID)
	}
	// The newest record with the commit lacks web, so the commit resolves to
	// the newest stable record that contains the service.
	got, err = SelectRollbackTargetFromHistory(history, "feedface", "web", testListDeployments)
	if err != nil || got.ID != webOld.ID {
		t.Fatalf("commit target = %v, %v; want %s", got, err, webOld.ID)
	}
	got, err = SelectRollbackTargetFromHistory(history, webOld.ID, "web", testListDeployments)
	if err != nil || got.ID != webOld.ID {
		t.Fatalf("exact target = %v, %v", got, err)
	}
	if _, err := SelectRollbackTargetFromHistory(history, "nope", "web", testListDeployments); err == nil || !strings.Contains(err.Error(), "failed to find deployment nope") {
		t.Fatalf("unknown target error = %v", err)
	}
}

func planByName(plans []RollbackServicePlan) map[string]RollbackServicePlan {
	out := make(map[string]RollbackServicePlan, len(plans))
	for _, plan := range plans {
		out[plan.Name] = plan
	}
	return out
}

func TestReconstructServiceTargetsUsesNewestStableRecordAtOrBeforeTarget(t *testing.T) {
	d1 := rollbackRecord("D1", remotestate.StatusSuccess, 0, "web", "worker")
	d2 := rollbackRecord("D2", remotestate.StatusSuccess, time.Hour, "web")
	d3 := rollbackRecord("D3", remotestate.StatusSuccess, 2*time.Hour, "worker", "search")
	failed := rollbackRecord("F", remotestate.StatusFailed, 30*time.Minute, "worker")
	history := rollbackHistory(d1, failed, d2, d3)
	services := map[string]config.ServiceConfig{
		"web":      {Image: "web"},
		"worker":   {Image: "worker"},
		"search":   {Image: "search"},
		"migrate":  {Kind: config.ServiceKindRun, Image: "migrate"},
		"nightly":  {Kind: config.ServiceKindJob, Image: "nightly"},
		"postgres": {Image: "postgres:16", Persistent: true},
		"cache":    {Image: "redis"},
	}
	plans := planByName(ReconstructServiceTargets(history, d2, services, false))

	if plan := plans["web"]; plan.Action != RollbackActionRolledBack || plan.State.Image != "web:D2" || plan.SourceDeploymentID != "D2" {
		t.Fatalf("web plan = %#v", plan)
	}
	if plan := plans["worker"]; plan.Action != RollbackActionRolledBack || plan.State.Image != "worker:D1" || plan.SourceDeploymentID != "D1" {
		t.Fatalf("worker plan = %#v (failed records must be ignored)", plan)
	}
	if plan := plans["search"]; plan.Action != RollbackActionSkipped || plan.Reason != RollbackReasonNotPresent {
		t.Fatalf("search plan = %#v", plan)
	}
	if plan := plans["migrate"]; plan.Action != RollbackActionSkipped || plan.Reason != RollbackReasonKindRun {
		t.Fatalf("migrate plan = %#v", plan)
	}
	if plan := plans["nightly"]; plan.Action != RollbackActionSkipped || plan.Reason != RollbackReasonKindJob {
		t.Fatalf("nightly plan = %#v", plan)
	}
	if plan := plans["postgres"]; plan.Action != RollbackActionSkipped || plan.Reason != RollbackReasonPersistent {
		t.Fatalf("postgres plan = %#v", plan)
	}
	if plan := plans["cache"]; plan.Action != RollbackActionUnchanged {
		t.Fatalf("cache plan = %#v, want unchanged (absent from history)", plan)
	}
	if err := unresolvableRollbackError(ReconstructServiceTargets(history, d2, services, false)); err != nil {
		t.Fatalf("unresolvable error = %v", err)
	}

	withPersistent := planByName(ReconstructServiceTargets(rollbackHistory(rollbackRecord("P1", remotestate.StatusSuccess, 0, "postgres"), rollbackRecord("P2", remotestate.StatusSuccess, time.Hour, "postgres")), &remotestate.DeploymentState{ID: "P1", Timestamp: rollbackTestBase}, services, true))
	if plan := withPersistent["postgres"]; plan.Action != RollbackActionRolledBack || plan.State.Image != "postgres:P1" {
		t.Fatalf("included persistent plan = %#v", plan)
	}
}

func TestReconstructServiceTargetsFailsClosedWhenHistoryIsPruned(t *testing.T) {
	var records []*remotestate.DeploymentState
	for index := 0; index < remotestate.MaxHistoryEntries; index++ {
		records = append(records, rollbackRecord(fmt.Sprintf("D%02d", index), remotestate.StatusSuccess, time.Duration(index)*time.Hour, "web"))
	}
	// api appears only in records newer than the target.
	records[len(records)-1].Services["api"] = remotestate.ServiceState{Name: "api", Image: "api:new", Replicas: 1}
	target := records[len(records)-2]
	services := map[string]config.ServiceConfig{"web": {Image: "web"}, "api": {Image: "api"}}

	plans := ReconstructServiceTargets(rollbackHistory(records...), target, services, false)
	err := unresolvableRollbackError(plans)
	if err == nil || !strings.Contains(err.Error(), "api") || !strings.Contains(err.Error(), "tako rollback --service") {
		t.Fatalf("pruned error = %v", err)
	}
	if Classify(err) != ClassInvalid {
		t.Fatalf("pruned error class = %d, want ClassInvalid", Classify(err))
	}

	// Below the retention limit the same shape means api was added later.
	notPruned := planByName(ReconstructServiceTargets(rollbackHistory(records[1:]...), target, services, false))
	if plan := notPruned["api"]; plan.Action != RollbackActionSkipped || plan.Reason != RollbackReasonNotPresent {
		t.Fatalf("not pruned api plan = %#v", plan)
	}
}

func TestRollbackServiceUnchanged(t *testing.T) {
	target := remotestate.ServiceState{Image: "web:1", Replicas: 2, FilesContentHash: "files-a"}
	current := &remotestate.ServiceState{Image: "web:1", Replicas: 2, FilesContentHash: "files-a"}
	tests := []struct {
		name    string
		actual  *reconcile.ActualService
		current *remotestate.ServiceState
		want    bool
	}{
		{name: "equal", actual: &reconcile.ActualService{Image: "web:1", Replicas: 2}, current: current, want: true},
		{name: "image differs", actual: &reconcile.ActualService{Image: "web:2", Replicas: 2}, current: current},
		{name: "replicas differ", actual: &reconcile.ActualService{Image: "web:1", Replicas: 3}, current: current},
		{name: "files hash differs", actual: &reconcile.ActualService{Image: "web:1", Replicas: 2}, current: &remotestate.ServiceState{FilesContentHash: "files-b"}},
		{name: "not running", actual: nil, current: current},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := RollbackServiceUnchanged(target, test.actual, test.current); got != test.want {
				t.Fatalf("RollbackServiceUnchanged() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestPlanRollbackOutcomesMarksUnchangedServices(t *testing.T) {
	d1 := rollbackRecord("D1", remotestate.StatusSuccess, 0, "api", "worker")
	d2 := rollbackRecord("D2", remotestate.StatusSuccess, time.Hour, "api")
	history := rollbackHistory(d1, d2)
	services := map[string]config.ServiceConfig{"api": {Image: "api"}, "worker": {Image: "worker"}}
	plans := ReconstructServiceTargets(history, d1, services, false)
	actual := map[string]*reconcile.ActualService{
		"api":    {Image: "api:D2", Replicas: 1},
		"worker": {Image: "worker:D1", Replicas: 1},
	}
	outcomes := planRollbackOutcomes(plans, actual, history)
	byName := map[string]RollbackServiceOutcome{}
	for _, outcome := range outcomes {
		byName[outcome.Name] = outcome
	}
	if api := byName["api"]; api.Action != RollbackActionRolledBack || api.FromImage != "api:D2" || api.ToImage != "api:D1" || api.SourceDeploymentID != "D1" {
		t.Fatalf("api outcome = %#v", api)
	}
	if worker := byName["worker"]; worker.Action != RollbackActionUnchanged {
		t.Fatalf("worker outcome = %#v", worker)
	}
	if targets := rollbackTargetStates(plans, outcomes); len(targets) != 1 || targets["api"].Image != "api:D1" {
		t.Fatalf("targets = %#v", targets)
	}
}

func TestRollbackJobWarning(t *testing.T) {
	if got := rollbackJobWarning(map[string]config.ServiceConfig{"web": {}}); got != "" {
		t.Fatalf("warning without jobs = %q", got)
	}
	one := rollbackJobWarning(map[string]config.ServiceConfig{"nightly": {Kind: config.ServiceKindJob}})
	if one != "job nightly is not rolled back and keeps running the newer image and schedule" {
		t.Fatalf("single job warning = %q", one)
	}
	two := rollbackJobWarning(map[string]config.ServiceConfig{"nightly": {Kind: config.ServiceKindJob}, "cleanup": {Kind: config.ServiceKindJob}})
	if two != "jobs cleanup, nightly are not rolled back and keep running the newer image and schedule" {
		t.Fatalf("multi job warning = %q", two)
	}
}

func TestRollbackErrorsClassify(t *testing.T) {
	if Classify(&RollbackUnavailableError{Err: errors.New("no image")}) != ClassFailed {
		t.Fatal("unavailable image must be a failed operation (exit 1)")
	}
}
