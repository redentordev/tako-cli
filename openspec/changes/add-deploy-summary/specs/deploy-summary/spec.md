# Spec Delta

## Purpose

When a mutating operation finishes, the operator (or CI) gets one trustworthy answer to four questions: what changed, is it healthy, where is it, and how do I undo it. The answer is the same in human output, machine output and the GitHub Actions job summary.

## ADDED Requirements

### Requirement: Summary on every completed mutating operation
`tako deploy`, `tako rollback` (service and project scope), `tako promote` and `tako scale` SHALL produce an `OperationSummary` whenever they produce a result document, including on failure. In text mode the summary block SHALL be the last output of the command, after post-operation hooks and domain checks.

#### Scenario: Successful deploy ends with a summary
- **WHEN** `tako deploy -e production` updates `api` and `web` and leaves `postgres` unchanged
- **THEN** the final output block lists `api` and `web` as `updated` and `postgres` as `unchanged`
- **AND** shows the duration, the deployment display ID, URLs and an undo command

#### Scenario: Post-deploy hook output precedes the summary
- **WHEN** a `post-deploy` hook prints output
- **THEN** the hook output appears before the summary block
- **AND** a failed post-deploy hook is mentioned in the summary's notes

#### Scenario: Pre-deploy hook failure
- **WHEN** the `pre-deploy` hook exits non-zero before any mutation
- **THEN** the summary has outcome `failed`, every planned service `not_attempted`, and no undo command, with undo unavailable reason `nothing was changed`

#### Scenario: Apply fails before its first change
- **WHEN** hooks are configured and the deploy apply fails a preflight check before changing anything, without producing a result
- **THEN** the summary has outcome `failed`, every planned service `not_attempted`, and undo unavailable reason `nothing was changed`
- **AND** the exit code is the same as without hooks

### Requirement: Summary header
The summary SHALL contain the operation, project, environment (`name` and `kind`), outcome, start time and duration. When a history record was written, it SHALL also contain the deployment ID and display ID.

#### Scenario: Header fields
- **WHEN** a deploy writes history record `1767225600123456789`
- **THEN** the summary has `operation: "deploy"`, `deploymentId: "1767225600123456789"` and `displayId: "1767225600"`, plus the duration

### Requirement: Summary service rows
The summary SHALL contain one row per service in the operation's scope. Each row has:
- `name` and `change`;
- `strategy`, for long-running services;
- `fromImage`/`toImage`, and `fromRevision`/`toRevision` where known;
- desired and running replicas, and `health`;
- `error`, when the service failed.

`change` SHALL be one of `added`, `updated`, `unchanged`, `warmed`, `restored`, `failed`, `not_attempted`, `ran`, `removed`, `rolled_back`, `skipped`, `promoted`, `scaled`.

#### Scenario: Old and new images are reported
- **WHEN** `web` moves from `ghcr.io/acme/web:ab12cd3` to `ghcr.io/acme/web:ef45ab6`
- **THEN** the `web` row has `fromImage: "ghcr.io/acme/web:ab12cd3"` and `toImage: "ghcr.io/acme/web:ef45ab6"`

#### Scenario: Strategy distinguishes recreate from rolling
- **WHEN** `mongo` is redeployed with the recreate strategy and `web` with rolling
- **THEN** both rows have change `updated`, with `strategy: "recreate"` and `strategy: "rolling"` respectively

#### Scenario: Targeted deploy scope
- **WHEN** the operator runs `tako deploy --service web`
- **THEN** the summary lists `web` and any deploy-time run prerequisites of `web`, and no other services

### Requirement: Outcome classification
The summary `outcome` SHALL be:

| `outcome` | When |
| --- | --- |
| `succeeded` | a service changed and nothing failed |
| `up_to_date` | nothing changed |
| `warmed` | the only changes are revisions warmed for manual promotion |
| `partial` | failed, and a service is not at its pre-operation state (kept or unrestored) |
| `failed` | failed, and every service is at its pre-operation state |

Exit codes SHALL be unchanged by this requirement.

#### Scenario: Partial deploy
- **WHEN** `api` deploys successfully, then `web` fails its health check and is restored, and `worker` was planned after `web`
- **THEN** the outcome is `partial`
- **AND** `api` is `updated`, `web` is `restored` with its error, and `worker` is `not_attempted`
- **AND** the command exits 1

#### Scenario: Failure with everything restored
- **WHEN** the first and only planned service fails and its previous revision is restored
- **THEN** the outcome is `failed`

#### Scenario: Failure that could not be restored
- **WHEN** the only planned service fails and its recovery status is `failed`
- **THEN** the outcome is `partial`
- **AND** the summary includes the undo command

### Requirement: Post-operation health
After an operation that changed at least one service, Tako SHALL read the services' current state once, with a bounded timeout. It SHALL report each row's `health` as one of `healthy`, `unhealthy`, `starting`, `degraded`, `stopped`, `unknown` or `n/a`, and report running and desired replicas. A failed or timed-out read SHALL yield `unknown` with a note, and SHALL NOT fail the operation or change its exit code.

