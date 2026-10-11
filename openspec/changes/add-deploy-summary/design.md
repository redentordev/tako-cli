# Design

## Context

See proposal.md for motivation, and specs/deploy-summary/spec.md for the behavior contract. Current state (pre-change citations):

- **Result documents:**
  - `DeployResult` (`pkg/engine/types.go:220-243`) already has `services[]` (`ServiceOutcome`: name, image, action, replicas, strategy, release, run, recovery, error), `urls`, `internalUrls`, `startedAt`, `durationSeconds`, `deploymentId` and `hooks`.
  - `RollbackResult` (`pkg/engine/rollback.go:82-103`) has `services[]` (`RollbackServiceOutcome`, `rollback_project.go:41-56`) for project scope.
  - `PromoteResult` (`promote.go:49-63`) and `ScaleResult` (`scale.go:43-54`) are smaller.
  - None carries a previous image, health, or an undo hint.
- **Where data lives in deploy:**
  - **Pre-mutation actual state** (per-service `Image`, `Replicas`, `CurrentRevision`): gathered in `PlanDeploy` and kept on the session (`deploy.go:460-466`, `session.actualState`).
  - **Final actual state**: gathered after a successful rollout (`deploy.go:1087-1091`).
  - **Health**: the same `takod.ActualService.Health` aggregation that `tako ps` uses (`pkg/engine/status.go:44-79`, worst-wins health and per-node running counts).
  - **Domains**: DNS/TLS readiness comes from `MonitorDomainStatuses` (`deploy.go:1334`). Its `[]health.DomainStatus` return value is currently discarded.
  - **URLs**: built from a map iteration, so the order is nondeterministic (`deploy.go:1256-1263`). They are printed mid-output before domain checks (`deploy.go:1286-1325`).
- **Return paths in `DeploySession.Apply`:**
  - up-to-date fast path (`deploy.go:677-701`);
  - failure path, which returns the result with an error (`deploy.go:1161-1212`);
  - success (`deploy.go:1345-1348`);
  - several post-success bookkeeping failures that return `nil, err` (`deploy.go:1083-1136`);
  - a pre-hook failure, where cmd synthesizes `failedDeployResult` (`cmd/deploy.go:246-263`).
- **Output plumbing:**
  - `emitResultDocument` (`cmd/engine.go:68-84`) emits the terminal `result` event (NDJSON) and/or the JSON document. Text mode emits nothing.
  - `humanEventSink` (`cmd/engine.go:213-243`) prints any non-debug event's `message` verbatim, to stdout in text mode and to stderr in machine modes.
  - Post hooks run after `Apply` returns (`cmd/deploy.go:195-217`).
- **Redaction:** `events.Stream.Emit` redacts `message` and only top-level string `data` values (`pkg/takoapi/events/sinks.go:165-178`). Nested structs pass through. Result documents are not redacted wholesale; individual fields such as `Error` are redacted at the source (`cmd/deploy.go:260`). Since v0.14.1, project, environment, server and service names and image refs are registered public (`RegisterPublicConfigNames`).
- **Rollback syntax** (`cmd/rollback.go:34-86`, docs/MACHINE-INTERFACE.md "Rollback"):
  - `tako rollback [target]` is the project scope. The target is an exact ID, a unique ID prefix (the 10-character display ID from `FormatDeploymentID`, `internal/state/manager.go:409`), or a commit.
  - `tako rollback --service NAME [target]` is the single-service scope.
  - The default project target is the newest stable record strictly older than the newest record (`SelectProjectRollbackTarget`, `pkg/engine/rollback_target.go:61`).
  - Project rollback prompts unless `-y`, and skips `persistent: true` services unless `--include-persistent`.
- **Promote** (`promote.go:374-394`): candidate revisions include `actual.PreviousRevision`, so promoting the previous revision back is possible while that revision is still retained.
- **Scale** (`scale.go:240`): reads the current replica count before changing it.
- **Event compatibility rules:** consumers must ignore unknown types and fields (`events.go:49`, MACHINE-INTERFACE "Compatibility Promises"). New types and fields are additive within `tako.redentor.dev/v1alpha1`.
- **GitHub Actions:** nothing reads `GITHUB_STEP_SUMMARY` today.

