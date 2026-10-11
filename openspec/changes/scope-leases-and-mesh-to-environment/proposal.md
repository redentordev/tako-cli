# Proposal

## Why

Issue #114: with `preview: {servers: [preview]}` and `production: {servers: [production]}` in one `tako.yaml`, `tako deploy -e preview` fails with `failed to acquire remote deploy leases: failed to connect to lease node production` when the production host is unreachable or not yet created. Independent environments cannot be deployed, broken or rebuilt independently, and the documented workaround is splitting the config per environment.

Root cause (research on master 20caafe, cross-checked against tags v0.10.0 to v0.14.1):

- **The lease set is wider than the environment.** `DeployMutationTargets` (`pkg/engine/takod.go:93-125`) adds every schedulable `cfg.Servers` entry with the edge role when any service is proxied (`:106-111`), and every builder through `DeployLeaseTargets` (`:59-87`, loop at `:75-79`). `ServerConfig.HasPlatformRole` (`pkg/config/types.go:293-303`) returns true for servers with no `roles`. `Roles` is platform-owned and cannot be written in `tako.yaml` (`types.go:281`, `yaml:"-"`), so in every non-enrolled config *every server in the file* is an edge and a builder. Lease acquisition is all-or-nothing (`pkg/engine/leases.go:119-151`, error at `:127`), so one unreachable unrelated server blocks the deploy.
- **The deployer uses the same project-wide builder list.**
  - `remoteBuilderServers` (`pkg/deployer/takod_deploy.go:720-732`) feeds remote builds (`:764-850`). The first sorted builder per platform does the build, so a `staging` deploy can build on a server named `app-prod`.
  - It also feeds retained-image lookup (`locateRetainedImage`, `:939-960`, used by deploy and rollback). Both probe the platform of every listed server (`groupTakodNodesByPlatform`, `:1224`), so an unreachable server in another environment also breaks builds and image reuse, not just leasing.
  - `placementApplyTargets` (`pkg/engine/placement_apply.go:214-239`) has the same all-edges loop.
- **Mesh membership is already environment-scoped; mesh addressing is not stable.** Setup targets have been intersected with the environment's servers (`pkg/engine/deploy.go:395-408`) since 18f8a67, which predates v0.10.0. No code path makes a preview node a WireGuard peer of a production node. The reported "both hosts in one mesh" matches positional addressing: each single-server environment gives its only node `meshAddress(0)`, so both hosts get the same `.1` address on the same interface name and CIDR. The investigation found real defects in the same code:
  - **Addresses come from the position in each operation's target list** (`meshAddress(i)`, `takod_deploy.go:2594-2608`).
    - `tako run` sets up a single server (`pkg/engine/run.go:132,182-186`). That renumbers the node to `.1` and replaces its WireGuard peers with an empty list, which breaks cross-node proxy upstreams until the next full deploy.
    - Scale, promote and rollback use the environment's declaration order (`GetEnvironmentServers`, `types.go:944-982`), while deploy uses sorted order. So a non-sorted `servers:` list swaps addresses between operations.
    - Proxy upstream IPs (`meshHostIPForServer`, `pkg/deployer/takod_proxy.go:1140-1166`) and `tako domains hosts --address mesh` (`cmd/domain_hosts.go:183-200`) each recompute positions independently.
  - **Each node has one WireGuard config**, replaced wholesale on every apply (`pkg/takod/mesh.go:39-51`, `pkg/mesh/wireguard.go:131`), plus one `node.json` and one `peers.json` (`pkg/takod/metadata.go:34,41`). A server shared by two environments, or by two projects, keeps only the peers of whichever deploy ran last.
