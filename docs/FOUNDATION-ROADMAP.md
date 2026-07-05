# Tako Foundation Roadmap

This document tracks Tako CLI's platform foundation while preserving the
existing scope: deploy to operator-owned VPS hosts, through one `takod`
orchestration path. It does not propose cloud infrastructure provisioning or an
unmanaged Docker mode. For the current importable schema/client/planning
surface, see [API and SDK Foundation](API-SDK.md).

## Implementation Status

Completed foundation pieces:

- `pkg/takoapi` now defines importable identity, desired/actual/event, and
  deployment history schemas with version and kind constants.
- `pkg/takoapi/stateclient` provides a typed `/v1/state` client over the
  existing private `takodclient` SSH/Unix-socket transport.
- `pkg/deployplan` contains importable helpers for image references, source,
  archive, and image build tags, service selection, active proxy revision
  planning, and per-service revision IDs.
- `tako deploy` supports raw source-directory deployment with `--source`,
  explicit revision labels with `--revision`, targeted source deploys with
  `--service --source`, targeted archive deploys with `--service --archive`,
  and targeted prebuilt image deploys with `--service --image`.
- `tako run IMAGE --name ... --port ... --server ...` deploys public images to
  an existing SSH-reachable VPS/takod node without a local `tako.yaml`.
- `tako config export` and `tako config pull` materialize remote takod state
  into `tako.yaml`; SSH `--password` is redacted and is not written to output.
- `pkg/deployer.Deployer.SetOutput(io.Writer)` can redirect or silence deployer
  progress output.

Deferred items:

- Private registry authentication for `tako run` and other image workflows.
- Broader compose import and configless workflows beyond public images.
- Public network API, auth/TLS design, and operator opt-in serving mode.
- Broader stdout/progress injection beyond the deployer package.
- Long-term SDK compatibility guarantees and schema compatibility test suite.

## Current-State Review

### Strengths

- Tako already has a single runtime model: CLI requests flow through `takod`,
  the mesh, replicated state, labels, leases, reconciliation, and the shared
  proxy.
- `takod` exposes a broad node-local `/v1/*` API over a Unix socket for actual
  state, service reconciliation, state documents, leases, logs, stats, metrics,
  backups, mesh, images, and discovery.
- Reusable package candidates exist outside `cmd/`, especially `pkg/config`,
  `pkg/reconcile`, `pkg/state`, `pkg/ssh`, and `pkg/takodclient`.
- Runtime identity is already centralized in `pkg/runtimeid`, and actual state
  is derived from Docker labels rather than ad hoc container names.
- Git metadata display paths generally tolerate empty metadata, which is useful
  for future non-git deployment sources.

### Constraints And Gaps

- Git-backed deploy remains the normal default, while raw adapters include
  current `--source`, `--revision`, targeted `--service --source`, targeted
  `--service --archive`, targeted `--service --image`, and configless public
  image deployment through `tako run`. The `--service --image` path still
  requires an existing `tako.yaml` project with a defined service, server, and
  environment. `tako run` covers public images only and still requires SSH
  connection details for an existing VPS/takod node. Compose import is not
  implemented.
- Deployment source identity, revision identity, and git commit identity now
  have foundation types, but older history and rollback paths still need care:
  synthetic git commit values would be unsafe.
- State is split across local `.tako` cache, replicated deployment history, and
  desired/actual takod state schemas. Public mirrors now exist in `pkg/takoapi`,
  but long-term compatibility tests and migration tooling are still pending.
- `cmd/deploy.go` is still partly CLI-bound. Some lower packages print directly
  to stdout; only `pkg/deployer` has an output injection hook today.
- The takod API is not a public network API today. Access is via SSH exec plus
  curl to a Unix socket, with no public auth/TLS story.

## Target Architecture Principles

1. **One orchestration path.** Raw/docker-like inputs, git inputs, CI inputs,
   and future SDK calls must converge into the same `takod` desired-state,
   lease, label, mesh, proxy, and reconcile flow.
2. **Input adapters, not runtime modes.** New deploy UX should adapt source or
   image inputs into a canonical deployment request; it should not run direct
   unmanaged Docker commands.
3. **Separate identities.** Track source identity, revision identity, image
   identity, and git metadata as distinct fields. Git is one source type, not
   the universal revision model.
4. **State has one canonical API.** Local files may cache and accelerate UX, but
   accepted desired state and deployment history should be accessed through a
   stable get/set model backed by takod replication.
5. **Packages before public promises.** Extract reusable APIs and schemas before
   committing to external SDK compatibility or public network exposure.
6. **Owned-server scope.** The platform foundation should improve deployment and
   operations on existing hosts; it should not create cloud provider resources.

## Raw / Docker-Like Deployment Direction

Raw deployment should mean "bring a source directory, archive, or image ref into
Tako's normal reconcile path," not "run docker directly." The initial design
should introduce an internal deployment input model similar to:

- `source.type`: `git`, `directory`, `archive`, `image`
- `source.ref`: path, archive digest, image ref, or git remote/ref
- `revision.id`: Tako-generated durable revision ID, independent of git
- `revision.sourceDigest`: hash of the submitted source/config payload when
  available
- `git.commit`, `git.branch`, `git.dirty`: optional display/trace metadata only
- `image.ref` or `image.digest`: canonical runtime artifact identity

Desired future UX examples:

```bash
# Deploy the current directory even when it is not a git repo.
tako deploy --source .

# Deploy a prebuilt image through takod state/reconcile in a configured project.
tako deploy --service web --image ghcr.io/acme/web:2026-07-05 -e production

# CI supplies an explicit revision label without pretending it is a git commit.
tako deploy --source . --revision ci-20260705.12

# Deploy one service from a local source archive.
tako deploy --service web --archive app.tar.gz
```

Implementation seams:

- Refactor deploy planning out of `cmd/deploy.go` into a package-level planner
  that accepts a deployment input and produces a canonical desired revision.
- Make git resolution optional following the tolerant pattern already used by
  promote, while preserving current clean-git defaults for existing users.
- Change build tag selection to use revision or content identity when git commit
  is absent, and rely on image refs/digests for image-only deploys.
- Keep rollback keyed to accepted revision IDs and stored artifacts. Never write
  fake git commits to make non-git revisions fit old rollback paths.

## Configless Public Image Deploy Direction

The first configless public-image flow is implemented as `tako run`. It lets an
operator deploy a public Docker Hub or public registry image to an existing
Tako-managed VPS without writing `tako.yaml` first. Current
`tako deploy --service --image` remains the configured-project path and still
requires project YAML.

Implemented UX examples:

```bash
# Deploy a public image as a new Tako-managed app/service on an existing server.
tako run nginx:1.27 --name web --port 80 --server prod-1

# Add routing and replicas when the target server/environment can host them.
tako run caddy:2 --name docs --port 80 --domain docs.example.com \
  --environment production --server prod-1 --replicas 2
```

Minimal required inputs:

- Public image reference, preferably normalized to a digest after resolution.
- Stable app/service name via `--name`.
- Target existing server or environment that maps to operator-owned VPS hosts.
- Port mappings, domains, replicas, and environment variables only when needed
  to make the service reachable or reproducible.

Desired behavior:

- Synthesize the same canonical desired deployment/service document a
  `tako.yaml` project would produce: app, environment, target nodes, service
  image identity, ports/domains, replicas, revision ID, source type `image`, and
  config digest.
- Submit that document through takod state APIs, leases, labels, reconciliation,
  replicated history, and proxy configuration. Do not run direct unmanaged
  Docker containers from the CLI.
- Persist accepted desired state and deployment history remotely through takod;
  any local cache is rebuildable and not the source of truth.
- Derive configless project and service identity from the explicit `--name` so
  later commands can refer to stable IDs instead of image tags.

Milestone acceptance criteria:

- A public image such as `nginx:1.27` can be deployed to an existing named Tako
  server without a local `tako.yaml`.
- The resulting containers carry normal Tako labels and are visible through
  desired/actual state, deployment history, rollback targets, and proxy state.
- Re-running the same command updates the canonical desired document
  idempotently for the same explicit app identity.
- Failure modes clearly report missing server/environment, missing port/domain
  information, invalid image refs, and unsupported private registry auth.

Non-goals for the first milestone:

- Private registry authentication; add it later as an explicit credentials
  design, not as an implicit Docker config leak.
- Compose import or multi-service conversion.
- Discovery or import of arbitrary non-Tako Docker containers.
- Cloud provider infrastructure provisioning or bypassing takod.

Deferred extensions:

- Private registry auth with scoped credentials and auditability.
- Compose import that converts compose services into canonical Tako services.
- Additional config materialization polish for teams that want to transition
  from configless experimentation to checked-in `tako.yaml`.

## Canonical State Model And Get/Set Direction

The state foundation should define a public canonical deployment document that
can be stored remotely, cached locally, and consumed by takod reconciliation.
Local and internal schemas can then layer around it instead of competing.

Recommended layering:

1. **Canonical deployment document**: app, environment, services, desired
   revision, source identity, image identities, config digest, created metadata,
   and optional git metadata.
2. **Deployment history/event log**: append-only records for accepted revisions,
   status transitions, lease holders, rollback targets, and operator metadata.
3. **Actual state snapshot**: derived from takod Docker label inspection and
   runtime IDs; not hand-authored by clients.
4. **Local `.tako` cache**: best-effort mirror and UX cache, invalidatable and
   rebuildable from remote takod state.

Acceptance direction:

- Introduce get/set/list APIs around canonical documents before adding new UX.
- Move importable state schemas out of `internal/state` or mirror them in a
  public package with explicit versioning.
- Treat `GitCommit` as optional metadata. Rollback must not assume every
  revision can be checked out as a git ref.
- Use runtime labels and `pkg/runtimeid` as the source of actual container
  identity to avoid name drift.

## API And SDK Direction

### Package Extraction Seams

- Extract deploy planning from `cmd/deploy.go` into an importable package that
  has no Cobra/Viper dependency.
- Keep `pkg/config`, `pkg/reconcile`, `pkg/state`, `pkg/ssh`, and
  `pkg/takodclient` reusable; remove direct stdout printing from packages that
  should be SDK-safe by injecting loggers or event sinks.
- Promote or duplicate necessary `internal/state` schema types into a versioned
  public package such as `pkg/takoapi` or `pkg/schema`.
- Define typed request/response structs for public operations before exposing
  transport-specific clients.

### Public Schema And Versioning

- Version API documents independently from CLI release internals.
- Add compatibility tests for serialized state and request/response payloads.
- Prefer explicit optional fields over overloaded git fields.
- Document migration rules for canonical state documents.

### Transport And Auth

- Keep the current SSH-to-Unix-socket transport as the default private control
  plane while schemas settle.
- If a public/network API is introduced later, require a separate auth/TLS
  design, threat model, audit logging, and operator opt-in.
- Do not expose the existing Unix socket API as a public REST surface by default;
  it was designed as a node-local agent API.

## Phased Roadmap

### Phase 1: Document And Stabilize The Foundation

Milestones:

- Document canonical identity fields and state document shape.
- Inventory deploy/state/takod APIs that are safe to make importable.
- Add tests around optional git metadata display and rollback behavior.

Acceptance criteria:

- A non-git revision model is specified without synthetic git commits.
- State ownership between local cache, replicated history, and actual snapshots
  is explicit.
- No new runtime path bypasses takod.

Non-goals:

- New CLI flags.
- Public REST API.
- Cloud resource provisioning.

### Phase 2: Extract Deploy Planning And State Schemas

Milestones:

- Move deploy planning and desired revision construction out of `cmd/deploy.go`.
- Introduce importable canonical state/request schemas with version markers.
- Replace direct stdout in reusable packages with injected reporting hooks.

Acceptance criteria:

- CLI deploy behavior is unchanged for normal git deployments.
- Package-level deploy planning can be unit-tested without Cobra/Viper.
- External consumers can import stable schema types without using `internal/*`.

Non-goals:

- Public network serving.
- Raw deploy UX beyond internal plumbing.

### Phase 3: Add Raw Input Adapters

Milestones:

- Expand raw input adapters beyond the current directory, targeted archive, and
  targeted image deploy paths where needed.
- Build on `tako run` as the first no-`tako.yaml` workflow, producing canonical
  desired state for an existing server/environment.
- Generate durable revision IDs from explicit revision labels or content/image
  identity.
- Store optional git metadata only when a real git repository is available.

Acceptance criteria:

- A non-git directory deploy can flow through takod leases, labels, state, and
  reconciliation.
- An image deploy records image identity without requiring a build tag derived
  from git.
- A public image can be deployed without local project YAML while still using
  takod state, labels, leases, reconcile, history, and proxy paths.
- Rollback targets canonical revision IDs and does not attempt to checkout fake
  git refs.

Non-goals:

- Direct Docker execution from the CLI.
- Multiple orchestration modes.
- Private registry auth and compose import in the first configless image
  milestone.

### Phase 4: SDK And API Hardening

Milestones:

- Publish a Go SDK surface around config loading, deploy planning, takod client
  operations, and state APIs.
- Add schema compatibility tests and generated or documented API references.
- Decide whether a public network API is needed; if yes, design auth/TLS and
  operator enablement before implementation.

Acceptance criteria:

- SDK consumers can perform supported operations without importing `cmd/`.
- Transport boundaries are clear: SSH/Unix socket today, opt-in authenticated
  network API only after design approval.
- Public schemas carry version and migration guidance.

Non-goals:

- Browser UI.
- Multi-tenant cloud control plane.
- Provider-side infrastructure creation.

## Risks And Open Questions

- How should Tako generate revision IDs for large directories without expensive
  hashing or surprising ignored-file behavior?
- Should archive deploy support expand beyond the current targeted service
  adapter and supported `.tar`, `.tar.gz`, `.tgz`, and `.zip` formats?
- What is the right UX for selecting or remembering a default server/environment
  for `tako run` without hiding the owned-VPS scope?
- Which state fields are durable API contract versus CLI display detail?
- How much of the current takod `/v1/*` surface should become public SDK API,
  and how much should remain node-internal?
- What auth model is acceptable if operators later expose a network API?
- How should migration handle existing git-only deployment histories?
