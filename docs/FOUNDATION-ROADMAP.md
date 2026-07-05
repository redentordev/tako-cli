# Tako Foundation Roadmap

This document is a planning artifact for moving Tako CLI toward a stronger
PaaS-like platform foundation while preserving the existing scope: deploy to
operator-owned VPS hosts, through one `takod` orchestration path. It does not
propose cloud infrastructure provisioning or an unmanaged Docker mode.

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

- `tako deploy` currently requires git source state. It hard-errors outside a
  git repository or when dirty without `--allow-dirty`, constructs a git client
  unconditionally, and uses the git commit hash as the build tag.
- Existing fallback image-reference behavior for empty build tags is present in
  takod state handling, but the deploy path does not exercise it.
- Current deployment source identity, revision identity, and git commit identity
  are coupled. Rollback code may treat a non-empty `GitCommit` as a real git ref,
  so synthetic commit values would be unsafe.
- State is split across local `.tako` cache, replicated deployment history, and
  desired/actual takod state schemas. Some local state get/set helpers exist but
  are not active in the main flow.
- `cmd/deploy.go` is monolithic and CLI-bound. Some lower packages print
  directly to stdout, which limits SDK reuse.
- `internal/state` cannot be imported by external SDK consumers.
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

# Deploy a prebuilt image through takod state/reconcile.
tako deploy --image ghcr.io/acme/web:2026-07-05 -e production

# CI supplies an explicit revision label without pretending it is a git commit.
tako deploy --source . --revision ci-20260705.12
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

- Add directory/archive/image input adapters that produce canonical deployment
  requests.
- Generate durable revision IDs from explicit revision labels or content/image
  identity.
- Store optional git metadata only when a real git repository is available.

Acceptance criteria:

- A non-git directory deploy can flow through takod leases, labels, state, and
  reconciliation.
- An image deploy records image identity without requiring a build tag derived
  from git.
- Rollback targets canonical revision IDs and does not attempt to checkout fake
  git refs.

Non-goals:

- Direct Docker execution from the CLI.
- Multiple orchestration modes.

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
- What source archive format and ignore rules should be canonical for non-git
  directory deploys?
- Which state fields are durable API contract versus CLI display detail?
- How much of the current takod `/v1/*` surface should become public SDK API,
  and how much should remain node-internal?
- What auth model is acceptable if operators later expose a network API?
- How should migration handle existing git-only deployment histories?
