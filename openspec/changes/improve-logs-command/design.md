# Design

## Context

Observed behavior today (see proposal.md - Why for motivation):

- CLI: `cmd/logs.go:19-40` defines `--server/-s`, `--service` (marked required at `:39`), `--follow/-f`, `--tail/-n` (default 100). `runLogs` (`:42-70`) builds `engine.LogsRequest` and emits the result document.
- Engine: `pkg/engine/logs.go:80-226` `StreamLogs` rejects an empty service (`:93-95`), routes jobs to `streamJobLogs` (`:110-112`), rejects runs (`:113-115`), registers server passwords and service env values as secrets (`:125-132`), then fans out with `StreamLogNodesWith` (`:367-402`) — one goroutine per node, unbounded — calling `streamLogsFromNode` (`:303-363`) which reads the takod text stream line by line and emits `log.line` via `emitLogLine` (`:284-294`). The human prefix is `[node]` only when more than one node is selected (`formatLogLineMessage`, `:296-301`; `prefix := len(names) > 1` at `:372`).
- Failure summary: `SummarizeLogStreamResults` (`:406-422`) returns a plain `fmt.Errorf` for both partial and total failure, so `engine.Classify` (`pkg/engine/errors.go:83-115`) yields `ClassFailed` (exit 1) in both cases; connection errors from `streamLogsFromNode` (`:318-321`) are not wrapped in `ConnectivityError`.
- Jobs: `streamJobLogs` (`pkg/engine/logs.go:230-273`) refuses `--follow` (`:231-233`) and prints `runs.Runs[0].Output` from `JobRuns` (`pkg/engine/jobs.go:146-190`). That output is a capped buffer of the run (`pkg/takod/jobs.go:1018-1024`, `:1055`; `JobRunRecord.Output` at `:152-153`). Job containers are one-off `docker run --rm` containers labeled `tako.project/environment/service=<job>` and `tako.role=job` (`pkg/takod/jobs.go:1197-1211`, `jobRoleLabel` at `:41`). The refusal is therefore a design choice, not a technical limit: while a run is in progress its container is visible to `docker ps` and `docker logs -f` works; after exit it is removed.
- takod: `handleLogs` (`pkg/takod/server.go:2194-2236`) accepts `project, environment, service, tail, follow` as query params and calls `StreamServiceLogs` (`pkg/takod/logs.go:22-68`). It lists containers with `docker ps -a` filtered by project/env/service labels (`:98-114`), errors when none are found (`:34-36`, surfaced as HTTP 502 and thus a node failure even when the service is simply not placed on that node), and runs `docker logs --tail N [-f]` per container (`:116-130`). Non-follow streams containers sequentially; follow merges them through a mutex-guarded writer (`:47-67`) with no per-line attribution and no line-boundary guarantee across containers. Errors after streaming has begun are written with `http.Error` (`server.go:2232-2234`), which corrupts the body. Max tail is 10000 (`logs.go:20`). Route registration: `pkg/takod/lifecycle_gate.go:37`.
- Client endpoint: `takodclient.LogsEndpoint` (`pkg/takodclient/client.go:558-568`); streaming via `StreamOutputWithContext` (`:244-284`, GET only); POST streaming exists as `StreamRequestOutputWithContext` (`:307`).
- Capabilities: constants in `pkg/takod/server.go:119-203`, advertised in the status handler (`server.go:2544`); clients check with `takodclient.RequireCapability` (`pkg/takodclient/capability_error.go:23-40`) returning `CapabilityRequiredError`, which `Classify` maps to `ClassInvalid` (`errors.go:98-101`).
- Replica identity is available as the `tako.slot` label (`pkg/reconcile/config_hash.go:19`) and revision as `tako.revision` (`:15`); container names are compacted/hashed (`pkg/runtimeid/runtimeid.go:23-31`) and must not be parsed.
- `tako access` (`cmd/access.go`) duplicates the fan-out/summary pattern (`:200-235`, `:313-329`) and has its own `lockedLogWriter` (`:189-198`); it is left behaviorally unchanged.
- Machine docs: `docs/MACHINE-INTERFACE.md:61-67` documents `log.line` with `service`, `node`, `data.data`; `cmd/machine_coverage_test.go:40` already registers `tako logs`.

## Goals / Non-Goals

