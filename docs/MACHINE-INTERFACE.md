# Tako Machine Interface

This document is the contract for programs that drive the `tako` CLI as a
subprocess — control planes, CI systems, and scripts. It covers the
invocation model, structured output modes, event and result schemas, exit
codes, and compatibility promises. Human-facing behavior is unchanged when
none of these modes are enabled.

Go programs should prefer importing `pkg/engine` directly (see
`docs/API-SDK.md`); this interface exists for non-Go consumers.

## Invocation Model

- Run `tako` inside a **workspace directory per project + environment**:
  a directory containing `tako.yaml` (authored, generated, or materialized
  via `tako config export`/`tako config pull`) and the local `.tako/` state
  the CLI maintains. Concurrent operations for the *same* project+environment
  are serialized by the local state lock and remote takod leases; operations
  for different projects can run concurrently from separate workspaces.
- `tako run IMAGE ...` is the configless path and needs no workspace config;
  it still respects remote leases on the target node.
- Set `TAKO_NONINTERACTIVE=1` (or `CI=1`) in the environment. Any code path
  that would prompt then fails fast with a typed error instead of blocking
  on stdin.
- Set `TAKO_SKIP_UPDATE_CHECK=1` to suppress the background update check.
- SSH host key policy: pass `--host-key-mode strict` with a pre-provisioned
  `known_hosts`, or accept trust-on-first-use with `tofu` (default). The
  `ask` mode requires a terminal and must not be used by machine consumers.

## Output Modes

Two independent global flags control machine output. Enabling either one
reserves **stdout for parseable output only**; human rendering and build
logs move to **stderr**.

### `--output json`

Emits exactly one JSON document on stdout when the operation finishes: the
operation's result document (or plan document with `--plan-only`, or a
`ConfirmationRequired` document — see below). Progress is not on stdout.

### `--events ndjson`

Streams one JSON event per line on stdout **as the operation progresses**,
ending with a terminal event of type `result` whose `data.result` field
carries the same result document. Combine with `--output json` to also get
the bare result document appended after the event stream.

`tako logs --events ndjson` emits service log entries as structured
`log.line` events on stdout. Each event sets `service`, `node`, and
`data.data` (the raw log line without the trailing newline); `message`
contains the human rendering, including the node prefix used for multi-node
text output. `tako access --events ndjson` mirrors this with `access.line`
events carrying the raw proxy access-log entry in `data.data`, the source
node in `data.node`, and the formatted rendering in `message`.

### Event schema

Events follow `pkg/takoapi/events.Event` (apiVersion
`tako.redentor.dev/v1alpha1`, kind `Event`):

```json
{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "Event",
  "seq": 12,
  "time": "2026-07-06T12:00:00Z",
  "type": "deploy.service.reconciled",
  "phase": "deploy",
  "level": "info",
  "service": "web",
  "message": "  ✓ Service web reconciled by takod\n",
  "data": { "image": "registry.example.com/web:abc123" }
}
```

- `seq` is monotonic per process. `message` is the human rendering (may
  contain newlines); parse `type`, `phase`, `level`, `service`, and `data`
  instead of `message`.
- Consumers MUST ignore unknown event types, fields, and `data` keys —
  the schema grows additively.
- `level` is one of `debug`, `info`, `warn`, `error`. Debug events are
  always present in the stream (the CLI's `--verbose` gating applies only
  to human rendering).

### Result documents

