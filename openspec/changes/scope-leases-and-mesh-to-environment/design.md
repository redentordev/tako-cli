# Design

## Context

See proposal.md for motivation and specs/deploy-leases/spec.md and specs/mesh-networking/spec.md for the behavior contract. Observed current state (master 20caafe):

- **Lease acquisition.** `AcquireRemoteOperationLeasesContext` (`pkg/engine/leases.go:99-164`):
  - If any server is enrolled (`ClusterID`/`NodeID`), it takes one controller lease (`:205-298`).
  - Otherwise it takes a per-node lease through `manager.AcquireLeaseContext(ctx, operation, envName, DefaultLeaseTTL)` (`:133-134`), in parallel with no quorum. Any failure releases everything and returns a `LockedError` or a `ConnectivityError` (`:127`).
  - The node lease file is `<dataDir>/leases/<project>/<env>.json` (`pkg/takod/lease.go:337-342`), so leases are already per `(project, env)`.
  - `deployConsistency` accepts only `lease`, and `onUnreachableNode` accepts only `block` (`pkg/config/types.go:141-144`, `validator.go:402-424`).
- **Roles.** `ServerConfig.Roles`, `Lifecycle` and `MeshIP` are platform-owned and come only from enrolled inventory (`types.go:277-284`, `yaml:"-"`). In a non-enrolled config every server is role-less, so `HasPlatformRole` (`:293-303`) makes it an edge, a builder and a worker, and `Schedulable()` is always true.
- **Deploy lease set** (`pkg/engine/deploy.go:390-421`). It is `DeployMutationTargets(cfg, allServices, assignments)`, plus `AddPriorDesiredMutationTargets`, plus `connectivityServerNames` (all environment servers, `:265`, added by `deployMutationAuthorityTargets`, `:592`).
  - `DeployMutationTargets` (`takod.go:93-125`) adds every `cfg.Servers` edge when a service is proxied, every builder through `DeployLeaseTargets` (`:59-87`), and the controller. **This is the #114 error.**
  - Setup (mesh) targets are intersected with environment servers (`deploy.go:395-408`).
- **Proxy placement cannot leave the environment.** `ResolvePlacementTargets` → `validatePlacementTargets` (`pkg/config/placement.go:120-139`) rejects a placement target that is not an environment server. So `ResolveEnvironmentProxyTargets` (`types.go:1001-1029`) always returns a subset of the environment.
- **Deployer builder and image-source list.**
  - `remoteBuilderServers` (`pkg/deployer/takod_deploy.go:720-732`) returns every schedulable builder-role server in `cfg.Servers`. It is used by remote builds (`:688-711`, `:764-850`; the source is `sources[0]` per platform) and by `locateRetainedImage` (`:939-960`).
  - Both call `groupTakodNodesByPlatform` (`:1224`), which contacts every listed server.
  - `placementApplyTargets` (`pkg/engine/placement_apply.go:214-239`) adds every edge-role server when a service is proxied.
- **Other operations** lease environment servers only:
  - rollback `rollback.go:165,183`, project rollback `rollback_project.go:284,303`;
  - destroy `destroy.go:269`/`:357`, scale `scale.go:477-481`, promote `promote.go:100-136`, remove `remove.go:119/238`;
  - `run` leases the single requested server (`run.go:132-134`).

  Command-level leases (backup, cleanup, env push, maintenance, live, certs) each build their own target list. `cmd/upgrade.go` takes **no lease**. Non-enrolled upgrades target environment servers (`upgradeTargetServers` → `setupTargetServers`, `:289-299`), and enrolled upgrades use the cluster upgrade coordinator lock (`:211-218`).
