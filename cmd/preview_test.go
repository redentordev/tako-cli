package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/hooks"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/spf13/cobra"
)

const previewTemplateEnvironment = `  preview:
    abstract: true
    extends: production
    preview:
      ttl: 72h
`

func setPreviewFlag(t *testing.T, name string) {
	t.Helper()
	old, oldTTL, oldNoPrune := previewFlag, deployPreviewTTL, deployNoPrune
	t.Cleanup(func() { previewFlag, deployPreviewTTL, deployNoPrune = old, oldTTL, oldNoPrune })
	previewFlag = name
}

func stubPreviewRecord(t *testing.T, record *engine.PreviewListItem, calls *[]string) {
	t.Helper()
	old := findPreviewRecord
	t.Cleanup(func() { findPreviewRecord = old })
	findPreviewRecord = func(_ context.Context, _ *config.Config, name string) (*engine.PreviewListItem, error) {
		if calls != nil {
			*calls = append(*calls, name)
		}
		return record, nil
	}
}

func previewListItem(name string, base string) *engine.PreviewListItem {
	return &engine.PreviewListItem{PreviewRecord: takod.PreviewRecord{
		Kind: takod.PreviewRecordKind, Project: "demo", Environment: config.PreviewIdentity(name), Name: name, Base: base,
	}, Status: engine.PreviewStatusActive}
}

func TestPreviewFlagRejectedOnUnsupportedCommands(t *testing.T) {
	setPreviewFlag(t, "pr-1")
	err := rejectUnsupportedPreview(backupCmd)
	if exitCodeForError(err) != 2 || !strings.Contains(err.Error(), "--preview is not supported by tako backup") {
		t.Fatalf("backup err = %v", err)
	}
	for _, command := range []*cobra.Command{deployCmd, psCmd, logsCmd, execCmd, historyCmd, rollbackCmd, configExplainCmd} {
		if err := rejectUnsupportedPreview(command); err != nil {
			t.Fatalf("%s rejected --preview: %v", command.CommandPath(), err)
		}
	}
	previewFlag = "---"
	if err := rejectUnsupportedPreview(deployCmd); exitCodeForError(err) != 2 {
		t.Fatalf("invalid name err = %v", err)
	}
	previewFlag = ""
	if err := rejectUnsupportedPreview(backupCmd); err != nil {
		t.Fatalf("no --preview: %v", err)
	}
}

func TestLoadCommandConfigReadsRecordFirst(t *testing.T) {
	setupHookTest(t, previewTemplateEnvironment)
	setPreviewFlag(t, "PR 42")

	stubPreviewRecord(t, nil, nil)
	if _, err := loadCommandConfigOrError(context.Background(), ""); exitCodeForError(err) != 2 || !strings.Contains(err.Error(), "preview pr-42 not found") {
		t.Fatalf("missing record err = %v", err)
	}

	var calls []string
	stubPreviewRecord(t, previewListItem("pr-42", "preview"), &calls)
	envFlag = "production"
	if _, err := loadCommandConfigOrError(context.Background(), ""); exitCodeForError(err) != 2 || !strings.Contains(err.Error(), "was created from preview, not production") {
		t.Fatalf("base mismatch err = %v", err)
	}
	envFlag = ""
	cfg, err := loadCommandConfigOrError(context.Background(), "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := getEnvironmentName(cfg); got != "preview-pr-42" {
		t.Fatalf("environment = %q, want the preview identity", got)
	}
	if strings.Join(calls, ",") != "pr-42,pr-42" {
		t.Fatalf("record lookups = %v", calls)
	}
	envFlag = "preview"
	if _, err := loadCommandConfigOrError(context.Background(), ""); err != nil {
		t.Fatalf("-e equal to the record base: %v", err)
	}
}

func TestCommandsWithoutPreviewKeepPlainConfig(t *testing.T) {
	setupHookTest(t, previewTemplateEnvironment)
	setPreviewFlag(t, "")
	stubPreviewRecord(t, nil, nil)
	findPreviewRecord = func(context.Context, *config.Config, string) (*engine.PreviewListItem, error) {
		t.Fatal("no record lookup without --preview")
		return nil, nil
	}
	cfg, err := loadCommandConfigOrError(context.Background(), "")
	if err != nil || cfg.PreviewInstance() != nil || getEnvironmentName(cfg) != "production" {
		t.Fatalf("cfg = %v, err = %v", cfg, err)
	}
}

