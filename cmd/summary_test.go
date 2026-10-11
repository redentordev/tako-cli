package cmd

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/hooks"
	"github.com/redentordev/tako-cli/pkg/secrets"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/spf13/cobra"
)

var summaryCmdTestStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func sampleDeploySummary() *engine.OperationSummary {
	return &engine.OperationSummary{
		Operation:       engine.SummaryOperationDeploy,
		Project:         "shop",
		Environment:     engine.SummaryEnvironment{Name: "production", Kind: engine.SummaryEnvironmentKindStandard},
		Outcome:         engine.SummaryOutcomeSucceeded,
		DeploymentID:    "1767225600123456789",
		DisplayID:       "1767225600",
		Revision:        "ab12cd3",
		StartedAt:       summaryCmdTestStart,
		DurationSeconds: 102,
		Services: []engine.SummaryService{
			{Name: "api", Change: engine.SummaryChangeUpdated, Strategy: "rolling", FromImage: "ghcr.io/acme/api:ab12cd3", ToImage: "ghcr.io/acme/api:ef45ab6", Replicas: &engine.SummaryReplicas{Desired: 2, Running: 2}, Health: engine.SummaryHealthHealthy},
			{Name: "migrate", Change: engine.SummaryChangeRan, ToImage: "ghcr.io/acme/api:ef45ab6", Reason: "exit 0 in 4s", Health: engine.SummaryHealthNA},
			{Name: "postgres", Change: engine.SummaryChangeUnchanged, Strategy: "recreate", FromImage: "postgres:16", ToImage: "postgres:16", Replicas: &engine.SummaryReplicas{Desired: 1, Running: 1}, Health: engine.SummaryHealthHealthy},
		},
		URLs: []engine.SummaryURL{
			{Service: "web", URL: "https://shop.example.com", Kind: engine.SummaryURLKindPublic, Status: engine.SummaryURLReady},
			{Service: "web", URL: "https://www.shop.example.com", Kind: engine.SummaryURLKindPublic, Status: engine.SummaryURLPending, Detail: "TLS not issued yet"},
		},
		Undo: &engine.SummaryUndo{
			Command:            "tako rollback -e production 1767225000",
			Argv:               []string{"tako", "rollback", "-e", "production", "1767225000"},
			TargetDeploymentID: "1767225000123456789",
		},
		NextSteps: []engine.SummaryNextStep{{Reason: "www.shop.example.com is not ready (pending)", Command: "tako domains status -e production www.shop.example.com"}},
	}
}

