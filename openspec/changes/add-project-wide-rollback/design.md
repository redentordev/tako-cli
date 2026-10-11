# Design

## Context

See proposal.md for motivation; specs/rollback/spec.md for the behavior contract. Observed current state (all citations are pre-change):

- **CLI.** `cmd/rollback.go:18-41` declares `rollback [deployment-id]` with a required `--service` (`:39-40`). `runRollback` (`:43-77`) builds an `engine.RollbackRequest` with seams for the mesh history source (`selectRollbackHistorySource`, `:79-92`) and the deployment lister (`listDeploymentsFromHistory`, `cmd/history.go:159-196`). `historyNextSteps` (`cmd/history.go:153-157`) already advertises `tako rollback <deployment-id>`, which fails today without `--service`.
- **Engine.** `Engine.Rollback` (`pkg/engine/rollback.go:78-421`) is a single call with no plan/apply split (comment `:74-77`):
  1. Takes the local lock `.tako` (`:100-110`) and the remote leases for `mutationServers` (`:154-165`).
  2. Rejects `kind: run` / `kind: job` (`:123-125`).
  3. Selects the target (`SelectRollbackTargetFromHistory`, `:561-577`). An explicit ID must match exactly (`DeploymentFromHistory`, `:546-556`). Otherwise `PreviousStableServiceDeploymentFromHistory` (`:594-620`) skips the newest record containing the service and returns the next stable one (`success` or `rolled_back`, `:622-624`).
  4. Persists placement intent (`:301-305`).
  5. Calls `rollbackToTargetState` (`:423-441`), which uses a git worktree for build-backed services with a recorded commit (`:445-459`, `:461-482`).
  6. Writes one history record (`BuildRollbackDeployment`, `:634-665`, status `rolled_back`, services = just the one).
  7. Reconciles the proxy using `RollbackProxyInputs` (`:513-543`, single service), prunes revisions (`:343`), persists takod runtime state (`:353-381`), replicates history (`:385-395`), saves local state (`:397`, `:667-688`), and sends notifications (`:229-250`, `:314-323`, `:400-413`).
- **Deployer.** `Deployer.RollbackToState` (`pkg/deployer/deployer.go:381-422`) redeploys through `deployServiceTakod` (`pkg/deployer/takod_deploy.go:386-510`):
  - That rollout always runs the `Release` phase (`:496-498` → `runReleaseCommand`, `pkg/deployer/release.go:70-74`). So **today's single-service rollback re-runs `deploy.release` from the target image**.
  - Build-backed services always rebuild (`takodRollbackDeployOptionsForService`, `takod_deploy.go:370-384`).
  - Only shared-build images use exact retained-image transfer (`EnsurePreparedServiceImage`, `:293-306` → `transferExistingImageToNodes`, `:772-830`, via `inspectImageOnTakodNode`, `:1126`).
- **History model.**
  - `DeploymentState` (`internal/state/types.go:8-30`) holds a `Services` map that contains **only the services that operation touched**. `deploy` records `servicesToDeploy` (`pkg/engine/deploy.go:713`, `:972-983`, and run entries at `:917`/`:921`). Scale records only scaled services (`pkg/engine/scale.go:377-406`). Promote records one service (`pkg/engine/promote.go:402-445`).
  - IDs are `<unixnano>_<pid>` (`internal/state/manager.go:216-218`). `tako history` displays the first 10 characters (`FormatDeploymentID`, `:409-414`).
  - History keeps 50 entries (`MaxHistoryEntries`, `manager.go:18`).
  - There is no operation-type field. Records are distinguishable only by `Message`.
- **Run steps.** `kind: run` completion is derived from the newest history entry with `Kind == run` (`addRunHistoryActual`, `pkg/engine/run_steps.go:136-170`). Rollback records never contain run entries.
- **Image retention.** `tako cleanup` keeps 3 images per service by default (`cmd/cleanup.go:68`). Older targets may have no retained image.
- **Machine contract.**
  - `RollbackResult` is defined at `pkg/engine/rollback.go:59-72`.
  - The confirmation document pattern is `newOperationConfirmationRequiredDocument` (`cmd/engine.go:104-127`), used by remove (`cmd/remove.go:71-94`).
  - Exit classes come from `engine.Classify` (`pkg/engine/errors.go:82+`).
  - Lifecycle event types are defined in `pkg/takoapi/events/events.go:57-65`.
  - `tako rollback` is already listed in `cmd/machine_coverage_test.go:48`.
