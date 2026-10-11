# Tasks

## 1. Target resolution and snapshot reconstruction (pure functions)

- [ ] 1.1 Add `SelectProjectRollbackTarget` in `pkg/engine/rollback_target.go`. It returns the default target (newest stable record strictly older than the newest record) and resolves an explicit argument by exact ID, then unique ID prefix, then git commit (≥7 chars, newest stable match), returning `InvalidRequestError` with candidates on ambiguity. Verify with table tests in `pkg/engine/rollback_target_test.go`: success→success, failed head, rolled_back head (toggle), single record, display-ID prefix, ambiguous prefix, commit match, failed explicit target.
- [ ] 1.2 Extend single-service `SelectRollbackTargetFromHistory` (`pkg/engine/rollback.go:561`) to accept prefix and commit after an exact-ID miss. Verify that the existing `cmd/rollback_test.go` tests still pass and that new cases for prefix and commit resolution pass.
- [ ] 1.3 Add `ReconstructServiceTargets(history, target, services, includePersistent)`. It returns per-service planned outcomes: target `ServiceState` plus `sourceDeploymentId`, or `skipped` with a reason (`kind: run`, `kind: job`, `persistent`, `not present at target`), `unchanged`, or `unresolvable` when history is at `MaxHistoryEntries`. Verify with table tests covering the D1/D2/D3 multi-record example from the spec and the pruned and non-pruned "added after target" cases.
- [ ] 1.4 Add the unchanged-detection helper, which compares the target against actual image and replicas plus the history head's `FilesContentHash`/`SharedBuildHash`. Verify with unit tests for equal, image-differs, replicas-differ and files-hash-differs cases.

## 2. Deployer: release skip and retained-image reuse

- [ ] 2.1 Add `RollbackOptions{SkipRelease, ImageSource}` to `Deployer.RollbackToState` (`pkg/deployer/deployer.go:381`) and thread `SkipRelease` into the `Release` op of `deployServiceTakod` (`pkg/deployer/takod_deploy.go:496-498`). Update the single-service caller to pass `SkipRelease: false`. Verify with a deployer unit test that `runTakodServiceRollout` skips Release only when requested.
- [ ] 2.2 Factor a non-mutating `LocateRetainedImage(imageRef, requiredServers)` out of `transferExistingImageToNodes` (`takod_deploy.go:772-830`), with the same platform grouping and `sameImageContent` checks. Add a `preflightTakodCapability(..., takod.CapabilityImageDescriptorV1, ...)` gate. Verify with unit tests: found, missing, conflicting digests, and capability-missing → `CapabilityRequiredError`.
- [ ] 2.3 For `ImageSource == "retained"`, transfer the exact image to the assigned nodes and deploy with `BuildImage=false, PullImage=false`, for build-backed services too. Verify with a test that a build-backed rollback with a retained image performs no build, and that the rebuild path is still used when the image is not retained.

## 3. Engine: project rollback session

- [ ] 3.1 Add `RollbackRequest` fields (`IncludePersistent`, `RunRelease`) and a `Scope` derivation. Add `RollbackPlan`/`KindRollbackPlan` (with `hash` and a `Hash()` method over the mutation-determining fields from design D10), `RollbackServiceOutcome` (including optional `recovery`), and the additive `RollbackResult` fields (`scope`, `services`, `error`; `service` becomes omitempty). Verify with a JSON golden test that the single-service document is unchanged apart from `scope`, and a hash test that equal plans hash equal, a changed `toImage`/target/`imageSource` changes the hash, and warning text does not.
- [ ] 3.2 Add the job warning to `RollbackPlan.warnings` whenever the environment has `kind: job` services (they are also reported `skipped`). Verify with an engine plan test.
- [ ] 3.3 Implement `Engine.PlanRollback` → `RollbackSession`. It takes the lock and leases, loads history, gathers actual state, runs 1.1/1.3/1.4, and runs the image preflight (local `git cat-file -e` for rebuild, 2.2 for retained, `pull` for registry images). An unavailable image produces a failed result with every service `not_attempted`. Verify with engine tests using fake history and lister seams for plan contents and preflight failure.
- [ ] 3.4 Implement `RollbackSession.Apply`:
  - persist placement intent once for all planned services;
  - roll services back sequentially in `dependency.Resolver` order, using one worktree per distinct rebuild commit;
  - stop at the first failure and mark the remaining services `not_attempted`.

  Verify with an engine test using a fake deployer: the order is respected, the second service fails, and the third is `not_attempted`.