**Goals:**
- One framed, attributed, filterable log stream per node, regardless of service count.
- Filtering executed on the node so `--grep`/`--since` do not ship unmatched lines over SSH.
- Follow that survives container churn and covers jobs.
- Additive machine contract; precise partial-failure exit semantics.

**Non-Goals:**
- Global time-ordering of follow output across nodes (arrival order is accepted).
- Changing the legacy `/v1/logs` endpoint (kept for older CLIs).
- Refactoring `tako access` onto the new fan-out (possible follow-up).

## Decisions

### D1. New takod endpoint `POST /v1/logs/stream` with NDJSON frames, capability `logs.stream-v1`
Request body `LogsStreamRequest{project, environment, services []string, includeJobs []string, tail, since, until, grep, ignoreCase, follow}`; response is `application/x-ndjson` frames:
`{"type":"line","service","slot","container","revision","stream","time","data"}`, `{"type":"source","event":"attached|detached",...}`, `{"type":"run","job","event":"started|finished","exitCode"}`, and a terminal `{"type":"end"}` or `{"type":"error","message"}`.
takod runs `docker logs -t --tail N [--since] [--until] [-f]` per container with stdout and stderr on separate pipes (giving `stream`), splits the Docker timestamp into `time`, applies the compiled regex, and writes whole frames under a mutex.
- Why a new endpoint rather than new query params on `/v1/logs`: an older takod silently ignores unknown params and would return unfiltered text; a new path 404s on old agents (fails closed) and lets the body be JSON. The CLI still pre-checks the capability for a clear message.
- Why NDJSON frames: per-line attribution, timestamps, and in-band errors after streaming has started (fixes the `http.Error` mid-body issue for the new path).
- Alternative rejected: client-side filtering over the legacy text stream — ships every line over SSH (unbounded with `--since 24h`), cannot attribute replicas, and cannot implement "tail counts matches".

### D2. Grep semantics: RE2, server-side, tail counts matches
Pattern compiled with Go `regexp` on both CLI (early validation, exit 2) and takod (authoritative); `--ignore-case` prepends `(?i)`. Max 1024 bytes. With grep and no follow, takod scans `docker logs --tail <scan>` where scan = `maxLogTail` (10000) or the `--since/--until` window, keeping a ring buffer of the last N matches per container; in follow mode live lines are filtered as they arrive (after emitting the matching historical tail). Kamal applies grep after tail; we deliberately count matches because "last 10 errors" is the common incident query. The 10000-line scan bound is documented.

### D3. Time window
`--since/--until` accept Go duration, RFC3339, or Unix seconds; the CLI validates and normalizes absolute values to RFC3339 UTC but forwards durations unchanged so Docker evaluates them against the node clock (avoids CLI/node skew). `--until` with `--follow` is rejected. `--since` without an explicit `--tail` (detected via `cmd.Flags().Changed("tail")`) sends tail=10000.

### D4. Service selection resolves in the engine
`LogsRequest.Service string` becomes `Services []string` (the old field kept as a deprecated alias for API-SDK callers and folded in). Empty → all services where `!IsJob() && !IsRun()` from `cfg.GetServices`. Named jobs are passed as `includeJobs` so takod also matches `tako.role=job` containers. Named runs are rejected with the existing message (`logs.go:113-115`). Positional args are added (`cobra.ArbitraryArgs`) for parity with `tako access [service]`.

### D5. Fan-out: one stream per node, bounded in non-follow mode
`StreamLogNodesWith` gains a concurrency limit (semaphore, 8) applied only when `follow == false`; follow streams all nodes at once since a held slot would starve others (node counts per environment are small). The limit is an unexported constant local to `pkg/engine/logs.go` (`logsNonFollowNodeConcurrency = 8`); it is not shared with any other fan-out and not configurable (resolved). A future global `--parallel` setting would be its own change. Each node goroutine: connect → capability probe (`RequireCapability`, cached per node via the factory) → stream. A node error emits `events.TypeWarning` with `node` immediately. Summary classification: none failed → `success`; some failed → `partial`, returned as `AttentionError` (exit 6); all failed → `failed`, returned as `ConnectivityError` when every failure is a connection error (exit 4), `CapabilityRequiredError` when every failure is a capability gap (exit 2), otherwise plain (exit 1). `streamLogsFromNode`'s connect failure is wrapped in `ConnectivityError`.
- Alternative rejected: keep exit 1 for partial — indistinguishable from total failure for automation; exit 6 already means "partial success needing attention" (`errors.go:28-29`).

