# Tako API and SDK Foundation

This document describes the importable foundation APIs that exist today. These
packages are building blocks for Tako integrations and tests; they are not a
promise that `takod` exposes a public TCP REST API.

Non-Go consumers (control planes in other languages, CI scripts) should drive
the CLI through the machine interface instead: structured JSON/NDJSON output,
deterministic exit codes, and plan handoff are documented in
`docs/MACHINE-INTERFACE.md`.

## Importable Packages

### `pkg/engine`

`pkg/engine` is the deployment engine behind the CLI commands. It never
prompts, prints, or exits the process; progress flows through a typed event
sink and outcomes are returned as typed results and errors. The Cobra
commands in `cmd/` are thin adapters over this package.

- Construct with `engine.New(engine.Options{CLIVersion, CLICommit, Sink,
  BuildOutput, StateAutoSync})`. `Sink` receives the event stream
  (`pkg/takoapi/events`); `BuildOutput` redirects deployer build logs.
- Mutations with confirmation gates use a plan/apply session:
  `PlanDeploy(ctx, req)` / `PlanRun(ctx, req)` / `PlanRemove(ctx, req)` /
  `PlanDestroy(ctx, req)` return sessions exposing the serializable plan
  document (`Plan()`, `NeedsConfirmation()`), then `Apply(ctx)` executes
  and `Close()` releases the local lock, remote leases, and SSH
  connections.
- Simpler mutations are single calls: `Rollback`, `Promote`, `Scale`.
- Remote command execution is available as `Exec(ctx, ExecRequest)`, the
  engine backing `tako exec`: run a command in a running service container
  or as a one-off container from the service image. Output streams as
  events; the remote exit code returns as a typed `RemoteExitError` so
  adapters can mirror it. Release hooks are config-driven, not a separate
  API: a service `release:` block runs as a gated one-off container inside
  deploy, and `deployer.ReleaseRunFor(service)` exposes the recorded run.
- Scheduled jobs are available as `Jobs(ctx, JobsRequest)` (list with
  schedule/next-run/last-run), `JobRuns(ctx, JobRunsRequest)` (bounded run
  history with redacted output tails), and `TriggerJob(ctx,
  JobTriggerRequest)` (immediate run streaming output events and returning
  the remote exit code). `StreamLogs` on a `kind: job` service returns the
  latest run's recorded output.
- Public domain reachability is available as `MonitorDomainStatuses(ctx,
  checker, specs, options)`, which probes serving domains (including
  `proxy.domains` extras) and emits progress events; `tako domains status`
  wraps it into a `DomainsResult`.
- Config materialization is available as `ExportConfig(ctx,
  ConfigExportRequest)`, which reads desired/actual/history state through the
  private takod state client and returns a `ConfigExportResult` containing the
  generated `config`, warnings, source/target node details, redaction status,
  and YAML text. The engine does not write files, infer the current user, or
  expand `~` paths; adapters persist the config and supply explicit connection
  fields themselves (the CLI uses `tako config export --file/-o`).
- Status is available as `Status(ctx, StatusRequest)`, the engine backing
  `tako ps`. It resolves the project/environment/server/service selection,
  gathers actual state from takod nodes, and returns a `StatusResult` with
  selected servers and service rows (`running`, `desired`, `status`, `ports`,
  revision/warming metadata). The engine does not render the human table.
- Log streaming is available as `StreamLogs(ctx, LogsRequest)`, which emits
  `log.line` events carrying service/node/raw-line data and returns a
  `LogsResult` summary when the stream completes.
- Deployment history is available as `History(ctx, HistoryRequest)`, the engine
  backing `tako history --output json`. Adapters provide the mesh history source
  and deployment-listing seams shared with rollback; the engine applies the
  requested server/status/limit/include-failed options and returns a
  `HistoryResult` with source server and JSON-friendly deployment rows.