func TestConfigExplainPreviewIsOffline(t *testing.T) {
	setupHookTest(t, previewTemplateEnvironment)
	setPreviewFlag(t, "pr-42")
	stubPreviewRecord(t, nil, nil)
	findPreviewRecord = func(context.Context, *config.Config, string) (*engine.PreviewListItem, error) {
		t.Fatal("config explain --preview must not read a preview record")
		return nil, nil
	}
	var out bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&out)
	if err := runConfigExplain(command, nil); err != nil {
		t.Fatalf("explain: %v", err)
	}
	for _, want := range []string{"Environment: preview-pr-42", "Preview: pr-42 (from preview, ttl 72h, max 5)"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("explain output missing %q:\n%s", want, out.String())
		}
	}
}

func TestDeployPreviewFlagsRequirePreview(t *testing.T) {
	setupHookTest(t, previewTemplateEnvironment)
	setPreviewFlag(t, "")
	deployPreviewTTL = "1h"
	if err := runDeploy(&cobra.Command{}, nil); exitCodeForError(err) != 2 {
		t.Fatalf("--ttl without --preview: %v", err)
	}
}

const previewDeployHooks = previewTemplateEnvironment + `hooks:
  commands:
    pre-deploy: [@self]
    post-deploy: [@self]
`

func TestDeployPreviewReachesEngineAndHooks(t *testing.T) {
	_, runner := setupHookTest(t, previewDeployHooks)
	session := installFakeDeploy(t, false)
	expires := time.Date(2026, 10, 14, 9, 30, 0, 0, time.UTC)
	session.result.Preview = &engine.DeployPreviewResult{Name: "pr-42", Environment: "preview-pr-42", Base: "preview", Created: true, ExpiresAt: &expires, TTL: "1h"}
	var request engine.DeployRequest
	planDeploy = func(_ context.Context, req engine.DeployRequest) (deploySessionAPI, error) {
		request = req
		return session, nil
	}
	setPreviewFlag(t, "pr-42")
	deployPreviewTTL, deployNoPrune = "1h", true
	outputFormatFlag = outputFormatJSON

	var runErr error
	stdout := captureStdout(t, func() { runErr = runDeploy(&cobra.Command{}, nil) })
	if runErr != nil {
		t.Fatalf("runDeploy: %v", runErr)
	}
	if request.Environment != "preview-pr-42" || request.Preview == nil || request.Preview.TTL != "1h" || !request.Preview.NoPrune || request.Config.PreviewInstance() == nil {
		t.Fatalf("engine request = %+v preview = %+v", request, request.Preview)
	}
	pre := runner.call(t, hooks.PreDeploy)
	if pre.env["TAKO_ENVIRONMENT"] != "preview-pr-42" || pre.env["TAKO_PREVIEW_NAME"] != "pr-42" || pre.env["TAKO_PREVIEW_BASE"] != "preview" || pre.env["TAKO_PREVIEW_EXPIRES_AT"] == "" {
		t.Fatalf("pre-deploy env = %v", pre.env)
	}
	post := runner.call(t, hooks.PostDeploy)
	if post.env["TAKO_PREVIEW_EXPIRES_AT"] != "2026-10-14T09:30:00Z" {
		t.Fatalf("post-deploy expiry = %q", post.env["TAKO_PREVIEW_EXPIRES_AT"])
	}
	var result engine.DeployResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout: %v\n%s", err, stdout)
	}
	if result.Preview == nil || !result.Preview.Created || result.Preview.Name != "pr-42" {
		t.Fatalf("result preview = %+v", result.Preview)
	}
}

func TestNormalDeployHooksHaveNoPreviewVars(t *testing.T) {
	_, runner := setupHookTest(t, previewDeployHooks)
	installFakeDeploy(t, false)
	setPreviewFlag(t, "")
	outputFormatFlag = outputFormatJSON
	var runErr error
	captureStdout(t, func() { runErr = runDeploy(&cobra.Command{}, nil) })
	if runErr != nil {
		t.Fatalf("runDeploy: %v", runErr)
	}
	for _, name := range []hooks.Name{hooks.PreDeploy, hooks.PostDeploy} {
		for key := range runner.call(t, name).env {
			if strings.HasPrefix(key, "TAKO_PREVIEW") {
				t.Fatalf("%s got %s on a normal deploy", name, key)
			}
		}
	}
}