- **Dependency order.** `dependency.NewResolver(...).InferDependencies/ResolveOrder` (`pkg/dependency/resolver.go:18,27,119`), as used by deploy (`pkg/engine/deploy.go:840-851`).

## Goals / Non-Goals

**Goals:**
- One engine path that can restore N services under one lock and lease set, with a plan/apply split so confirmation and `--plan-only` are possible.
- Fail closed before mutation on any unresolvable target state or unavailable image.
- Keep the single-service CLI form byte-compatible in its JSON fields and flow (no prompt, release still runs).

**Non-Goals:**
- Changing the history record schema. The design works with existing records, including legacy ones.
- A takod-side rollback primitive. All orchestration stays CLI-side.
- Parallel per-service rollback.

## Decisions

### D1. Plan/apply session for project scope; single-service keeps the direct call
Add `Engine.PlanRollback(ctx, RollbackRequest) (*RollbackSession, error)`. The session does the following:
1. Acquires the local lock and remote leases (same calls as `rollback.go:100-110`, `:154-165`).
2. Loads history via `HistorySource`.
3. Gathers actual state.
4. Resolves the target and per-service snapshot (D2-D4).
5. Runs the image preflight (D5).

It exposes `Plan() RollbackPlan`, `Apply(ctx) (*RollbackResult, error)` and `Close()`, mirroring `PlanRemove`/`session.Apply` in `cmd/remove.go:64-96`. Leases are held across the confirmation prompt, so the plan cannot go stale between review and apply.

`Engine.Rollback` with `Service != ""` keeps its current body. Its internals are refactored onto shared helpers (D6), with no behavior change except D5's retained-image reuse.

*Alternative:* route single-service through the session as well. Rejected because the session would force a prompt or a `--yes` on an existing non-interactive contract.

### D2. Default target = newest stable record strictly older than the newest record
New `SelectProjectRollbackTarget(history, arg, lister)`:
- Sort records newest first using the existing lister with `IncludeFailed: true`.
- With no argument, skip index 0 (any status, including `in_progress`/`failed`) and return the first record whose status is `success` or `rolled_back`.

This mirrors the per-service rule (`rollback.go:594-620`) at project granularity. A repeated rollback therefore toggles back to the deployment that was rolled away from. If the newest record is a `scale`/`promote` record, the default target is the state before that operation. The plan output shows the target's ID, timestamp, commit and message, so the operator sees exactly what will be restored.

*Alternative:* skip non-deploy records (scale/promote) when picking the default. Rejected because records have no type field and `Message` matching is brittle. Explicit targets cover the other cases.

### D3. Explicit target resolution
Resolution order:
1. Exact ID (existing `DeploymentFromHistory`).
2. Unique ID prefix. The 10-character display ID from `cmd/history.go:121-124` is the common input.
3. Git commit: `GitCommit` equal to the argument, or `GitCommitShort`/`GitCommit` having it as a prefix when it is ≥7 characters. Pick the newest stable record that matches.

Ambiguity lists candidates and fails with `InvalidRequestError` (exit 2). A non-stable target fails with exit 2 (same rule as `validateRollbackTarget`, `:579-590`).

The single-service form gains the same prefix/commit resolution (additive: exact IDs still match first).

The positional argument stays canonical; no `--to` flag is added, to avoid two spellings of one input. The `revision` wording in the request maps to git commit. Tako build tags are not separately indexed in history.

### D4. Per-service snapshot reconstruction
`ReconstructServiceTargets(history, target, services)` checks each service in the current config, in this order:

| Case | Outcome |
| --- | --- |
| `kind: run` / `kind: job` | Skipped with reason `kind: run` / `kind: job` |
| `persistent: true` without `--include-persistent` | Skipped with reason `persistent` (`pkg/config/types.go:352`) |
| A stable record with timestamp ≤ target contains the service | The newest such record's `ServiceState` is the target |
| No record ≤ target, records > target exist, history not pruned (`len < MaxHistoryEntries`) | The service was added after the target: skipped with reason `not present at target` |
| No record ≤ target, records > target exist, history pruned (`len == MaxHistoryEntries`) | `unresolvable`: the plan fails with exit 2 naming the service and suggesting per-service rollback |
| No record at all in retained history | `unchanged` |

History alone cannot prove a service is new rather than pruned, so the pruned case fails closed. Records with timestamp ≤ target but status `failed`/`in_progress`/`warmed` are ignored when picking a service's target state.