- State pull orchestration is available as `StatePull(ctx, StatePullRequest)`.
  Adapters provide seams for selecting deployment history, syncing deployment
  records locally, and the two runtime recovery fallbacks. The engine returns a
  `StatePullResult` with status (`synced_history`, `recovered_mesh_actual`,
  `recovered_running_mesh`, or `none_found`), source server, synced count,
  latest deployment summary, and non-fatal recovery warning/error details.
- State status summarization is available as `StateStatus(ctx,
  StateStatusRequest)`. Adapters collect local `.tako` and remote node
  documents (history, desired, actual, node-actual, leases, agent/mesh status)
  and pass them to the engine; the engine returns a `StateStatusResult` with
  per-node summaries, best-known sources, sync recommendations, counts,
  unreachable guidance, and a populated `error` when no selected node is
  reachable. The engine does not connect to nodes or render the human status
  sections.
- Remote operation lease inspection/unlock is available as `StateLease(ctx,
  StateLeaseRequest)` and `ReleaseStateLease(ctx, StateLeaseReleaseRequest)`.
  `ReleaseStateLease` force-releases only the exact requested lease ID on
  selected nodes, refuses active leases unless `Force` is true, and returns a
  `StateLeaseReleaseResult` with released nodes and per-node lease/error data.
- Runtime-state retired-node cleanup is available as `StateForgetNode(ctx,
  StateForgetNodeRequest)`. Adapters provide already-collected reachable node
  runtime managers (the CLI reuses the state-repair inventory seam) and own
  confirmation/lease handling. The engine validates the request, removes the
  retired node from aggregate actual state, deletes standalone node-actual
  snapshots, appends cleanup events, and returns a `StateForgetNodeResult`.
- State repair is available as `StateRepair(ctx, StateRepairRequest)`. Adapters
  collect reachable nodes and source candidates (history, desired, aggregate
  actual, and node-actual snapshots), provide write-capable managers, and own
  SSH, lease acquisition, rendering, and local `.tako` sync. The engine selects
  the freshest repairable sources, optionally calls `BeforeWrite` before remote
  writes, writes cloned documents to each node, prunes stale node-actual
  documents, and returns a `StateRepairResult` with selected sources, per-node
  warnings/errors, aggregate write counts, local-sync fields for adapters to
  fill, and `error` when repair is incomplete or fails.
- Mutation contexts are cancellation-aware for local checks, takod JSON
  state/lease requests, remote lease fan-out, deployment-history
  replication, and the long deployer streams: build-context archive
  uploads, local buildx/unregistry pushes, reconcile-service requests, and
  release exec streams all derive from the caller's context
  (`Deployer.SetBaseContext`, wired by deploy/run/scale). Leases acquired
  after cancellation are best-effort released and cancellation classifies
  as `cancelled`. Deploy/run writes an `in_progress`
  remote deployment record before service/proxy mutations start, so a hard kill
  after mutations begin leaves history evidence. Cancelled deploy/run failure
  paths use a short cleanup context to persist failed/interrupted deployment
  state and history; repeated saves for the same deployment ID replace the
  history entry rather than duplicating it. Local locks and remote leases are
  force-releasable and expire if left stale.
- Errors are classified for exit-code mapping via `engine.Classify`:
  invalid request, locked/leased, connectivity, cancelled, attention.
- Every emitted event passes through a secrets redactor; operations
  register service env values and SSH passwords before emitting.
  `RegisterSecret(value)` adds ad-hoc values; `RegisterRegistrySecrets(cfg)`
  registers every `registries:` password (deploy and run call it before
  any event is emitted).

#### Config export example

```go
eng := engine.New(engine.Options{})
result, err := eng.ExportConfig(ctx, engine.ConfigExportRequest{
    Project:     "myapp",
    Environment: "production",
    Server:      "prod-1.example.com",
    User:        "deploy",
    SSHPort:     22,
    SSHKey:      "/home/deploy/.ssh/id_rsa", // callers expand ~ before passing
})
if err != nil {
    return err
}
_ = result.Config // *config.Config for SDK callers
_ = result.YAML   // exact YAML text when callers want to write tako.yaml
```