func TestRollbackPreviewReachesEngineWithIdentity(t *testing.T) {
	setupHookTest(t, previewTemplateEnvironment)
	setPreviewFlag(t, "pr-42")
	stubPreviewRecord(t, previewListItem("pr-42", "preview"), nil)
	oldService := rollbackService
	t.Cleanup(func() { rollbackService = oldService })
	rollbackService = "web"
	var request engine.RollbackRequest
	runRollbackEngine = func(_ context.Context, req engine.RollbackRequest) (*engine.RollbackResult, error) {
		request = req
		return nil, errors.New("stop")
	}
	command := &cobra.Command{}
	command.Flags().String("service", "", "")
	_ = command.Flags().Set("service", "web")
	_ = runRollback(command, nil)
	if request.Environment != "preview-pr-42" || request.Config == nil || request.Config.PreviewInstance() == nil {
		t.Fatalf("rollback request environment = %q", request.Environment)
	}
}

// TestPreviewCommandsLoadThroughRecordLookup pins that ps, logs, exec and
// history load their config through the record lookup and take the
// environment from getEnvironmentName, which returns the preview identity
// (covered by TestLoadCommandConfigReadsRecordFirst).
func TestPreviewCommandsLoadThroughRecordLookup(t *testing.T) {
	for _, file := range []string{"ps.go", "logs.go", "exec.go", "history.go", "rollback.go"} {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		text := string(source)
		if !strings.Contains(text, "loadCommandConfigOrError(cmd.Context(), cfgFile)") || !strings.Contains(text, "getEnvironmentName(cfg)") {
			t.Fatalf("%s does not load the preview through the record lookup", file)
		}
		if strings.Contains(text, "config.LoadConfig(cfgFile)") {
			t.Fatalf("%s still loads the config without --preview support", file)
		}
	}
}

func TestCheckExpiredPreviewsWarns(t *testing.T) {
	expired := previewListItem("old", "preview")
	expired.Status = engine.PreviewStatusExpired
	list := func(items ...engine.PreviewListItem) func(context.Context, *config.Config) (*engine.PreviewListResult, error) {
		return func(context.Context, *config.Config) (*engine.PreviewListResult, error) {
			return &engine.PreviewListResult{Items: items}, nil
		}
	}
	var results []checkResult
	record := func(r checkResult) { results = append(results, r) }
	checkExpiredPreviews(context.Background(), record, &config.Config{}, list(*expired, *previewListItem("fresh", "preview")))
	if len(results) != 1 || results[0].status != "WARN" || !strings.Contains(results[0].message, "old") || !strings.Contains(results[0].fix, "tako preview prune") {
		t.Fatalf("results = %+v", results)
	}
	results = nil
	checkExpiredPreviews(context.Background(), record, &config.Config{}, list(*previewListItem("fresh", "preview")))
	if len(results) != 1 || results[0].status != "PASS" {
		t.Fatalf("results = %+v", results)
	}
	results = nil
	checkExpiredPreviews(context.Background(), record, &config.Config{}, func(context.Context, *config.Config) (*engine.PreviewListResult, error) {
		return nil, errors.New("unreachable")
	})
	if len(results) != 1 || results[0].status != "WARN" {
		t.Fatalf("results = %+v", results)
	}
}

func TestPreviewSummaryHumanLine(t *testing.T) {
	summary := &engine.OperationSummary{
		Operation: engine.SummaryOperationDeploy, Project: "demo", Outcome: engine.SummaryOutcomeSucceeded,
		Environment: engine.SummaryEnvironment{Name: "preview-pr-42", Kind: engine.SummaryEnvironmentKindPreview, Preview: &engine.SummaryPreview{Name: "pr-42", Base: "preview", ExpiresAt: "2026-10-18T14:02:00Z"}},
	}
	text := renderSummaryText(summary, false)
	if !strings.Contains(text, "Preview pr-42 (from preview) · expires 2026-10-18 14:02 UTC") {
		t.Fatalf("summary text:\n%s", text)
	}
	summary.Environment = engine.SummaryEnvironment{Name: "production", Kind: engine.SummaryEnvironmentKindStandard}
	if strings.Contains(renderSummaryText(summary, false), "Preview ") {
		t.Fatal("standard summary shows a preview line")
	}
}

