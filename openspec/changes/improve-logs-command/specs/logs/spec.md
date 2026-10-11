# Spec Delta

## Purpose

Defines how `tako logs` selects, filters, follows, attributes, and reports container logs for an environment's services and jobs across its takod nodes, for both human and machine consumers.

## ADDED Requirements

### Requirement: Service selection
`tako logs` SHALL accept zero or more services via repeatable `--service` (also comma-separated) and positional arguments, merging and de-duplicating them. With no service given it SHALL select every long-running service in the environment, excluding jobs and deploy-time runs. Every named service MUST exist in the environment, or the command SHALL fail with exit code 2 before contacting any node.

#### Scenario: No service streams all long-running services
- **WHEN** an environment defines services `web`, `worker`, job `nightly`, and run `migrate`, and the user runs `tako logs`
- **THEN** logs are shown for `web` and `worker` only

#### Scenario: Multiple services by flag and argument
- **WHEN** the user runs `tako logs api --service web,worker --service web`
- **THEN** logs are shown for exactly `api`, `web`, and `worker`

#### Scenario: Unknown service rejected
- **WHEN** the user runs `tako logs --service nope`
- **THEN** the command exits 2 with an error naming `nope` and the environment, and no node is contacted

#### Scenario: Deploy-time run rejected
- **WHEN** the user names a service that is a deploy-time run
- **THEN** the command exits 2 with the existing message that runs have no persistent container logs

### Requirement: Time window filtering
`tako logs` SHALL accept `--since` and `--until`, each either a duration (for example `10m`, `1h30m`), an RFC3339 timestamp, or a Unix timestamp, and SHALL show only log lines inside that window. Relative durations SHALL be evaluated against each node's clock. Invalid values, `--until` earlier than `--since` when both are absolute, and `--until` combined with `--follow` SHALL be rejected with exit code 2.

#### Scenario: Since duration
- **WHEN** the user runs `tako logs --service web --since 15m`
- **THEN** only lines emitted in the last 15 minutes (by node clock) are shown

#### Scenario: Since without explicit tail widens the tail
- **WHEN** the user passes `--since` and does not pass `--tail`
- **THEN** the per-container line limit is the maximum allowed (10000) instead of the default 100

#### Scenario: Until with follow rejected
- **WHEN** the user runs `tako logs --until 2026-10-01T00:00:00Z -f`
- **THEN** the command exits 2 explaining that `--until` cannot be combined with `--follow`

#### Scenario: Malformed since rejected
- **WHEN** the user runs `tako logs --since yesterday`
- **THEN** the command exits 2 naming the accepted formats

### Requirement: Regex filtering
`tako logs` SHALL accept `-g/--grep <pattern>` interpreted as an RE2 regular expression and show only lines matching it; `--ignore-case` SHALL make the match case-insensitive. Patterns that do not compile or exceed 1024 bytes SHALL be rejected with exit code 2 before any node is contacted. When `--grep` is set, `--tail N` SHALL count matching lines per container.

#### Scenario: Grep filters lines
- **WHEN** the user runs `tako logs --service web --grep "status=5\d\d"`
- **THEN** only lines matching the regex are shown

#### Scenario: Case-insensitive grep
- **WHEN** the user runs `tako logs --grep error --ignore-case` and a line contains `ERROR`
- **THEN** that line is shown

#### Scenario: Tail counts matches
- **WHEN** a container has 5000 lines of which 30 match and the user runs `tako logs --service web --grep timeout --tail 10`
- **THEN** the 10 most recent matching lines from that container are shown

#### Scenario: Invalid regex rejected
- **WHEN** the user runs `tako logs --grep "("`
- **THEN** the command exits 2 with the regex compile error and no node is contacted

### Requirement: Timestamps
`tako logs` SHALL accept `-t/--timestamps`; when set, each human log line SHALL be preceded by the Docker-recorded RFC3339Nano timestamp of that line. Machine events SHALL carry the timestamp regardless of the flag whenever the node supplied one.

#### Scenario: Human timestamps
- **WHEN** the user runs `tako logs --service web -t`
- **THEN** each line begins with its RFC3339Nano timestamp

#### Scenario: Machine timestamp always present
- **WHEN** the user runs `tako logs --service web --events ndjson` against a node advertising `logs.stream-v1`
- **THEN** each `log.line` event has `data.time` set even without `--timestamps`

### Requirement: Source attribution in human output
When more than one source (service, replica slot, or node) is shown, every human log line SHALL be prefixed with `service.slot@node`, padded to a common width. With exactly one source, lines SHALL be unprefixed.

#### Scenario: Multi-replica prefix
- **WHEN** service `web` runs slots 1 and 2 on node `node-a` and the user runs `tako logs --service web`
- **THEN** lines are prefixed `web.1@node-a` or `web.2@node-a`

#### Scenario: Single source unprefixed
- **WHEN** exactly one container on one node is shown
- **THEN** lines are printed without a prefix

### Requirement: Source prefix color
On a terminal with `NO_COLOR` unset and no machine output mode, each source prefix SHALL be colored from a fixed palette of 6 ANSI colors chosen by a hash of the prefix. Otherwise output SHALL contain no ANSI escape sequences. No command-line flag SHALL control color.

#### Scenario: Plain output when piped
- **WHEN** stdout is not a terminal
- **THEN** prefixes are present but contain no ANSI escape sequences

#### Scenario: NO_COLOR disables color on a terminal
- **WHEN** stdout is a terminal and `NO_COLOR` is set
- **THEN** prefixes contain no ANSI escape sequences