Unchanged detection compares the following against the target `ServiceState`:
- the actual aggregate image and replicas (`reconcile.GatherActualStateFromServers`),
- the current history head's `FilesContentHash` and `SharedBuildHash` for that service.

### D5. Image availability preflight before any mutation
For each service to roll back, the image source is chosen in this order:
1. **`retained`:** `inspectImageOnTakodNode` finds the exact `ServiceState.Image` on any target or builder node of each required platform, with consistent content (reuses `transferExistingImageToNodes`' search, factored into a non-mutating `LocateRetainedImage`). Used for any service kind (shared-build, build-backed, registry image).
2. **`rebuild`:** the service is build-backed (`service.Build != ""`) and `RollbackTargetCommit(target)` resolves locally (`git cat-file -e <commit>^{commit}`).
3. **`pull`:** the service uses `image:` (registry). Registry reachability is not proven in preflight. A pull failure surfaces at apply time (assumption A2).
4. Otherwise the service is unavailable.

Shared-build services (`SharedBuildHash != ""`) accept only `retained`, which matches `deployer.go:403-413`.

Any unavailable service fails planning. The result has status `failed`, every candidate is marked `not_attempted`, and the error lists services and images. The exit code is 1, because this is an operational fact rather than a malformed request.

The descriptor endpoint is gated with `preflightTakodCapability(nodes, takod.CapabilityImageDescriptorV1, "exact image reuse")` (`pkg/takod/server.go:171-173`), so an older takod returns `CapabilityRequiredError` (exit 2, upgrade message).

At apply time, `retained` uses `EnsurePreparedServiceImage`-style transfer and then deploys with `BuildImage=false, PullImage=false`. `rebuild` groups services by commit and creates one worktree per distinct commit (reusing `withRollbackGitWorktree`, `:461-482`) instead of one per service.

**takod capability gate:** no new takod behavior. Only existing endpoints (`/v1/images/inspect`, `/v1/images/export|import`, the deploy endpoints) are used, behind the existing `image.descriptor-v1` capability. No new capability string is needed.

### D6. Execution: shared per-service step, release skip option
- Change `Deployer.RollbackToState(serviceName, state)` to `RollbackToState(serviceName, state, RollbackOptions{SkipRelease bool, ImageSource string})`.
- `deployServiceTakod`'s `takodServiceRolloutOperations.Release` becomes a no-op when `SkipRelease` is set.
- The single-service engine path passes `SkipRelease: false`, preserving current behavior.
- The project path passes `SkipRelease: !req.RunRelease`.

Why release is not re-run by default:
- Release commands are conventionally forward migrations or cache warmers bound to the image that introduced them.
- Running an older image's `migrate` against a schema already advanced by the newer release is at best a no-op. At worst it fails, which would abort the rollback mid-way, or it misbehaves.
- Rolling code back never rolls data back.
- `kind: run` steps are fingerprinted per deploy (`run_steps.go:172-176`). Re-running them would also write run entries that change future deploy skip decisions.
- The operator can opt in with `--run-release` when the release step is known to be idempotent.

The project apply loop:
1. Iterate `ResolveOrder()` restricted to services planned `rolled_back`, one at a time (`ctx.Err()` checked between services).
2. Emit `deploy.service.started`, call `rollbackToTargetState` with the chosen source, then emit `deploy.service.reconciled` or `deploy.service.failed`.
3. On the first failure, break and mark the rest `not_attempted`.
4. Persist placement intent once before the loop. Generalize `PersistTakodDesiredIntentWithPlacementBaseline` inputs: `placementServices` takes target replicas for every rolled-back service, and `intentImageRefs` takes every target image (`rollback.go:274-305`).

Blue-green services roll out through the normal `deployServiceTakod` path (new revision, health gate, switch). Services with `deploy.promotion: manual` are activated directly rather than warmed, same as today's single-service rollback (no `WarmOnly` in `takodRollbackDeployOptionsForService`). This is documented, not changed.

### D7. Post-apply convergence even on partial failure
After the loop, whether or not it succeeded:
1. Gather actual state.
2. Run `RollbackProxyInputs`, generalized to a `map[string]ServiceState` of **successfully rolled-back** services (`:513-543`).
3. `ReconcileProxy`, then `PruneTakodServiceRevisions` for those services.
4. `PersistTakodRuntimeStateWithPlacementBaseline` with event type `rollback.succeeded` or `rollback.partial`.
5. Save one `DeploymentState`. `BuildRollbackDeployment` is generalized to take a services map. Status is `rolled_back`, or `failed` with `Error` set. The message is `rolled back project to deployment <id> (<n> services)`.
6. Replicate, save local state, send notifications.

This keeps routes and recorded state truthful about what is running. Bookkeeping failures after a successful mutation keep the existing "rollback succeeded but …" wrapping (`:331`, `:336-381`). When nothing changed (`up_to_date`), steps 2-6 are skipped and no history record is written.

*Alternative:* attempt automatic roll-forward of the already-rolled-back services on failure. Rejected because it doubles risk during an incident, and the operator can rerun `tako rollback` (default target = toggle back) or `tako deploy`.

### D8. Machine contract
- `RollbackResult` (`rollback.go:59-72`) gains the following additive fields: `Scope string json:"scope"`, `Services []RollbackServiceOutcome json:"services,omitempty"`, `Error string json:"error,omitempty"`. `Service` becomes `json:"service,omitempty"`; the single form always sets it, so its output is unchanged apart from the new `scope`.
- `RollbackServiceOutcome` fields: `name`, `action` (`rolled_back|unchanged|skipped|failed|not_attempted`), `fromImage`, `toImage`, `replicas`, `sourceDeploymentId`, `imageSource` (`retained|rebuild|pull`), `reason`, `error`, and `recovery` (`omitempty`). `recovery` carries the per-service `RecoveryOutcome` from `make-rolling-default-deploys` unchanged when a service's rollout fails and is restored; it is absent if that change has not landed.
- Status values are `rolled_back`, `up_to_date` and `failed`.
- `RollbackPlan` document (kind `RollbackPlan`, `KindRollbackPlan`) with these fields:
  - `target`: `id`, `displayId`, `timestamp`, `commit`, `version`, `message`
  - `services`: planned outcomes
  - `warnings`: includes, whenever any `kind: job` service exists in the environment, one entry such as `jobs nightly, cleanup are not rolled back and keep running the newer image and schedule` (resolved: jobs are reported `skipped` *and* warned about)
  - `releaseCommands`: `skip` or `run`
  - `hash`: the plan hash (D10)
- `ConfirmationRequired` reuses `operationConfirmationRequiredDocument` with an added optional `plan` field (`json:"plan,omitempty"`, typed `any`). It carries `operation: "rollback"` and the target servers.
- The new event constant is `TypeDeployServiceSkipped = "deploy.service.skipped"`. All rollback events set `Data["operation"]="rollback"` and `Data["targetDeploymentId"]`. The existing single-service path also adds these data keys (additive).
- Exit codes:

  | Code | Meaning |
  | --- | --- |
  | 0 | success or `up_to_date`, or a human declined at the prompt |
  | 1 | apply failure, partial failure, or unavailable image (failure returned with the result emitted) |
  | 2 | target or snapshot invalid, confirmation required (including a non-interactive prompt), plan drift against `--plan <file>`, or capability missing |
  | 3 | lock or lease held |
  | 4 | connectivity |
  | 5 | cancelled |

  Code 6 is not used. A partial rollback leaves a mixed environment, which is a failure rather than an attention-worthy success.

### D9. CLI surface (`cmd/rollback.go`)
- Drop `MarkFlagRequired("service")` (`:40`).
- Add `--include-persistent`, `--run-release`, `--plan-only`, `--plan <file>`, and `-y/--yes`. All five are project-scope only in v1; combining any of them with `--service` is an invalid request (exit 2). This keeps the single-service contract unchanged.
- Human confirmation uses `requireDeployPromptAllowed` and a `[y/N]` prompt after the rendered plan table (pattern from `cmd/remove.go:79-93`). `requireDeployPromptAllowed` returns a *plain* error (`cmd/deploy.go:293-315`), which deploy surfaces as exit 1. Rollback must not copy deploy here: like remove (`cmd/remove.go:79-81`), it wraps that error as `&engine.ConfirmationRequiredError{Reason: err.Error()}` so a non-interactive session (`TAKO_NONINTERACTIVE`, `CI`, no TTY) exits 2.
- Update `Long`/examples. Regenerate `man/tako-rollback.1`.

### D10. `--plan <file>` hash verification (in v1)
- `RollbackPlan` gains `Hash string json:"hash"` and a `Hash()` method that hashes a canonical encoding of the plan contents that determine the mutation: environment, target ID, and for each service its `name`, `action`, `toImage`, `replicas`, `imageSource` and `sourceDeploymentId`, plus `releaseCommands` and `includePersistent`. Volatile fields (timestamps, `fromImage` display text, `warnings` wording) are excluded, so re-planning against unchanged state yields the same hash. `--plan-only` fills `hash` in the emitted document.
- Generalize `verifyPlanFileMatches` (`cmd/engine.go:155`, used by deploy at `cmd/deploy.go:144-148`) into `verifyPlanFileMatches[T hashedPlan](planPath string, current T) error`, where `hashedPlan` is `interface{ Hash() string }`. It reads the file, decodes it into `T`, and compares hashes. A wrong `kind` (for example a `DeployPlan` passed to rollback) is reported as an invalid plan file. Deploy keeps its exact messages and exit codes.
- `tako rollback --plan reviewed.json` runs `PlanRollback`, verifies the hash while the lock and leases are held, and on drift returns `InvalidRequestError` ("plan drift detected ... re-run with --plan-only and review again"), exit 2, with nothing mutated. On a match it still applies the confirmation rules (`--yes` or a prompt); `--plan` is not an implicit approval, matching deploy.
- *Alternative rejected:* defer to a later change. Without it, a CI pipeline cannot prove the rollback it applies is the one a human reviewed, which is exactly the incident scenario rollback is for.

## Risks / Trade-offs

- [Default target surprises the operator after scale/promote records] → The plan always shows the target and per-service diffs. Human mode requires confirmation. Explicit IDs and commits are supported.
- [Snapshot reconstruction is wrong when history was pruned] → Fail closed (D4) and point to per-service rollback.
- [`pull` source not verified up front, so a registry outage fails mid-rollback] → Services needing pulls are ordered by dependency only. The failure is reported per service and D7 still converges bookkeeping. Pre-pulling is future work (see below).
- [Persistent service included with an incompatible older image] → Opt-in flag, warning event, and plan warning.
- [Leases held during an interactive prompt block other operators] → Same trade-off deploy already makes. Lease TTL and renewal apply.
- [Retained-image reuse changes single-service behavior (no rebuild)] → The content is identical by tag plus digest consistency check (`sameImageContent`). The rebuild path remains the fallback. This is noted in the proposal.
- [Jobs keep the newer image while long-running services roll back] → Documented, reported as `skipped` with reason `kind: job`, and called out in the plan's `warnings[]` so the operator sees it before confirming. Rolling jobs back is future work.

## Migration Plan

- Pure CLI change. No takod upgrade is needed beyond nodes already advertising `image.descriptor-v1`; older nodes fail closed with an upgrade message.
- No history migration. Legacy records work because only `Services`, `Status`, `Timestamp`, `GitCommit*` and `ID` are read.
- Rollback of this feature is reverting the CLI. Records it wrote are ordinary `rolled_back`/`failed` records readable by older CLIs.
- Sibling ordering: if `add-local-deploy-hooks` lands first, the project rollback session calls its nil-safe `BeforeMutation` callback after the image preflight and sets `TAKO_SERVICES` to every service being rolled back. If `make-rolling-default-deploys` lands first, per-service `recovery` objects from failed rollouts are carried into `services[]` unchanged.

## Assumptions

- A1. Every long-running service's history `ServiceState.Image` is the exact runtime image ref (true for deploy `:973-983`, scale, promote and rollback writers).
- A2. Registry credentials and reachability are proven only at pull time. The preflight does not contact registries.
- A3. The local workspace is the project's git repository when `rebuild` is needed (same requirement as today's `withRollbackGitWorktree`).
- A4 (corrected). `TAKO_NONINTERACTIVE`/`CI` makes `requireDeployPromptAllowed` return an error. For deploy that error is plain and exits 1 (`cmd/deploy.go:293-315`); remove wraps it in `ConfirmationRequiredError` and exits 2 (`cmd/remove.go:79-81`). Rollback follows remove (D9), so it exits 2.

## Future Work

- Rolling back `kind: job` images and schedules (requires `ApplyJobSchedules` with historical image refs). Deferred; v1 reports jobs `skipped` and adds a plan warning.
- Pre-pulling `pull`-source images on all target nodes before the first mutation. Deferred; if it is added, it becomes the default behavior rather than a `--prepull` flag.
- Making single-service rollback skip `deploy.release` by default, for consistency with project scope. This is a separate future change at the next breaking-change window, after one release that prints a deprecation warning on single-service rollbacks that run a release.
