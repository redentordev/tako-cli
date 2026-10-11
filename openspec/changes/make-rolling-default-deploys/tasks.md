# Tasks

## 1. Config: default selection and rollbackOnFailure

- [ ] 1.1 Add `DeployConfig.StrategyDefaulted bool` (`yaml:"-" json:"-"`) and change `RollbackOnFailure` to `*bool` in `pkg/config/types.go`. Fix the compile fallout (`validator.go:1805` → `!= nil`). Verify with `go build ./...`.
- [ ] 1.2 Implement `config.DefaultDeployStrategy(*ServiceConfig)` per design D1, including the trial rolling validation on a copy, and call it at `validator.go:997`. Verify with a table test in `pkg/config/types_test.go` covering: stateless proxied, internal port-only, persistent, `ports`, `volumes`, portless, `maxSurge` < and ≥ replicas, `maxUnavailable`, `gracePeriod`, `promotion`, `smokeTest`, invalid `readiness` (expect recreate and no error), kind job/run.
- [ ] 1.3 Add a regression test that loads every `examples/**/tako.yaml` and the existing config fixtures and asserts that validation still succeeds. This is the "validates today" guarantee. Verify with `go test ./pkg/config/...`.
- [ ] 1.4 Update the existing expectations that assume a recreate default (`pkg/config/types_test.go:913`, `cmd/config_test.go:79`) and confirm that `tako config` prints `deploy.strategy: rolling (default)` for an eligible service. Verify with `go test ./cmd/ -run Config`.
- [ ] 1.5 Make `deployplan.EffectiveDeployStrategy` delegate to `DefaultDeployStrategy` for empty strategies. Update `pkg/deployplan/revision_identity_test.go:25-32`. Verify with `go test ./pkg/deployplan/...`.
- [ ] 1.6 Update `schema/tako.schema.json`: the deploy description (line 1096) states the new default rules, `rollbackOnFailure` gets a description and `"default": true`, and the `ports` description (line 585) mentions the implicit recreate. Verify with `go test ./schema/...`.

## 2. Readiness gate for defaulted rolling

- [ ] 2.1 Add the named constant `implicitTCPReadinessAttempts = 60` in `pkg/deployer` (not derived from `deploymentHealthWaitAttempts`). In `buildTakodHealthSpec` (`pkg/deployer/takod_deploy.go:1680-1685`), return the implicit TCP `HealthSpec` with `WaitAttempts: implicitTCPReadinessAttempts` when the strategy is defaulted rolling with no `healthCheck` or `readiness`. Verify with deployer unit tests asserting a non-nil spec with `Port == service.Port` and `WaitAttempts == 60` for defaulted rolling, and nil for explicit rolling and for recreate.
- [ ] 2.2 Make the implicit-probe timeout error name the probe and suggest `healthCheck` or `deploy.strategy: recreate`. Verify with a takod reconcile test using the fake docker runner and a closed port.

## 3. takod retain/restore/finalize protocol

- [ ] 3.1 Add `CapabilityDeployRestoreOnFailureV1` to `pkg/takod/server.go` and advertise it. Verify by updating the capability list expectations in `pkg/takod/server_test.go:75`.
- [ ] 3.2 Add `FailurePolicy` to `ReconcileServiceRequest`, and `RetainedContainers`/`RestoreToken` to the response, with request validation (only `remove|restore|retain-failed`). Verify with cases in `TestValidateReconcileServiceRequest`.
- [ ] 3.0 Add `runtimeid.RetainedContainerName(project, env, service string, slot int)` in `pkg/runtimeid/runtimeid.go`, built with `compactName` (design D9). Verify with unit tests: deterministic output, ≤128 characters for very long inputs, passes `isSafeContainerName` (`pkg/takod/reconcile.go:695-697`), and never equals the live slot name.
- [ ] 3.3 Implement `retainServiceContainersForReconcile` (leftover sweep by recomputing `RetainedContainerName` from container labels, graceful stop without rm, rename to the retained name) and wire it at `reconcile.go:192` for `recreate` + `restore`. No suffix matching and no fallback scheme. Verify with a fake-docker test asserting the `stop` → `rename` order and the absence of `rm` for the old containers, plus a test that a container named `*--prev` whose labels do not recompute to its name is left alone.
- [ ] 3.4 Implement local restore on the failure branches (`reconcile.go:211-229`): cleanup new, rename back, start, typed error `{restored, restoreError}`. Verify with tests for health failure, start failure and network-connect failure, and with a restore-failure test asserting `restored:false`.
- [ ] 3.5 Implement the `POST` finalize and restore endpoints (idempotent, scoped by project/env/service/revision) in `pkg/takod/server.go`. Verify with handler tests for success, unknown token (no-op), and repeat calls.
- [ ] 3.6 Implement `retain-failed`: stop without rm for both strategies. Verify with a rolling test (old revision untouched, failed revision containers stopped and present) and a recreate test, plus a test that the next same-revision reconcile removes them (`TestReconcileServiceNoDowntimeStrategyReplacesOnlyTargetRevision` style).
- [ ] 3.7 Add a persistent-service test: restore keeps the same mounts and publishes, and issues no volume create or remove commands. Verify with `go test ./pkg/takod/ -run Restore` (Linux CI; note any Windows-only path failures).