Mutation commands return versioned result documents (for example
`DeployResult`, kind `DeployResult`, apiVersion
`tako.redentor.dev/v1alpha1`) containing: project, environment, `status`
(`success`, `warmed`, `failed`, ...), revision, per-service outcomes
(`deployed`, `warmed`, `removed`, `up_to_date`, `failed`), URLs, timings,
`planHash`, and `error` when failed. `tako ps --output json` returns a
`StatusResult` document with project/environment, selected server(s), optional
service filter, and service rows (`name`, `running`, `desired`, `status`,
`ports`, `revision`, `warming`, `internal`, `image`, `strategy`, `health`,
`nodes`). `health` aggregates the docker health-check state of the service's
active containers (worst wins: `unhealthy` > `starting` > `healthy`) and is
empty when no container defines a health check or the node agent predates
health capture. `nodes` breaks placement down per selected node (`name`,
`running`, `warming`, `health`); nodes not running the service are omitted,
and job/run rows carry no breakdown. `tako logs` returns a
`LogsResult` document with project, environment, service, tail/follow options,
per-node stream outcomes, timings, and `error` when any node stream failed.
`tako access` returns an `AccessResult` document with the same shape (project,
environment, optional service filter, tail/follow options, per-node stream
outcomes, timings, `error`); the log entries themselves stream as
`access.line` events.
`tako history --output json` returns a `HistoryResult` document with
project/environment, requested server/status/limit filters, the source server
selected from the mesh, and deployment rows (`id`, `displayId`, `commit`,
`timestamp`, `version`, `status`, `durationSeconds`, `duration`, `message`,
`error`). `tako config export` and `tako config pull` return a
`ConfigExportResult` document with project/environment, source node, target
nodes, present state documents, generated server entries, warnings,
password-redaction status, `outputPath` when a file was written, the
materialized `config` object, and `yaml` when no file was written. `tako state
pull --output json` returns a `StatePullResult` with project/environment,
requested server, status (`synced_history`, `recovered_mesh_actual`,
`recovered_running_mesh`, or `none_found`), source server and latest deployment
summary when history was synced, synced count, recovery counts, and recovery
warning/error details when fallbacks failed. `tako state lease --output json`
returns a `StateLeaseResult` with selected servers and per-node lease/error
entries. `tako state lease release --output json --id <lease> --force` returns
a `StateLeaseReleaseResult` with the exact `leaseId`, selected servers,
released node names/count, and node lease/error entries. `tako state status
--output json` returns a `StateStatusResult` with project/environment,
requested server filter, local `.tako` summary, per-node remote history,
desired/actual runtime state, lease and reachability details, best-known
sources, sync recommendations, node counts, unreachable-node guidance, and
`error` when no selected node is reachable. `tako state forget-node NODE --yes
--output json` returns a `StateForgetNodeResult` with selected servers, the
retired node, requested server filter, per-node cleanup outcomes
(`nodeActualExisted`, `aggregatePruned`, warnings/errors), and a summary of
reachable nodes, standalone snapshots found, and aggregate actual states
pruned. `tako state repair --output json` returns a `StateRepairResult` with
project/environment, requested server filter, reachable node count, selected
history/desired/actual/node-actual sources, per-node and aggregate write
counts, warning/error details, local `.tako` sync status/count, and final
`error` when repair is incomplete or fails. `tako rollback` returns a
`RollbackResult` with project/environment, the service, the target
`deploymentId`, restored `version`, `status`, and timings. `tako promote`
returns a `PromoteResult` with project/environment, the service, the promoted
`revision` and `image`, `status`, and timings. `tako scale` (and its `start`/
`stop` wrappers) returns a `ScaleResult` with project/environment, `status`,
per-service outcomes with replica counts, timings, and `error` when
reconciliation failed. `tako placement plan cordon|drain|rebalance` returns a
`PlacementMovementPlan` bound to `inputRevisionId`, with current/proposed
assignments, explicit moves, persistent-volume blockers, `executable`, and a
content-derived `planId`. `tako placement verify PLAN --plan-id ID` accepts
only an intact plan whose digest matches the separately recorded review ID.
`tako remove` returns a `RemoveResult` with
project/environment, `scoped` when `--server` narrowed the target set,
per-server outcomes (`name`, `host`, `removed`, `error`), timings, and
`error` when removal was incomplete. `tako destroy` returns a `DestroyResult`
with project/environment, `mode` (`DECOMMISSION` or `PURGE`), `purgeAll`,
per-server outcomes (`name`, `host`, `destroyed`, `error`), timings, and
`error` when destruction was incomplete. `tako validate --output json`
returns a `ValidateResult` with the resolved `configPath`,
project/environment, `valid`, `findings` (`severity`, `path`, `field`,
`message`) for errors and non-fatal validation warnings, and the resolved environment summary (runtime,
state backend, consistency, mesh, server/service counts) when valid;
invalid configs exit with code 2 and still emit the document. `tako doctor
--output json` returns a `DoctorResult` with project/environment,
`skipRemote`, overall `status` (`ok` or `attention`), pass/warn/fail
counts, and `checks` (`name`, `status` `pass|warn|fail|skip`, `detail`,
`remediation`); any failed check exits with code 6 and still emits the
document. `tako drift --output json` (single-shot; `--watch` is rejected in
machine modes) returns a `DriftResult` with project/environment, `drifted`,
per-drift entries (`service`, `type`, `severity`, `expected`, `actual`,
`details`), `servicesOk`, and timings; detected drift exits with code 6 and
still emits the document. `tako metrics --output json` (single-shot;
`--live` is rejected in machine modes) returns a `MetricsResult` with
project/environment, the `--server` filter when set, `collectedAt`, and
per-node samples where `metrics` carries the takod `/v1/metrics` document
verbatim (monitoring-agent schema) or `error` says why the read failed;
all nodes failing exits 1, a partial read exits 6, both still emit the
document. `tako stats --output json` (point-in-time; `--live` is rejected
in machine modes) returns a `StatsResult` with project/environment, the
`--service`/`--all` filters, `collectedAt`, and per-node samples whose
`containers` reuse the takod stats schema (`name`, `cpuPercent`,
`memUsage`, `memPercent`, `netIO`, `blockIO`, `pids`); the same
all-fail/partial exit rule applies. `tako stats --follow --events ndjson`
streams one `stats.sample` event per node per `--interval` (default 5s)
whose `data` carries `server`, `host`, and `containers` in the same shape,
until cancelled. `tako secrets list --output json` returns a
`SecretsListResult` with the `--env` filter and secret `keys` only —
values never appear in machine output (test-enforced). `tako secrets
validate --output json` returns a `SecretsValidateResult` with
project/environment, `valid`, `required` key names, and `missing` key
names; missing secrets exit with code 2 and still emit the document. `tako
certs push|ls|rm --output json` returns a `CertsResult` with the action,
optional domain, timing, and per-proxy-node records (`server`, `host`,
`certificates`, optional `error`). Certificate rows contain `domain`, `source`,
`notBefore`, `notAfter`, `issuedAt`, and `updatedAt`. Managed DNS-01 rows add
`ownerProject`, `ownerEnvironment`, `dnsProvider`, `caProvider`, optional `staging`,
`orphaned`, `lastAttemptAt`, `lastSuccessAt`, `lastError`, and `retryAfter`.
These additive fields make failed first issuance and renewal cooldowns
pollable; PEM, private keys, and provider credentials never appear. All target
nodes are capability-checked before push/remove,
and an old agent returns a typed upgrade-required error naming `tako upgrade
servers`. Certificate operations intentionally do not acquire project leases:
node-local atomic replacement plus Caddy's graceful reload makes a concurrent
deploy benign, with the last valid Caddyfile winning. The store is node-global
but is not replicated state, is invisible to drift, is excluded from backups,
and is lost on node replacement; operators must retain and re-push their own
certificate copies. `tako
domains status --output json` returns a `DomainsResult` with
project/environment, the expected DNS targets, `allActive`, and per-domain
entries (`service`, `domain`, `role` `serving|redirect|ad-hoc`, `state`,
`dns`, `tls`, resolved IPs, cname, the additive explicit `cdn` declaration,
message, optional `warning`, and errors). `state: active,dns: proxied` is
reported only when `cdn` is explicitly `cloudflare` or `generic`; an
undeclared suspected CDN stays wrong/unready. Pending domains also exit 6,
and every strict failure still emits the document. Serving entries cover the
primary `proxy.domain` plus every additional `proxy.domains` hostname. `tako domains hosts --output
json` returns a `DomainsHostsResult` with the `--address` mode and
`entries` (`service`, `host`, `address`, `server`, `source`). `tako
discovery exports --output json` returns a `DiscoveryExportsResult` with
the environment (or `allEnvironments`) and per-node export records
(takod discovery schema: `network`, `project`, `environment`, `service`,
`alias`) or a per-node `error`; the command-local `--json` flag predates
this contract and is rejected together with the global machine modes.
`tako maintenance`, `tako live`, and `tako cleanup` return an
`ActionResult` acknowledgement with `action` (`maintenance.enable`,
`maintenance.disable`, `cleanup`), optional `service`, overall `outcome`
(`ok`, `partial`, `failed`), and per-server outcomes (`done`, `error`,
cleanup `warnings`); cleanup errors/warnings exit 6 (previously 0) and
still emit the document. `tako start`/`tako stop` return the same
`ScaleResult` as `tako scale`. Every `tako backup` action (`list`,
`create` — single volume or `--all`, `restore`, `delete`, `cleanup`)
returns a `BackupResult` with the action, volume/backupId when relevant,
and per-node outcomes whose `backups` reuse the takod backup schema plus
`deleted` counts, `skipped` volumes, and per-node `error`. `tako setup`
returns a `SetupResult` with per-node provisioning outcomes: `mode`
(`fresh`, `reapply`, or `converge` — converge re-runs only firewall,
deploy access, and the takod runtime and reports the untouched steps as
`skipped`), per-step outcomes keyed by stable step names (`os-check`,
`packages`, `docker`, `wireguard`, `firewall`, `hardening`,
`auto-recovery`, `deploy-user`, `monitor-agent`, `takod-install`,
`takod-service`), the detected `os`, installed `dockerVersion` and
`takodVersion`, the applied `firewallPorts`, and the node's recorded SSH
`hostKey` (`type`, base64 `key`, SHA256 `fingerprint`) so callers can pin
it for `--host-key-mode strict`; with `--events ndjson` each step also
emits `setup.step.started/.completed/.failed/.skipped` events carrying
`data.step`. Setup aborts on the first failing node and the document lists
the nodes attempted. `tako upgrade servers` returns an
`UpgradeServersResult` with the `targetVersion`, `dryRun`, and per-node
`{server, fromVersion, toVersion, stage, protocol, rolledBack, outcome}` entries (`upgraded`/`failed`/`rolled-back`/`blocked`
on apply; `current`/`upgrade-needed`/`downgrade-blocked`/`setup-required`/`status-unavailable`
on `--dry-run`). Enrolled nodes are ordered as worker canary, remaining
workers, and controller last; a failed stage blocks later stages. Partial
failure exits 6, total failure exits 1, and both still emit the document. `tako clone-setup`
returns a `CloneSetupResult` (doctor-style `checks` with pass/warn/fail
counts covering config, .env, SSH connectivity, env bundle, state, and
secrets); machine modes skip its interactive fix-up prompts and any failed
check exits 6. `tako exec` returns an `ExecResult` with the resolved
`server`/`host`, the target `container`, `mode` (`attach` or `oneoff`), the
command, the remote `exitCode`, and `durationMs`; output streams as
`exec.output` events between `exec.started` and `exec.completed`. In machine
modes the tako process exits 0 whenever the exec ran to completion — the
remote code lives in the document; text mode mirrors the remote exit code
for scripting. Deploys with a `deploy.release` command emit
`deploy.release.started/.output/.completed/.failed` events, the plan's
changes carry `releaseCommand`, and the `DeployResult` service outcome
gains a `release` entry `{command, server, image, exitCode, durationMs}` —
a failing release aborts the rollout before cutover and the deploy fails
with the standard taxonomy. Interactive exec (`-i`/`-t`) is rejected in machine modes with exit 2:
raw terminal bytes are not events. Control planes drive interactive exec
through the ptystream frame protocol (below) over their own SSH
connection. `tako jobs` returns a `JobsResult` listing each
scheduled `kind: job` service with its owning `server`, `schedule`,
optional `timezone` (UTC when omitted), `image`, `command`,
`timeoutSeconds`, the owning node's `nextRun`, and the most recent run
(`lastRun`: trigger, container, timestamps, `exitCode`, `status` —
`succeeded`/`failed`/`timeout`/`skipped`). `tako jobs runs [JOB]` returns a
`JobRunsResult` with the bounded run history (newest first, last 50 per
job) including each run's redacted `output` tail. `tako jobs trigger JOB`
returns a `JobTriggerResult` with the run's `server`, `container`,
`exitCode`, and `durationMs`; output streams as `jobs.trigger.output`
events between `jobs.trigger.started` and `jobs.trigger.completed`, and —
like exec — machine modes exit 0 whenever the run completed while text mode
mirrors the job's exit code. Job services appear in the `StatusResult` with
`kind: "job"`, their `schedule`, the last run's status (`lastRun`), and
`nextRun` instead of replica counts; `tako logs JOB` returns the recorded
output of the latest run (no `--follow`). Deploys reconcile job schedules
declaratively and emit `deploy.jobs.applied` events per node. `tako proxy
hash-password` returns a `ProxyHashPasswordResult` with the bcrypt `cost` and
`hash` for `proxy.basicAuth.passwordBcrypt`; the plaintext password is read
from stdin (machine modes require piped stdin) and never appears in any
output. The
Go definitions in `pkg/engine` (`types.go` and per-command files) are the
source of truth.