#### Scenario: Replica missing on one node
- **WHEN** `web` desires 3 replicas and only 2 are running after the deploy
- **THEN** the `web` row reports replicas `2/3` and health `degraded`
- **AND** next steps include `tako ps -e production`

#### Scenario: Health read fails
- **WHEN** the post-operation state read times out
- **THEN** every changed row reports health `unknown`, a note says the health was not read, and the exit code is the same as it would otherwise be

#### Scenario: Run steps have no health
- **WHEN** a `kind: run` step ran during the deploy
- **THEN** its row has change `ran` and health `n/a`

### Requirement: URLs with readiness
The summary SHALL list the public URLs (`https://` + each proxy domain) and the internal route URLs of the services in scope, sorted by service and then URL, each with a `kind` (`public` or `internal`). When the deploy checked domain DNS/TLS, each public URL SHALL carry `status` `ready`, `pending` or `failed`. Otherwise it SHALL carry `not_checked`. `DeployResult.urls` and `internalUrls` SHALL be sorted the same way.

#### Scenario: Deterministic order
- **WHEN** the same configuration is deployed twice
- **THEN** `summary.urls` and `DeployResult.urls` list URLs in the same order both times

#### Scenario: TLS still pending
- **WHEN** the domain check times out (non-strict) while `www.shop.example.com` has no certificate yet
- **THEN** that URL has status `pending` with a detail
- **AND** next steps include `tako domains status -e production www.shop.example.com`

#### Scenario: Domain check skipped
- **WHEN** the operator passes `--skip-domain-check`
- **THEN** every public URL has status `not_checked`

### Requirement: Undo command
The summary SHALL include an undo command pinned to an explicit target, both as `argv` and as a shell-quoted `command`, unless no safe undo exists. When no safe undo exists, `undo.unavailable` SHALL state why. The command SHALL select the operation's environment explicitly (`-e <environment>` for a declared environment), and SHALL include `--config <path>` when a non-default config path was used. It SHALL NOT include `-y`, and SHALL NOT rely on a command's default target.

#### Scenario: Deploy undo targets the previous stable deployment
- **WHEN** the newest stable deployment before this deploy has ID `1767225000123456789`
- **AND** the deploy succeeds
- **THEN** the undo is `tako rollback -e production 1767225000`
- **AND** `undo.targetDeploymentId` is `1767225000123456789`

#### Scenario: First deployment has no undo
- **WHEN** no stable deployment exists before this deploy
- **THEN** the undo has no command, and `unavailable` is `first deployment; nothing to roll back to`

#### Scenario: Nothing went live
- **WHEN** the deploy outcome is `up_to_date` or `warmed`
- **THEN** the undo has no command, and `unavailable` explains that nothing serving changed

#### Scenario: Persistent service changed
- **WHEN** a deploy updates `postgres` (`persistent: true`) and `web`
- **THEN** the undo command is the project rollback
- **AND** an undo note states that persistent services are skipped unless `--include-persistent` is added

#### Scenario: Project rollback undo rolls forward
- **WHEN** a project rollback rolls the environment back from deployment D3 to D2
- **THEN** the undo is `tako rollback -e production <displayId of D3>`

#### Scenario: Scale undo restores the previous counts
- **WHEN** `tako scale web=5` changes `web` from 2 to 5 replicas
- **THEN** the undo is `tako scale -e production web=2`

#### Scenario: Scale undo uses the previous desired count
- **WHEN** the prior desired revision has `web` at 2 replicas while 4 `web` containers are running (an inactive blue-green set is still up)
- **AND** `tako scale web=5` succeeds
- **THEN** the row reads `2 → 5 replicas` and the undo is `tako scale -e production web=2`

#### Scenario: Scale undo with an unknown previous count
- **WHEN** a scaled service has no replica count in the prior desired revision
- **THEN** the undo has no command, and `unavailable` is `previous replica count unknown`

#### Scenario: Promote undo
- **WHEN** `tako promote web` switches from revision `r1` to `r2`, and `r1` is still retained
- **THEN** the undo is `tako promote web -e production --revision r1`

#### Scenario: Promote undo when the previous revision is gone
- **WHEN** `tako promote web` switches from `r1` to `r2`, `r1` is no longer retained, and the newest stable record before the promotion that contains `web` has display ID `1767225000`
- **THEN** the undo is `tako rollback -e production --service web 1767225000`

#### Scenario: Custom config path
- **WHEN** the operator ran `tako --config deploy/tako.yaml deploy`
- **THEN** the undo argv includes `--config deploy/tako.yaml`

#### Scenario: History unreadable
- **WHEN** deployment history cannot be loaded at plan time
- **THEN** the deploy still proceeds
- **AND** the undo has no command, `unavailable` is `deployment history unreadable`, and next steps include `tako history -e production`

### Requirement: Next steps
The summary SHALL include up to five next steps, each with a reason and a command, derived from the outcome:
- `tako promote` for warmed services;
- `tako logs --service NAME --since 10m` for failed or restored services;
- `tako domains status` for pending or failed domains;
- `tako ps` for unhealthy or degraded services;
- `tako history` after a partial failure.