#### Scenario: Same source gets the same color
- **WHEN** `web.1@node-a` lines are printed in two separate invocations on a terminal
- **THEN** the prefix uses the same color in both

### Requirement: Node fan-out and per-node failures
`tako logs` SHALL query every environment node (or only `--server`) using one log stream per node regardless of the number of services, with at most 8 nodes queried concurrently in non-follow mode and all nodes concurrently in follow mode. A node with no matching containers SHALL contribute zero lines and SHALL NOT be a failure. A node that fails SHALL produce a `warning` event naming the node immediately while other nodes continue.

#### Scenario: Service absent on a node
- **WHEN** `web` is placed only on `node-a` of a two-node environment and the user runs `tako logs --service web`
- **THEN** `node-a` lines are shown, `node-b` is reported with status `success` and zero lines, and the command exits 0

#### Scenario: One node unreachable
- **WHEN** one of three nodes is unreachable
- **THEN** a `warning` naming that node is emitted, the other two nodes' lines are shown, the result status is `partial`, and the command exits 6

#### Scenario: All nodes unreachable
- **WHEN** every target node fails with a connection error
- **THEN** the result status is `failed` and the command exits 4

#### Scenario: No containers anywhere
- **WHEN** no target node has any container for the selected services
- **THEN** the command reports that no containers were found for the selected services and exits 1

### Requirement: Follow mode for services
With `--follow`, `tako logs` SHALL stream new lines from all selected sources until interrupted. Containers for the selected services that start after the stream began (for example during a deploy or restart) SHALL be attached automatically without restarting the command, and lines from removed containers SHALL stop without ending the stream.

#### Scenario: Follow across a deploy
- **WHEN** the user runs `tako logs --service web -f` and a deploy replaces `web` containers
- **THEN** lines from the new containers appear in the same stream with their new slot attribution

#### Scenario: Interrupt ends follow
- **WHEN** the user presses Ctrl-C during follow
- **THEN** streams on all nodes are closed and the command exits 5 with status `failed` and a cancellation error

### Requirement: Job logs
For a named job, non-follow `tako logs` SHALL show the recorded output of the most recent run, applying `--grep` and `--tail` to those lines. With `--follow` it SHALL stream the output of the job run in progress, if any, and of every subsequent run on the job's owning node until interrupted, separating runs with a run header. Job lines SHALL use slot attribution `run` in prefixes.

#### Scenario: Follow a running job
- **WHEN** job `nightly` is currently running and the user runs `tako logs --service nightly -f`
- **THEN** its live output is streamed and the command keeps waiting for the next run after it exits

#### Scenario: Follow an idle job
- **WHEN** job `nightly` is not running and the user runs `tako logs nightly -f`
- **THEN** the command prints that it is waiting for the next run and streams that run's output when it starts

#### Scenario: Non-follow job shows last run
- **WHEN** the user runs `tako logs nightly --grep failed`
- **THEN** only matching lines of the most recent recorded run are shown

### Requirement: takod capability gate
Node-side filtering, attribution, and job/redeploy-aware follow SHALL require the takod capability `logs.stream-v1`. When a node lacks it and the request uses `--since`, `--until`, `--grep`, `--timestamps`, more than one service, or follow on a job, that node SHALL fail with an error naming the capability and `tako upgrade servers`, classified as invalid (exit 2 when every node fails this way). Requests using none of these SHALL fall back to the legacy log stream on such nodes.

#### Scenario: Old agent with new flag
- **WHEN** a node lacks `logs.stream-v1` and the user runs `tako logs --service web --grep x`
- **THEN** that node fails with an upgrade message naming `logs.stream-v1` and no unfiltered lines from that node are shown

#### Scenario: Old agent with plain request
- **WHEN** a node lacks `logs.stream-v1` and the user runs `tako logs --service web`
- **THEN** logs are streamed from that node as before, attributed to the node only

### Requirement: Machine events for log lines
In `--events ndjson` mode each log line SHALL be a `log.line` event with top-level `service` and `node`, and `data.service`, `data.node`, `data.data` unchanged from today. Events SHALL additionally carry, when known, `data.replica`, `data.container`, `data.revision`, `data.stream` (`stdout` or `stderr`), and `data.time` (RFC3339Nano). Secrets registered by the engine MUST be redacted in `data.data` and `message`; line content SHALL stay in the top-level `data.data` string.

#### Scenario: Additive event fields
- **WHEN** a consumer reads `log.line` events from an upgraded node
- **THEN** each event has `data.data`, `data.service`, `data.node`, plus `data.replica`, `data.container`, `data.stream`, and `data.time`

#### Scenario: Secret redacted
- **WHEN** a log line contains a configured service env value
- **THEN** the value is redacted in both `data.data` and `message`

### Requirement: Logs result document
The `LogsResult` document SHALL keep all existing fields and add `services` (selected service names), `since`, `until`, `grep`, `ignoreCase`, `timestamps`, and per-node `lines` (lines emitted). `service` SHALL be set only when exactly one service is selected. `status` SHALL be `success`, `partial` (some nodes failed), or `failed`.

#### Scenario: Multi-service result
- **WHEN** the user runs `tako logs --output json` with no service in an environment with `web` and `worker`
- **THEN** the document has `services: ["web","worker"]`, empty `service`, and one `nodes` entry per target node with its `lines` count

#### Scenario: Partial result
- **WHEN** one of two nodes fails
- **THEN** the document has `status: "partial"`, the failing node has `status: "failed"` with `error`, and the process exits 6