### Terminal stream contract (ptystream)

Interactive exec speaks a versioned framed byte protocol instead of events.
Transport: open an SSH `direct-streamlocal@openssh.com` channel to
`/run/tako/takod.sock`, then send a real HTTP/1.1 request over it:

```
POST /v1/exec HTTP/1.1
Host: takod
Content-Type: application/json
Connection: Upgrade
Upgrade: tako-pty/1

{"project":"myapp","environment":"production","service":"web",
 "mode":"attach","command":["sh"],"interactive":true,"pty":true,
 "cols":120,"rows":40}
```

The exec request body is the same document non-interactive exec posts, plus
`interactive` (attach stdin), `pty` (allocate a server-side pseudo-terminal;
implies `interactive`), initial `cols`/`rows`, and optional
`idleTimeoutSeconds`. Validation failures return normal HTTP errors; on
success the server replies `101 Switching Protocols` with
`Upgrade: tako-pty/1` and the connection becomes a full-duplex frame stream.

Each frame is 1 type byte + 4-byte big-endian payload length + payload
(max 1 MiB). Client-to-server: `1` stdin bytes (a zero-length stdin frame
closes remote stdin for piping), `3` resize (cols, rows as big-endian
uint16s). Server-to-client: `5` container name (first), `2` output bytes
(PTY merges stdout/stderr), `6` fatal error text, `4` exit code (big-endian
int32, terminal frame; `-1` means the run failed without a code). Unknown
frame types must be ignored — the protocol grows additively; incompatible
changes bump the Upgrade token.

