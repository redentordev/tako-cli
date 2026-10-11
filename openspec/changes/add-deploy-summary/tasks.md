# Tasks

## 1. Summary model and builder (pure)

- [x] 1.1 Add `pkg/engine/summary.go` with the `OperationSummary`, `SummaryEnvironment`, `SummaryService`, `SummaryReplicas`, `SummaryURL`, `SummaryUndo` and `SummaryNextStep` types (design D1), and change/outcome constants (D2, D9). Verify with a JSON golden test of a fully populated summary and an `omitempty` test of a minimal one.
- [x] 1.2 Add `summaryBuilder` (constructed with the engine's redactor) with methods to add rows, URLs, undo, next steps and notes. It sorts rows and URLs, derives `outcome` from rows plus status (an unrestored `failed` row makes a failure `partial`, D9), caps next steps at 5, and redacts every free-text field (D11). Verify with table tests for each outcome (`succeeded`, `up_to_date`, `warmed`, `partial` via a kept change, `partial` via recovery status `failed`, `failed`), and a redaction test where a registered secret inside an error is masked while an image and service name containing a registered short secret are not.
- [x] 1.3 Add `undoFor*` helpers: deploy (previous stable ID, first deploy, nothing live, persistent note), project rollback (roll-forward), single-service rollback, promote (retained `fromRevision`, else rollback pinned to the newest stable record before the promote record that contains the service, else unavailable), and scale. Each produces argv and a POSIX-quoted command (D6) through one environment-selector helper. Verify with table tests, including `--config` with spaces, that `-y` is absent, and that no undo omits its target.
- [x] 1.4 Add the next-step derivation (D7). Verify with table tests: warmed, failed, restored, pending domain, degraded, and partial.

## 2. Health read

- [x] 2.1 Factor the per-node actual-state read and worst-wins health and running/desired aggregation out of `pkg/engine/status.go` into a reusable `readServiceHealth(ctx, servers, services, timeout)` that returns per-service desired, running, health and nodes. `tako ps` keeps its output. Verify that the existing status tests still pass, and with a new test that the health mapping gives `healthy`, `unhealthy`, `starting`, `degraded`, `stopped` and `unknown`-on-error.
- [x] 2.2 Apply a 10s timeout with per-node parallel reads, and map errors to `unknown` plus a redacted note. Verify with a fake reader that blocks past the timeout.

## 3. Deploy integration

- [x] 3.1 In `PlanDeploy`, keep the plan-time `actualState` images and revisions per service (already on the session). Load history once under the lease (reuse the existing call at `deploy.go:507-516`) and record `previousStableDeploymentID`, or a non-fatal `historyErr`. Verify with an engine test using a fake history: stable predecessor, none, and a load error.
- [x] 3.2 Keep the `MonitorDomainStatuses` return value and map it to per-URL `ready`/`pending`/`failed`; use `not_checked` when skipped or internal. Sort `result.URLs` and `result.InternalURLs`. Verify with tests for ordering stability and status mapping.
- [x] 3.3 Add `finishSummary` and call it on every `Apply` return path: fast path, failure, strict-domain error, success. It first redacts `result.Error` and every `result.Services[].Error` in place (raw today at `deploy.go:962`, `:1197`). Convert the bookkeeping-failure returns (`deploy.go:1083-1136`) to return the populated result (status `failed`, redacted `error`, summary outcome `partial` with undo, a note) together with the unchanged error. Verify with engine tests per path, asserting the summary exists, the error class is unchanged (`engine.Classify`), the rows include `not_attempted` services after a failure, and a registered secret inside a health-failure error is absent from `result.error` and `services[].error`.
- [x] 3.4 Remove the mid-output URL block (`deploy.go:1286-1325`) and move the `tako domains hosts` hint into summary notes. Update golden transcripts.
- [x] 3.5 In `cmd/deploy.go`, give `failedDeployResult` a `failed` summary with planned services `not_attempted` and undo `unavailable: "nothing was changed"`. Verify with the existing pre-hook failure test, extended.

## 4. Rollback, promote, scale

- [x] 4.1 Project rollback (`rollback_project.go`): build rows from `RollbackServiceOutcome`; undo is the roll-forward to the newest record before the rollback. Single-service rollback (`rollback.go`): one row; undo is `--service NAME <prev displayId>`. Verify with engine tests for `rolled_back`, `up_to_date` (no undo), and partial with `not_attempted`.
- [x] 4.2 Promote (`promote.go`): one row with from/to revision; undo per D6. When `fromRevision` is not retained, do a best-effort history read (non-fatal) and pin the rollback to the newest stable record before the promote record that contains the service; on a read error the undo is unavailable with `deployment history unreadable` and a `tako history` next step. Verify the retained, not-retained, no-stable-predecessor and history-error cases.
- [x] 4.3 Scale (`scale.go`): rows with `scaled`/`unchanged` and replicas; undo with the previous counts. Redact `ScaleResult` error fields in place (`scale.go:278`, `:302`). Verify with a multi-service scale test and a registered secret inside a scale error.
- [x] 4.4 Add the `Summary *OperationSummary json:"summary,omitempty"` field to `RollbackResult`, `PromoteResult` and `ScaleResult` (and `DeployResult` in `types.go`). Verify with JSON golden tests that existing fields are byte-identical apart from the added `summary`.

## 5. CLI emission and rendering

- [x] 5.1 Add `events.TypeOperationSummary = "operation.summary"` and `events.PhaseSummary = "summary"`.
- [x] 5.2 Add `cmd/summary.go`:
  - `emitOperationSummary(summary)` emits the event (message = text rendering, `data.summary`, `data.operation`) and runs the step-summary writer.
  - `withConfigFlag(undo)` inserts `--config` when `cfgFile != ""`.
  - Call it from `deploy`, `rollback`, `promote` and `scale` after hook outcomes are attached (adding hook-failure notes), immediately before `emitResultDocument`.

  Verify with `cmd/machine_output_test.go`: in NDJSON, `operation.summary` is second to last and `data.summary` deep-equals `result.summary`; `--output json` stdout is one document; in text mode the block is the last stdout output.
- [x] 5.3 Add `renderSummaryText(summary, color)`: header, table, URLs, error, next steps, undo; compact `repo:tagA → :tagB`; no ANSI when NO_COLOR or not a TTY. Verify with golden text tests for success, partial, up-to-date, warmed and first deploy.

## 6. GitHub Actions step summary

- [x] 6.1 Add `writeGitHubStepSummary(summary)`. It is gated on `GITHUB_STEP_SUMMARY` and on `TAKO_STEP_SUMMARY` not being `off`, `0`, `false` or `no` (case-insensitive). It appends with `O_APPEND|O_CREATE`, escapes Markdown, caps output at 200 rows and 64 KiB with a truncation note, and turns errors into a `warning` event. Verify with tests: written to a temp file, opt-out, unset, unwritable path (warning, exit unchanged), truncation, and a redacted secret absent.

## 7. Docs

- [x] 7.1 `docs/MACHINE-INTERFACE.md`: document the `OperationSummary` schema, the `change`/`outcome`/`health`/URL `status` vocabularies, the `operation.summary` event and its ordering, the undo semantics, the redaction guarantee, and that `urls` are now sorted.
- [x] 7.2 `docs/CI-CD.md`: add a step-summary section (what it shows, `TAKO_STEP_SUMMARY=off`, works with Gitea and Forgejo runners) and a note on reading `summary.undo.argv` in pipelines.
- [x] 7.3 `docs/API-SDK.md`: add the summary type to the result types list. Check the README command output examples.

## 8. Verification

- [x] 8.1 Run `go build ./...` and `go vet ./...`. Run `go test ./...` on Linux (WSL Ubuntu-24.04) and compare against master: no new failures. Check `gofmt -l` on Linux.
- [ ] 8.2 Manual run against a test server: deploy (first and second), a forced health failure with restore, a rollback using the printed undo command, promote, and scale. Check the text block, `--events ndjson` ordering, and the step-summary file in a GitHub Actions run.
