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
`planHash`, and `error` when failed. The Go definitions in `pkg/engine`
(`types.go` and per-command files) are the source of truth.

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
with code 2.

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
```