### D6. Missing containers are not a node failure (new path)
takod returns frames for zero sources and `end`; the engine fails only if the sum of attached sources across all nodes is zero (exit 1, "no containers found for <services>"). The legacy path keeps its 502 behavior; when using the legacy fallback, the engine treats an HTTP error body equal to `no containers found for service X` as zero sources to give consistent behavior with old agents.

### D7. Follow survives container churn; job follow
In follow mode takod runs `docker events --filter type=container --filter event=start` with the project/environment labels plus a service filter, and attaches `docker logs -t -f --since <container start>` to each new matching container; `die`/`destroy` emit `source detached`. For jobs this yields "follow the current run, then every next run": on start a `run started` frame, on exit a `run finished` frame with the exit code read from the `die` event's `exitCode` attribute. Runs shorter than the attach latency may lose early lines; `--since <start>` on attach recovers lines still held by Docker, and `--rm` removal can race — documented, and the run record (`tako jobs runs`) remains the durable source. Non-follow job logs keep the existing run-record path (no capability needed), with `--grep/--tail` applied client-side to the recorded output; `--since/--until` select the newest run whose `startedAt` lies in the window; `--timestamps` is a no-op with one warning (records have no per-line times).
- Alternative rejected: polling `docker ps` every N seconds — slower to attach and loses more early output.

### D8. Capability gate and legacy fallback
`CapabilityLogsStreamV1 = "logs.stream-v1"` added next to the others (`pkg/takod/server.go:119-203`) and to the advertised list (`server.go:2544`). Per node: capability present → new endpoint. Absent → if the request uses no new feature (single non-job service, no since/until/grep/timestamps) use legacy `LogsEndpoint` as today; else fail that node with `CapabilityRequiredError{Feature: "filtered/attributed logs"}` whose message names `tako upgrade servers`. Multi-service on an old agent is rejected rather than looping N legacy streams, to keep stream-per-node and avoid N× SSH sessions.