Server-side sessions default to a 4h absolute timeout (`timeoutSeconds`)
and a 30m idle timeout; on client disconnect the process is killed and
one-off containers are removed. The Go definitions live in
`pkg/takoapi/ptystream`.

### Command coverage

Every command falls into exactly one category — there is no undocumented
machine behavior:

| Category | Commands |
| -------- | -------- |
| Full contract (result document + NDJSON events + typed exit codes) | `deploy`, `run`, `ps`, `logs`, `access`, `history`, `config export`, `config pull`, `state pull\|lease\|lease release\|status\|forget-node\|repair`, `rollback`, `promote`, `scale`, `start`, `stop`, `placement plan cordon\|drain\|rebalance`, `placement verify\|apply`, `remove`, `destroy`, `validate`, `doctor`, `drift`, `metrics`, `stats`, `secrets list`, `secrets validate`, `certs push\|ls\|rm`, `domains status`, `domains hosts`, `discovery exports`, `maintenance`, `live`, `cleanup`, `backup`, `setup`, `clone-setup`, `upgrade servers`, `exec`, `jobs`, `jobs runs`, `jobs trigger`, `proxy hash-password` |
| Event streams (`--events ndjson`) | `logs` (`log.line`), `access` (`access.line`), `stats --follow` (`stats.sample`), `setup` (`setup.step.*`), `exec` (`exec.*`), `deploy` release steps (`deploy.release.*`), DNS-01 issuance (`cert.issue.started\|completed\|failed\|skipped`), node renewal (`cert.renew.completed\|failed` in the state-event log), `jobs trigger` (`jobs.trigger.*`), `deploy` job schedules (`deploy.jobs.applied`), `certs push\|ls\|rm` (`certificate.operation`) |
| Machine-native output format | `prometheus` (Prometheus exposition format on stdout) |
| Human-only by design | `init`, `platform init`, `platform backup create\|verify\|restore`, `platform controller promotion verify`, `platform join-token create`, `platform node list\|enroll\|ready\|schedulable\|cordon\|drain\|remove`, `config explain`, `monitor`, `env`, `secrets init\|set\|delete\|fetch\|import` (local mutations; `fetch`/`import` print redacted command-local JSON), `upgrade` (CLI self-update; `upgrade servers` keeps the full contract) |
| Infrastructure-only | `takod run`, hidden `platform worker run\|prepare-enrollment\|verify-enrollment\|reconcile-mesh`, hidden `platform node upgrade-publication-guard`, and hidden internal E2E helpers |