## Goals / Non-Goals

**Goals:**
- One uniform `OperationSummary` shape across deploy, rollback, promote and scale. It is the same object in the result document, the NDJSON event, the human block and the GitHub Markdown.
- The summary is always produced when the command produces a result document, including on failure.
- An undo command that is copy-pasteable and pinned to an explicit target.
- No new remote round trips on the critical path, except one bounded, best-effort health read.

**Non-Goals:**
- Changing existing result fields, outcome action strings, or exit codes.
- Storing summaries in history.
- Non-GitHub CI integrations.

## Decisions

### D1. `OperationSummary` type (new `pkg/engine/summary.go`)

```go
type OperationSummary struct {
    Operation    string              `json:"operation"`    // deploy | rollback | promote | scale
    Project      string              `json:"project"`
    Environment  SummaryEnvironment  `json:"environment"`
    Outcome      string              `json:"outcome"`      // succeeded | warmed | up_to_date | partial | failed
    DeploymentID string              `json:"deploymentId,omitempty"`
    DisplayID    string              `json:"displayId,omitempty"`
    Revision     string              `json:"revision,omitempty"`
    Commit       string              `json:"commit,omitempty"` // short hash
    StartedAt    time.Time           `json:"startedAt"`
    DurationSeconds float64          `json:"durationSeconds"`
    Services     []SummaryService    `json:"services"`
    URLs         []SummaryURL        `json:"urls,omitempty"`
    Undo         *SummaryUndo        `json:"undo,omitempty"`
    NextSteps    []SummaryNextStep   `json:"nextSteps,omitempty"`
    Error        string              `json:"error,omitempty"`
    Notes        []string            `json:"notes,omitempty"`
}
type SummaryEnvironment struct {
    Name string `json:"name"`
    Kind string `json:"kind"` // "standard"; add-preview-environments adds "preview"
}
type SummaryService struct {
    Name         string          `json:"name"`
    Change       string          `json:"change"`   // see D2
    Strategy     string          `json:"strategy,omitempty"`
    FromImage    string          `json:"fromImage,omitempty"`
    ToImage      string          `json:"toImage,omitempty"`
    FromRevision string          `json:"fromRevision,omitempty"`
    ToRevision   string          `json:"toRevision,omitempty"`
    Replicas     *SummaryReplicas `json:"replicas,omitempty"`
    Health       string          `json:"health"`   // see D4
    Nodes        []string        `json:"nodes,omitempty"`
    Reason       string          `json:"reason,omitempty"`
    Error        string          `json:"error,omitempty"`
}
type SummaryReplicas struct{ Desired, Running int }   // json: desired, running
type SummaryURL struct {
    Service string `json:"service"`
    URL     string `json:"url"`
    Kind    string `json:"kind"`             // public | internal; consumers treat other values as opaque
    Status  string `json:"status,omitempty"` // ready | pending | failed | not_checked
    Detail  string `json:"detail,omitempty"`
}
type SummaryUndo struct {
    Command string   `json:"command,omitempty"` // shell-quoted, ready to paste
    Argv    []string `json:"argv,omitempty"`
    TargetDeploymentID string `json:"targetDeploymentId,omitempty"`
    Unavailable string `json:"unavailable,omitempty"` // reason when no command
    Notes   []string `json:"notes,omitempty"`
}
type SummaryNextStep struct {
    Reason  string `json:"reason"`
    Command string `json:"command"`
}
```

Placement:
- `DeployResult`, `RollbackResult`, `PromoteResult` and `ScaleResult` each gain `Summary *OperationSummary json:"summary,omitempty"`.
- Rows are sorted by name in JSON. The text renderer keeps that order.
- `Environment.Kind` is always set, to `standard`. Consumers must tolerate other values, which is how preview environments plug in.
- `URLs[].Kind` follows the same rule.