### D9. Rendering in `cmd/logs.go`
Engine emits `log.line` with `Message` = rendered human line. Prefix `service.slot@node` (slot = `tako.slot`; `run` for jobs; omitted slot → `service@node` on legacy path). Prefix shown when the set of distinct sources seen so far or expected (>1 service, >1 node, or replicas>1 per config) exceeds one; width padding uses the longest expected prefix. Color (resolved): a fixed palette of 6 basic ANSI foreground colors (31 red, 32 green, 33 yellow, 34 blue, 35 magenta, 36 cyan), indexed by `fnv32a(prefix) % 6`. It is enabled only when `term.IsTerminal(stdout)`, `NO_COLOR` is unset (any value, including empty-but-set, disables it), and `!machineOutputEnabled()`. There are no new flags: no `--color` and no `--no-prefix`. Colorization is done in the CLI renderer, never in `message` of machine events. `--timestamps` prepends `data.time`.
Non-follow ordering: lines are emitted in arrival order per node (each container's lines in order); no global sort (keeps memory bounded and output streaming).

### D10. Machine contract
`log.line` keeps `service`, `node`, `data.service`, `data.node`, `data.data`; adds `data.replica` (string slot or `run`), `data.container`, `data.revision`, `data.stream`, `data.time` — all omitted on the legacy path. `LogsResult` adds `services []string`, `since`, `until`, `grep`, `ignoreCase`, `timestamps`, `LogsNodeResult.lines`, `LogsNodeResult.sources`; `service` populated only for single selection; status `partial` added. The grep pattern is passed through `RedactSensitive` before placement in the result. Redaction of line content continues through the event stream redactor (`pkg/engine/engine.go:91-96`). That redactor covers `Message` and only *top-level* string values in `Data` (`pkg/takoapi/events/sinks.go:106-129`), so the line content MUST stay in the top-level `data.data` string; it must never be nested inside a map or slice in `Data`.

### D11. Redaction covers sourced and aliased secrets (bug fix, in scope)
**Observed bug.** Two gaps let secret values reach `tako logs` output (and rollback/promote/scale errors) in plaintext:
- `StreamLogs` registers only server passwords and literal `service.Env` values (`pkg/engine/logs.go:125-132`). Values resolved from `secretSources` and from local `.tako/secrets*` are never registered.
- Secret refs may be aliased as `CONTAINER_VAR:SECRET_KEY` (`pkg/secrets/manager.go:492-501`). `buildExecEnvFileContent` (`pkg/engine/exec.go:326`) and `registerServiceSecretValues` (`exec.go:358`) call `mgr.Get(ref)` with the *whole* ref, e.g. `API_KEY:STRIPE_KEY`, which never resolves, so aliased secrets are never registered anywhere.

**Fix.**
- (a) Add `secrets.SecretRefKey(ref string) string` in `pkg/secrets`, returning the part after the first `:` when present, else the whole ref. Use it in `CreateEnvFile` (`manager.go:492-501`, replacing the inline `SplitN`), in `buildExecEnvFileContent` (`exec.go:326`), and in `registerServiceSecretValues` (`exec.go:358`).
- (b) In `StreamLogs`, call `e.registerServiceSecretValues(cfg, envName, services)` right after the existing env/password registration (after `logs.go:132`), for *all* selected services, including named jobs.
- (c) `registerServiceSecretValues` returns an error instead of returning silently when the secrets manager cannot be created or a referenced secret cannot be resolved (`exec.go:352-355`). `StreamLogs` turns that error into one `events.TypeWarning` ("could not resolve secrets for <env>; values from secretSources/.tako/secrets will not be redacted: <redacted error>") and continues streaming. Logs is read-only and incident-critical, so it does not fail closed.
- (d) Wire the same helper into `Rollback` (`pkg/engine/rollback.go:141-149`), `Promote` (`promote.go:107-115`) and `Scale` (`scale.go:128-136`) next to their existing registration blocks, with the same warning on failure. These matter because a health-check failure error embeds the last 50 container log lines (`pkg/takod/reconcile.go:1385-1389`), which can contain secret values.
- (e) Tests stub the provider CLI exactly as `pkg/secrets/provider_test.go` does, configure one `secretSources` value and one aliased ref (`API_KEY:STRIPE_KEY`), stream a fake node line that contains both values, and assert the emitted `log.line` masks both in `data.data` and `message`.

No takod change and no capability are involved; registration is CLI-side only.
- *Alternative rejected:* fail `tako logs` when secrets cannot be resolved. It would block incident debugging when a vault provider is down.

## Risks / Trade-offs

- [Exit code for partial failure changes 1 → 6] → Called out as BREAKING in proposal; documented in MACHINE-INTERFACE; any-non-zero consumers unaffected.
- [Grep scan bound of 10000 lines per container may miss older matches] → Documented; `--since` narrows the window; matches the existing `maxLogTail` safety limit.
- [`docker events` watcher adds a long-lived process per follow request on the node] → Bound by the request context; killed on client disconnect; one watcher per request, not per container.
- [Short job runs may lose output when followed] → Attach with `--since` container start; document that `tako jobs runs` is authoritative.
- [Secret resolution needs provider CLIs and credentials on the operator machine; when unavailable, sourced values cannot be registered] → One `warning` event says sourced values will not be redacted (D11); the stream continues.
- [`tako logs` now invokes vault provider CLIs, adding latency before the first line] → Resolution happens once per invocation, before fan-out; services without `secrets` skip it.
- [Grep can confirm presence of a redacted value via match/no-match] → Same data is already readable through unfiltered logs by the same operator; accepted.
- [Clock skew between nodes makes interleaved follow output appear out of order] → `--timestamps` exposes node time; global ordering is a non-goal.

## Migration Plan

1. Ship takod with `/v1/logs/stream` and `logs.stream-v1`; legacy `/v1/logs` unchanged.
2. Ship CLI; plain `tako logs --service X` works against both agent versions. New flags require `tako upgrade servers`.
3. Rollback: downgrading the CLI restores old behavior; downgrading takod makes new flags fail closed with the upgrade message.

## Accepted Assumptions

- `docker events` and `docker logs --since/--until/-t` are available on all supported Docker versions installed by `tako setup` (Docker ≥ 20.10). When `docker logs` fails mid-stream, takod reports it with an in-stream `{"type":"error"}` frame (D1), never with `http.Error` after the first byte.
- The event stream redactor applies to `Message` and top-level string values in `Data` only. Line content therefore stays in top-level `data.data` (D10).
- Environments have few enough nodes that unbounded follow fan-out is acceptable (today's behavior).
