# Proposal

## Why

`tako logs` is the first thing operators reach for during an incident, but today it only answers "show me the tail of one service": `--service` is mandatory, there is no time window or search, replicas on a node are merged without attribution, job logs cannot be followed, and a node that simply does not host the service is reported as a failure. Kamal's `kamal app logs` (`--since`, `--lines`, `--grep`, `--follow`, across roles) sets the baseline users expect, and closing this gap is part of the Kamal-parity track.

## What Changes

- `--service` becomes optional and repeatable (`--service web --service worker`, `--service web,worker`, or positional `tako logs web worker`). With no service, logs stream from every long-running service in the environment.
- Each human line carries a stable source prefix `service.slot@node` when more than one source is shown, colorized per source on a TTY (honoring `NO_COLOR`), plain otherwise.
- New flags: `--since` and `--until` (Go duration or RFC3339/Unix timestamp, passed to `docker logs`), `-g/--grep <RE2 regex>` with `--ignore-case`, and `-t/--timestamps`.
- Filtering (`--since/--until/--grep`) and source attribution run on takod through a new framed endpoint `POST /v1/logs/stream`, gated on a new capability `logs.stream-v1`. Plain `tako logs --service X` keeps working against older agents through the legacy `/v1/logs` path; any new feature against an older agent fails closed with an upgrade message.
- `--grep` semantics: with a grep, `--tail N` counts matching lines (per container), not scanned lines.
- Job logs: `--follow` is now allowed for jobs and follows the run in progress plus any subsequent runs until interrupted; non-follow job logs keep showing the most recent recorded run.
- Follow mode survives container replacement (deploy/restart): new matching containers are attached automatically.
- Node fan-out: one stream per node regardless of how many services are selected; non-follow fan-out is bounded (8 concurrent nodes). A node with no matching containers is no longer an error.
- Machine interface: `log.line` events gain additive `data` keys `replica`, `container`, `stream`, `time`, `revision`. `LogsResult` gains `services`, `since`, `until`, `grep`, `ignoreCase`, `timestamps`, per-node `lines`, and status `partial`.
- **BREAKING (machine contract, exit codes)**: when some but not all target nodes fail, `tako logs` now exits 6 (attention, `status: "partial"`) instead of 1; a per-node `warning` event is emitted as soon as a node fails. All-nodes-failed still exits non-zero (connectivity class 4 when every failure is a connection failure, otherwise 1). Migration: consumers treating any non-zero exit as failure are unaffected; consumers matching exit 1 for partial failure should accept 6.
- **BREAKING (human output only)**: the multi-node prefix changes from `[node]` to `service.slot@node`. Machine output is unaffected (`message` is documented as human rendering).
- **Bug fix (secret redaction):** values resolved from `secretSources` and local `.tako/secrets*` are now registered for redaction in `tako logs`, and aliased secret refs (`API_KEY:STRIPE_KEY`) are now registered everywhere (today they never resolve for redaction). If secrets cannot be resolved, `tako logs` emits a `warning` that sourced values will not be redacted and continues. `tako rollback`, `tako promote` and `tako scale` register the same values, because their health-failure errors embed container log lines.
- No `tako.yaml` changes; no schema changes; no new color or prefix flags.

Out of scope:
- Persisted/central log aggregation, log shipping, or retention changes (`logging:` config is untouched).
- `tako access` behavior (proxy access logs) beyond sharing helpers.
- Deploy-time `run` services (still rejected with the existing message).
- `--grep-options` passthrough to `grep(1)`, inverted matches, and context lines (`-A/-B`); regex flags beyond case-insensitivity can be written inline (`(?i)`, `(?s)`).
- Changing the `-s` (server) and `-n` (tail) short flags to match Kamal; command aliases are owned by the `add-command-aliases` change.

## Capabilities

### New Capabilities
- `logs`: behavior of `tako logs` — service/source selection, filtering (time window, regex, tail), follow semantics for services and jobs, multi-node fan-out and partial-failure reporting, human rendering, and the `log.line`/`LogsResult` machine contract, including the takod `logs.stream-v1` capability gate.
- `secret-redaction`: which resolved secret values (literal env, `.tako/secrets*`, `secretSources`, aliased refs) are registered with the event redactor for `tako logs`, `tako rollback`, `tako promote` and `tako scale`, and what happens when they cannot be resolved.

### Modified Capabilities
- None (no main specs exist yet).

## Impact

- CLI: `cmd/logs.go` (flags, positional args, rendering), `cmd/logs_test.go`.
- Engine: `pkg/engine/logs.go` (`LogsRequest`, `LogsResult`, `StreamLogs`, `streamJobLogs`, `StreamLogNodesWith`, `SummarizeLogStreamResults`), `pkg/engine/logs_test.go`.
- Secret redaction: new `secrets.SecretRefKey` in `pkg/secrets`, used by `pkg/secrets/manager.go` (`CreateEnvFile`) and `pkg/engine/exec.go` (`buildExecEnvFileContent`, `registerServiceSecretValues`, which now returns an error); registration added to `pkg/engine/logs.go`, `rollback.go`, `promote.go` and `scale.go`.
- takod: new `pkg/takod/logs_stream.go` (framed multi-service streaming with filter, container watch), route in `pkg/takod/lifecycle_gate.go`, capability constant and advertisement in `pkg/takod/server.go`; legacy `/v1/logs` unchanged.
- Client: `pkg/takodclient/client.go` (endpoint/request helper), reuse `StreamRequestOutputWithContext`.
- Events: `pkg/takoapi/events/events.go` doc comment for `log.line` data keys.
- Docs/generated: `docs/MACHINE-INTERFACE.md`, `docs/API-SDK.md`, `man/tako-logs.1`, `completions/*`; `cmd/machine_coverage_test.go` already lists `tako logs`.
- Requires a takod upgrade (`tako upgrade servers`) for any new filtering/attribution feature; older agents keep serving plain logs.
- Related changes: `add-command-aliases` (short aliases), `clean-up-dead-config-and-docs` (docs touch points may overlap).