- **Replicated state** is per environment: `getReplicaServers` uses `GetEnvironmentServers(env)` (`internal/state/replicator.go:225-241`). Node paths are `/var/lib/tako/{desired,actual}/<project>/<env>`.
- **Mesh.**
  - `SetupTakodRuntime` (`takod_deploy.go:102-138`) runs on `getTakodTargetServers` (`:2624-2639`), which is either the operation's `SetTargetServers` list or the environment list in declaration order.
  - It verifies agents with `GET /v1/status` (`:143-160`), fetches keys (`POST /v1/mesh/key`, `:2473`) and builds peers with positional addresses (`buildTakodMeshPeers` `:2547-2569`, `meshAddress(i)` `:2594-2608`). Only the internal platform `MeshIP` is honored (`:2610-2622`).
  - It then writes `PUT /v1/metadata` and `POST /v1/mesh/apply` per node (`prepareTakodNode`, `:1470-1539`). Enrolled nodes skip both.
  - takod writes `<dataDir>/node.json` and `<dataDir>/mesh/peers.json` (`pkg/takod/metadata.go:22-48`) and returns them as the `node` and `peers` maps of `GET /v1/status` (`server.go:2671-2672`).
    - `node.json` carries `project`, `environment`, `mesh.address`, `mesh.networkCIDR`, `mesh.interface` and `mesh.publicKey`.
    - `peers.json` carries `project`, `environment` and `peers[].{name,address,publicKey}` (`takod_deploy.go:37-77`).
  - `POST /v1/mesh/apply` replaces the whole interface config (`pkg/takod/mesh.go:39-51`).
  - Callers with narrower or reordered target lists: deploy uses sorted mutation targets ∩ environment, while rollback (`rollback.go:333`), project rollback (`rollback_project.go:393`), scale (`scale.go:188`) and promote (`promote.go:183`) use declaration order. `run` passes one server (`run.go:182`).
  - Positions are also recomputed by `meshHostIPForServer` (`takod_proxy.go:1140-1166`, proxy upstreams) and `meshHostIPs` (`cmd/domain_hosts.go:183-200`, declaration order, offline).
- **Environment scoping (v0.14.2, `fix/v0.14.2:pkg/config/environment_scope.go`).** When a config has unset `${VAR}` references, `-e ENV` keeps every top-level section, `ENV` with its extends chain, and the servers `ENV` can use. Those are its `servers` list, placement servers, and every server when `ENV` uses a selector, has no explicit list, or the config is enrolled. Out-of-scope servers that reference unset variables are removed before decoding, and only `DeferredEnvironmentVariables()` records that anything was removed.

## Goals / Non-Goals

**Goals:**
- An environment's operations need connectivity only to nodes in that environment, plus nodes that still hold its prior desired assignments.
- No reduction in safety: any node an operation mutates is leased, and node-global mutations exclude every environment on that node.
- A node's mesh address never changes because of which operation ran, its target subset, server declaration order, or which environment variables are exported.
- Shared servers keep working for every environment's and project's mesh traffic once takod supports peer sets.
- Existing meshes keep their addresses.

**Non-Goals:**
- Partial deploys that skip unreachable members of the selected environment. Members of the environment remain mandatory.
- Changes to enrolled/controller mode.

## Decisions

### D1. Non-enrolled edge, builder and image-source candidates are environment members (PR1)
In a config without enrolled servers, the following are the environment's schedulable members (`ResolveSchedulableEnvironmentTargets(GetEnvironmentServers(env))`):
- the edge candidates (`ResolveEnvironmentProxyTargets`, already a subset);
- the builder candidates;
- the retained-image source candidates.

One helper, `config.EnvironmentBuilderCandidates(cfg, env)`, implements the builder rule. It is used by:
- `DeployLeaseTargets` and `DeployMutationTargets`, which also use environment proxy targets instead of every edge;
- `placementApplyTargets`;
- `Deployer.remoteBuilderServers` and therefore `locateRetainedImage`.

Lease computation and deployer behavior therefore come from the same function, so no build or image export can reach an unleased node.

Enrolled configs keep today's behavior: inventory roles, a cluster-wide builder list, and the single controller lease.

Because environment servers are always in the deploy lease set (authority fan-out), the non-enrolled deploy lease set reduces to **environment servers ∪ configured prior desired owners**. A table test pins the #114 configuration: the preview lease set is `{preview}`, and no takod call is made to `production`.