- [ ] 3.5 Generalize `RollbackProxyInputs` and `BuildRollbackDeployment` to service maps, and run post-apply convergence: proxy, prune, takod runtime state (`rollback.succeeded`/`rollback.partial`), one history record (`rolled_back`/`failed`), replication, local state, and notifications. These run for successfully rolled-back services even on partial failure, and are skipped on `up_to_date`. Verify with tests on the history record contents (only rolled-back services, no `kind: run` entries) and on the up-to-date path writing nothing.
- [ ] 3.6 Add `events.TypeDeployServiceSkipped` and emit the rollback event sequence with `data.operation`/`data.targetDeploymentId`. Add the same data keys to the single-service path. Verify with an event-capture test asserting types and data keys, and that no `deploy.release.*` event appears without `--run-release`.
- [ ] 3.7 Map errors to exit classes: unresolvable or ambiguous target → InvalidRequest (2); preflight unavailable, apply failure or partial failure → 1; persistent-included warning → `warning` event. Verify with `engine.Classify` assertions in tests.

## 4. CLI, docs, generated artifacts

- [ ] 4.1 Update `cmd/rollback.go`:
  - drop `MarkFlagRequired("service")`;
  - add `--include-persistent`, `--run-release`, `--plan-only`, `--plan <file>`, `-y/--yes`, and reject them with `--service`;
  - add plan table rendering, the human `[y/N]` prompt via `requireDeployPromptAllowed` with its error wrapped as `&engine.ConfirmationRequiredError{Reason: err.Error()}` (copy `cmd/remove.go:79-81`, not deploy's plain error at `cmd/deploy.go:293-315`), and machine-mode `ConfirmationRequired` (`operation: "rollback"`, optional `plan` field added to `operationConfirmationRequiredDocument` in `cmd/engine.go`);
  - update `Long`/examples.

  Verify with `cmd/rollback_test.go` tests for flag validation, confirmation document emission (exit 2), a text-mode run with `CI=true` and no `--yes` exiting 2, and that `--plan-only` performs no apply.
- [ ] 4.2 Generalize `verifyPlanFileMatches` (`cmd/engine.go:155`) over a `hashedPlan` interface (`Hash() string`) with typed decoding, keep deploy's call site (`cmd/deploy.go:144-148`) and messages unchanged, and call it from rollback after `PlanRollback` and before confirmation. Verify with tests: deploy plan drift still exits 2 with the same message; rollback `--plan` with a matching file applies; with drift exits 2 and calls no apply; a `DeployPlan` file passed to rollback exits 2; `--plan` with `--service` exits 2.
- [ ] 4.3 Add machine-output tests in `cmd/machine_output_test.go` for `RollbackPlan`, the project `RollbackResult`, and the ConfirmationRequired path. Confirm `cmd/machine_coverage_test.go` still lists `tako rollback` as full-contract. Verify with `go test ./cmd/...`.
- [ ] 4.4 Update `docs/MACHINE-INTERFACE.md`: the RollbackResult and RollbackPlan shapes (including `hash`, `services[].recovery` and the job warning), `--plan <file>` drift semantics, `scope`, statuses (`rolled_back|up_to_date|failed`), the `deploy.service.skipped` event, rollback in the confirmation section, and exit-code notes. Verify the text against the 3.1 golden test fields.
- [ ] 4.5 Update `docs/CONFIGURATION.md`. In the rollback notes near lines 456-470 and 1331-1336, document project-wide rollback, persistent/run/job handling, release skip, and the image-retention interaction with `tako cleanup --keep`. Also update the `README.md` command table and `examples/01-simple-web/README.md`. Verify that the documented commands match the `tako rollback --help` output.
- [ ] 4.6 Regenerate `man/tako-rollback.1` with `make man`, and completions if flags are embedded. Confirm `schema/tako.schema.json` is unchanged (no config types touched). Verify with a clean `git diff --exit-code` after regeneration in CI.
- [ ] 4.7 Run `go build ./...` and `go test ./...`. Failures that occur only on Windows (symlink, fsync or POSIX paths) are pre-existing and not regressions.

## Workflow follow-up

- Manual mesh check on a 2-node test environment: deploy D1, deploy D2 changing two services, `tako rollback --plan-only`, `tako rollback -y`, then `tako history` shows one `rolled_back` record; then `tako rollback -y` toggles back to D2.
- Archive the change after review.
- Future changes (not this one): roll back `kind: job` images and schedules; pre-pull `pull`-source images by default; make single-service rollback skip `deploy.release` at the next breaking window after one release of deprecation warnings.