#### State pull example

```go
result, err := eng.StatePull(ctx, engine.StatePullRequest{
    Config:      cfg,
    Environment: "production",
    HistorySource: func() (string, *state.DeploymentHistory, error) {
        return source, history, nil // choose the freshest reachable history
    },
    SyncDeployments: func(deployments []*state.DeploymentState) (int, error) {
        return syncLocal(deployments)
    },
    RecoverFromMeshActual: func() (engine.StatePullRecoveryResult, error) {
        return recoverFromReplicatedActual()
    },
    RecoverFromRunningMesh: func() (engine.StatePullRecoveryResult, error) {
        return recoverFromRunningContainers()
    },
})
if err != nil {
    return err
}
_ = result.Status
_ = result.Latest
```

#### State status example

```go
result, err := eng.StateStatus(ctx, engine.StateStatusRequest{
    Project:     "myapp",
    Environment: "production",
    Local: engine.StateStatusLocalInput{Path: ".tako", Exists: true, Current: currentLocal},
    Nodes: []engine.StateStatusRemoteNodeInput{
        {Name: "node-a", Host: "10.0.0.1", History: history, Desired: desired, Actual: actual, Lease: lease},
    },
})
if err != nil && result == nil {
    return err
}
_ = result.BestKnown
_ = result.Sync.Recommendations
```

#### State lease release example

```go
result, err := eng.ReleaseStateLease(ctx, engine.StateLeaseReleaseRequest{
    Config:      cfg,
    Environment: "production",
    ID:          "1234-lease-id",
    Force:       true,
})
if err != nil {
    return err
}
_ = result.Released // node names where the exact lease ID was released
```

#### State forget-node example

```go
result, err := eng.StateForgetNode(ctx, engine.StateForgetNodeRequest{
    Config:      cfg,
    Environment: "production",
    NodeName:    "old-node",
    Nodes: []engine.StateForgetNodeNode{
        {Name: "node-a", Runtime: runtimeManager}, // supplied by adapter inventory
    },
})
if err != nil {
    return err
}
_ = result.Summary.AggregateActualStatesPruned
```

#### State repair example

```go
result, err := eng.StateRepair(ctx, engine.StateRepairRequest{
    Config:      cfg,
    Environment: "production",
    Nodes: []engine.StateRepairNode{
        {Name: "node-a", HistoryManager: historyManager, Runtime: runtimeManager},
    },
    Histories: []engine.StateRepairHistoryCandidate{{Source: "node-a", History: history}},
    Desired:   []engine.StateRepairDesiredCandidate{{Source: "node-a", Desired: desired}},
    BeforeWrite: func(ctx context.Context, result *engine.StateRepairResult) error {
        return acquireRepairLeases(result.Servers) // adapter-owned
    },
})
if err != nil && result == nil {
    return err
}
_ = result.Sources
_ = result.Writes.Counts
```

### `pkg/takoapi/events`

Canonical engine event schema: `Event` (apiVersion, kind, seq, time, type,
phase, level, service, node, message, data), the `Sink` interface, and
ready-made sinks (`NopSink`, `BufferSink`, `FanoutSink`, `NDJSONSink`).
`Stream` stamps sequence/time/schema and applies redaction. Event `message`
carries the exact human rendering; machine consumers parse `type`/`data`.

### `pkg/takoapi`

`pkg/takoapi` contains transport-neutral schema and identity types:

- API/version constants such as `APIVersionCurrent` and state schema constants.
- Deployment identity types that keep source, deployment revision, service
  revision, image identity, and optional git metadata separate.
- Canonical desired, actual, actual-node, event, deployment, and deployment
  history documents used around takod state.

Use this package when code needs to construct or decode Tako state documents
without importing CLI commands or `internal/*` packages.