// TestPreviewDocumentsGolden pins DeployResult.preview and the preview
// result documents.
func TestPreviewDocumentsGolden(t *testing.T) {
	expires := time.Date(2026, 10, 18, 14, 2, 0, 0, time.UTC)
	deploy := engine.DeployResult{
		APIVersion: takoapi.APIVersionCurrent, Kind: engine.KindDeployResult, Project: "demo", Environment: "preview-pr-42",
		Status: takoapi.StatusSuccess, Services: []engine.ServiceOutcome{}, StartedAt: time.Date(2026, 10, 11, 14, 2, 0, 0, time.UTC),
		Preview: &engine.DeployPreviewResult{Name: "pr-42", Environment: "preview-pr-42", Base: "preview", Created: true, ExpiresAt: &expires, TTL: "168h", Hosts: map[string]string{"web": "pr-42.preview.example.com"}},
	}
	payload, _ := json.Marshal(deploy.Preview)
	if string(payload) != `{"name":"pr-42","environment":"preview-pr-42","base":"preview","created":true,"expiresAt":"2026-10-18T14:02:00Z","ttl":"168h","hosts":{"web":"pr-42.preview.example.com"}}` {
		t.Fatalf("DeployResult.preview drifted: %s", payload)
	}

	list := engine.PreviewListResult{
		APIVersion: takoapi.APIVersionCurrent, Kind: engine.KindPreviewList, Project: "demo",
		Items: []engine.PreviewListItem{{PreviewRecord: takod.PreviewRecord{
			APIVersion: takod.PreviewRecordAPIVersion, Kind: takod.PreviewRecordKind, Project: "demo", Environment: "preview-pr-42",
			Name: "pr-42", Base: "preview", Servers: []string{"app-1"}, CreatedAt: expires, UpdatedAt: expires, ExpiresAt: &expires, TTL: "168h",
		}, Status: engine.PreviewStatusActive, Nodes: []string{"app-1"}}},
		Nodes: []engine.PreviewNodeStatus{{Name: "app-1", Status: "ok"}},
	}
	payload, _ = json.Marshal(list)
	want := `{"apiVersion":"tako.redentor.dev/v1alpha1","kind":"PreviewList","project":"demo","items":[{"apiVersion":"tako.redentor.dev/v1alpha1","kind":"PreviewRecord","project":"demo","environment":"preview-pr-42","name":"pr-42","base":"preview","servers":["app-1"],"createdAt":"2026-10-18T14:02:00Z","updatedAt":"2026-10-18T14:02:00Z","expiresAt":"2026-10-18T14:02:00Z","ttl":"168h","status":"active","nodes":["app-1"]}],"nodes":[{"name":"app-1","status":"ok"}]}`
	if string(payload) != want {
		t.Fatalf("PreviewList drifted:\n%s", payload)
	}

	destroy := engine.PreviewDestroyResult{
		APIVersion: takoapi.APIVersionCurrent, Kind: engine.KindPreviewDestroyResult, Project: "demo", Name: "pr-42", Environment: "preview-pr-42", Base: "preview",
		Status: engine.PreviewDestroyStatusDestroyed, Servers: []engine.PreviewDestroyServer{{Name: "app-1", Status: "destroyed", ContainersRemoved: 2, VolumesRemoved: 1}},
		VolumesRemoved: 1, StartedAt: expires, Duration: 3.5,
	}
	payload, _ = json.Marshal(destroy)
	want = `{"apiVersion":"tako.redentor.dev/v1alpha1","kind":"PreviewDestroyResult","project":"demo","name":"pr-42","environment":"preview-pr-42","base":"preview","status":"destroyed","servers":[{"name":"app-1","status":"destroyed","containersRemoved":2,"volumesRemoved":1}],"volumesRemoved":1,"startedAt":"2026-10-18T14:02:00Z","durationSeconds":3.5}`
	if string(payload) != want {
		t.Fatalf("PreviewDestroyResult drifted:\n%s", payload)
	}

	prune := engine.PreviewPruneResult{
		APIVersion: takoapi.APIVersionCurrent, Kind: engine.KindPreviewPruneResult, Project: "demo",
		Destroyed: []engine.PreviewPruneEntry{{Name: "old", Environment: "preview-old", Base: "preview", ExpiresAt: &expires}},
		Skipped:   []engine.PreviewPruneEntry{{Name: "busy", Environment: "preview-busy", Base: "preview", Reason: "in use"}},
		Failed:    []engine.PreviewPruneEntry{},
		Nodes:     []engine.PreviewNodeStatus{},
		StartedAt: expires,
	}
	payload, _ = json.Marshal(prune)
	want = `{"apiVersion":"tako.redentor.dev/v1alpha1","kind":"PreviewPruneResult","project":"demo","destroyed":[{"name":"old","environment":"preview-old","base":"preview","expiresAt":"2026-10-18T14:02:00Z"}],"skipped":[{"name":"busy","environment":"preview-busy","base":"preview","reason":"in use"}],"failed":[],"nodes":[],"startedAt":"2026-10-18T14:02:00Z","durationSeconds":0}`
	if string(payload) != want {
		t.Fatalf("PreviewPruneResult drifted:\n%s", payload)
	}
}