## 4. CLI orchestration and capability gate

- [ ] 4.1 Derive the failure policy from strategy and `RollbackOnFailure` in `deployServiceToTakodNode` (~`takod_deploy.go:1490-1549`). Verify with unit tests for the mapping table (design D6).
- [ ] 4.2 Add the capability preflight to the `Preflight` closure and extend the `needsCapabilities` predicate (`takod_deploy.go:448`). An explicit setting fails closed; a defaulted one downgrades the service to `remove` on all of its nodes when any node lacks the capability, omits `FailurePolicy` from every request, and emits one warning event naming the nodes and `tako upgrade servers`. Verify with fake-client tests for all four capability scenarios from the spec, including the two-node case where only one node is old.
- [ ] 4.3 Change the `Reconcile` closure to collect per-node results, then run finalize on success and restore on the succeeded nodes on failure, producing a `RecoveryReport`. Verify with a two-node fake test (A ok, B fail → restore called on A only; all ok → finalize on both; finalize error → warning, deploy still succeeds).

## 5. Engine reporting: plan, result, events, notification, history

- [ ] 5.1 Add `Strategy`/`StrategySource` to `PlanChange` and fill them in `newDeployPlanDocument`. Verify with an engine plan test asserting the JSON fields, and update plan-hash golden values if any.
- [ ] 5.2 Add the "Deploy strategy default changed: recreate → rolling" reason in `detectChanges` (`pkg/reconcile/reconcile.go:304-311`) and the strategy column in `FormatPlanVerbose`. Verify with reconcile plan tests.
- [ ] 5.3 Add `OutcomeRestored`, `ServiceOutcome.Strategy` and `ServiceOutcome.Recovery` and populate them in the failure branch (`pkg/engine/deploy.go:942-956`). Verify with engine tests for restored, failed restore, disabled and unsupported, each still exiting 1.
- [ ] 5.4 Add the event types in `pkg/takoapi/events/events.go` and emit them in order (failed → restore.started → restored or restore_failed). Verify with an ndjson event-order test and a test that recovery payloads contain no env values.
- [ ] 5.5 Add recovery status to the failure notification `Details`/`Message` (`deploy.go:1158-1171`). Verify with a notifier fake test.
- [ ] 5.6 Add `deployStrategy` and `recovery` to `ServiceStateDocument` (`pkg/takoapi/state.go:258`) and record them for the failed service. Verify with a history round-trip test.
- [ ] 5.7 Document the new `PlanChange`/`ServiceOutcome` fields, the `restored` action, the events, and the plan-hash note in `docs/MACHINE-INTERFACE.md`. Verify that `go test ./cmd/ -run Machine` still passes (no new commands, so `cmd/machine_coverage_test.go` is unchanged).

## 6. User documentation

- [ ] 6.1 Add a "Deploy strategies" section to `docs/CONFIGURATION.md` covering the default-selection rules, implicit TCP readiness (described as "about a minute"), `maxSurge` default (= replicas), `rollbackOnFailure` semantics, the restore limitations (data migrations), the overlap memory cost, and the opt-out. Fix the "(the default)" wording at line 1433. Verify that the doc renders and that its YAML snippets validate with `tako validate`.
- [ ] 6.2 Rewrite `docs/ORCHESTRATION-MODEL.md` "Deployment Strategies" (lines 347-372) for the new default, the retain/restore protocol, the capability gate and the per-service restore scope. Verify by review against spec scenarios.
- [ ] 6.3 Update the README feature lines (21, 248) to mention the rolling default and restore-on-failure. Run `make man` and confirm that `man/` and `completions/` have no diff, since no flags or help text change.

## 7. Integration checks

- [ ] 7.1 Run `go build ./...` and `go test ./...`. Triage Windows-only failures against known symlink/fsync/POSIX cases.
- [ ] 7.2 Run a manual end-to-end check on a Linux VPS: an upgrade from a recreate-defaulted deploy shows the plan reason, then a zero-downtime transition (curl loop sees no 5xx); a broken image on a recreate service restores the old container; a broken image on a defaulted-rolling service keeps the old revision serving; an old takod node triggers the fallback warning.

## Workflow follow-up

- Cross-check with sibling changes `add-project-wide-rollback` (consumes per-service recovery), `improve-logs-command` (filtering stopped retained containers) and `add-local-deploy-hooks` (hooks on restore events) before archive.
- Archive after review; verify the archived `openspec/specs/deploy-strategies/spec.md`.
