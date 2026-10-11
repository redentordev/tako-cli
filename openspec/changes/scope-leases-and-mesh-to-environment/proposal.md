# Proposal

## Why

Issue #114: with `preview: {servers: [preview]}` and `production: {servers: [production]}` in one `tako.yaml`, `tako deploy -e preview` fails with `failed to acquire remote deploy leases: failed to connect to lease node production` when the production host is unreachable or not yet created. Independent environments cannot be deployed, broken or rebuilt independently, and the documented workaround is splitting the config per environment.

Root cause (research):

- **Lease set is wider than the environment.** `DeployMutationTargets` (`pkg/engine/takod.go:93-125`) adds every schedulable `cfg.Servers` entry with the edge role when any service is proxied (`:106-111`). It also adds every builder through `DeployLeaseTargets` (`:59-87`, loop at `:75-79`). `ServerConfig.HasPlatformRole` (`pkg/config/types.go:293-303`) returns true for servers with no `roles`, so in every non-enrolled config *every server in the file* is an edge and a builder. Lease acquisition is all-or-nothing (`pkg/engine/leases.go:467-561`, error at `:539/:546`), so one unreachable unrelated server blocks the deploy.
- **Mesh state is not environment-safe.** Mesh peers for a deploy are built from the environment's setup targets (`pkg/engine/deploy.go:395-408` → `pkg/deployer/takod_deploy.go:102-138`). However:
  - Addresses are assigned by **position in this operation's target list** (`buildTakodMeshPeers` → `meshAddress(i)`, `takod_deploy.go:2547-2608`). The same node can get different addresses in different environments or in narrower operations.
  - Each node holds **one** `mesh/peers.json` and one WireGuard interface (`pkg/takod/metadata.go:34,41`). A server shared by two environments is overwritten by whichever environment deployed last, dropping the other environment's peers.
  - The `mesh:` block (CIDR, interface, port) is project-wide, so two environments compute colliding addresses (both start at `.1`).

Other environment-scoped operations already lease only environment servers: rollback, project rollback, destroy, scale, promote, remove and placement-apply (research table in design.md). Replicated history is already per environment (`internal/state/replicator.go:225-241`), and takod leases are already keyed per `(project, env)` (`pkg/takod/lease.go:337-342`). Node-wide operations such as `tako upgrade` (`cmd/upgrade.go:291-418`) take no lease at all.

## What Changes

- **Deploy leases cover exactly the nodes the resolved deploy mutates.** That set is the assigned workers, the environment's resolved proxy targets, the builder the build plan selects, the controller when enrolled, nodes holding prior desired assignments for this environment, and the environment's own servers (authority fan-out). A server outside the environment is leased only when the plan actually mutates it, for example an explicitly configured proxy target or a selected builder. Role-less servers outside the environment are never implicit edges or builders for it.
- **Unreachable servers outside the environment never block.** `deployConsistency: lease` and `onUnreachableNode: block` keep their all-or-nothing meaning, applied to the scoped set.
- **Node-scoped maintenance lease.** Operations that change node-global state take a new exclusive node lease on each node they touch: `tako upgrade`, server setup/provisioning, `state forget-node` and mesh re-key. While it is held, environment leases on that node fail, and it cannot be acquired while any environment lease is live. This keeps two environments that share a server safe from node-wide mutations. It is gated by a takod capability; older agents fall back to acquiring the environment lease for every `(project, env)` that lists the node.
- **Mesh membership is per environment; the interface stays per node.**
  - takod stores one peer set per `(project, env)` and renders WireGuard from the union of the sets on that node.
  - A preview deploy contacts only preview servers, and preview-only and production-only nodes never become peers.
  - A shared server is a member of both sets.
- **Stable mesh addresses.** Addresses are resolved in this order:
  1. a pinned address: the platform-assigned `MeshIP` (internal today, `types.go:279`) or a new optional `servers.<name>.meshIP` key;
  2. the address already recorded on the node (sticky, so existing meshes are not renumbered);
  3. a deterministic allocation over all configured server names, so every environment computes the same address without contacting the others.

  takod rejects a union that has one address with two different public keys, and the error names both peers.
- **Migration.** takod converts an existing single `peers.json` into a peer set keyed by the `Project`/`Environment` tags it already carries. No addresses change.
- No **BREAKING** config changes. Behavior changes for the better: a deploy no longer needs connectivity to servers outside its environment.

## Out of Scope

- Separate WireGuard interfaces or CIDRs per environment (see design D4 alternatives).
- Enrolled/platform-controller mode. Its single controller lease and controller-owned membership (`pkg/platform/membership.go`) are unchanged.
- New `onUnreachableNode` values such as `skip`. Only `block` exists, and it stays.
- Cross-project lease coordination on shared nodes beyond the node maintenance lease. Proxy file writes are already per `(project, env)` file, and takod serializes the render.

## Capabilities

### New Capabilities
- `deploy-leases`: which nodes each operation leases, how environment and node-maintenance leases interact, and what happens with unreachable nodes.
- `mesh-networking`: per-environment mesh membership, stable addressing, shared-server union, and migration of existing meshes.

### Modified Capabilities
- None. No existing main spec covers leases or the mesh.

## Impact

- **Engine:** `pkg/engine/takod.go:59-125` (`DeployLeaseTargets`, `DeployMutationTargets`), `pkg/engine/deploy.go:390-421`, `pkg/engine/leases.go` (node lease kind, fallback), `pkg/engine/run.go:134`.
- **Config:** `pkg/config/types.go:293-303` (role semantics documented; no behavior change for enrolled inventory), `pkg/config/placement.go:89-118`.
- **Deployer/mesh:**
  - `pkg/deployer/takod_deploy.go:102-138`, `:1470-1539`, `:2473-2622` (address resolution, peer-set metadata).
  - `pkg/mesh` (WireGuard config from the union).
- **takod:**
  - `pkg/takod/lease.go` (node maintenance lease).
  - `pkg/takod/metadata.go` and `server.go:2671` (per-`(project, env)` peer sets, union render, conflict check, legacy migration).
  - New capabilities `lease.node-maintenance-v1` and `mesh.peer-sets-v1`.
- **Commands:** `cmd/upgrade.go`, setup/provision commands, and `cmd/state.go:2263-2293` take the node lease.
- **Docs:** `docs/ORCHESTRATION-MODEL.md` (leases, mesh), `docs/CONFIGURATION.md` (environments sharing servers; `meshIP`), and the troubleshooting entry for the #114 error.
