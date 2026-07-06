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
text output.

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
`ports`, `revision`, `warming`, `internal`). `tako logs` returns a
`LogsResult` document with project, environment, service, tail/follow options,
per-node stream outcomes, timings, and `error` when any node stream failed.
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
pruned. The Go definitions in `pkg/engine` (`types.go` and per-command files)
are the source of truth.

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
   servers, services, typed changes with reasons, `destructive`, `empty`,
   the human plan text, and a content hash (via `planHash` on results).
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
```