*Alternative rejected:* keep the wide set and tolerate unreachable nodes outside the environment. A leased-but-skipped node is unprotected, and a node that is not mutated does not need a lease.

### D2. Unreachable nodes and error reasons (PR1)
`onUnreachableNode: block` keeps its all-or-nothing meaning over the scoped set. The only nodes outside the environment that can be in the set are prior desired owners. Their lease or connectivity error says why the node is in scope, for example `node old-1 holds prior desired assignments for environment preview`. This lets the operator see that the node must be reachable to stop the old containers, or that `tako remove --server old-1` comes first.

### D3. Pruned prior owners fail closed (PR1)
v0.14.2 prunes out-of-scope servers with unset variables, and `AddPriorDesiredMutationTargets` skips assignments whose node is not in `cfg.Servers`. Without a guard, a server that left the environment's list but still runs its containers would silently be neither leased nor cleaned.
- `#120` gains `Config.ScopedOutServers() map[string][]string` (server name → unset variables).
- `AddPriorDesiredMutationTargets` returns an invalid-request error (exit 2) before any lease is taken when a prior owner is scoped out. Example: `server old-1 holds prior desired assignments for preview but was not loaded because ${OLD_1_HOST} is unset; export it and retry`.
- A prior owner that is absent from `servers:` entirely keeps today's behavior: skipped, with the retired-server warning from local state.

No other #114 computation reads servers outside the environment scope, so pruning cannot otherwise change lease sets or mesh addresses (D5).

### D4. Mesh membership and apply scope (PR2)
- **Peer list:** every mesh apply for an environment lists all of its schedulable members, sorted by name, whatever subset the operation mutates. Membership was already per environment and stays that way. Nodes that belong only to different environments are never peers.
- **Apply targets:** deploy, rollback, project rollback, scale and promote send `PUT /v1/metadata` and `POST /v1/mesh/apply` to every member. In non-enrolled mode they already lease and contact every member (`deploy.go:265,415`, `rollback.go:165`, `scale.go:477-481`, `promote.go:100-136`), so this adds no connectivity requirement.
  - Workload scope (`SetTargetServers`) stays separate from mesh scope. A deploy whose workloads touch a subset still reconciles workloads on that subset only.
- **`tako run`:** it only runs `verifyTakodAgents`. It never writes node metadata or mesh config. Today it rebuilds a one-node mesh, which on a multi-node environment renumbers the node and removes all of its peers. A fresh node that never received a deploy behaves as it effectively does today: it has no usable mesh until the first deploy.
- **Enrolled nodes:** unchanged. The platform owns their mesh, so app mesh apply is skipped (`takod_deploy.go:1508-1515`), and the resolver returns the platform `MeshIP`.

### D5. Stable address resolution (PR2, CLI only)
Inputs are the `GET /v1/status` results for every member, which `verifyTakodAgents` already fetches:
- each member's `node.mesh.address` (its *recorded* address);
- each member's `peers.peers[].address` (addresses in use on its interface).

Resolution, in one pure function `resolveMeshAddresses(cidr, members, records)`:

1. Process members in sorted name order. A member with a platform `MeshIP` (enrolled) uses it.
2. A member keeps its recorded address if the address parses, lies inside `mesh.networkCIDR`, and has not already been taken by an earlier member in this pass. A later member that records the same address loses it. This deterministically repairs duplicates left by today's churn and emits one warning naming the members.
3. Every remaining member gets the lowest host address of `mesh.networkCIDR` above the network address that is not in the *used* set: kept addresses, plus all addresses in members' recorded peer lists that belong to keys outside this membership.
4. A member whose resolved address differs from its record is applied in the same operation (D4 applies to all members).

Properties:
- On a fresh environment the result equals today's deploy (`.1, .2, …` in sorted order). Existing nodes keep their recorded addresses, so nothing is renumbered on upgrade.
- The result does not depend on the operation's target list, declaration order, or servers outside the environment. That makes it immune to v0.14.2 pruning.
- On a shared node, both environments read the same recorded address for that node, so they agree on it.
- An address that another environment or project uses on a shared member is avoided, because it appears in that member's recorded peers.