*Alternative:* put the fields directly on each result. Rejected because there would be four divergent shapes, and the NDJSON event, renderer and GitHub writer want one type.

### D2. Normalized `change` vocabulary

The summary does not reuse each document's `action` strings. They differ per command (`deployed` vs `rolled_back` vs `up_to_date`), and `deployed` does not say whether the service was added or updated. The mapping:

| Source | `change` |
| --- | --- |
| deploy `ChangeAdd` + `deployed` | `added` |
| deploy `ChangeUpdate` + `deployed` (or `--force`/build reconcile) | `updated` |
| `up_to_date`, or a service in scope but not in `servicesToDeploy` | `unchanged` |
| `warmed` | `warmed` |
| `restored` (recovery restored) | `restored` |
| `failed` | `failed` |
| planned but after the first failure | `not_attempted` |
| `ran` (`kind: run`) | `ran` |
| `removed` | `removed` |
| jobs whose schedule was applied | `updated` (strategy empty) |
| rollback `rolled_back` / `unchanged` / `skipped` / `failed` / `not_attempted` | same word |
| promote success | `promoted` |
| scale | `scaled` (or `unchanged` when the count is equal) |

`strategy` carries `recreate` / `rolling` / `blue_green` for long-running services, so "recreated" vs "rolled" is visible without adding more change words.

The service scope:
- **Deploy:** every service in `session.services`. With `--service`, that is the targeted service plus its run prerequisites. Removals come from the plan.
- **Rollback:** all services in the project-rollback outcome list, or the single service.
- **Promote:** one service.
- **Scale:** the services named on the command line.

### D3. Images and revisions

- **Deploy:**
  - `fromImage` / `fromRevision` come from the plan-time `session.actualState[name]` (`Image`, `CurrentRevision`).
  - `toImage` is the outcome image. `toRevision` comes from final actual state, when it was read.
  - Unchanged services get `fromImage == toImage`. The human renderer prints the image once.
- **Rollback:** `fromImage` / `toImage` from `RollbackServiceOutcome`. Single scope uses the pre-rollback actual image and the target `ServiceState.Image`.
- **Promote:** `fromRevision` is the old `CurrentRevision`, `toRevision` the promoted one.
- **Scale:** images are equal. Replicas are shown as old→new in `replicas.desired` plus `notes`.

Human rendering compacts `repo:tagA → repo:tagB` to `repo:tagA → tagB` when the repositories match. JSON always keeps full refs.

### D4. Health and replicas: one bounded best-effort read

After the mutation, and only for operations that changed something, the builder calls a new `Engine.summaryHealth(ctx, servers, services)`:
- It reuses the status aggregation from `status.go` (per-node `takod.ActualService` reads, worst-wins `Health`, running vs desired). Deploy already holds `finalNodeActualState` on success (`deploy.go:1087`), but that is `reconcile.ActualService` without `Health`, so a separate read is needed.
- It is bounded by a 10-second timeout and runs in parallel per node.
- On error or timeout, every row gets `health: "unknown"` and a note `health not read: <redacted error>`. It never fails the operation and never changes the exit code.

`health` values:

| Value | Meaning |
| --- | --- |
| `healthy` | Every active container is healthy, or there is no Docker healthcheck and running == desired |
| `unhealthy` | At least one active container reports unhealthy |
| `starting` | At least one container's healthcheck is still starting |
| `degraded` | running < desired |
| `stopped` | desired 0 or running 0 by intent (scaled to 0) |
| `unknown` | The read failed or was skipped |
| `n/a` | `kind: run` / `kind: job` rows, `removed`, `not_attempted` |

Reasons for the extra read:
- The rollout's health gate already proved readiness at cutover.
- The post-read additionally catches crash-looping replicas on other nodes and partial replica placement. That is what an operator wants to see last.
- Its cost is one parallel round trip, comparable to `tako ps`.

### D5. URLs and domain readiness