func TestRenderSummaryTextGolden(t *testing.T) {
	rule := strings.Repeat("─", summaryRuleWidth)
	want := "\n" + rule + "\n" +
		" ✓ Deployed shop → production in 1m42s · deployment 1767225600 · ab12cd3\n" +
		rule + "\n" +
		" SERVICE   CHANGE     STRATEGY  IMAGE                                REPLICAS  HEALTH\n" +
		" api       updated    rolling   ghcr.io/acme/api:ab12cd3 → :ef45ab6  2/2       healthy\n" +
		" migrate   ran        -         exit 0 in 4s                         -         n/a\n" +
		" postgres  unchanged  recreate  postgres:16                          1/1       healthy\n" +
		"\n URLs\n" +
		"   web  https://shop.example.com      ready\n" +
		"   web  https://www.shop.example.com  pending (TLS not issued yet)\n" +
		"\n Next steps\n" +
		"   tako domains status -e production www.shop.example.com  # www.shop.example.com is not ready (pending)\n" +
		"\n Undo\n" +
		"   tako rollback -e production 1767225000\n" +
		rule + "\n"
	got := renderSummaryText(sampleDeploySummary(), false)
	if got != want {
		t.Fatalf("text summary =\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(got, "\033[") {
		t.Fatal("uncolored summary contains ANSI escapes")
	}
	if colored := renderSummaryText(sampleDeploySummary(), true); !strings.Contains(colored, "\033[") {
		t.Fatal("colored summary has no ANSI escapes")
	}
}

func TestRenderSummaryTextVariants(t *testing.T) {
	partial := sampleDeploySummary()
	partial.Outcome = engine.SummaryOutcomePartial
	partial.Error = "deployment failed for service web"
	partial.Services = append(partial.Services, engine.SummaryService{Name: "web", Change: engine.SummaryChangeRestored, FromImage: "web:1", ToImage: "web:1", Health: engine.SummaryHealthHealthy, Error: "health check failed"})
	text := renderSummaryText(partial, false)
	for _, want := range []string{"✗ Deploy failed (partial): shop → production", "Error: deployment failed for service web", "web: health check failed"} {
		if !strings.Contains(text, want) {
			t.Fatalf("partial text missing %q:\n%s", want, text)
		}
	}
	if strings.Index(text, "Next steps") > strings.Index(text, "Undo") {
		t.Fatalf("next steps must precede undo:\n%s", text)
	}

	upToDate := &engine.OperationSummary{Operation: "deploy", Project: "shop", Environment: engine.SummaryEnvironment{Name: "production"}, Outcome: engine.SummaryOutcomeUpToDate,
		Services: []engine.SummaryService{{Name: "web", Change: engine.SummaryChangeUnchanged, FromImage: "web:1", ToImage: "web:1", Health: engine.SummaryHealthUnknown}},
		Undo:     &engine.SummaryUndo{Unavailable: engine.UndoUnavailableNothingChanged}}
	text = renderSummaryText(upToDate, false)
	if !strings.Contains(text, "✓ shop → production is up to date (deploy)") || !strings.Contains(text, "unavailable: nothing changed") {
		t.Fatalf("up-to-date text:\n%s", text)
	}

	warmed := sampleDeploySummary()
	warmed.Outcome = engine.SummaryOutcomeWarmed
	warmed.Undo = &engine.SummaryUndo{Unavailable: engine.UndoUnavailableWarmed}
	if text := renderSummaryText(warmed, false); !strings.Contains(text, "! Warmed shop → production") || !strings.Contains(text, engine.UndoUnavailableWarmed) {
		t.Fatalf("warmed text:\n%s", text)
	}

	first := sampleDeploySummary()
	first.Undo = &engine.SummaryUndo{Unavailable: engine.UndoUnavailableFirstDeploy}
	if text := renderSummaryText(first, false); !strings.Contains(text, "unavailable: "+engine.UndoUnavailableFirstDeploy) {
		t.Fatalf("first deploy text:\n%s", text)
	}
}

func TestSummaryImageCellCompactsSameRepository(t *testing.T) {
	cases := map[[2]string]string{
		{"ghcr.io/acme/web:aa", "ghcr.io/acme/web:bb"}:           "ghcr.io/acme/web:aa → :bb",
		{"registry:5000/web:aa", "registry:5000/web:bb"}:         "registry:5000/web:aa → :bb",
		{"web:aa", "other:bb"}:                                   "web:aa → other:bb",
		{"web:aa", "web:aa"}:                                     "web:aa",
		{"", "web:aa"}:                                           "web:aa",
		{"web@sha256:111", "web@sha256:222"}:                     "web@sha256:111 → @sha256:222",
		{"registry:5000/web", "registry:5000/web:latest-branch"}: "registry:5000/web → :latest-branch",
	}
	for refs, want := range cases {
		if got := summaryImageCell(engine.SummaryService{FromImage: refs[0], ToImage: refs[1]}); got != want {
			t.Fatalf("summaryImageCell(%q, %q) = %q, want %q", refs[0], refs[1], got, want)
		}
	}
}

// A full commit SHA tag must survive output redaction after compaction: the
// bare 40-hex new tag used to be masked as a base64 pattern match.
func TestSummaryImageCellKeepsFullSHATagsReadable(t *testing.T) {
	const fromSHA = "0123456789abcdef0123456789abcdef01234567"
	const toSHA = "89abcdef0123456789abcdef0123456789abcdef"
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	redactor := secrets.NewRedactor()
	for refs, want := range map[[2]string]string{
		{"ghcr.io/acme/web:" + fromSHA, "ghcr.io/acme/web:" + toSHA}:  toSHA,
		{"ghcr.io/acme/web@sha256:111", "ghcr.io/acme/web@" + digest}: digest,
	} {
		cell := summaryImageCell(engine.SummaryService{FromImage: refs[0], ToImage: refs[1]})
		if got := redactor.Redact(cell); got != cell || !strings.Contains(got, want) {
			t.Fatalf("redacted image cell = %q, want %q unchanged (contains %q)", got, cell, want)
		}
	}
}

func TestWithConfigFlagQuotesPathAndKeepsTarget(t *testing.T) {
	summary := sampleDeploySummary()
	withConfigFlag(summary, "deploy dir/tako.yaml")
	withConfigFlag(summary, "deploy dir/tako.yaml")
	wantArgv := []string{"tako", "--config", "deploy dir/tako.yaml", "rollback", "-e", "production", "1767225000"}
	if !reflect.DeepEqual(summary.Undo.Argv, wantArgv) {
		t.Fatalf("argv = %v, want %v", summary.Undo.Argv, wantArgv)
	}
	if summary.Undo.Command != "tako --config 'deploy dir/tako.yaml' rollback -e production 1767225000" {
		t.Fatalf("command = %q", summary.Undo.Command)
	}
	if summary.NextSteps[0].Command != "tako --config 'deploy dir/tako.yaml' domains status -e production www.shop.example.com" {
		t.Fatalf("next step = %q", summary.NextSteps[0].Command)
	}
	for _, arg := range summary.Undo.Argv {
		if arg == "-y" || arg == "--yes" {
			t.Fatalf("undo argv contains %s", arg)
		}
	}

	unchanged := sampleDeploySummary()
	withConfigFlag(unchanged, "")
	if unchanged.Undo.Command != "tako rollback -e production 1767225000" {
		t.Fatalf("default config changed undo: %q", unchanged.Undo.Command)
	}
}

func TestAddHookNotes(t *testing.T) {
	code := 1
	summary := sampleDeploySummary()
	outcomes := []engine.HookOutcome{
		{Name: "pre-deploy", Status: engine.HookCompleted},
		{Name: "post-deploy", Status: engine.HookFailed, ExitCode: &code},
		{Name: "on-failure", Status: engine.HookTimedOut},
	}
	addHookNotes(summary, outcomes)
	addHookNotes(summary, outcomes)
	want := []string{"post-deploy hook failed (exit 1)", "on-failure hook timed out"}
	if !reflect.DeepEqual(summary.Notes, want) {
		t.Fatalf("notes = %v, want %v", summary.Notes, want)
	}
}

func TestWriteGitHubStepSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "step-summary.md")
	if err := os.WriteFile(path, []byte("existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_STEP_SUMMARY", path)
	t.Setenv("TAKO_STEP_SUMMARY", "")
	sink := &events.BufferSink{}

	writeGitHubStepSummary(sampleDeploySummary(), sink)
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(payload)
	for _, want := range []string{
		"existing\n### ✅ Tako deploy · shop → production",
		"| Service | Change | Strategy | Image | Replicas | Health |",
		"| api | updated | rolling | `ghcr.io/acme/api:ab12cd3 → :ef45ab6` | 2/2 | healthy |",
		"- web: https://www.shop.example.com (pending: TLS not issued yet)",
		"```sh\ntako rollback -e production 1767225000\n```",
		"Deployment: `1767225600`",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("step summary missing %q:\n%s", want, content)
		}
	}
	if len(sink.Events()) != 0 {
		t.Fatalf("unexpected events: %+v", sink.Events())
	}

	for _, value := range []string{"off", "0", "FALSE", "No"} {
		t.Setenv("TAKO_STEP_SUMMARY", value)
		writeGitHubStepSummary(sampleDeploySummary(), sink)
		after, _ := os.ReadFile(path)
		if string(after) != content {
			t.Fatalf("TAKO_STEP_SUMMARY=%s still wrote the summary", value)
		}
	}

	t.Setenv("TAKO_STEP_SUMMARY", "")
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	writeGitHubStepSummary(sampleDeploySummary(), sink)
	if after, _ := os.ReadFile(path); string(after) != content {
		t.Fatal("unset GITHUB_STEP_SUMMARY still wrote the summary")
	}
}