Human-only commands reject `--output json` and `--events ndjson` with a
typed invalid-request error (exit code 2) instead of printing human text to
a stdout the caller expected to parse. The categorization is test-enforced:
`cmd/machine_coverage_test.go` walks the registered command tree and fails
when a runnable command is missing from this table or listed twice.

Interactive-only flags (`drift --watch`, `metrics --live`, `stats --live`)
are rejected with exit code 2 when a machine mode is enabled.

## Config Export / Pull

Use `--file/-o` for the generated config path:

```bash
tako config pull --project myapp --server prod-1.example.com -o tako.yaml
```

In machine modes stdout remains parseable. Without `--file`, raw YAML is not
printed to stdout; it is carried in the result document's `yaml` field:

```bash
tako config export --project myapp --server prod-1.example.com --output json \
  > config-export.json
jq -r '.yaml' config-export.json > tako.yaml
```

The old command-local `--output FILE` form is accepted for config export/pull
when `FILE` is not `text` or `json`. Values `--output text` and
`--output json` select the global output mode for compatibility with the
machine-output contract.

## Plan / Approve / Apply

Destructive plans (service updates/removals) need approval. The machine
flow mirrors the interactive confirmation:

1. **Plan**: `tako deploy --plan-only --output json` computes the plan,
   holds nothing, applies nothing, and prints a `DeployPlan` document:
   servers, services, typed changes with reasons, redacted `sharedBuilds`
   artifact identities (`key`, resolved `image`, and `consumers`),
   `destructive`, `empty`, the human plan text, and a content hash (via
   `planHash` on results).
   `tako run ... --plan-only` behaves the same.