`meshAddress(i)` and `cmd/domain_hosts.go:meshHostIPs` are deleted. Proxy upstream IPs come from the operation's resolved map. `tako domains hosts --address mesh` (and `auto` without `privateHost`) reads `GET /v1/status` from the proxy nodes and fails for an unreachable node instead of guessing.

The deploy warns once per node when a member's recorded `node.project`/`node.environment` differs from the deploying `(project, env)`, while the node lacks `mesh.peer-sets-v1`: `server s is shared with app/production; its mesh peers are replaced by whichever deploy runs last until takod is upgraded`.

*Alternative rejected:* hash-of-name allocation with linear probing over all configured servers (the earlier draft). Probing depends on which servers are present, which changes with v0.14.2 pruning and with servers added later. It still needs a recorded-address step to stay stable, and it can collide across projects.

### D6. Node maintenance lease (PR3)
takod gains a second lease kind, gated by capability `lease.node-maintenance-v1`:
- **Storage:** `<dataDir>/node-leases/maintenance.json`, outside `leases/` so no project or environment name can collide with it. The format is versioned and frozen, because a new takod must read a lease written before its own restart.
- **Mutual exclusion:** acquiring an environment lease fails with `LockedError` while a live maintenance lease exists. Acquiring the maintenance lease fails immediately, with exit 3, while any live environment lease of any project exists on the node; the error lists the holders. Both checks and writes run under one takod lease mutex.
- **Holders:**
  - `tako upgrade servers` for non-enrolled nodes. Enrolled upgrades keep the cluster upgrade coordinator.
  - `tako setup` when converging a node whose takod answers.

  `state forget-node` and `state repair` are **not** holders. They write `(project, env)` state and already take environment leases (`cmd/state.go:1190`).
- **Scope per node, just in time:** the lease is acquired immediately before that node's mutation and released after verification, against the restarted agent. It uses a 15-minute TTL and no background renewal, because the agent restarts mid-operation. If the release fails, the lease expires on its own.
- **Older agent (capability absent):** the operation acquires the *selected environment's* lease on that node for the same window, and warns that other environments and projects on the node are not fenced. The first upgrade necessarily takes this path.
  - This deviates from the project's "fail closed on missing capability" rule. Failing would make the fixing upgrade impossible, and today's behavior is no lease at all.
  - Enumerating "every environment in the config" was rejected: v0.14.2 prunes other environments, so the list would be silently incomplete.
- **Downgrade:** an older takod ignores `node-leases/`. The stale file expires by TTL.