func TestWriteGitHubStepSummaryUnwritableWarns(t *testing.T) {
	t.Setenv("GITHUB_STEP_SUMMARY", filepath.Join(t.TempDir(), "missing-dir", "summary.md"))
	t.Setenv("TAKO_STEP_SUMMARY", "")
	sink := &events.BufferSink{}
	writeGitHubStepSummary(sampleDeploySummary(), sink)
	emitted := sink.Events()
	if len(emitted) != 1 || emitted[0].Type != events.TypeWarning || !strings.Contains(emitted[0].Message, "step summary not written") {
		t.Fatalf("events = %+v, want one warning", emitted)
	}
}

func TestStepSummaryTruncatesAndEscapes(t *testing.T) {
	summary := sampleDeploySummary()
	summary.Services = nil
	for i := 0; i < 400; i++ {
		summary.Services = append(summary.Services, engine.SummaryService{Name: strings.Repeat("s", 200) + string(rune('a'+i%26)), Change: engine.SummaryChangeUpdated, Health: engine.SummaryHealthHealthy, Reason: strings.Repeat("r", 300)})
	}
	content := renderSummaryMarkdown(summary)
	if len(content) > stepSummaryMaxBytes {
		t.Fatalf("content is %d bytes, over the cap", len(content))
	}
	if !strings.HasSuffix(content, stepSummaryTruncationNote) {
		t.Fatalf("truncated content does not end with the note: %q", content[len(content)-200:])
	}

	rows := sampleDeploySummary()
	rows.Services = nil
	for i := 0; i < stepSummaryMaxRows+5; i++ {
		rows.Services = append(rows.Services, engine.SummaryService{Name: "svc", Change: engine.SummaryChangeUnchanged, Health: engine.SummaryHealthHealthy})
	}
	if content := renderSummaryMarkdown(rows); !strings.Contains(content, "_5 more services not shown._") {
		t.Fatal("row cap note missing")
	}

	escaped := sampleDeploySummary()
	escaped.Error = "bad | <script> ```fence```"
	escaped.Notes = []string{"a|b <c> `d`"}
	content = renderSummaryMarkdown(escaped)
	if !strings.Contains(content, `- a\|b &lt;c&gt; \`+"`d\\`") {
		t.Fatalf("note not escaped:\n%s", content)
	}
	if !strings.Contains(content, "````\nbad | <script> ```fence```\n````") {
		t.Fatalf("error fence not lengthened:\n%s", content)
	}
}

func TestStepSummaryNeverCarriesRegisteredSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "summary.md")
	t.Setenv("GITHUB_STEP_SUMMARY", path)
	t.Setenv("TAKO_STEP_SUMMARY", "")
	eng := engine.New(engine.Options{})
	eng.RegisterSecret("hunter2-super-secret")
	summary := eng.FailedBeforeMutationSummary("deploy", "shop", "production", []string{"web"}, summaryCmdTestStart, "hook failed: token=hunter2-super-secret")
	writeGitHubStepSummary(summary, nil)
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "hunter2-super-secret") || strings.Contains(renderSummaryText(summary, false), "hunter2-super-secret") {
		t.Fatalf("registered secret leaked into the summary renderings:\n%s", payload)
	}
}

// captureStreamsDraining redirects stdout and stderr while draining both
// pipes concurrently, so output larger than the OS pipe buffer (the summary
// event plus the result document) cannot deadlock the writer.
func captureStreamsDraining(t *testing.T, fn func()) (string, string) {
	t.Helper()
	drain := func(target **os.File) (func() string, func()) {
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatalf("pipe: %v", err)
		}
		original := *target
		*target = writer
		done := make(chan string, 1)
		go func() {
			payload, _ := io.ReadAll(reader)
			reader.Close()
			done <- string(payload)
		}()
		restore := func() { *target = original }
		return func() string {
			writer.Close()
			return <-done
		}, restore
	}
	finishOut, restoreOut := drain(&os.Stdout)
	finishErr, restoreErr := drain(&os.Stderr)
	func() {
		defer restoreOut()
		defer restoreErr()
		fn()
	}()
	return finishOut(), finishErr()
}

// installSummaryDeploy installs a fake deploy whose result carries a summary.
func installSummaryDeploy(t *testing.T) *fakeDeploySession {
	t.Helper()
	session := installFakeDeploy(t, false)
	summary := sampleDeploySummary()
	summary.Project = "demo"
	session.result.Summary = summary
	return session
}

func TestDeployNDJSONEmitsSummaryBeforeResult(t *testing.T) {
	setupHookTest(t, "")
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	installSummaryDeploy(t)
	eventsFormatFlag = eventsFormatNDJSON

	var runErr error
	stdout, stderr := captureStreamsDraining(t, func() {
		cliEngineInstance = nil
		runErr = runDeploy(&cobra.Command{}, nil)
	})
	if runErr != nil {
		t.Fatalf("runDeploy: %v", runErr)
	}
	type wireEvent struct {
		Type    string         `json:"type"`
		Phase   string         `json:"phase"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	var stream []wireEvent
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		var event wireEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("stdout line is not an event: %q", scanner.Text())
		}
		stream = append(stream, event)
	}
	if len(stream) < 2 {
		t.Fatalf("stream too short: %+v", stream)
	}
	summaryEvent, resultEvent := stream[len(stream)-2], stream[len(stream)-1]
	if summaryEvent.Type != events.TypeOperationSummary || summaryEvent.Phase != events.PhaseSummary || resultEvent.Type != events.TypeResult {
		t.Fatalf("last events = %s/%s, %s", summaryEvent.Type, summaryEvent.Phase, resultEvent.Type)
	}
	if summaryEvent.Data["operation"] != "deploy" {
		t.Fatalf("data.operation = %v", summaryEvent.Data["operation"])
	}
	result, _ := resultEvent.Data["result"].(map[string]any)
	if !reflect.DeepEqual(summaryEvent.Data["summary"], result["summary"]) || result["summary"] == nil {
		t.Fatalf("data.summary != result.summary\n%v\n%v", summaryEvent.Data["summary"], result["summary"])
	}
	if strings.Contains(summaryEvent.Message, "\033[") || !strings.Contains(summaryEvent.Message, "Deployed demo → production") {
		t.Fatalf("summary message = %q", summaryEvent.Message)
	}
	if !strings.Contains(stderr, "Deployed demo → production") {
		t.Fatalf("human summary not on stderr:\n%s", stderr)
	}
}