- URLs come from the same sources as today (`Proxy.GetAllDomains()` for public routes, `GetAllHosts()` for internal routes). They are sorted by service, then URL. `DeployResult.urls` and `internalUrls` become sorted as well; the content is unchanged.
- When `MonitorDomainStatuses` ran, its return value is kept and mapped per domain:
  - `ready`: DNS ok and TLS ok.
  - `pending`: still waiting at the timeout, without strict mode.
  - `failed`: strict-mode failure, or a TLS/DNS error state.
  - `detail` carries the redacted message.
- With `--skip-domain-check`, or for internal hosts, the status is `not_checked`.
- `dynamicDomains` are not listed, because they are open-ended. Notes say "dynamic domains enabled for <service>".
- The mid-output URL block (`deploy.go:1286-1325`) is removed. The `tako domains hosts` hint moves into `notes`. The "✓ Deployment completed successfully!" line stays as progress output.

### D6. Undo command derivation

The builder produces `argv` and `command`. `command` is a POSIX-shell-quoted join, so it is copy-pasteable on Windows PowerShell for these argument shapes, which use no special characters. The cmd layer adds `--config <path>` when `cfgFile != ""`. The environment is always selected explicitly: `-e <env>` for a declared environment. (The sibling `add-preview-environments` substitutes `--preview <name>` for a preview, because `-e preview-<name>` names no declared environment; every command the builder emits goes through one environment-selector helper so that substitution is a single change.) `argv` and `command` carry only config names, deployment IDs, revisions, replica counts and the config path, so they are not passed through the redactor and stay runnable.

| Operation | Undo |
| --- | --- |
| deploy, outcome `succeeded` or `partial` (D9) | `tako rollback -e ENV <displayId of P>` where P is the newest stable record (`success`/`rolled_back`) before this deploy's record. `targetDeploymentId` = P's full ID. |
| deploy with no stable predecessor (first deploy) | none: `unavailable: "first deployment; nothing to roll back to"`; next step `tako destroy`/`tako remove` is **not** suggested |
| deploy `up_to_date` / `warmed` (nothing went live) | none: `unavailable: "nothing changed"` / `"warmed revision is not serving; nothing to undo"` |
| deploy where a changed service is `persistent: true` | the command as above plus a note: "persistent services X are skipped by project rollback; add --include-persistent to include them" (the flag is not added automatically) |
| project rollback `rolled_back`/`failed` | `tako rollback -e ENV <displayId of the newest record before the rollback ran>`, which rolls forward to what was rolled away from |
| single-service rollback | `tako rollback -e ENV --service NAME <displayId of the record before the rollback>` |
| promote | `tako promote NAME -e ENV --revision <fromRevision>` when the post-promote actual state (already gathered by promote, `promote.go:275`) still lists `fromRevision` as retained; otherwise `tako rollback -e ENV --service NAME <displayId of R>` where R is the newest stable record older than the promote's own record that contains NAME. R comes from one best-effort history read after the promote record is saved; on failure the undo is unavailable with `deployment history unreadable` and next step `tako history -e ENV` |
| scale | `tako scale -e ENV NAME=<old> [NAME2=<old2>]` |

How P is found:
- `PlanDeploy` already holds the lease. It loads history once, which today it does only for run services (`deploy.go:507-516`), and records `previousStableDeploymentID`.
- A history load error is non-fatal. The summary then has `unavailable: "deployment history unreadable"` and a next step `tako history -e ENV`.
- This is one extra remote read at plan time. It is cheap compared with the actual-state gather, and it happens under the already-held lease.

Why the target is explicit rather than a bare `tako rollback`:
- The default target moves as soon as anyone deploys again.
- An explicit target in a CI log stays correct.
- The displayed `displayId` is accepted by rollback's prefix resolution. If it ever becomes ambiguous, rollback fails closed with exit 2, so a stale command cannot silently do the wrong thing.

The command omits `-y`, so a human sees the plan and confirms.

Every undo is pinned. An unpinned `tako rollback --service NAME` is never emitted: its default target moves with the next deploy, so a command copied from an old CI log could roll back the wrong release.

### D7. Next steps

These are emitted in order and redacted:

| Situation | Next step |
| --- | --- |
| A warmed service | `tako promote NAME -e ENV` (replaces the separate "Promote when ready" line's role in the summary; that progress line stays) |
| A failed or restored service | `tako logs -e ENV --service NAME --since 10m` |
| A domain `pending` / `failed` | `tako domains status -e ENV DOMAIN` |
| Health `unhealthy` / `degraded` | `tako ps -e ENV` |
| Partial failure | `tako history -e ENV` |

There are at most 5 entries.

### D8. Building on every return path

- `DeploySession.Apply` gets a single `finishSummary(result, err)` helper, called on each return:
  - the fast path;
  - the failure path;
  - success;
  - strict-domain error.
- The bookkeeping-failure returns at `deploy.go:1083-1136` change from `return nil, err` to setting `result.Status = failed` and `result.Error = redacted`, building the summary with `outcome: partial` (the rollout is live, so rows keep `added`/`updated` and the undo is derived as usual) and the note "rollout succeeded; state bookkeeping failed: …", and returning `result, err`. The error, and therefore the exit class, is unchanged. Today those paths emit no result document, so this is additive.
- `failedDeployResult` (pre-hook failure) builds a summary with `outcome: failed`, every planned service `not_attempted`, and no undo (`unavailable: "nothing was changed"`).
- The cmd layer attaches hook outcomes, then calls `emitOperationSummary(result.Summary)`, then `emitResultDocument(result)`. This lets the summary mention a failed post-deploy hook in `notes`, e.g. "post-deploy hook smoke failed (exit 1)".

Rollback, promote and scale build the summary at the end of their engine method, on every path that returns a result. The cmd layer adds hook notes the same way.

### D9. Outcome

| `outcome` | When |
| --- | --- |
| `succeeded` | Status success / rolled_back / promoted, with at least one service changed |
| `up_to_date` | Nothing changed |
| `warmed` | Deploy succeeded, but manual-promotion services are only warmed (`DeploymentSuccessStatus`) |
| `partial` | Failed, but at least one service differs from its pre-operation state: it kept a new state (`added`/`updated`/`rolled_back` before the failure), or it failed and its recovery status is not `restored` (`failed`, `disabled`, `unsupported`) |
| `failed` | Failed with every service back at its pre-operation state (all restored, not attempted, or failed before cutover) |

Exit codes are unchanged. `partial` and `failed` both exit 1, as today. The `partial` rule deliberately counts an unrestored failure as a change: that is exactly the case where the operator needs the undo command most.

### D10. Emission: event, human text, JSON

`cmd/summary.go` adds `emitOperationSummary(s *engine.OperationSummary)`, which emits:

```go
events.Event{Type: events.TypeOperationSummary, Phase: events.PhaseSummary, Level: info,
             Message: renderSummaryText(s, colorEnabled), Data: {"summary": s, "operation": s.Operation}}
```

- **Text mode:** `humanEventSink` prints `message` to stdout, as the last block before exit.
- **Machine modes:** the human message goes to stderr (existing sink behavior). In NDJSON mode the event is on stdout, immediately before `result`.
- **`--output json`:** stdout still holds exactly one document, the result, which contains `summary`.
- The stream redacts `message`. `data.summary` is a struct and is not walked, which is why D11 pre-redacts.

New constants: `TypeOperationSummary = "operation.summary"` and `PhaseSummary = "summary"`. Both are additive.

The text layout (width-aware, no color when NO_COLOR or not a TTY):

```
──────────────────────────────────────────────────────────────
 ✓ Deployed shop → production in 1m42s · deployment 1767225600 · ab12cd3
──────────────────────────────────────────────────────────────
 SERVICE  CHANGE     STRATEGY  IMAGE                            REPLICAS  HEALTH
 api      updated    rolling   ghcr.io/acme/api:ab12cd3 → ef45  2/2       healthy
 web      updated    rolling   ghcr.io/acme/web:ab12cd3 → ef45  3/3       healthy
 postgres unchanged  recreate  postgres:16                      1/1       healthy
 migrate  ran        -         exit 0 in 4s                     -         n/a

 URLs
   web  https://shop.example.com      ready
   web  https://www.shop.example.com  pending (TLS not issued yet)

 Undo
   tako rollback -e production 1767225000
──────────────────────────────────────────────────────────────
```

Failure variant: the header reads `✗ Deploy failed (partial)`, an `Error` line follows the table, and next steps come before Undo.

### D11. Redaction

`newSummaryBuilder(e *Engine)` passes these fields through `e.RedactSensitive`:
- `Error`, `Reason`, `Notes`, `NextSteps[].Command`, `Undo.Notes`, `URLs[].Detail` and `SummaryService.Error`;
- the `Commit` string, defensively.

Images, URLs, service, environment and project names, IDs and revisions are emitted as-is. They are public by v0.14.1's `RegisterPublicConfigNames`, so they would survive redaction anyway, and redacting them again could only mask a value that is also registered as a secret, which is the intended behavior.

The GitHub writer and text renderer consume only the built summary, so they inherit the redaction. A test registers a secret that appears in an error string and asserts that it is absent from the JSON, the event, the text and the Markdown.

The result documents themselves have the same gap today: deploy copies raw error text into `DeployResult.error` and `services[].error` (`deploy.go:962`, `:1197`), and scale does the same (`scale.go:278`, `:302`). Health-failure errors embed recent container log lines, so `--output json` can carry a secret that the event stream masks. Because `finishSummary` (and the scale builder) already touches every return path, it also redacts those existing fields in place with `e.RedactSensitive` before the summary copies them. This changes only text that contained a registered secret or matched a pattern, which the documented redaction guarantee already forbids in result documents. Rollback and promote already redact at the source.

Re-redacting text is safe: a second pass cannot reveal anything, and the sibling `explain-secret-redaction` (D5) makes it byte-identical, so a field the summary re-renders through the event stream keeps its markers intact.

### D12. GitHub Actions step summary

`writeGitHubStepSummary(s)` runs inside `emitOperationSummary` when all of these hold:
- `GITHUB_STEP_SUMMARY` is non-empty;
- `TAKO_STEP_SUMMARY` is not `off`, `0`, `false` or `no` (case-insensitive; the same vocabulary as `TAKO_REDACTION_LABELS` in `explain-secret-redaction`);
- the summary is non-nil.

It appends (`O_APPEND|O_CREATE`, mode 0644) Markdown:
- `### ✅ Tako deploy · shop → production` (or `❌ … failed` / `⚠️ … partial`);
- a one-line meta row (duration, deployment ID, commit);
- a service table, with backticks around images;
- a URL list;
- the undo command in a fenced `sh` block;
- next steps;
- the error inside `<details>`, as a fenced code block so redaction markers and other text render verbatim instead of as Markdown.

Limits:
- The writer caps at 200 service rows and 64 KiB (GitHub's limit is 1 MiB per step), with a truncation note.
- Markdown-special characters in free text (`|`, `<`, `` ` ``) are escaped.
- A write error emits a `warning` event, `step summary not written: …`, and never changes the exit code.

It runs regardless of `--output` / `--events`, because the file is not stdout.

*Alternative:* gate on `GITHUB_ACTIONS=true` as well. Rejected, because Gitea and Forgejo runners set `GITHUB_STEP_SUMMARY` with the same semantics. Checking only the variable that names the file is the precise condition.

## Risks / Trade-offs

- [The extra health read slows every deploy end by up to the timeout] → Parallel per node, a 10s cap, skipped when nothing changed. Typical cost is under 1s on a single node.
- [The summary duplicates fields already on `DeployResult` (`urls`, `durationSeconds`)] → Accepted. Existing fields keep their meaning for compatibility, and `summary` is the presentation-ready, cross-command view. Docs say new integrations should read `summary`.
- [A stale undo command if someone deploys in between] → The target is explicit, so it still restores the intended state. Rollback's plan and confirmation show exactly what would change.
- [`not_attempted` rows for deploy are a new concept for `DeployResult` consumers] → They appear only in `summary.services`. `DeployResult.services` keeps listing attempted services only.
- [Returning a result on bookkeeping failure changes what JSON consumers see on those paths] → Previously there was no document, which was a contract gap. Status `failed` and exit codes are unchanged.
- [Text-mode output changes (URL block moves)] → Human text is not a contract. Golden transcript tests are updated.
- [The undo command is not runnable as-is on Windows `cmd.exe` when the config path has spaces] → The config path is quoted POSIX-style. Docs note it. The argv array is the exact form for automation.
- [Redacting `DeployResult.error`, `services[].error` and the scale equivalents changes those strings] → Only text that carried a registered secret or matched a secret pattern changes; leaving it raw contradicts the documented redaction guarantee.
- [The pinned promote fallback needs one more history read] → Best effort after the promote record is saved, under the leases promote already holds; a failure only makes the undo unavailable.

## Migration Plan

- Pure CLI change, with no takod or state-format change. Older CLIs ignore nothing, since they never see the new fields.
- Machine consumers may start reading `summary` / `operation.summary` immediately. Nothing they read today changes, except that `urls` is now sorted.
- Sibling ordering:
  - `add-preview-environments` sets `Environment.Kind = "preview"`, adds `Environment.Preview = {name, base, expiresAt}` (shape owned by that change's design), and swaps the environment selector in derived commands to `--preview <name>`. Preview URLs keep `kind: public`/`internal`, so a consumer that filters public URLs works for previews unchanged. All of these are additive. If it lands first, it adds those fields to this type as part of its own work.
  - `explain-secret-redaction` changes the mask text only, and summaries inherit it automatically.

## Assumptions

- A1. `takod.ActualService.Health` aggregation from `status.go` is available on every node agent that supports deploy. Older agents leave it empty, and the row falls back to the running/desired comparison.
- A2. Loading history in `PlanDeploy` under the held lease is safe and cheap, since it is the same call used for run services today.
- A3. `FormatDeploymentID` display IDs (10 characters) stay unique in practice. Rollback fails closed if they are not.

## Resolved questions

1. **Are `scale` and `promote` in scope for v1?** Yes. They share the builder and the additive `summary` field, and a uniform shape across mutating commands is the point of the change.
2. **Does the human summary also print in machine modes (to stderr)?** Yes, with no extra flag. It follows the existing `humanEventSink` rule for every human message, so stdout stays parseable.
3. **Is there an opt-out for the human block?** No, not in v1. The block replaces the mid-output URL block rather than adding noise, and an opt-out can be added later without breaking anything.
4. **Does the undo command include `-y`?** No. A human should see the rollback plan first; automation appends `-y` to `undo.argv`.
5. **Does promote undo prefer `--revision <previous>` over a rollback?** Yes, when the post-promote actual state still retains the previous revision. Otherwise the fallback is a rollback pinned to an explicit record (changed from the unpinned `tako rollback --service NAME`, whose default target moves with the next deploy).
6. **Is the step summary on by default?** Yes, whenever `GITHUB_STEP_SUMMARY` is set, with `TAKO_STEP_SUMMARY=off|0|false|no` to disable. It is a side file, so it cannot break stdout or exit codes; the opt-out vocabulary matches `TAKO_REDACTION_LABELS`.
7. **Does the health read wait for `starting` containers?** No. It reports `starting` truthfully; the rollout gate already waited, and a second wait would slow every deploy.
8. **Is the event `operation.summary` or `deploy.summary`?** `operation.summary`. It covers four commands and `data.operation` distinguishes them, as rollback events already do.

Review additions (not open questions in the draft):
- **Bookkeeping failure after a live rollout is `partial`, not `failed`** (D8). The new state is serving, so the operator needs the undo command.
- **An unrestored failed service makes the outcome `partial`** (D9). The environment differs from where it started, which is the case where undo matters most.
- **Existing result error fields are redacted** (D11). `DeployResult.error`, `services[].error` and the scale equivalents carry raw error text today, including health-failure log lines.
- **The environment selector is one helper** (D6). This lets previews use `--preview <name>`, since `-e preview-<name>` cannot load.