2. **Review**: present `changes`/`humanText` to the approver; persist the
   plan document.
3. **Apply**: rerun with `--yes` to approve. Pass `--plan <file>` (the saved
   plan document) to make the apply fail with exit code 2 if the freshly
   computed plan no longer matches the reviewed one (hash comparison over
   decision-relevant fields; the human text is excluded).

Without `--yes`, a destructive plan in machine mode does not prompt: it
emits a `ConfirmationRequired` document (reason + full plan) and exits
with code 2. `tako state forget-node NODE` follows the same non-interactive
rule for its state mutation: in machine modes it emits a `ConfirmationRequired`
document with `operation: "state.forget-node"` unless `--yes` is passed.
`tako remove` and `tako destroy` likewise never prompt in machine modes:
without `--yes` (or `--force`) they emit a `ConfirmationRequired` document
carrying `operation` (`remove` or `destroy`), `project`, `environment`, and
the target `servers`, and exit with code 2.

## Exit Codes

| Code | Meaning |
| ---- | ------- |
| 0 | Success. |
| 1 | Operation ran and failed (build/deploy/runtime error). |
| 2 | Invalid request: bad flags, bad config, validation failure, plan drift, or confirmation required without `--yes`. |
| 3 | Another operation holds the local state lock or a remote lease. Retry later; see `tako state lease`. |
| 4 | SSH/node connectivity failure. |
| 5 | Cancelled (signal/timeout). |
| 6 | Partial success needing operator attention (for example strict domain checks still pending). |