func TestDeployJSONOutputStaysOneDocumentWithSummary(t *testing.T) {
	setupHookTest(t, "")
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	installSummaryDeploy(t)
	outputFormatFlag = outputFormatJSON

	var runErr error
	stdout, stderr := captureStreamsDraining(t, func() {
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
	if result.Summary == nil || result.Summary.Outcome != engine.SummaryOutcomeSucceeded {
		t.Fatalf("summary = %+v", result.Summary)
	}
	if !strings.Contains(stderr, "Deployed demo → production") {
		t.Fatalf("human summary not on stderr:\n%s", stderr)
	}
}

func TestDeployTextModeEndsWithSummary(t *testing.T) {
	_, runner := setupHookTest(t, allDeployHooks)
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	t.Setenv("NO_COLOR", "1")
	installSummaryDeploy(t)
	runner.results[hooks.PostDeploy] = failedOutcome(1, errors.New("hook post-deploy exited with status 1"))

	var runErr error
	stdout, _ := captureStreamsDraining(t, func() {
		cliEngineInstance = nil
		runErr = runDeploy(&cobra.Command{}, nil)
	})
	if engine.Classify(runErr) != engine.ClassAttention {
		t.Fatalf("err = %v, want attention", runErr)
	}
	rule := strings.Repeat("─", summaryRuleWidth)
	if !strings.HasSuffix(stdout, rule+"\n") || !strings.Contains(stdout, "Deployed demo → production") {
		t.Fatalf("text output does not end with the summary block:\n%s", stdout)
	}
	tail := stdout[strings.LastIndex(stdout, "Deployed demo"):]
	if !strings.Contains(tail, "post-deploy hook failed (exit 1)") {
		t.Fatalf("summary does not mention the failed post-deploy hook:\n%s", tail)
	}
}

func TestDeployStepSummaryWrittenUnderNDJSON(t *testing.T) {
	setupHookTest(t, "")
	path := filepath.Join(t.TempDir(), "summary.md")
	t.Setenv("GITHUB_STEP_SUMMARY", path)
	t.Setenv("TAKO_STEP_SUMMARY", "")
	installSummaryDeploy(t)
	eventsFormatFlag = eventsFormatNDJSON

	var runErr error
	stdout, _ := captureStreamsDraining(t, func() {
		cliEngineInstance = nil
		runErr = runDeploy(&cobra.Command{}, nil)
	})
	if runErr != nil {
		t.Fatalf("runDeploy: %v", runErr)
	}
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if !json.Valid([]byte(line)) {
			t.Fatalf("stdout line is not JSON: %q", line)
		}
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), "Tako deploy · demo → production") {
		t.Fatalf("step summary = %s", payload)
	}
}
