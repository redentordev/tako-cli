package engine

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/health"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodstate"
)

var summaryTestStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func identityRedact(value string) string { return value }

func summaryTestEnv() SummaryEnvironment {
	return SummaryEnvironment{Name: "production", Kind: SummaryEnvironmentKindStandard}
}

func TestOperationSummaryJSONGolden(t *testing.T) {
	summary := OperationSummary{
		Operation:       SummaryOperationDeploy,
		Project:         "shop",
		Environment:     summaryTestEnv(),
		Outcome:         SummaryOutcomeSucceeded,
		DeploymentID:    "1767225600123456789",
		DisplayID:       "1767225600",
		Revision:        "ab12cd3",
		Commit:          "ab12cd3",
		StartedAt:       summaryTestStart,
		DurationSeconds: 102.5,
		Services: []SummaryService{{
			Name: "web", Change: SummaryChangeUpdated, Strategy: "rolling",
			FromImage: "ghcr.io/acme/web:aa", ToImage: "ghcr.io/acme/web:bb",
			FromRevision: "r1", ToRevision: "r2",
			Replicas: &SummaryReplicas{Desired: 3, Running: 3}, Health: SummaryHealthHealthy,
			Nodes: []string{"node-a"}, Reason: "why", Error: "boom",
		}},
		URLs:      []SummaryURL{{Service: "web", URL: "https://shop.example.com", Kind: SummaryURLKindPublic, Status: SummaryURLReady, Detail: "ok"}},
		Undo:      &SummaryUndo{Command: "tako rollback -e production 1767225000", Argv: []string{"tako", "rollback", "-e", "production", "1767225000"}, TargetDeploymentID: "1767225000123456789", Notes: []string{"n"}},
		NextSteps: []SummaryNextStep{{Reason: "r", Command: "tako ps -e production"}},
		Error:     "e",
		Notes:     []string{"note"},
	}
	payload, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"operation":"deploy","project":"shop","environment":{"name":"production","kind":"standard"},"outcome":"succeeded","deploymentId":"1767225600123456789","displayId":"1767225600","revision":"ab12cd3","commit":"ab12cd3","startedAt":"2026-01-01T00:00:00Z","durationSeconds":102.5,"services":[{"name":"web","change":"updated","strategy":"rolling","fromImage":"ghcr.io/acme/web:aa","toImage":"ghcr.io/acme/web:bb","fromRevision":"r1","toRevision":"r2","replicas":{"desired":3,"running":3},"health":"healthy","nodes":["node-a"],"reason":"why","error":"boom"}],"urls":[{"service":"web","url":"https://shop.example.com","kind":"public","status":"ready","detail":"ok"}],"undo":{"command":"tako rollback -e production 1767225000","argv":["tako","rollback","-e","production","1767225000"],"targetDeploymentId":"1767225000123456789","notes":["n"]},"nextSteps":[{"reason":"r","command":"tako ps -e production"}],"error":"e","notes":["note"]}`
	if string(payload) != want {
		t.Fatalf("summary JSON =\n%s\nwant\n%s", payload, want)
	}
}

func TestOperationSummaryJSONOmitsEmptyFields(t *testing.T) {
	summary := newSummaryBuilder(nil, SummaryOperationScale, "shop", "production", summaryTestStart).build()
	payload, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"operation":"scale","project":"shop","environment":{"name":"production","kind":"standard"},"outcome":"up_to_date","startedAt":"2026-01-01T00:00:00Z","durationSeconds":0,"services":[]}`
	if string(payload) != want {
		t.Fatalf("minimal summary JSON =\n%s\nwant\n%s", payload, want)
	}
}