Environment leases on a shared node do not conflict with each other (`lease.go:337-342`), and their mutable state is disjoint per `(project, env)`. The node-global resources they share are:
- the WireGuard interface (made union-safe by D7);
- the merged Caddyfile (per-`(project, env)` route files, serialized by takod's render mutex);
- the Docker daemon.

### D7. Shared-node peer sets (PR4)
Options considered:
1. **A project mesh that tolerates unreachable peers.** Rejected. Preview nodes would peer with production nodes, and peers would be dropped silently.
2. **Separate interface and CIDR per environment.** Rejected. It needs N interfaces and ports per shared server, plus routing overlap and firewall rules.
3. **Chosen: one interface per node, one peer set per `(project, env)`, union render.**

takod with `mesh.peer-sets-v1`:
- **New request:** `PUT /v1/mesh/peer-sets` carries `{project, environment, config, node, peers}`. takod validates it, stores `mesh/peer-sets/<project>/<env>.json`, and renders the union, all under one mutex. A rejected union stores nothing and leaves the interface unchanged.
- **Union rules:**
  - Peers are deduplicated by public key.
  - One address held by two keys, or one key holding two addresses, is a conflict.
  - Sets that have peers other than the node itself must agree on `networkCIDR`, `subnetBits`, `listenPort`, and the node's own address.
  - Sets with no other peers (single-node environments) add no peers and impose no interface settings. This keeps today's co-location of unrelated single-node projects working even when their `mesh:` blocks differ. When no set has peers, the most recent set's settings apply, as today.
  - A conflict error names both `(project, env, server)` entries and suggests redeploying the environment that owns the stale address.
- **Removal:** `DELETE /v1/mesh/peer-sets/<project>/<env>` removes a set and re-renders. `tako destroy` calls it on environment members that advertise the capability.
- **Status:** `GET /v1/status` adds `meshPeerSets` (per set: project, environment, peers). The D5 used-set reads it when present, instead of `peers`.
- **Migration:**
  - On first use, takod converts an existing `mesh/peers.json` into the set named by its `project`/`environment` tags. The deployer has always written those tags (`takodMeshDesiredState`).
  - An untagged file becomes `_legacy`, which is dropped on the next peer-set write.
- **Old CLI → new takod:** the legacy `PUT /v1/metadata` still writes `node.json`/`peers.json`. The following legacy `POST /v1/mesh/apply` is keyed by the tags of that just-written `peers.json`, stored as that set, and rendered as the union under the same conflict check.
- **New CLI → old takod:** the legacy path, plus the D5 shared-node warning. This is today's behavior, not a new feature request, so it does not fail closed.

## Risks / Trade-offs

- **[Under-leasing: a mutated node is missing from the lease set]** → D1 derives leases and deployer candidates from one helper. The "mutated implies leased" harness records every mutating takod call (deploy, run, rollback, project rollback, scale, promote) and asserts its node was leased.
- **[Builds move into the environment]** → Builds and image exports now always run on environment members. That is the intended fix: a staging build no longer loads a production server. Each environment contains its own targets, so every target platform still has a builder.
- **[Mesh apply now reaches every member on deploys with subset workloads]** → Those members are already leased and contacted, and an unchanged config re-applies idempotently.
- **[A recorded address is wrong because of today's churn]** → D5 step 2 repairs duplicates deterministically, and every member whose address changed is re-applied in the same operation.
- **[Concurrent first deploys of two environments that share an unrecorded server]** → They may choose different addresses. Before PR4, the last writer wins and the next deploy reads the record. After PR4, the union check rejects the second one, which succeeds on retry.
- **[Node lease starvation by continuous deploys]** → Acquisition fails fast with the holders listed. Upgrade is an operator action and can be retried.
- **[Mixed takod versions]** → PR1 and PR2 need no takod change. PR3 and PR4 keep today's behavior with explicit warnings on old agents.
- **[`tako domains hosts --address mesh` now needs connectivity]** → It previously printed positional guesses that could be wrong, so correctness wins. `--address private|ssh` stay offline.

## Migration Plan

- **PR1 and PR2 (CLI only):** behavior changes immediately. Addresses stay as recorded, and duplicate addresses are repaired with a warning on the first full-membership operation.
- **PR3 and PR4:** run `tako upgrade servers` once; the first run uses the D6 fallback. The first deploy of each environment after that writes its peer set, and the legacy file is migrated in place.
- **No config edits.**
- **Rollback to an older CLI:**
  - it writes the legacy file, which a new takod keys by tags and unions (D7);
  - it goes back to positional addressing, which can renumber again. That is the old bug, not a new failure mode.

## Implementation order

1. **PR1: lease and builder scoping** (tasks 1.x). CLI only. Lands after #120 (v0.14.2), whose `servicesBuildImages` refactor of `takod.go` it builds on and whose environment-scope code gains `ScopedOutServers()`. Closes #114's reported error.
2. **PR2: stable mesh addressing** (tasks 2.x). CLI only. Independent of PR1 but reviewed after it, because both touch `deploy.go:390-421`.
3. **PR3: node maintenance lease** (tasks 3.x). takod and CLI, behind `lease.node-maintenance-v1`.
4. **PR4: shared-node peer sets** (tasks 4.x). takod and CLI, behind `mesh.peer-sets-v1`. It depends on PR2's resolver.
5. **Docs** ship with each PR for its own section (tasks 1.8, 2.7, 3.3 and 4.5). The troubleshooting entry ships with PR1.

## Resolved Questions

1. **Split the mesh per environment, or keep a project mesh?**
   - **Decision:** membership stays per environment, which is already true on master since before v0.10.0. A per-node union of `(project, env)` peer sets for shared nodes ships last (PR4, D7).
   - **Rationale:** no code path peers nodes of different environments. The real defects are address churn and last-writer-wins on shared nodes, so the plan fixes those instead of redesigning membership.
2. **Should explicit `roles: [builder]`/`[edge]` servers outside the environment stay candidates?**
   - **Decision:** the question does not arise in non-enrolled configs. `roles` cannot be written in `tako.yaml` (`yaml:"-"`), and proxy placement outside the environment is already rejected (`placement.go:120-139`). Non-enrolled candidates are environment members (D1), and enrolled mode is unchanged.
   - **Rationale:** the earlier spec scenarios for an explicit builder or proxy target outside the environment described impossible configs, so they were removed.
3. **Add a user-facing `servers.<name>.meshIP`?**
   - **Decision:** no, not in this change.
   - **Rationale:** recorded addresses are a durable pin, and allocation avoids every address visible on members. A new key would also overlap the platform-owned `MeshIP` field. It can be added later if conflict reports show a need.
4. **Should the node maintenance lease also cover `tako cleanup`/`backup`?**
   - **Decision:** no.
   - **Rationale:** they operate on `(project, env)` resources and keep the environment lease. `state forget-node` and `state repair` are also removed from the holder list for the same reason.
5. **Ship leases and mesh separately?**
   - **Decision:** yes, as four PRs (see Implementation order).
   - **Rationale:** PR1 is CLI-only and fixes the reported error. PR2 is CLI-only and fixes the churn. Only PR3 and PR4 need takod capabilities.
6. **Can the "both hosts end up in one mesh" symptom be reproduced?**
   - **Decision:** not as peering. Positional addressing gives both single-node environments the same `.1` address on the same interface, which looks like one mesh. PR2's regression tests pin the real churn defects instead (`run` renumbering, declaration-order swaps, subset deploys).
   - **Rationale:** setup targets have been intersected with environment servers since 18f8a67, which is an ancestor of v0.10.0 through v0.14.1.
7. **How does v0.14.2 environment scoping interact with this change?**
   - **Decision:** lease sets, builder candidates and mesh membership read only environment members, which are always in scope. A prior desired owner that was pruned fails closed (D3), and allocation never reads servers outside the environment (D5).
   - **Rationale:** a pruned server must never be silently skipped when it is needed, and addresses must not depend on which variables are exported.
8. **What should the fallback be for older agents without the node maintenance lease?**
   - **Decision:** take the selected environment's lease on that node and warn (D6).
   - **Rationale:** with pruning, the config's environment list is not reliable, and the first upgrade must still work.
9. **Should `tako run` keep preparing the mesh?**
   - **Decision:** no. It only verifies the agent (D4).
   - **Rationale:** it contacts one server, so it cannot build a correct environment-wide peer list, and today it actively breaks the node's mesh.

## Implementation notes

Deviations from the plan, each chosen as the safest equivalent:

- **PR1 helpers.** Besides `config.EnvironmentBuilderCandidates`, a sibling `config.EnvironmentEdgeCandidates` holds the edge rule, and `Config.HasEnrolledServers()` decides between the environment-scoped and inventory-role paths. `DeployLeaseTargets` and `DeployMutationTargets` gained an `environment` parameter. When proxy placement cannot be resolved, the edge set falls back to every schedulable environment member (never to servers outside the environment), so a lease set can only grow inside the environment.
- **Deploy target computation** moved into `resolveDeployTargets` (`pkg/engine/deploy.go`) so the #114 regression and the conformance harness exercise the exact code the deploy runs.
- **D2 error reasons** travel to the lease fan-out through `WithLeaseScopeReasons(ctx, …)`; the reason is appended to both the connectivity and the locked error of that node.
- **Former-member remediation.** `tako remove --server` only accepts environment members, so the docs point at making the node reachable (the deploy then stops its containers) or `tako state forget-node` for a destroyed node, instead of `tako remove --server old-1`.
- **1.6 harness.** The engine has no fake transport that can drive whole operations, so the "mutated implies leased" harness (`TestMutatedNodesAreLeased`) is a target-set conformance test: for deploy, run, rollback, project rollback, scale and promote it derives the lease set with the operation's own helper and the mutable set from the deployer (`RemoteBuilderCandidates`, workload targets, edge candidates), and fails when a mutable node is unleased. Reverting `remoteBuilderServers` to project-wide makes it fail.
- **1.7 audit.** backup, cleanup, env push, maintenance, live, certs and Postgres verification already resolve only environment servers; the audit added a target-list test and no code change.
- **Resolver location (2.2).** `resolveMeshAddresses` is `mesh.ResolveAddresses` in `pkg/mesh/addresses.go`, so the deployer and `tako domains hosts` share it. It also gives an enrolled node's platform `MeshIP` precedence (step 0), which matches how enrolled clusters already address nodes.
- **Lazy resolution for operations without mesh apply.** Placement apply and proxy-only reconciles never call `SetupTakodRuntime`. `meshHostIPForServer` resolves the members' addresses read-only from `GET /v1/status` the first time it needs one, so proxy upstreams always use the recorded addresses. It writes nothing.
- **Migration.** Nodes deployed by earlier versions already record `node.json` `mesh.address`, so the first deploy with this version keeps every existing address even where sorted positional order would differ (`TestMigrationPreservesRecordedMeshAddresses`, resolver table tests). A takod too old to report `node` in its status has no record; those members get the lowest free address in sorted order, which equals positional addressing for the sorted target lists most configs used.
- **Mesh members versus leases.** Mesh apply now reaches every schedulable member. Deploy (via the environment connectivity set), rollback, project rollback, scale and promote already lease all schedulable members, which `TestSubsetDeployLeasesEveryMeshMember` pins for deploy and scale, so no node is written without a lease.
- **2.4 test.** The engine test drives `preflightAndVerifyTakodRuntime` with a recording runtime and checks `run.go` never prepares the mesh. The request-level proof that verify sends no metadata or mesh request is the deployer test `TestVerifyTakodRuntimeSendsNoMeshRequests`.
- **2.5 `domains hosts`.** An enrolled node's platform `MeshIP` is used without contacting the node. Other proxy nodes are read over SSH; an unreachable node or a node without a record fails with a message pointing at `--address private|ssh`.
- **Maintenance lease endpoint (3.1).** takod serves it at `GET|POST|DELETE /v1/node-lease/maintenance` (listed with `/v1/lease` among lifecycle-safe paths). The v1 file is `{version, id, operation, who, pid, createdAt, expiresAt}`. A file that cannot be parsed or has another version fails closed for both lease kinds instead of being ignored. The maximum TTL is 2 hours; the CLI asks for 15 minutes.
- **Setup scope (3.2).** `tako setup` takes the lease on every node whose takod answers, not only in converge mode, because a reapply also reinstalls takod under running workloads. A node whose takod does not answer is set up or upgraded without a lease: no agent is running that could grant environment leases.
- **Upgrade exit code (3.2).** A refused lease marks that node `blocked` with the holders in `error`, the other non-enrolled nodes still run, and the command exits 3 (a `LockedError`), taking precedence over the partial-failure exit 6.
- **Test scope (3.2).** The engine tests use an in-memory agent whose state outlives the agent instance, which models the restart. The full `upgradeServerNode` flow needs SSH and a provisioner, so the cmd tests cover the lease helper, the fallback warning, the skipped-lease path and the exit-3 mapping, and the release is a `defer` placed right after acquisition.