### `pkg/takoapi/stateclient`

`pkg/takoapi/stateclient` is a typed client for takod `/v1/state` documents. It
uses the existing private `pkg/takodclient` request executor abstraction, which
normally runs commands over SSH and talks to takod through its Unix socket.

Supported helpers include reading and writing desired state, aggregate actual
state, per-node actual state, deleting per-node actual state, deployment
history, single deployment records, and appending state events. The
`ReplicateDeploymentContext` helper writes a deployment record first and then an
optional deployment history document, matching the write primitive used by the
CLI's internal mesh replication. SSH/config-aware fan-out remains in
`internal/state`; it delegates each per-node write to this public stateclient
helper. Every helper has a `Context` variant (for example `ReadDesiredContext`
and `AppendEventContext`); the legacy non-context methods remain and call the
context variants with `context.Background()`.

The state client also exposes typed `/v1/lease` helpers over the same private
transport: `ReadLeaseContext`, `AcquireLeaseContext`, and
`ReleaseLeaseContext` plus non-context wrappers. Lease request/response structs
are public in `stateclient` and mirror the node-local takod JSON shape without
importing `internal/state`.

### `pkg/deployplan`

`pkg/deployplan` contains CLI-independent deploy planning helpers, including:

- Image reference selection and merging with deployed/actual state.
- Source, archive, and image build tag generation for non-git,
  archive-backed, and image-backed deploys.
- Service selection for targeted deploys and force behavior, including targeted
  archive deploy adapters used by `tako deploy --service <name> --archive <file>`.
- Active revision planning for rolling and blue/green proxy behavior.
- Stable per-service revision IDs derived from project, environment, service,
  image reference, service config hash, and deploy strategy.

Use this package for planning logic that should be unit-testable without Cobra,
Viper, or command output.

## Current Image Deploy Boundary

Tako currently has two image deploy paths:

- `tako deploy --service <name> --image <ref>` deploys an image for one service
  in an existing configured Tako project. It still requires `tako.yaml` with a
  defined service, server, and environment.
- `tako run IMAGE --name <name> --port <port> --server <host>` is the first
  configless path. It deploys a public image to an existing SSH-reachable
  VPS/takod node without local `tako.yaml`, while still using takod state,
  labels, leases, history, and proxy reconciliation.

`tako run` supports private registries via `--registry-user` and
`--registry-password-stdin` (never an argv password flag); configured
projects declare a top-level `registries:` block with `${ENV_VAR}`
passwords. Compose import, cloud provisioning, and discovery of arbitrary
non-Tako Docker containers remain outside the current boundary.

## Transport Boundary

Today the state client is intentionally private-control-plane only:

```text
integration code -> takodclient.RequestExecutor -> SSH executor -> curl --unix-socket /run/tako/takod.sock -> takod
```

There is no public network REST API, no public TCP listener contract, and no
external auth/TLS story for exposing takod directly. Integrations should treat
the Unix-socket API as node-local and use the same owned-server SSH boundary as
the CLI. If Tako later adds a public network API, it will need a separate
authentication, TLS, auditing, and operator opt-in design.

## State Client Examples

These examples use fake executor language so they do not contain credentials or
host-specific SSH details. In real code, provide an executor that satisfies
`takodclient.RequestExecutor` and reaches an operator-owned server.