func TestResultDocumentsOmitSummaryWhenNil(t *testing.T) {
	for name, document := range map[string]any{
		"deploy":   DeployResult{},
		"rollback": RollbackResult{},
		"promote":  PromoteResult{},
		"scale":    ScaleResult{},
	} {
		payload, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(payload), `"summary"`) {
			t.Fatalf("%s result without summary serialized a summary field: %s", name, payload)
		}
	}
	payload, err := json.Marshal(DeployResult{Summary: &OperationSummary{Operation: "deploy"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"summary":{"operation":"deploy"`) {
		t.Fatalf("deploy result summary not serialized: %s", payload)
	}
}

func TestSummaryOutcomeTable(t *testing.T) {
	cases := []struct {
		name       string
		rows       []SummaryService
		unrestored []string
		failed     bool
		want       string
	}{
		{name: "succeeded", rows: []SummaryService{{Name: "api", Change: SummaryChangeUpdated}, {Name: "db", Change: SummaryChangeUnchanged}}, want: SummaryOutcomeSucceeded},
		{name: "up to date", rows: []SummaryService{{Name: "api", Change: SummaryChangeUnchanged}}, want: SummaryOutcomeUpToDate},
		{name: "warmed", rows: []SummaryService{{Name: "web", Change: SummaryChangeWarmed}, {Name: "db", Change: SummaryChangeUnchanged}}, want: SummaryOutcomeWarmed},
		{name: "partial via kept change", rows: []SummaryService{{Name: "api", Change: SummaryChangeUpdated}, {Name: "web", Change: SummaryChangeRestored}, {Name: "worker", Change: SummaryChangeNotAttempted}}, failed: true, want: SummaryOutcomePartial},
		{name: "partial via unrestored failure", rows: []SummaryService{{Name: "web", Change: SummaryChangeFailed}}, unrestored: []string{"web"}, failed: true, want: SummaryOutcomePartial},
		{name: "failed with everything restored", rows: []SummaryService{{Name: "web", Change: SummaryChangeRestored}}, failed: true, want: SummaryOutcomeFailed},
		{name: "failed before cutover", rows: []SummaryService{{Name: "web", Change: SummaryChangeFailed}}, failed: true, want: SummaryOutcomeFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			builder := newSummaryBuilder(nil, SummaryOperationDeploy, "shop", "production", summaryTestStart)
			for _, row := range tc.rows {
				if row.Change == SummaryChangeFailed {
					builder.addFailedService(row, slices.Contains(tc.unrestored, row.Name))
					continue
				}
				builder.addService(row)
			}
			if tc.failed {
				builder.setFailed("boom")
			}
			if got := builder.build().Outcome; got != tc.want {
				t.Fatalf("outcome = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSummaryBuilderSortsAndCapsNextSteps(t *testing.T) {
	builder := newSummaryBuilder(nil, SummaryOperationDeploy, "shop", "production", summaryTestStart)
	for _, name := range []string{"f", "e", "d", "c", "b", "a"} {
		builder.addFailedService(SummaryService{Name: name}, true)
	}
	builder.addURL(SummaryURL{Service: "web", URL: "http://web.internal", Kind: SummaryURLKindInternal})
	builder.addURL(SummaryURL{Service: "web", URL: "https://z.example.com", Kind: SummaryURLKindPublic})
	builder.addURL(SummaryURL{Service: "api", URL: "https://b.example.com", Kind: SummaryURLKindPublic})
	builder.addURL(SummaryURL{Service: "web", URL: "https://a.example.com", Kind: SummaryURLKindPublic})
	builder.setFailed("boom")
	summary := builder.build()

	var names []string
	for _, row := range summary.Services {
		names = append(names, row.Name)
		if row.Health != SummaryHealthUnknown {
			t.Fatalf("row %s health = %q, want unknown", row.Name, row.Health)
		}
	}
	if !slices.Equal(names, []string{"a", "b", "c", "d", "e", "f"}) {
		t.Fatalf("rows = %v, want sorted", names)
	}
	var urls []string
	for _, url := range summary.URLs {
		urls = append(urls, url.URL)
	}
	if !slices.Equal(urls, []string{"https://b.example.com", "https://a.example.com", "https://z.example.com", "http://web.internal"}) {
		t.Fatalf("urls = %v", urls)
	}
	if len(summary.NextSteps) != summaryNextStepLimit {
		t.Fatalf("next steps = %d, want capped at %d", len(summary.NextSteps), summaryNextStepLimit)
	}
}

func TestSummaryRedactsFreeTextButNotNamesOrImages(t *testing.T) {
	eng := New(Options{})
	image := "registry.example.com/site-renderer:ec646b3956498855b6dbdaf4e9f730d54d0e1ec0"
	services := map[string]config.ServiceConfig{"site-renderer": {Image: image, Env: map[string]string{"SHORT": "site"}}}
	cfg := &config.Config{Project: config.ProjectConfig{Name: "shop"}}
	eng.RegisterPublicConfigNames(cfg, services)
	eng.RegisterSecret("site")
	eng.RegisterSecret("hunter2-super-secret")

	builder := eng.newSummaryBuilder(SummaryOperationDeploy, "shop", "production", summaryTestStart)
	builder.addFailedService(SummaryService{Name: "site-renderer", FromImage: image, ToImage: image, Error: "health failed: token=hunter2-super-secret", Reason: "reason hunter2-super-secret"}, true)
	builder.addURL(SummaryURL{Service: "site-renderer", URL: "https://site.example.com", Kind: SummaryURLKindPublic, Status: SummaryURLPending, Detail: "detail hunter2-super-secret"})
	builder.addNote("note hunter2-super-secret")
	builder.setRevision("r1", "hunter2-super-secret")
	builder.setFailed("deploy failed: hunter2-super-secret")
	builder.setUndo(&SummaryUndo{Unavailable: "hunter2-super-secret", Notes: []string{"hunter2-super-secret"}})
	summary := builder.build()

	payload, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "hunter2-super-secret") {
		t.Fatalf("summary leaked a registered secret: %s", payload)
	}
	row := summary.Services[0]
	if row.Name != "site-renderer" || row.FromImage != image || row.ToImage != image {
		t.Fatalf("name or image masked: %+v", row)
	}
	if summary.URLs[0].URL != "https://site.example.com" {
		t.Fatalf("url masked: %q", summary.URLs[0].URL)
	}
}

func TestUndoForDeploy(t *testing.T) {
	env := summaryTestEnv()
	previous := &remotestate.DeploymentState{ID: "1767225000123456789"}
	cases := []struct {
		name        string
		outcome     string
		previous    *remotestate.DeploymentState
		historyErr  error
		persistent  []string
		wantCommand string
		wantReason  string
	}{
		{name: "stable predecessor", outcome: SummaryOutcomeSucceeded, previous: previous, wantCommand: "tako rollback -e production 1767225000"},
		{name: "partial keeps undo", outcome: SummaryOutcomePartial, previous: previous, wantCommand: "tako rollback -e production 1767225000"},
		{name: "first deploy", outcome: SummaryOutcomeSucceeded, wantReason: UndoUnavailableFirstDeploy},
		{name: "history error", outcome: SummaryOutcomeSucceeded, previous: previous, historyErr: errors.New("boom"), wantReason: UndoUnavailableHistory},
		{name: "up to date", outcome: SummaryOutcomeUpToDate, previous: previous, wantReason: UndoUnavailableNothingChanged},
		{name: "warmed", outcome: SummaryOutcomeWarmed, previous: previous, wantReason: UndoUnavailableWarmed},
		{name: "failed", outcome: SummaryOutcomeFailed, previous: previous, wantReason: UndoUnavailableRestored},
		{name: "persistent", outcome: SummaryOutcomeSucceeded, previous: previous, persistent: []string{"postgres"}, wantCommand: "tako rollback -e production 1767225000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			undo := undoForDeploy(env, tc.outcome, tc.previous, tc.historyErr, tc.persistent)
			assertUndo(t, undo, tc.wantCommand, tc.wantReason)
			if tc.wantCommand != "" && undo.TargetDeploymentID != previous.ID {
				t.Fatalf("target = %q, want %q", undo.TargetDeploymentID, previous.ID)
			}
			if len(tc.persistent) > 0 && (len(undo.Notes) != 1 || !strings.Contains(undo.Notes[0], "--include-persistent")) {
				t.Fatalf("persistent note missing: %v", undo.Notes)
			}
		})
	}
}

func TestUndoForRollbackPromoteAndScale(t *testing.T) {
	env := summaryTestEnv()
	d3 := &remotestate.DeploymentState{ID: "1767225900123456789"}
	d2 := &remotestate.DeploymentState{ID: "1767225000123456789"}

	assertUndo(t, undoForRollback(env, SummaryOutcomeSucceeded, "", d3, d2), "tako rollback -e production 1767225900", "")
	assertUndo(t, undoForRollback(env, SummaryOutcomeSucceeded, "web", d3, d2), "tako rollback -e production --service web 1767225900", "")
	assertUndo(t, undoForRollback(env, SummaryOutcomeUpToDate, "", d3, d2), "", UndoUnavailableNothingChanged)
	assertUndo(t, undoForRollback(env, SummaryOutcomeSucceeded, "", d2, d2), "", "no newer stable deployment to roll forward to")
	assertUndo(t, undoForRollback(env, SummaryOutcomeSucceeded, "", nil, d2), "", "no newer stable deployment to roll forward to")

	assertUndo(t, undoForPromote(env, "web", "r1", true, nil, nil), "tako promote web -e production --revision r1", "")
	assertUndo(t, undoForPromote(env, "web", "r1", false, &remotestate.DeploymentState{ID: "1767225000123456789"}, nil), "tako rollback -e production --service web 1767225000", "")
	undo := undoForPromote(env, "web", "r1", false, nil, nil)
	if undo.Command != "" || !strings.Contains(undo.Unavailable, "no longer retained") {
		t.Fatalf("no stable predecessor undo = %+v", undo)
	}
	assertUndo(t, undoForPromote(env, "web", "r1", false, nil, errors.New("boom")), "", UndoUnavailableHistory)

	assertUndo(t, undoForScale(env, map[string]int{"web": 2, "api": 1}, []string{"web", "api"}), "tako scale -e production api=1 web=2", "")
	assertUndo(t, undoForScale(env, map[string]int{"web": 2}, nil), "", UndoUnavailableNothingChanged)

	quoted := newUndo([]string{"tako", "--config", "deploy dir/tako.yaml", "rollback", "-e", "it's", "1"}, "")
	if quoted.Command != `tako --config 'deploy dir/tako.yaml' rollback -e 'it'\''s' 1` {
		t.Fatalf("quoted command = %q", quoted.Command)
	}
}

func assertUndo(t *testing.T, undo *SummaryUndo, wantCommand string, wantReason string) {
	t.Helper()
	if undo == nil {
		t.Fatal("undo is nil")
	}
	if undo.Command != wantCommand {
		t.Fatalf("undo command = %q, want %q", undo.Command, wantCommand)
	}
	if wantCommand != "" {
		if ShellJoin(undo.Argv) != undo.Command {
			t.Fatalf("argv %v does not match command %q", undo.Argv, undo.Command)
		}
		if slices.Contains(undo.Argv, "-y") || slices.Contains(undo.Argv, "--yes") {
			t.Fatalf("undo argv contains -y: %v", undo.Argv)
		}
		if !slices.Contains(undo.Argv, "-e") {
			t.Fatalf("undo argv does not select the environment: %v", undo.Argv)
		}
		if undo.Unavailable != "" {
			t.Fatalf("undo with command also unavailable: %+v", undo)
		}
	}
	if wantReason != "" && undo.Unavailable != wantReason {
		t.Fatalf("undo unavailable = %q, want %q", undo.Unavailable, wantReason)
	}
}

func TestSummaryNextSteps(t *testing.T) {
	cases := []struct {
		name   string
		build  func(*summaryBuilder)
		expect []string
	}{
		{name: "warmed", build: func(b *summaryBuilder) {
			b.addService(SummaryService{Name: "web", Change: SummaryChangeWarmed})
		}, expect: []string{"tako promote web -e production"}},
		{name: "failed", build: func(b *summaryBuilder) {
			b.addFailedService(SummaryService{Name: "web"}, false)
			b.setFailed("boom")
		}, expect: []string{"tako logs -e production --service web --since 10m"}},
		{name: "restored", build: func(b *summaryBuilder) {
			b.addService(SummaryService{Name: "web", Change: SummaryChangeRestored})
			b.setFailed("boom")
		}, expect: []string{"tako logs -e production --service web --since 10m"}},
		{name: "pending domain", build: func(b *summaryBuilder) {
			b.addService(SummaryService{Name: "web", Change: SummaryChangeUpdated})
			b.addURL(SummaryURL{Service: "web", URL: "https://www.shop.example.com", Kind: SummaryURLKindPublic, Status: SummaryURLPending})
		}, expect: []string{"tako domains status -e production www.shop.example.com"}},
		{name: "degraded", build: func(b *summaryBuilder) {
			b.addService(SummaryService{Name: "web", Change: SummaryChangeUpdated})
			b.applyHealth(map[string]serviceHealth{"web": {Desired: 3, Running: 2, Health: SummaryHealthDegraded}}, nil)
		}, expect: []string{"tako ps -e production"}},
		{name: "partial", build: func(b *summaryBuilder) {
			b.addService(SummaryService{Name: "api", Change: SummaryChangeUpdated})
			b.addService(SummaryService{Name: "web", Change: SummaryChangeRestored})
			b.setFailed("boom")
		}, expect: []string{"tako logs -e production --service web --since 10m", "tako history -e production"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			builder := newSummaryBuilder(nil, SummaryOperationDeploy, "shop", "production", summaryTestStart)
			tc.build(builder)
			var commands []string
			for _, step := range builder.build().NextSteps {
				commands = append(commands, step.Command)
				if step.Reason == "" {
					t.Fatalf("next step without reason: %+v", step)
				}
			}
			if !slices.Equal(commands, tc.expect) {
				t.Fatalf("next steps = %v, want %v", commands, tc.expect)
			}
		})
	}
}

func TestSummaryHealthMapping(t *testing.T) {
	cases := []struct {
		desired, running int
		docker           string
		want             string
	}{
		{3, 3, takod.HealthStateHealthy, SummaryHealthHealthy},
		{3, 3, "", SummaryHealthHealthy},
		{3, 3, takod.HealthStateUnhealthy, SummaryHealthUnhealthy},
		{3, 3, takod.HealthStateStarting, SummaryHealthStarting},
		{3, 2, takod.HealthStateHealthy, SummaryHealthDegraded},
		{0, 0, "", SummaryHealthStopped},
	}
	for _, tc := range cases {
		if got := summaryHealthFor(tc.desired, tc.running, tc.docker); got != tc.want {
			t.Fatalf("summaryHealthFor(%d, %d, %q) = %q, want %q", tc.desired, tc.running, tc.docker, got, tc.want)
		}
	}

	builder := newSummaryBuilder(nil, SummaryOperationDeploy, "shop", "production", summaryTestStart)
	builder.addService(SummaryService{Name: "web", Change: SummaryChangeUpdated})
	builder.applyHealth(nil, errors.New("timed out after 10s"))
	summary := builder.build()
	if summary.Services[0].Health != SummaryHealthUnknown {
		t.Fatalf("health on read error = %q, want unknown", summary.Services[0].Health)
	}
	if len(summary.Notes) != 1 || !strings.HasPrefix(summary.Notes[0], "health not read: ") {
		t.Fatalf("notes = %v, want health-not-read note", summary.Notes)
	}
}

func TestReadServiceHealthAggregatesAcrossNodes(t *testing.T) {
	servers := map[string]config.ServerConfig{"node-a": {Host: "node-a"}, "node-b": {Host: "node-b"}}
	services := map[string]config.ServiceConfig{
		"web":     {Image: "nginx:alpine", Port: 80, Replicas: 2},
		"migrate": {Image: "migrate:1", Kind: config.ServiceKindRun},
	}
	read := func(serverName string, _ config.ServerConfig) (map[string]*takod.ActualService, error) {
		if serverName == "node-b" {
			return map[string]*takod.ActualService{}, nil
		}
		return map[string]*takod.ActualService{"web": {Name: "web", Image: "nginx:alpine", Replicas: 1, Health: takod.HealthStateHealthy}}, nil
	}
	got, err := readServiceHealth(context.Background(), servers, services, []string{"node-a", "node-b"}, []string{"node-a", "node-b"}, time.Second, read)
	if err != nil {
		t.Fatalf("readServiceHealth: %v", err)
	}
	web := got["web"]
	if web.Desired != 2 || web.Running != 1 || web.Health != SummaryHealthDegraded {
		t.Fatalf("web health = %+v, want 1/2 degraded", web)
	}
	if got["migrate"].Health != SummaryHealthNA {
		t.Fatalf("run step health = %+v, want n/a", got["migrate"])
	}
}

func TestReadServiceHealthTimesOut(t *testing.T) {
	servers := map[string]config.ServerConfig{"node-a": {Host: "node-a"}}
	services := map[string]config.ServiceConfig{"web": {Image: "nginx:alpine", Port: 80}}
	release := make(chan struct{})
	defer close(release)
	read := func(string, config.ServerConfig) (map[string]*takod.ActualService, error) {
		<-release
		return nil, nil
	}
	started := time.Now()
	_, err := readServiceHealth(context.Background(), servers, services, []string{"node-a"}, []string{"node-a"}, 50*time.Millisecond, read)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("readServiceHealth did not honor its timeout")
	}

	failing := func(string, config.ServerConfig) (map[string]*takod.ActualService, error) {
		return nil, errors.New("unreachable")
	}
	if _, err := readServiceHealth(context.Background(), servers, services, []string{"node-a"}, []string{"node-a"}, time.Second, failing); err == nil {
		t.Fatal("expected read error")
	}
}

func summaryDeployServices() map[string]config.ServiceConfig {
	return map[string]config.ServiceConfig{
		"api":      {Image: "ghcr.io/acme/api:new", Port: 8080},
		"web":      {Image: "ghcr.io/acme/web:new", Port: 80, Proxy: &config.ProxyConfig{Domain: "shop.example.com", Domains: []string{"www.shop.example.com"}}},
		"worker":   {Image: "ghcr.io/acme/worker:new"},
		"postgres": {Image: "postgres:16", Port: 5432, Persistent: true},
	}
}

func summaryActualBefore() map[string]*reconcile.ActualService {
	return map[string]*reconcile.ActualService{
		"api":      {Name: "api", Image: "ghcr.io/acme/api:old", CurrentRevision: "r1"},
		"web":      {Name: "web", Image: "ghcr.io/acme/web:old", CurrentRevision: "r1"},
		"worker":   {Name: "worker", Image: "ghcr.io/acme/worker:old"},
		"postgres": {Name: "postgres", Image: "postgres:16"},
	}
}

func summaryRowsByName(summary *OperationSummary) map[string]SummaryService {
	rows := map[string]SummaryService{}
	for _, row := range summary.Services {
		rows[row.Name] = row
	}
	return rows
}

func TestBuildDeploySummarySuccess(t *testing.T) {
	services := summaryDeployServices()
	in := deploySummaryInput{
		Project: "shop", Environment: "production", StartedAt: summaryTestStart, Duration: 12,
		DeploymentID: "1767225600123456789", Revision: "ab12cd3",
		Services:         services,
		ServicesToDeploy: map[string]config.ServiceConfig{"api": services["api"], "web": services["web"]},
		Outcomes: []ServiceOutcome{
			{Name: "web", Image: "ghcr.io/acme/web:new", Action: OutcomeDeployed},
			{Name: "api", Image: "ghcr.io/acme/api:new", Action: OutcomeDeployed},
		},
		ActualBefore:   summaryActualBefore(),
		PreviousStable: &remotestate.DeploymentState{ID: "1767225000123456789"},
		DomainChecked:  true,
		DomainStatuses: []health.DomainStatus{
			{Domain: "shop.example.com", State: health.DomainStateActive},
			{Domain: "www.shop.example.com", State: health.DomainStatePendingTLS, Message: "TLS not issued yet"},
		},
		HealthRead: true,
		Health: map[string]serviceHealth{
			"api": {Desired: 1, Running: 1, Health: SummaryHealthHealthy},
			"web": {Desired: 3, Running: 2, Health: SummaryHealthDegraded},
		},
	}
	summary := buildDeploySummary(identityRedact, in)
	if summary.Outcome != SummaryOutcomeSucceeded {
		t.Fatalf("outcome = %q", summary.Outcome)
	}
	if summary.DisplayID != "1767225600" {
		t.Fatalf("displayId = %q", summary.DisplayID)
	}
	rows := summaryRowsByName(summary)
	if rows["web"].Change != SummaryChangeUpdated || rows["web"].FromImage != "ghcr.io/acme/web:old" || rows["web"].ToImage != "ghcr.io/acme/web:new" {
		t.Fatalf("web row = %+v", rows["web"])
	}
	if rows["web"].Replicas == nil || rows["web"].Replicas.Running != 2 || rows["web"].Health != SummaryHealthDegraded {
		t.Fatalf("web health = %+v", rows["web"])
	}
	if rows["postgres"].Change != SummaryChangeUnchanged || rows["postgres"].ToImage != "postgres:16" {
		t.Fatalf("postgres row = %+v", rows["postgres"])
	}
	if summary.Undo == nil || summary.Undo.Command != "tako rollback -e production 1767225000" {
		t.Fatalf("undo = %+v", summary.Undo)
	}
	if len(summary.URLs) != 2 || summary.URLs[0].URL != "https://shop.example.com" || summary.URLs[0].Status != SummaryURLReady {
		t.Fatalf("urls = %+v", summary.URLs)
	}
	if summary.URLs[1].Status != SummaryURLPending || summary.URLs[1].Detail != "TLS not issued yet" {
		t.Fatalf("pending url = %+v", summary.URLs[1])
	}
	var commands []string
	for _, step := range summary.NextSteps {
		commands = append(commands, step.Command)
	}
	for _, want := range []string{"tako domains status -e production www.shop.example.com", "tako ps -e production"} {
		if !slices.Contains(commands, want) {
			t.Fatalf("next steps %v missing %q", commands, want)
		}
	}
}

func TestBuildDeploySummaryFirstDeployAndHistoryError(t *testing.T) {
	services := map[string]config.ServiceConfig{"web": {Image: "web:new", Port: 80}}
	in := deploySummaryInput{
		Project: "shop", Environment: "production", StartedAt: summaryTestStart,
		Services: services, ServicesToDeploy: services,
		Plan:     &reconcile.ReconciliationPlan{Changes: []reconcile.ServiceChange{{ServiceName: "web", Type: reconcile.ChangeAdd}}},
		Outcomes: []ServiceOutcome{{Name: "web", Image: "web:new", Action: OutcomeDeployed}},
	}
	summary := buildDeploySummary(identityRedact, in)
	if summary.Services[0].Change != SummaryChangeAdded {
		t.Fatalf("first deploy row = %+v", summary.Services[0])
	}
	if summary.Undo.Unavailable != UndoUnavailableFirstDeploy {
		t.Fatalf("first deploy undo = %+v", summary.Undo)
	}

	in.HistoryErr = errors.New("history unreadable")
	summary = buildDeploySummary(identityRedact, in)
	if summary.Undo.Unavailable != UndoUnavailableHistory {
		t.Fatalf("history error undo = %+v", summary.Undo)
	}
	found := false
	for _, step := range summary.NextSteps {
		found = found || step.Command == "tako history -e production"
	}
	if !found {
		t.Fatalf("history next step missing: %+v", summary.NextSteps)
	}
}

func TestBuildDeploySummaryPartialWithNotAttempted(t *testing.T) {
	services := summaryDeployServices()
	in := deploySummaryInput{
		Project: "shop", Environment: "production", StartedAt: summaryTestStart,
		Services:         services,
		ServicesToDeploy: map[string]config.ServiceConfig{"api": services["api"], "web": services["web"], "worker": services["worker"]},
		Outcomes: []ServiceOutcome{
			{Name: "api", Image: "ghcr.io/acme/api:new", Action: OutcomeDeployed},
			{Name: "web", Image: "ghcr.io/acme/web:new", Action: OutcomeRestored, Error: "health check failed", Recovery: &RecoveryOutcome{Status: deployer.RecoveryRestored}},
		},
		ActualBefore:   summaryActualBefore(),
		PreviousStable: &remotestate.DeploymentState{ID: "1767225000123456789"},
		Failed:         true,
		Error:          "deployment failed for service web",
	}
	summary := buildDeploySummary(identityRedact, in)
	if summary.Outcome != SummaryOutcomePartial {
		t.Fatalf("outcome = %q, want partial", summary.Outcome)
	}
	rows := summaryRowsByName(summary)
	if rows["api"].Change != SummaryChangeUpdated || rows["web"].Change != SummaryChangeRestored || rows["worker"].Change != SummaryChangeNotAttempted {
		t.Fatalf("rows = %+v", rows)
	}
	if rows["web"].Error != "health check failed" {
		t.Fatalf("web error = %q", rows["web"].Error)
	}
	if summary.Undo.Command == "" {
		t.Fatalf("partial deploy lost its undo: %+v", summary.Undo)
	}

	// Only web planned, restored: everything is back -> failed, no undo.
	in.ServicesToDeploy = map[string]config.ServiceConfig{"web": services["web"]}
	in.Outcomes = in.Outcomes[1:]
	summary = buildDeploySummary(identityRedact, in)
	if summary.Outcome != SummaryOutcomeFailed || summary.Undo.Command != "" {
		t.Fatalf("restored-only deploy = %q undo %+v", summary.Outcome, summary.Undo)
	}

	// Recovery failed: unrestored -> partial with undo.
	in.Outcomes = []ServiceOutcome{{Name: "web", Image: "ghcr.io/acme/web:new", Action: OutcomeFailed, Error: "boom", Recovery: &RecoveryOutcome{Status: deployer.RecoveryFailed}}}
	summary = buildDeploySummary(identityRedact, in)
	if summary.Outcome != SummaryOutcomePartial || summary.Undo.Command == "" {
		t.Fatalf("unrestored deploy = %q undo %+v", summary.Outcome, summary.Undo)
	}
}

func TestBuildDeploySummaryBookkeepingFailureAndRunRows(t *testing.T) {
	services := map[string]config.ServiceConfig{
		"web":     {Image: "web:new", Port: 80},
		"migrate": {Image: "web:new", Kind: config.ServiceKindRun},
	}
	in := deploySummaryInput{
		Project: "shop", Environment: "production", StartedAt: summaryTestStart,
		Services: services, ServicesToDeploy: services,
		Outcomes: []ServiceOutcome{
			{Name: "migrate", Image: "web:new", Action: OutcomeRan, Run: &RunOutcome{ExitCode: 0, DurationMs: 4000}},
			{Name: "web", Image: "web:new", Action: OutcomeDeployed},
		},
		PreviousStable:   &remotestate.DeploymentState{ID: "1767225000123456789"},
		Failed:           true,
		Error:            "failed to persist takod state",
		BookkeepingError: "failed to persist takod state",
		HealthRead:       true,
		HealthErr:        errors.New("timed out after 10s"),
	}
	summary := buildDeploySummary(identityRedact, in)
	if summary.Outcome != SummaryOutcomePartial {
		t.Fatalf("outcome = %q, want partial", summary.Outcome)
	}
	if summary.Undo.Command == "" {
		t.Fatalf("bookkeeping failure lost undo: %+v", summary.Undo)
	}
	if !slices.ContainsFunc(summary.Notes, func(note string) bool {
		return strings.HasPrefix(note, "rollout succeeded; state bookkeeping failed: ")
	}) {
		t.Fatalf("notes = %v", summary.Notes)
	}
	rows := summaryRowsByName(summary)
	if rows["migrate"].Change != SummaryChangeRan || rows["migrate"].Health != SummaryHealthNA || rows["migrate"].Reason != "exit 0 in 4s" {
		t.Fatalf("run row = %+v", rows["migrate"])
	}
	if rows["web"].Health != SummaryHealthUnknown {
		t.Fatalf("web health = %q, want unknown after read error", rows["web"].Health)
	}
}

func TestBuildDeploySummaryUpToDateAndWarmed(t *testing.T) {
	services := map[string]config.ServiceConfig{"web": {Image: "web:new", Port: 80, Proxy: &config.ProxyConfig{Domain: "shop.example.com"}}}
	in := deploySummaryInput{Project: "shop", Environment: "production", StartedAt: summaryTestStart, Services: services, ActualBefore: map[string]*reconcile.ActualService{"web": {Image: "web:new"}}}
	summary := buildDeploySummary(identityRedact, in)
	if summary.Outcome != SummaryOutcomeUpToDate || summary.Undo.Unavailable != UndoUnavailableNothingChanged {
		t.Fatalf("up to date = %q %+v", summary.Outcome, summary.Undo)
	}
	if summary.URLs[0].Status != SummaryURLNotChecked {
		t.Fatalf("unchecked url = %+v", summary.URLs[0])
	}

	in.ServicesToDeploy = services
	in.Outcomes = []ServiceOutcome{{Name: "web", Image: "web:next", Action: OutcomeWarmed}}
	summary = buildDeploySummary(identityRedact, in)
	if summary.Outcome != SummaryOutcomeWarmed || summary.Undo.Unavailable != UndoUnavailableWarmed {
		t.Fatalf("warmed = %q %+v", summary.Outcome, summary.Undo)
	}
	if len(summary.NextSteps) == 0 || summary.NextSteps[0].Command != "tako promote web -e production" {
		t.Fatalf("warmed next steps = %+v", summary.NextSteps)
	}
}

func TestDeploySummaryURLsSortedAndMapped(t *testing.T) {
	services := map[string]config.ServiceConfig{
		"web":   {Image: "web", Port: 80, Proxy: &config.ProxyConfig{Domain: "z.example.com", Domains: []string{"a.example.com"}}},
		"admin": {Image: "admin", Port: 80, Proxy: &config.ProxyConfig{Visibility: config.ProxyVisibilityInternal, Host: "admin.internal"}},
		"api":   {Image: "api", Port: 80, Proxy: &config.ProxyConfig{Domain: "api.example.com"}},
	}
	for range 3 {
		public, internal := deployResultURLs(services)
		if !slices.Equal(public, []string{"https://api.example.com", "https://a.example.com", "https://z.example.com"}) {
			t.Fatalf("public urls = %v", public)
		}
		if !slices.Equal(internal, []string{"http://admin.internal"}) {
			t.Fatalf("internal urls = %v", internal)
		}
	}
	urls, notes := deploySummaryURLs("production", services, true, true, []health.DomainStatus{
		{Domain: "z.example.com", State: health.DomainStatePendingTLS},
		{Domain: "a.example.com", State: health.DomainStateWrongDNS, Message: "points elsewhere"},
	})
	byURL := map[string]SummaryURL{}
	for _, url := range urls {
		byURL[url.URL] = url
	}
	if byURL["https://z.example.com"].Status != SummaryURLFailed || byURL["https://a.example.com"].Status != SummaryURLFailed {
		t.Fatalf("strict statuses = %+v", urls)
	}
	if byURL["https://api.example.com"].Status != SummaryURLNotChecked {
		t.Fatalf("unreported domain = %+v", byURL["https://api.example.com"])
	}
	if byURL["http://admin.internal"].Kind != SummaryURLKindInternal || byURL["http://admin.internal"].Status != SummaryURLNotChecked {
		t.Fatalf("internal url = %+v", byURL["http://admin.internal"])
	}
	if !slices.ContainsFunc(notes, func(note string) bool { return strings.Contains(note, "tako domains hosts -e production") }) {
		t.Fatalf("notes = %v, want hosts hint", notes)
	}
	if status, _ := summaryURLStatus(health.DomainStatus{State: health.DomainStatePendingDNS}, false); status != SummaryURLPending {
		t.Fatalf("non-strict pending = %q", status)
	}
}

func TestNewestStableRecord(t *testing.T) {
	base := summaryTestStart
	history := &remotestate.DeploymentHistory{Deployments: []*remotestate.DeploymentState{
		{ID: "1", Timestamp: base, Status: remotestate.StatusSuccess, Services: map[string]remotestate.ServiceState{"web": {}}},
		{ID: "2", Timestamp: base.Add(time.Minute), Status: remotestate.StatusFailed, Services: map[string]remotestate.ServiceState{"web": {}}},
		{ID: "3", Timestamp: base.Add(2 * time.Minute), Status: remotestate.StatusRolledBack, Services: map[string]remotestate.ServiceState{"api": {}}},
		{ID: "4", Timestamp: base.Add(3 * time.Minute), Status: remotestate.StatusSuccess, Services: map[string]remotestate.ServiceState{"web": {}}},
	}}
	if got := newestStableRecord(history, "", time.Time{}, ""); got == nil || got.ID != "4" {
		t.Fatalf("newest = %+v", got)
	}
	if got := newestStableRecord(history, "", time.Time{}, "4"); got == nil || got.ID != "3" {
		t.Fatalf("newest excluding 4 = %+v", got)
	}
	if got := newestStableRecord(history, "web", base.Add(3*time.Minute), ""); got == nil || got.ID != "1" {
		t.Fatalf("newest web before 4 = %+v", got)
	}
	if got := newestStableRecord(nil, "", time.Time{}, ""); got != nil {
		t.Fatalf("nil history = %+v", got)
	}
}

func TestBuildRollbackSummary(t *testing.T) {
	services := map[string]config.ServiceConfig{"web": {Image: "web", Port: 80}, "api": {Image: "api", Port: 80}, "worker": {Image: "worker"}}
	target := &remotestate.DeploymentState{ID: "1767225000123456789", Version: "v1"}
	rollForward := &remotestate.DeploymentState{ID: "1767225900123456789"}
	probed := false
	in := rollbackSummaryInput{
		Project: "shop", Environment: "production", StartedAt: summaryTestStart, DeploymentID: "1767226000123456789",
		Services: services,
		Outcomes: []RollbackServiceOutcome{
			{Name: "web", Action: RollbackActionRolledBack, FromImage: "web:2", ToImage: "web:1"},
			{Name: "api", Action: RollbackActionUnchanged, ToImage: "api:1"},
		},
		Target: target, RollForward: rollForward,
		Health: func() (map[string]serviceHealth, error) {
			probed = true
			return map[string]serviceHealth{"web": {Desired: 1, Running: 1, Health: SummaryHealthHealthy}}, nil
		},
	}
	summary := buildRollbackSummary(identityRedact, in)
	if summary.Outcome != SummaryOutcomeSucceeded || summary.Undo.Command != "tako rollback -e production 1767225900" {
		t.Fatalf("rollback summary = %q %+v", summary.Outcome, summary.Undo)
	}
	if !probed || summaryRowsByName(summary)["web"].Health != SummaryHealthHealthy {
		t.Fatalf("health not applied: %+v", summary.Services)
	}

	probed = false
	in.Outcomes = []RollbackServiceOutcome{{Name: "web", Action: RollbackActionUnchanged, ToImage: "web:1"}}
	summary = buildRollbackSummary(identityRedact, in)
	if summary.Outcome != SummaryOutcomeUpToDate || summary.Undo.Command != "" || probed {
		t.Fatalf("up-to-date rollback = %q %+v probed=%v", summary.Outcome, summary.Undo, probed)
	}

	in.Outcomes = []RollbackServiceOutcome{
		{Name: "api", Action: RollbackActionRolledBack, ToImage: "api:1"},
		{Name: "web", Action: RollbackActionFailed, Error: "boom", Recovery: &RecoveryOutcome{Status: deployer.RecoveryRestored}},
		{Name: "worker", Action: RollbackActionNotAttempted},
	}
	in.Failed, in.Error = true, "rollback failed"
	summary = buildRollbackSummary(identityRedact, in)
	rows := summaryRowsByName(summary)
	if summary.Outcome != SummaryOutcomePartial || rows["worker"].Change != SummaryChangeNotAttempted || rows["worker"].Health != SummaryHealthNA {
		t.Fatalf("partial rollback = %q %+v", summary.Outcome, rows)
	}

	in.Service = "web"
	in.Failed, in.Error = false, ""
	in.Outcomes = []RollbackServiceOutcome{{Name: "web", Action: RollbackActionRolledBack, ToImage: "web:1"}}
	summary = buildRollbackSummary(identityRedact, in)
	if summary.Undo.Command != "tako rollback -e production --service web 1767225900" {
		t.Fatalf("single-service undo = %+v", summary.Undo)
	}
}

func TestBuildPromoteSummary(t *testing.T) {
	in := promoteSummaryInput{
		Project: "shop", Environment: "production", StartedAt: summaryTestStart,
		Service: "web", FromRevision: "r1", ToRevision: "r2", FromImage: "web:1", ToImage: "web:2", Retained: true,
	}
	summary := buildPromoteSummary(identityRedact, in)
	if summary.Outcome != SummaryOutcomeSucceeded || summary.Services[0].Change != SummaryChangePromoted {
		t.Fatalf("promote = %q %+v", summary.Outcome, summary.Services)
	}
	if summary.Undo.Command != "tako promote web -e production --revision r1" {
		t.Fatalf("retained undo = %+v", summary.Undo)
	}
	in.Retained = false
	in.Fallback = &remotestate.DeploymentState{ID: "1767225000123456789"}
	if summary := buildPromoteSummary(identityRedact, in); summary.Undo.Command != "tako rollback -e production --service web 1767225000" {
		t.Fatalf("not-retained undo = %+v", summary.Undo)
	}
	in.Fallback = nil
	if summary := buildPromoteSummary(identityRedact, in); summary.Undo.Command != "" || summary.Undo.Unavailable == "" {
		t.Fatalf("no-predecessor undo = %+v", summary.Undo)
	}
	in.HistoryErr = errors.New("boom")
	summary = buildPromoteSummary(identityRedact, in)
	if summary.Undo.Unavailable != UndoUnavailableHistory {
		t.Fatalf("history error undo = %+v", summary.Undo)
	}
	if !slices.ContainsFunc(summary.NextSteps, func(step SummaryNextStep) bool { return step.Command == "tako history -e production" }) {
		t.Fatalf("history next step missing: %+v", summary.NextSteps)
	}
}

func TestBuildScaleSummary(t *testing.T) {
	eng := New(Options{})
	eng.RegisterSecret("hunter2-super-secret")
	services := map[string]config.ServiceConfig{"web": {Image: "web", Port: 80}, "api": {Image: "api", Port: 80}, "worker": {Image: "worker"}}
	in := scaleSummaryInput{
		Project: "shop", Environment: "production", StartedAt: summaryTestStart, DeploymentID: "1767225600123456789",
		Services: services,
		Outcomes: []ServiceOutcome{
			{Name: "web", Image: "web", Action: OutcomeDeployed, Replicas: 5},
			{Name: "api", Image: "api", Action: OutcomeDeployed, Replicas: 1},
		},
		Previous: map[string]int{"web": 2, "api": 1},
	}
	summary := buildScaleSummary(eng.RedactSensitive, in)
	rows := summaryRowsByName(summary)
	if summary.Outcome != SummaryOutcomeSucceeded || rows["web"].Change != SummaryChangeScaled || rows["api"].Change != SummaryChangeUnchanged {
		t.Fatalf("scale = %q %+v", summary.Outcome, rows)
	}
	if summary.Undo.Command != "tako scale -e production web=2" {
		t.Fatalf("scale undo = %+v", summary.Undo)
	}

	in.Outcomes = append(in.Outcomes, ServiceOutcome{Name: "worker", Image: "worker", Action: OutcomeFailed, Replicas: 3, Error: "scale failed token=hunter2-super-secret"})
	in.Previous["worker"] = 1
	in.Failed, in.Error = true, "scaling completed with 1 error(s)"
	summary = buildScaleSummary(eng.RedactSensitive, in)
	if summary.Outcome != SummaryOutcomePartial || summary.Undo.Command != "tako scale -e production web=2 worker=1" {
		t.Fatalf("failed scale = %q %+v", summary.Outcome, summary.Undo)
	}
	payload, _ := json.Marshal(summary)
	if strings.Contains(string(payload), "hunter2-super-secret") {
		t.Fatalf("scale summary leaked secret: %s", payload)
	}
}

// The previous count comes from the prior desired revision, not from the
// running containers (which include an inactive blue-green set), and an
// unknown previous count never yields an undo like `web=0`.
func TestScaleSummaryUsesPriorDesiredReplicas(t *testing.T) {
	prior := &takodstate.DesiredRevision{Services: map[string]takodstate.DesiredService{
		"web": {Name: "web", Replicas: 2},
		"api": {Name: "api", Replicas: 0},
	}}
	targets := map[string]int{"web": 5, "api": 1, "worker": 3}
	previous := priorReplicaCounts(prior, targets)
	if len(previous) != 2 || previous["web"] != 2 || previous["api"] != 0 {
		t.Fatalf("previous = %v, want web=2 api=0 and worker unknown", previous)
	}
	if got := priorReplicaCounts(nil, targets); len(got) != 0 {
		t.Fatalf("no prior revision: previous = %v, want empty", got)
	}

	eng := New(Options{})
	services := map[string]config.ServiceConfig{"web": {Image: "web", Port: 80}, "worker": {Image: "worker"}}
	in := scaleSummaryInput{
		Project: "shop", Environment: "production", StartedAt: summaryTestStart, DeploymentID: "1767225600123456789",
		Services: services,
		Outcomes: []ServiceOutcome{{Name: "web", Image: "web", Action: OutcomeDeployed, Replicas: 5}},
		Previous: previous,
	}
	summary := buildScaleSummary(eng.RedactSensitive, in)
	if row := summaryRowsByName(summary)["web"]; row.Reason != "2 → 5 replicas" || summary.Undo.Command != "tako scale -e production web=2" {
		t.Fatalf("web row = %+v undo = %+v", row, summary.Undo)
	}

	in.Outcomes = append(in.Outcomes, ServiceOutcome{Name: "worker", Image: "worker", Action: OutcomeDeployed, Replicas: 3})
	summary = buildScaleSummary(eng.RedactSensitive, in)
	row := summaryRowsByName(summary)["worker"]
	if row.Change != SummaryChangeScaled || strings.Contains(row.Reason, "0 →") || !strings.Contains(row.Reason, "unknown") {
		t.Fatalf("worker row = %+v", row)
	}
	if summary.Undo == nil || summary.Undo.Command != "" || summary.Undo.Unavailable != UndoUnavailableUnknownReplicas {
		t.Fatalf("undo with unknown previous count = %+v", summary.Undo)
	}
}

func TestFailedSummariesForSynthesizedResults(t *testing.T) {
	eng := New(Options{})
	summary := eng.FailedBeforeMutationSummary(SummaryOperationDeploy, "shop", "production", []string{"web", "api", "web"}, summaryTestStart, "pre-deploy hook failed")
	if summary.Outcome != SummaryOutcomeFailed || len(summary.Services) != 2 {
		t.Fatalf("before-mutation summary = %+v", summary)
	}
	for _, row := range summary.Services {
		if row.Change != SummaryChangeNotAttempted || row.Health != SummaryHealthNA {
			t.Fatalf("row = %+v", row)
		}
	}
	if summary.Undo == nil || summary.Undo.Command != "" || summary.Undo.Unavailable != UndoUnavailableNotMutated {
		t.Fatalf("undo = %+v", summary.Undo)
	}

	summary = eng.FailedWithoutResultSummary(SummaryOperationRollback, "shop", "production", []string{"web"}, summaryTestStart, "boom")
	if summary.Outcome != SummaryOutcomePartial || summary.Services[0].Change != SummaryChangeFailed || summary.Undo.Unavailable != UndoUnavailableUnknownState {
		t.Fatalf("unknown-state summary = %+v", summary)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"production":         "production",
		"deploy/tako.yaml":   "deploy/tako.yaml",
		"web=2":              "web=2",
		"":                   "''",
		"two words":          "'two words'",
		"it's":               `'it'\''s'`,
		"$(rm -rf /)":        "'$(rm -rf /)'",
		"C:\\path\\tako.yml": `'C:\path\tako.yml'`,
	}
	for input, want := range cases {
		if got := ShellQuote(input); got != want {
			t.Fatalf("ShellQuote(%q) = %q, want %q", input, got, want)
		}
	}
}