- **v0.14.2 interaction (PR #120).** With `-e`, servers outside the selected environment's scope that reference unset variables are pruned before decoding (`pkg/config/environment_scope.go` on `fix/v0.14.2`). This has two consequences:
  - Any computation over "all configured servers" now depends on which variables happen to be exported.
  - A prior desired owner pruned this way is silently skipped by `AddPriorDesiredMutationTargets` (`takod.go:131-151`) instead of failing.

Rollback, project rollback, destroy, scale, promote, remove and `run` already lease only environment servers (table in design.md), but they still reach other environments through the deployer's builder and image-source list. `tako upgrade servers` takes no lease at all.

## What Changes

The change ships as four PRs (design.md "Implementation order").

- **PR1: environment-scoped lease, builder and image-source targets (CLI only; fixes the reported error).**
  - In non-enrolled configs, an environment's edge, builder and image-source candidates are its schedulable members. The deploy lease set is the environment's servers plus the configured servers that hold prior desired assignments for the environment.
  - The same rule applies to `DeployMutationTargets`, `DeployLeaseTargets`, `placementApplyTargets`, `remoteBuilderServers` and `locateRetainedImage`, so a build or image export never runs on an unleased node.
  - A prior desired owner that environment scoping pruned for unset variables fails the operation before any lease is taken. The error names the server and the variables.
  - Enrolled (platform) configs are unchanged.
- **PR2: stable mesh addresses and environment-wide peer lists (CLI only).**
  - Every mesh apply lists all schedulable members of the environment as peers. Mesh apply is sent only by operations that already lease every member: deploy, rollback, project rollback, scale and promote. `tako run` verifies the agent and never rewrites mesh addresses, peers or node metadata.
  - A node keeps the address recorded in its takod node metadata, which existing agents already return from `GET /v1/status`, when that address lies inside `mesh.networkCIDR`. Nodes without a usable record get the lowest free host address that no member records for itself or for a peer. Unrecorded members are assigned in sorted name order.
  - Duplicate recorded addresses, which today's churn can leave behind, are repaired deterministically: the member whose name sorts first keeps the address, and the others are reassigned. Tako warns when this happens.
  - One resolver serves mesh apply, proxy upstream addresses and `tako domains hosts --address mesh`.
  - The deploy warns when a member's recorded metadata belongs to another project or environment, because that shared node's peers are last-writer-wins until PR4.
- **PR3: node maintenance lease (takod and CLI, capability `lease.node-maintenance-v1`).**
  - `tako upgrade servers` and `tako setup` converging a running node take an exclusive per-node lease. It cannot be granted while any environment lease of any project is live on that node, and while it is held, environment leases on that node fail.
  - With an older agent, Tako takes the selected environment's lease on that node instead and warns that other environments and projects are not fenced.
- **PR4: shared-node mesh peer sets (takod and CLI, capability `mesh.peer-sets-v1`).**
  - takod stores one peer set per `(project, env)` and renders WireGuard from the union of the sets on that node.
  - takod rejects a union in which one address maps to two keys, one key maps to two addresses, or two sets that have peers disagree on CIDR, listen port or the node's own address. A rejected union changes nothing on the node.
  - An existing `peers.json` migrates into the set named by its own tags. Legacy CLI requests are keyed the same way.
- **No BREAKING config changes and no new config keys.**
  - Deploys no longer need connectivity to servers outside the environment.
  - `tako run` no longer touches the mesh.
  - `tako domains hosts --address mesh` now reads addresses from the proxy nodes, so it needs connectivity to them.

## Out of Scope

- Separate WireGuard interfaces or CIDRs per environment (design D7 alternatives).
- Enrolled/platform-controller mode. Its single controller lease, inventory roles and controller-owned membership (`pkg/platform/membership.go`) are unchanged.
- New `onUnreachableNode` values such as `skip`. Only `block` exists, and it stays.
- A user-facing `servers.<name>.meshIP` pin. The recorded address acts as the pin, and adding the key later is purely additive.
- `tako doctor` mesh conflict reporting. Conflicts are reported when a mesh apply would create them.
- Cross-project lease coordination beyond the node maintenance lease. Proxy route files are already per `(project, env)`, and takod serializes the render.
- Cleaning up stale peer sets on servers removed from an environment. Peers are mutual, so a stale one-sided entry carries no traffic. `tako destroy` removes the set.

## Capabilities

### New Capabilities
- `deploy-leases`: which nodes each operation leases or contacts, how environment and node-maintenance leases interact, how pruned servers are treated, and what happens with unreachable nodes.
- `mesh-networking`: environment-wide peer lists, stable address resolution, narrow operations, shared-server peer-set union, conflict handling and migration.

### Modified Capabilities
- None. No existing main spec covers leases or the mesh.

## Impact

- **PR1 (engine, config, deployer):**
  - `pkg/engine/takod.go:59-125` (`DeployLeaseTargets`, `DeployMutationTargets`, `AddPriorDesiredMutationTargets`).
  - `pkg/engine/deploy.go:390-421`, `pkg/engine/placement_apply.go:214-239` and `pkg/engine/leases.go` (reason text in errors).
  - `pkg/deployer/takod_deploy.go:720-732` (`remoteBuilderServers`) and `:939-960` (`locateRetainedImage`).
  - `pkg/config/environment_scope.go` (from #120): expose the names of pruned servers.
  - Command target audit: `cmd/backup.go`, `cmd/cleanup.go`, `cmd/env.go`, `cmd/maintenance.go`, `cmd/live.go`, `cmd/certs.go`.
- **PR2 (deployer, cmd):**
  - `pkg/deployer/takod_deploy.go:102-138`, `:1470-1600`, `:2547-2639` (peer list, apply scope, address resolver).
  - `pkg/deployer/takod_proxy.go:1140-1166`, `pkg/engine/run.go:182-186` and `cmd/domain_hosts.go:125-200`.
  - No takod change: the resolver reads the `node` and `peers` maps that `GET /v1/status` already returns (`pkg/takod/server.go:2671-2672`).
- **PR3 (takod and CLI):** `pkg/takod/lease.go` and `server.go` (capability `lease.node-maintenance-v1`), `pkg/engine/leases.go`, `cmd/upgrade.go:156-287` and `cmd/setup.go`.
- **PR4 (takod and CLI):**
  - takod: `pkg/takod/metadata.go`, `pkg/takod/mesh.go`, `server.go:1720-1813` and `:2671` (per-`(project, env)` peer sets, union render, conflict check, legacy keying, capability `mesh.peer-sets-v1`).
  - CLI: the deployer's mesh path and `pkg/engine/destroy.go` (remove the environment's set).
- **Docs:** `docs/ORCHESTRATION-MODEL.md` (lease scope, node lease, mesh addressing and peer sets) and `docs/CONFIGURATION.md` ("Shared Nodes" mesh notes; `domains hosts` connectivity). Add a troubleshooting entry for the #114 error.