The mapping is derived from typed engine error classes
(`pkg/engine.Classify`); errors that predate the taxonomy report 1.

## Redaction Guarantees

- Service environment values and SSH passwords are registered with a
  redactor before any event is emitted; event messages and string data
  values pass through it.
- Registry passwords from the top-level `registries:` block are registered
  with the same redactor. Config validation rejects literal registry
  passwords before environment expansion — they must be `${ENV_VAR}`
  references — so plaintext credentials never live in `tako.yaml`.
- Registry credentials are request-scoped: they ride typed request bodies
  to `takod` (never argv, query strings, or replicated state), back an
  ephemeral `DOCKER_CONFIG` for the single pull/build, and are deleted
  before the response returns. An authentication failure surfaces as an
  `image.pull.auth_failed` event and a failed result (exit 1); the
  credential values themselves never appear in events or result documents.
- Deployment records store env **keys** only (`<redacted>` values).
- Generated configs (`tako config export`) redact SSH passwords and
  placeholder env values.

Do not log stderr blindly: build logs may echo application output that
Tako cannot classify.

## Compatibility Promises

- Event and result schemas follow the `takoapi` versioning rules: additive
  changes within `v1alpha1` (new event types, new fields, new `data` keys);
  breaking changes require a new apiVersion. Consumers must tolerate
  unknown fields.
- `message` strings and human/stderr output are NOT part of the contract;
  never parse them.
- Exit codes above are stable. New codes may be added; treat unknown
  non-zero codes as failure.
- Flags documented here (`--output`, `--events`, `--plan-only`, `--plan`,
  `--yes`, `TAKO_NONINTERACTIVE`) are stable.

## Example Session (control-plane shaped)

```bash
export TAKO_NONINTERACTIVE=1 TAKO_SKIP_UPDATE_CHECK=1
cd /workspaces/myapp-production

# 1. Plan and capture the document for review
tako deploy --plan-only --output json > plan.json

# 2. After approval, apply with drift protection and stream progress
tako deploy --yes --plan plan.json --events ndjson > events.ndjson
echo "exit=$?"

# 3. The last line of events.ndjson is the terminal result event
tail -n 1 events.ndjson | jq '.data.result.status'

# 4. Query current service status without human table output on stdout
tako ps --output json > status.json
jq '.services[] | {name, running, desired, status}' status.json

# 5. Query deployment history without human table output on stdout
tako history --output json > history.json
jq '.deployments[] | {id, status, timestamp}' history.json

# 6. Refresh local .tako state from the mesh without prose on stdout
tako state pull --output json > state-pull.json
jq '.status' state-pull.json

# 7. Inspect or force-release a remote operation lease by exact ID
tako state lease --output json > leases.json
tako state lease release --output json --id "$LEASE_ID" --force > lease-release.json

# 8. Inspect local/remote state sync without prose on stdout
tako state status --output json > state-status.json
jq '.bestKnown.history.source, .sync.recommendations[]' state-status.json

# 9. After removing a retired node from tako.yaml, clean replicated runtime state
tako state forget-node old-node --yes --output json > forget-node.json
jq '.summary' forget-node.json

# 10. Repair replicated state from the freshest reachable copies without prose
tako state repair --output json > state-repair.json
jq '.selectedSources, .writes.counts, .localSync' state-repair.json
```