#### Scenario: Warmed service
- **WHEN** `web` uses manual promotion and the deploy warms a new revision
- **THEN** the outcome is `warmed`, and next steps include `tako promote web -e production`

#### Scenario: Failed service
- **WHEN** `web` fails its rollout
- **THEN** next steps include `tako logs -e production --service web --since 10m`

### Requirement: Summary event
With `--events ndjson`, Tako SHALL emit one `operation.summary` event (phase `summary`) immediately before the terminal `result` event. Its `data.summary` SHALL be the same object as the result document's `summary`, and `data.operation` SHALL name the command.

#### Scenario: NDJSON ordering
- **WHEN** the operator runs `tako deploy -y --events ndjson`
- **THEN** the second-to-last line is an `operation.summary` event and the last line is the `result` event
- **AND** `data.summary` equals `data.result.summary`

### Requirement: Summary in result documents
The `DeployResult`, `RollbackResult`, `PromoteResult` and `ScaleResult` documents SHALL carry the summary in an additive `summary` field. `--output json` SHALL still write exactly one document to stdout. Existing result fields, event types and exit codes SHALL keep their meaning.

#### Scenario: JSON output stays one document
- **WHEN** the operator runs `tako deploy -y --output json`
- **THEN** stdout contains exactly one `DeployResult` document with a `summary` object
- **AND** the human summary block is written to stderr

#### Scenario: Result returned when bookkeeping fails after rollout
- **WHEN** a deploy's rollout succeeds but persisting takod state fails
- **THEN** a `DeployResult` with status `failed`, `error`, and a summary with outcome `partial` noting that the rollout succeeded but state bookkeeping failed is emitted
- **AND** the summary keeps the undo command
- **AND** the command exits with the same code as before this change

### Requirement: Summary redaction
Every free-text field of the summary SHALL pass through the engine's secret redactor before the summary is emitted in any form. This covers errors, reasons, notes, next-step commands, URL details and the commit. The text block, the JSON object, the NDJSON event and the GitHub Markdown SHALL never contain a registered secret value. Image references, service, environment and project names, URLs and deployment IDs are public values and are shown unmasked.

#### Scenario: Secret in an error message
- **WHEN** a service fails with an error containing the value of a registered secret
- **THEN** that value appears masked in the summary's JSON, NDJSON event, text block and step-summary Markdown

#### Scenario: Names and images not masked
- **WHEN** a service named `site-renderer` deploys image `registry.example.com/site-renderer:ec646b3956498855b6dbdaf4e9f730d54d0e1ec0`
- **AND** a short env value `site` is registered as a secret
- **THEN** the summary shows the service name and image unmasked

### Requirement: Result error fields are redacted
The `error` field and every per-service `error` field of `DeployResult` and `ScaleResult` SHALL pass through the engine's secret redactor before the result document is emitted, in every output mode.

#### Scenario: Secret in a health-failure error
- **WHEN** a deploy fails a health check and the error embeds a container log line containing the value of a registered secret
- **THEN** that value appears masked in the result document's `error` and `services[].error`

### Requirement: GitHub Actions step summary
When the `GITHUB_STEP_SUMMARY` environment variable names a file and `TAKO_STEP_SUMMARY` is not `off`, `0`, `false` or `no` (case-insensitive), Tako SHALL append a Markdown rendering of the summary to that file in every output mode. The rendering contains a status heading, the service table, URLs, the undo command in a code block, next steps, and the error.

#### Scenario: Step summary written in CI
- **WHEN** `tako deploy -y --events ndjson` runs in GitHub Actions
- **THEN** the job summary shows the deploy heading, the service table, URLs and the undo command
- **AND** stdout still contains only NDJSON events

#### Scenario: Opt-out
- **WHEN** `TAKO_STEP_SUMMARY=off` is set
- **THEN** nothing is written to the `GITHUB_STEP_SUMMARY` file

### Requirement: Step summary failures and limits
A failure to write the step summary SHALL emit a `warning` event and SHALL NOT change the exit code. The content appended per operation SHALL be capped at 64 KiB, with a truncation note.

#### Scenario: Oversized summary is truncated
- **WHEN** the rendered Markdown would exceed 64 KiB
- **THEN** the appended content is cut to the limit and ends with a truncation note

#### Scenario: Unwritable summary file
- **WHEN** the `GITHUB_STEP_SUMMARY` path cannot be opened
- **THEN** a `warning` event reports it, and the exit code is unchanged

#### Scenario: Not in CI
- **WHEN** `GITHUB_STEP_SUMMARY` is unset
- **THEN** no file is written

### Requirement: Environment kind for future preview environments
The summary's `environment.kind` SHALL be `standard` for environments declared under `environments:`, and each URL SHALL carry a `kind` of `public` or `internal`. Consumers SHALL treat unknown `environment.kind` and URL `kind` values as opaque, so that preview environments can report `environment.kind: preview` without a schema change.

#### Scenario: Standard environment
- **WHEN** any deploy to a declared environment completes
- **THEN** `summary.environment` is `{ "name": "<env>", "kind": "standard" }`
