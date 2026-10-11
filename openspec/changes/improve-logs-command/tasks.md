# Tasks

## 1. takod framed log stream (node side)

- [x] 1.1 Add `CapabilityLogsStreamV1 = "logs.stream-v1"` in `pkg/takod/server.go` and append it to the advertised capabilities list (`server.go:2544`); verify a `handleStatus` test asserts it is advertised.
- [x] 1.2 Add `LogsStreamRequest` + validation in new `pkg/takod/logs_stream.go` (safe project/env/service names, services non-empty, tail 0..10000, grep ≤1024 bytes and compiles as RE2, since/until accepted formats, until+follow rejected); verify with table tests in `pkg/takod/logs_stream_test.go`.
- [x] 1.3 Implement container discovery for multiple services (and `includeJobs` matching `tako.role=job`) returning name, `tako.slot`, `tako.revision`; zero containers is not an error; verify with the stubbed docker runner used in `pkg/takod/reconcile_test.go`.
- [x] 1.4 Implement per-container `docker logs -t` runner with split stdout/stderr, timestamp parsing, regex filter, and match ring buffer for tail-counts-matches; emit whole NDJSON `line` frames under a mutex; verify unit tests for ordering within a container, stream tagging, and "last N matches".
- [x] 1.5 Implement follow-mode `docker events` watcher that attaches new containers (`--since` start time) and emits `source attached/detached` and job `run started/finished` frames; verify with a fake events feed test, including a container replaced mid-stream.
- [x] 1.6 Add `handleLogsStream` (POST, JSON body, `application/x-ndjson`, terminal `end`/`error` frame instead of `http.Error` after first byte) and register `/v1/logs/stream` in `pkg/takod/lifecycle_gate.go:37`; verify handler tests for 405, 400 validation, success framing, and mid-stream error framing. Leave `/v1/logs` unchanged (existing `server_test.go` logs tests still pass).

## 2. Client and engine request model

- [x] 2.1 Add `takodclient.LogsStreamEndpoint` and a frame decoder type in `pkg/takodclient`; verify a decode test including unknown frame types being ignored.
- [x] 2.2 Extend `engine.LogsRequest` with `Services`, `Since`, `Until`, `Grep`, `IgnoreCase`, `Timestamps`, `TailExplicit` (keep `Service` as folded-in alias) and add validation (regex compile, time parse, until+follow) returning `InvalidRequestError`; verify `pkg/engine/logs_test.go` cases for each rejection exit class 2.
- [x] 2.3 Implement service resolution: empty → long-running services, named jobs → `includeJobs`, runs rejected, unknown rejected, de-duplication; verify table tests.
- [x] 2.4 Extend `LogsResult`/`LogsNodeResult` with `services`, `since`, `until`, `grep` (redacted), `ignoreCase`, `timestamps`, `lines`, `sources`, and `partial` status; verify JSON golden test that all pre-existing fields keep their names.

## 3. Engine fan-out, gating, and failure classification

- [x] 3.1 Add bounded concurrency (unexported constant `logsNonFollowNodeConcurrency = 8` local to `pkg/engine/logs.go`, non-follow only) to `StreamLogNodesWith` and keep deterministic result ordering; verify a test that counts max in-flight streams with 12 fake nodes.
- [x] 3.2 Per-node capability probe with legacy fallback rules (D8) and `CapabilityRequiredError` naming `tako upgrade servers`; verify tests: old agent + plain request → legacy endpoint; old agent + `--grep` → capability error; old agent + 2 services → capability error.
- [x] 3.3 Map frames to `log.line` events with additive `data.replica/container/revision/stream/time`; treat legacy "no containers found" as zero sources; verify event-shape tests and a redaction test that a registered env value is masked in `data.data` and `message`.
- [x] 3.4 Emit `warning` event per failed node immediately; replace `SummarizeLogStreamResults` classification with success/partial(`AttentionError`)/failed(`ConnectivityError` | `CapabilityRequiredError` | plain) and the zero-sources-anywhere error; wrap connect failures in `ConnectivityError`; verify `Classify` exit-class tests for each case (0, 6, 4, 2, 1).
- [x] 3.5 Update `docs/API-SDK.md` (StreamLogs request/response fields) and verify the doc names every new `LogsRequest` field.

