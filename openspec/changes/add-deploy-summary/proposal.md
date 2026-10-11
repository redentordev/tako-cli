# Proposal

## Why

When `tako deploy` finishes, the operator has to piece together what happened from scrolling progress output:

- **What changed.** The plan table at the top shows *intended* changes. Nothing at the end confirms what actually changed, and from which image to which.
- **Unchanged services are not shown.** `DeployResult.services` lists only the services the run touched, except on the all-up-to-date fast path (`pkg/engine/deploy.go:696-698`).
- **No health picture.** Nothing reports whether each service is healthy and at its desired replica count after the rollout.
- **Undo is not discoverable.** The operator has to run `tako history`, find the right ID, and remember the rollback syntax.
- **URLs are printed too early.** They appear in the middle of the output, before the DNS/TLS check (`deploy.go:1286-1343`). The URL list is built by iterating a map, so its order changes from run to run (`deploy.go:1256-1263`).
- **Failure paths are thin.** A failed deploy ends with `takod deployment failed`. A bookkeeping failure after a successful rollout returns no result document at all (`deploy.go:1083-1136` return `nil, err`).

CI makes it worse. In GitHub Actions the answer to "did it work and what's live" is buried in a log that has to be grepped. `tako rollback`, `tako promote` and `tako scale` have the same gap.

## What Changes

- **Summary block.** `deploy`, `rollback` (both scopes), `promote` and `scale` end with one summary block. It is the last thing the command prints, after post-hooks and domain checks. It shows:
  - the operation, project, environment, outcome and duration, plus the deployment ID or display ID;
  - one row per service in scope, with change (`added`, `updated`, `unchanged`, `restored`, `failed`, `not_attempted`, `warmed`, `ran`, `rolled_back`, `promoted`, `scaled`, `skipped`, `removed`), strategy, image old→new, replicas running/desired, and health;
  - URLs, with domain readiness where it was checked;
  - next steps, such as `tako promote …` for warmed services and `tako logs …` for failed ones;
  - the exact undo command.
- **Machine contract.** Every result document of those four commands gains an additive `summary` object, an `OperationSummary`. With `--events ndjson`, a new additive event type `operation.summary` carries the same object in `data.summary`. It is emitted immediately before the terminal `result` event. Its `message` is the human rendering, so text mode prints the block through the existing human event sink.
- **GitHub Actions step summary.** When `GITHUB_STEP_SUMMARY` is set, Tako appends a Markdown rendering of the summary to that file. This happens in every output mode, because it is a side file and not stdout. Opt out with `TAKO_STEP_SUMMARY=off`. A write failure is a warning and never changes the exit code.
- **Undo command:**
  - **Deploy:** an explicit project rollback to the stable deployment that was current before this run, e.g. `tako rollback -e production 1767225000`.
  - **Rollback:** a roll-forward to the record it rolled away from.
  - **Promote:** `tako rollback --service NAME`.
  - **Scale:** `tako scale NAME=<previous>`.
  - **No safe undo** (first deploy, nothing changed, history unreadable): the undo is omitted with a reason.
  - `--config` is included when a non-default config path was used.
- **Redaction.** The summary is built from already-redacted strings. The event stream redacts only top-level string `data` values (`pkg/takoapi/events/sinks.go:165-178`), and result documents are not redacted at all. Images, service names, URLs and IDs are public values per v0.14.1. Error text, commit messages and next-step text go through `Engine.RedactSensitive`.
- **Supporting behavior changes:**
  - URL lists in `DeployResult.urls` and the summary are sorted.
  - The mid-output "Your application is available at" block is folded into the summary. Human text is not a contract.
  - Post-rollout bookkeeping failures in deploy now return the partial `DeployResult` (status `failed`, `error` set, summary present) instead of `nil`. Exit codes do not change.
- **Preview environments.** The summary carries `environment.kind` (`standard` today) and per-URL `kind`. The sibling change `add-preview-environments` can then report `kind: preview` and an expiry without redesigning the summary.
- No **BREAKING** changes. Every field and event type is additive, existing result fields keep their meaning, and exit codes do not change.

## Out of Scope

- Redaction labels such as `[REDACTED:NAME]` (sibling change `explain-secret-redaction`).
- Preview environment lifecycle (sibling change `add-preview-environments`). This change only reserves the `kind` fields.
- Summaries for `remove`, `destroy`, `setup`, `jobs trigger` and `run`. They can adopt `OperationSummary` later.
- Non-GitHub CI summary files (GitLab, Buildkite annotations). The writer is pluggable, but only GitHub ships.
- A published GitHub Action or PR comments. The step summary is a file write only.
- Persisting summaries in deployment history.

## Capabilities

### New Capabilities
- `deploy-summary`: the end-of-operation summary for deploy, rollback, promote and scale. It covers the human block, the `OperationSummary` machine object and `operation.summary` event, undo-command derivation, health and URL reporting, failure-path content, redaction, and the GitHub Actions step summary.

### Modified Capabilities
- None. The rollback result additions are additive fields described under `deploy-summary`.

## Impact

- **Code:**
  - New `pkg/engine/summary.go`: types and builder.
  - `pkg/engine/types.go`: the `DeployResult.Summary` field.
  - `pkg/engine/deploy.go`: plan-time "from" images and the previous stable deployment ID, summary on every return path, sorted URLs, the URL block removed, partial result on bookkeeping failure.
  - `pkg/engine/rollback.go`, `rollback_project.go`, `promote.go`, `scale.go`: summaries.
  - `pkg/engine/status.go`: reusable post-operation health read.
  - `pkg/takoapi/events/events.go`: `TypeOperationSummary`.
  - New `cmd/summary.go`: emit, text render, GitHub writer.
  - `cmd/deploy.go`, `cmd/rollback.go`, `cmd/promote.go`, `cmd/scale.go`: emit the summary after hooks, before the result document.
- **takod:** no changes. Health comes from the existing actual-state reads that `tako ps` uses.
- **Docs:** `docs/MACHINE-INTERFACE.md` (summary object, event, ordering), `docs/CI-CD.md` (step summary and opt-out), `docs/API-SDK.md` (summary type).
- **Tests:** summary builder unit tests, deploy/rollback/promote/scale engine tests, `cmd/machine_output_test.go` golden documents and event order, the GitHub writer, and redaction.