```go
package example

import (
    "context"
    "io"
    "time"

    "github.com/redentordev/tako-cli/pkg/takoapi"
    "github.com/redentordev/tako-cli/pkg/takoapi/stateclient"
)

type fakeExecutor struct{}

func (fakeExecutor) ExecuteWithContext(ctx context.Context, cmd string) (string, error) {
    // Pseudocode: run cmd on the target server, for example over an SSH session.
    // The command is built by takodclient and uses curl against takod's Unix socket.
    return `{"found":true,"content":"{}"}`, nil
}

func (fakeExecutor) ExecuteWithInput(ctx context.Context, cmd string, input io.Reader) (string, error) {
    // Pseudocode: run cmd on the target server and stream input as the JSON body.
    return `{"found":true}`, nil
}

func readDesired() error {
    client := stateclient.New(fakeExecutor{}).
        WithSocket("/run/tako/takod.sock").
        WithTimeout(30 * time.Second)

    desired, err := client.ReadDesired("web-app", "production")
    if err != nil {
        return err
    }
    _ = desired.RevisionID
    return nil
}

func writeDesired() error {
    client := stateclient.New(fakeExecutor{})

    doc := takoapi.NewDesiredStateDocument("web-app", "production", "ci-20260705.12")
    doc.Source = "directory:."
    doc.TargetNodes = []string{"prod-1"}
    doc.Services["web"] = takoapi.DesiredServiceDocument{
        Name:     "web",
        Build:    ".",
        Replicas: 1,
    }

    return client.WriteDesired(doc)
}

func replicateDeployment(ctx context.Context) error {
    client := stateclient.New(fakeExecutor{})

    deployment := takoapi.DeploymentStateDocument{
        ID:          "deploy_123",
        ProjectName: "web-app",
        Environment: "production",
        Status:      takoapi.StatusSuccess,
    }
    history := &takoapi.DeploymentHistoryDocument{
        ProjectName:  "web-app",
        Environment:  "production",
        Deployments: []*takoapi.DeploymentStateDocument{&deployment},
        LastUpdated: time.Now().UTC(),
    }

    return client.ReplicateDeploymentContext(ctx, deployment, history)
}

func readLease(ctx context.Context) error {
    client := stateclient.New(fakeExecutor{})

    response, err := client.ReadLeaseContext(ctx, "web-app", "production")
    if err != nil {
        return err
    }
    if response.Found {
        _ = response.Lease.ID
    }
    return nil
}
```

For reads, `stateclient.ErrNotFound` indicates takod reported that the requested
state document was absent.

## Schema Versioning and Migration Guidance

- New canonical documents should use `takoapi.APIVersionCurrent` and the
  package constructors where available.
- State documents also carry `SchemaVersion`; current `/v1/state` documents use
  `takoapi.StateSchemaVersionCurrent`.
- Prefer additive fields. Readers should ignore unknown fields and tolerate
  omitted optional fields.
- Compatibility policy: within an apiVersion (`v1alpha1` today), changes are
  additive only — new documents, new fields, new event types, new `data`
  keys. Renaming or removing fields, changing a field's type or meaning, or
  changing the semantics of an existing event type requires a new
  apiVersion. Engine result documents (`DeployPlan`, `DeployResult`,
  `RollbackResult`, ...) and events follow the same policy as state
  documents.
- Git metadata is optional display/trace information. Do not require it for
  directory, archive, image, CI, or other non-git inputs.
- Archive-backed deploys should use archive content identity (or an explicit
  revision label) for build tags; `pkg/deployplan.ArchiveBuildTag` implements
  the current deterministic tag helper.
- Do not treat a git commit as the deployment revision ID. Use
  `RevisionIdentity.ID`, deployment history `ID`, or service revision IDs for
  Tako revision identity.
- Deployment history schemas in `takoapi` intentionally mirror the existing
  internal JSON shape so replicated history can be decoded without importing
  `internal/state`.
- Avoid synthetic git commits to force non-git deploys through old code paths;
  leave git fields empty when no real git metadata exists.

## Deploy Progress Output

`pkg/deployer.Deployer` supports progress output injection:

```go
var out bytes.Buffer

deploy := &deployer.Deployer{}
deploy.SetOutput(&out)     // capture progress output
deploy.SetOutput(io.Discard) // silence progress output
deploy.SetOutput(nil)      // reset to os.Stdout
```

This only documents the deployer output hook that exists today. Broader stdout
injection across all reusable packages remains deferred.