## 4. Job logs

- [x] 4.1 Remove the follow refusal in `streamJobLogs`; follow routes the job through the framed stream on the owning node (resolve via existing `resolveJobOwner`), rendering run headers from `run` frames; verify tests for running job, idle job waiting message, and two consecutive runs.
- [x] 4.2 Non-follow job path: apply `--grep`/`--tail` client-side to the last run output, select newest run within `--since/--until`, warn once on `--timestamps`; verify unit tests on canned `JobRunsResult` data.

## 5. CLI flags and rendering

- [x] 5.1 In `cmd/logs.go`: make `--service` a repeatable `StringSlice`, drop `MarkFlagRequired`, accept positional services, add `--since`, `--until`, `-g/--grep`, `--ignore-case`, `-t/--timestamps`, detect explicit `--tail`; update `Long` help and examples; verify `cmd/logs_test.go` flag parsing tests.
- [x] 5.2 Implement prefix renderer (`service.slot@node`, padded, single-source unprefixed) and color: fixed 6-color ANSI palette (31-36) indexed by FNV-32a of the prefix, enabled only on a TTY with `NO_COLOR` unset and machine output off; add no color/prefix flags; verify tests that piped output and `NO_COLOR` output have no ANSI escapes, machine `message` never contains escapes, and the same prefix always maps to the same color.
- [x] 5.3 Update `docs/MACHINE-INTERFACE.md` `log.line` section (new data keys, `LogsResult` fields, `partial` status and exit 6) and the `log.line` doc comment in `pkg/takoapi/events/events.go`; verify `cmd/machine_coverage_test.go` still passes (`go test ./cmd -run Machine`).
- [x] 5.4 Regenerate `man/` (`make man`) and `completions/`; verify `man/tako-logs.1` lists the new flags and `git diff --stat` touches only logs-related generated files.

## 6. Secret redaction fix (design D11)

- [x] 6.1 Add `secrets.SecretRefKey(ref string) string` in `pkg/secrets` (part after the first `:`, else the whole ref) and use it in `CreateEnvFile` (`pkg/secrets/manager.go:492-501`), `buildExecEnvFileContent` (`pkg/engine/exec.go:326`) and `registerServiceSecretValues` (`exec.go:358`); verify table tests for `KEY`, `VAR:KEY`, and `VAR:KEY:EXTRA`, and an exec test that an aliased secret value is registered.
- [x] 6.2 Change `registerServiceSecretValues` to return an error instead of returning silently on manager-creation or resolution failure (`exec.go:352-355`); update existing callers to keep their current behavior; verify `go build ./...`.
- [x] 6.3 Call `e.registerServiceSecretValues(cfg, envName, services)` in `StreamLogs` after `pkg/engine/logs.go:132` for all selected services including jobs; on error emit one `events.TypeWarning` that sourced values will not be redacted and continue; verify an engine test with a failing provider asserting one warning and that streaming proceeds.
- [x] 6.4 Wire the same helper (with the same warning on failure) into `Rollback` (`pkg/engine/rollback.go:141-149`), `Promote` (`promote.go:107-115`) and `Scale` (`scale.go:128-136`); verify an engine test per command where a fake health-failure error embedding a sourced value is redacted in the result error.
- [x] 6.5 Add an end-to-end engine test that stubs the provider CLI the way `pkg/secrets/provider_test.go` does, configures one `secretSources` value and one aliased ref `API_KEY:STRIPE_KEY`, streams a fake node line containing both, and asserts the `log.line` event masks both in `data.data` and `message`; verify `go test ./pkg/engine ./pkg/secrets`.
- [x] 6.6 Remove any documented "secretSources values are not redacted" limitation and document the unresolved-secrets warning in `docs/MACHINE-INTERFACE.md`; verify by review.

## 7. Integration verification

- [x] 7.1 Run `go build ./...` and `go test ./...` (ignoring known Windows-only failures) and verify no new failures.
- [ ] 7.2 Manual check against a two-node test environment with one upgraded and one legacy agent: plain logs, `--grep`, `-f` across a `tako deploy`, and a followed job run; record observed exit codes (0/6/2) in the PR description.
