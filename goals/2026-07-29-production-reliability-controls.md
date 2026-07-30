# Goal: Production Reliability Controls

Date: 2026-07-29
Status: active
Branch: `goal/production-reliability-controls`

## Objective

Close the production-safety gaps identified for long-running workers, public
ingress, PostgreSQL recovery, managed volumes, scheduled work, and container
logs. Each milestone is implemented, tested, reviewed by two independent
agents, corrected, and committed before the next milestone starts.

## Validated Baseline

- `stopGracePeriod` is validated, fingerprinted, sent to takod, and rendered
  as Docker `--stop-timeout`, but normal reconciliation uses `docker rm -f`.
- The built-in proxy supports TLS, basic authentication, IP allowlists, and
  trusted proxies. It has no path policy, request-body limit, or custom
  response-header policy.
- Native backups archive live Docker volumes. S3 upload exists, but workload
  restore is local-only and no isolated PostgreSQL restore proof exists.
- A `kind: run` service can initialize volume ownership, but it is keyed to run
  history rather than creation of a fresh managed volume.
- Scheduled jobs persist bounded success/failure history. Neither job failures
  nor stale backups have durable alert episodes and delivery.
- Service stdout/stderr uses the Docker daemon's logging defaults. Caddy access
  log rotation is separate and does not protect application container logs.

## Milestone 1: Graceful Lifecycle

- Replace force removal in normal reconciliation and project cleanup with
  Docker stop semantics followed by non-force removal.
- Keep force removal only for explicit hard execution timeouts and fail-closed
  security paths.
- Capability-gate configured graceful stops and allow client requests to span
  the validated 24-hour maximum drain period.
- Require Caddy to acknowledge a published route configuration before stale
  revisions are pruned.
- Withdraw routes before stopping services removed from desired state.

Acceptance: tests prove stop precedes remove, no normal reconciliation emits
`rm -f`, long grace periods extend request deadlines, route reload is
synchronous, and deploy ordering is route withdrawal then service removal.

Evidence: completed in `f780a3e`; full build/tests, affected-package race
tests, Docker lifecycle proof, and two independent reviews passed.

## Milestone 2: Proxy Route Policy

- Add deny/allow path-prefix rules with deny precedence and segment-aware
  matching.
- Add per-route request-body limits and deterministic custom response headers.
- Validate path, size, header name, and header value inputs against bypass and
  Caddyfile-injection risks.
- Preserve policy in desired state/config export and capability-gate agents so
  security controls cannot be silently ignored.

Acceptance: generated Caddy configuration blocks internal paths, rejects
oversized bodies before proxying, applies configured headers, and passes route
policy conformance and state round-trip tests.

Evidence: implementation complete. Full build/tests, affected-package race
tests, live Caddy 2.10 path/body/header/error-path proofs, and two independent
reviews passed.

## Milestone 3: PostgreSQL Remote Restore Verification

- Preserve generic custom `pg_dump` S3 jobs without command inspection or
  reserved names.
- Add durable backup artifact identity and remote integrity verification.
- Add a non-destructive remote verification workflow that downloads the exact
  artifact, validates its digest, restores into an isolated PostgreSQL
  container/volume, runs readiness and a verification query, then cleans up.
- Keep production restore a separately confirmed destructive operation.

Acceptance: a custom dump job remains valid, corrupt/missing remote artifacts
fail closed, and an isolated PostgreSQL dump/restore round trip proves row-level
data without touching the production volume.

Evidence: implementation complete. Full build/tests, affected-package race
tests, Linux/Windows builds, generated manuals, real digest-pinned PostgreSQL
row-level restore plus hard scratch-quota overflow proof, exact S3 HTTP boundary
tests, crash-recovery tests, and two independent reviews passed.

## Milestone 4: Managed Volume Ownership

- Add creation-only UID/GID initialization for explicitly declared,
  Tako-managed local volumes.
- Never change existing or external volumes; existing-data migration remains
  an explicit `kind: run` operation.
- Serialize creation/initialization and persist enough ownership evidence to
  recover safely after interruption.

Acceptance: fresh volume roots receive the configured owner exactly once while
existing, external, bind, and unsupported-driver volumes remain untouched.

Evidence: implementation complete. Full build/tests, focused race suites,
Linux/Windows builds, generated manuals, crash/replacement/restore recovery
tests, desired-state secret-redaction tests, and two independent final reviews
passed. The opt-in live Docker empty-volume proof was attempted but the local
Colima VM was full; the same invariant is covered by the helper-command and
fresh-volume tests without writing recovery files into application data.

## Milestone 5: Scheduler And Backup Alerts

- Persist scheduled job and backup attempt facts needed to detect consecutive
  failure, timeout, skipped runs, no-success, and backup age.
- Persist deduplicated alert episodes and recovery transitions across restarts.
- Deliver through existing Slack, Discord, and generic webhook transports with
  bounded retry and without output or credential disclosure.
- Export machine-readable status/metrics so an external observer can detect a
  dead owner node; node-local delivery is not represented as node-down proof.

Acceptance: failure, stale, deduplication, retry, restart, and single recovery
notifications are test-covered, with no secrets in persisted or emitted data.

Evidence: implementation complete. Full tests, native/Linux/Windows builds,
`go vet`, focused changed-path race suites, schema/config tests, durable
dispatch/rotation/retirement and crash-recovery tests, snapshot-concurrency and
cancellable-shutdown tests, enrolled fence/lifecycle tests, credential-redaction
tests, Prometheus owner-presence/reachability tests, and two independent final
reviews passed. Full-package `pkg/takod` race execution remains blocked by the
pre-existing live Docker ownership test on the full local Colima VM; all
Milestone 5 race subsets pass.

## Milestone 6: Service Log Rotation

- Add opt-in per-service Docker logging configuration for the `local` and
  `json-file` drivers with bounded `maxSize` and `maxFiles`.
- Leave unset services on daemon defaults and reject drivers incompatible with
  `tako logs`.
- Include logging in config hashing, desired state/config export, agent
  capability preflight, and deterministic Docker arguments.

Acceptance: schema/validation, state round trip, drift detection, capability
failure, and Docker argument tests pass while existing service behavior stays
unchanged when logging is omitted.

## Completion Gate

- `go build ./...`
- `go test ./...`
- Every milestone has two completed diff reviews and all actionable findings
  are resolved before its conventional commit.
- Final audit maps each requested capability to code, tests, documentation, and
  commit evidence.
