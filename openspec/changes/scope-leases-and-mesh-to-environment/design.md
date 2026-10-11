# Design

## Context

See proposal.md for motivation and specs/deploy-leases/spec.md and specs/mesh-networking/spec.md for the behavior contract. Observed current state (pre-change citations):

- **Lease acquisition.** `AcquireRemoteOperationLeasesContext` (`pkg/engine/leases.go:99-164`):
  - If any server is enrolled (`ClusterID`/`NodeID`), it takes one controller lease (`:205-298`).
  - Otherwise it takes a per-node lease through `remotestate...AcquireLeaseContext(ctx, op, envName, DefaultLeaseTTL)` (`:133-134`), in parallel with no quorum (`AcquireRemoteOperationLeasesWithContext`, `:467-561`). Any failure releases everything and returns `failed to acquire remote %s leases` as `LockedError`/`ConnectivityError`.
  - The node lease file is `<dataDir>/leases/<project>/<env>.json` (`pkg/takod/lease.go:337-342`), so leases are already per `(project, env)`.
  - `deployConsistency` accepts only `lease` and `onUnreachableNode` only `block` (`pkg/config/types.go:141-144`, `validator.go:402-424`).
- **Deploy lease set** (`pkg/engine/deploy.go:390-421`). It is `DeployMutationTargets(cfg, allServices, assignments)` ∪ `AddPriorDesiredMutationTargets` ∪ `connectivityServerNames` (the environment's servers, `:265`). `DeployMutationTargets` (`takod.go:93-125`) adds every `cfg.Servers` edge when any service is proxied, every builder through `DeployLeaseTargets` (`:59-87`), and the controller. `HasPlatformRole` (`types.go:293-303`) is true for role-less servers. **This is the #114 bug.** Setup targets are already intersected with environment servers (`deploy.go:395-408`), so the over-reach affects leases and connectivity, not workload placement.
- **Other operations** already lease environment servers only: rollback `rollback.go:158-183`, project rollback `rollback_project.go:276-303`, destroy `destroy.go:269`/`:357`, scale `scale.go:141`/`:477`, promote `promote.go:100/136`, remove `remove.go:119/238`, placement-apply `placement_apply.go:62/100`. `run` leases the single requested server (`run.go:134`). Command-level leases go through `cmd/remote_lease.go:19-22` (backup, cleanup, env push, maintenance, live, certs); each builds its own target list. `cmd/upgrade.go` iterates `cfg.Servers` (`:291`, `:312`, `:352`, `:418`) and takes **no lease**.
- **Replicated state** is per environment: `getReplicaServers` uses `GetEnvironmentServers(env)` (`internal/state/replicator.go:225-241`); node paths are `/var/lib/tako/{desired,actual}/<project>/<env>` (`pkg/takod/cleanup.go:518-530`).
- **Mesh.**
  - `SetupTakodRuntime` (`pkg/deployer/takod_deploy.go:102-138`) runs on `getTakodTargetServers` (`:2624-2639`, the setup targets).
  - It fetches keys (`ensureTakodMeshKeys`, `:2473`), builds peers with positional addresses (`buildTakodMeshPeers` `:2547-2569`, `meshAddress(i)` `:2594-2608`; `meshAddressForServer` honours only the internal platform `MeshIP`, `:2610-2622`, `types.go:279`), and then writes `/v1/metadata` and `/v1/mesh/apply` per node (`prepareTakodNode`, `:1470-1539`).
  - Each node keeps a single `<dataDir>/node.json` and `<dataDir>/mesh/peers.json` (`pkg/takod/metadata.go:34,41`), tagged with Project/Environment.
  - Any unreachable member fails the step (`:2528-2543`, `takodNodeActionsError` `:1443-1467`).
  - No migration logic exists.
- **Environments** select servers with `servers` or `serverSelector` (`GetEnvironmentServers`, `types.go:944-982`). Two environments may list the same server. `environments.<e>.proxy` may place the proxy on specific servers (`ResolveEnvironmentProxyTargets`, `types.go:987-1013`). Imports (`ServiceConfig.Imports`, `:400`) only read other projects' same-environment services and never mutate them.

## Goals / Non-Goals

**Goals:**
- An environment's operations need connectivity only to nodes they mutate.
- No reduction in safety: any node an operation mutates is leased. Node-global mutations exclude every environment on that node.
- Shared servers keep working for both environments' mesh traffic.
- Existing meshes keep their addresses.

**Non-Goals:**
- Partial deploys that skip unreachable members of the selected environment. Members of the environment remain mandatory.
- Changes to enrolled/controller mode.

## Decisions

### D1. Lease set = resolved mutation set
`DeployMutationTargets` is rewritten to take the environment's server list and the resolved plan:

- `assigned workers` ∪ `prior desired owners` ∪ `environment servers` (authority/alert fan-out, unchanged);
- ∪ `ResolveEnvironmentProxyTargets(env)` when any service is proxied. This replaces "every edge in the file". Servers outside the environment appear only if the environment's `proxy` placement names them explicitly.
- ∪ `selected builder`, when the build plan uses a remote builder. Builder selection runs before leasing and returns one node. Role-less servers are builder candidates only if they are environment members. Explicit `roles: [builder]` servers stay candidates project-wide, because an explicit role is a deliberate shared-builder designation.
- ∪ `controller`, when enrolled (unchanged).

`DeployLeaseTargets` callers get the same treatment. A table test pins the #114 configuration: the preview lease set is `{preview}`.

*Alternative rejected:* keep the wide set and tolerate unreachable nodes outside the environment. A node that is leased but skipped is not protected, and a node that is not mutated does not need a lease. Precise scoping is both safer and simpler.

### D2. Unreachable nodes
`onUnreachableNode: block` keeps its all-or-nothing meaning over the scoped set. If a node outside the environment is in the set (explicit proxy target or selected builder) and is unreachable, the deploy fails as today. The error says why that node is in scope ("proxy target for environment preview" or "selected builder").

### D3. Node maintenance lease
takod gains a second lease kind, `node`, stored at `<dataDir>/leases/_node.json` and gated by capability `lease.node-maintenance-v1`:
- An environment lease acquisition fails with `LockedError` while a live node lease exists. A node lease acquisition fails while any live environment lease exists on the node, for any project.
- Holders are `tako upgrade`, server setup/provision, `state forget-node`/repair operations that rewrite node-global files, and a future mesh re-key.
- Fallback for agents without the capability: acquire the environment lease for every `(project, env)` in the current config whose servers include the node. This is weaker, since other projects are not covered, and Tako prints a warning recommending a takod upgrade. `tako upgrade` of takod itself necessarily runs against an older agent, so the fallback is the normal path for the first upgrade.

This keeps two environments that share a server safe from node-wide mutation even though environment leases on that node do not conflict with each other. They never did (`lease.go:337-342`), and their mutable state is disjoint per `(project, env)`. The node-global resources they share are the WireGuard interface (made union-safe by D5), the merged Caddyfile (already per-`(project, env)` files, serialized by takod's render mutex) and the Docker daemon.

### D4. Mesh topology: per-environment membership, per-node interface
Options considered:
1. **Project mesh, tolerate unreachable peers.** Rejected. Preview nodes would peer with production nodes (no isolation), every deploy would still contact every node for keys, and "tolerate" would silently drop peers.
2. **Separate interface and CIDR per environment.** Rejected. It needs N interfaces and ports on shared servers, routing overlap, and a firewall rule per interface. It is heavy for little gain.
3. **Chosen: membership per `(project, env)`, one interface per node, union on shared nodes.** A preview-only node's `AllowedIPs` lists only preview peers, so it cannot reach production-only nodes. A shared server peers with both, which is inherent to sharing a host.

The deployer sends `PUT /v1/metadata` with a peer set keyed by `(project, env)`. takod stores `mesh/peer-sets/<project>/<env>.json` and renders WireGuard from the union of all sets (deduplicated by public key). Destroying an environment removes its set (`destroy` already leases the environment's nodes). The mesh step contacts only setup targets, which is unchanged and already environment-scoped.

### D5. Stable, conflict-checked addressing
Address resolution for each peer:
1. A pinned address: the platform `MeshIP`, or a new optional `servers.<name>.meshIP` (validated inside `mesh.networkCIDR`, unique across servers).
2. Otherwise the address recorded on the node. It is returned by the existing key-fetch call (`ensureTakodMeshKeys`), which already contacts each member.
3. Otherwise a deterministic allocation:
   - Sort all `cfg.Servers` names; collect pinned and recorded addresses known for this operation.
   - For each unpinned name in order, take `hash(name) mod hostSpace`, then probe linearly to the first free host.
   - The computation is config-only, so preview and production agree on addresses without contacting each other.

Positional `meshAddress(i)` is removed. takod validates the union before applying it: one address held by two public keys (or one key holding two addresses) is rejected, and the error names both `(project, env, server)` entries and suggests `meshIP`. Nothing is applied.

### D6. Migration
- **takod side:** on startup, or on the first `mesh.peer-sets-v1` request, an existing `mesh/peers.json` becomes `peer-sets/<Project>/<Environment>.json` using its tags. If the tags are missing, it becomes a `_legacy` set that is kept until the next metadata write from any environment containing that node replaces it. The node's current address is recorded in `node.json`, so step 2 of D5 preserves it.
- **CLI side:** if any target node lacks `mesh.peer-sets-v1`, the old single-file path is used. If the node is shared by several environments of the config, Tako emits a warning that the last deploy wins until takod is upgraded.
- Existing two-environment meshes on shared servers may already have colliding positional addresses. The D5 conflict check surfaces them on the first deploy after upgrade, with the fix (`meshIP` or redeploy order) in the message. `tako doctor` reports the same check read-only.

## Risks / Trade-offs

- **[Under-leasing: a mutated node is missing from the set]** → Mitigation: lease computation and mutation must come from the same resolved plan. A test hooks the deployer's node-call recorder and asserts every node receiving a mutating takod request was leased (applied to deploy, rollback, scale, promote).
- **[Builder selection moves before leasing]** → It is a pure function of config and node labels. If it needs reachability probes, the probe is limited to candidates in scope.
- **[Node lease starvation: continuous deploys block upgrade]** → The node lease waits up to the existing acquire timeout (TTL/4) and then fails with the holder's details. Upgrade is an operator action and can be retried.
- **[Hash allocation collisions on small CIDRs]** → Linear probing is deterministic. The /24 default holds 254 nodes, and validation rejects configs with more servers than host space.
- **[Hash probing is order-dependent: a newly added server can claim the home slot of a not-yet-recorded server]** → Recorded addresses (D5 step 2) win once a node has joined any mesh, the takod union conflict check fails closed on any disagreement, and `meshIP` pins remove the ambiguity entirely.
- **[Mixed takod versions]** → Capability fallbacks (D3, D6) keep today's behavior with explicit warnings rather than failing.

## Migration Plan

Upgrade the CLI first. Leases are scoped immediately (CLI-only). Then run `tako upgrade` for takod. The first deploy per environment after that migrates mesh files in place (D6). No config edits are required. `meshIP` is needed only if the conflict check reports an existing collision. Rollback: an older CLI writes the legacy single file, which a new takod treats as a full replacement of that environment's set. This is safe but loses the union until the new CLI deploys again.

## Open Questions

1. **Split the mesh per environment, or keep a project mesh?** Recommended: split membership per `(project, env)` with a per-node interface union (D4). It gives isolation without extra interfaces.
2. **Should explicit `roles: [builder]`/`[edge]` servers outside the environment still be candidates?** Recommended: builders yes, because an explicit role is deliberate sharing. Edges only through the environment's `proxy` placement.
3. **Add a user-facing `servers.<name>.meshIP`?** Recommended: yes, optional. It is the escape hatch for conflicts found by D5. The default stays automatic.
4. **Should the node maintenance lease also cover `tako cleanup`/`backup`?** Recommended: no. They operate on `(project, env)` resources and keep the environment lease. Only node-global writers take the node lease.
5. **Ship leases and mesh separately?** Recommended: yes, as two PRs. D1-D2 (CLI-only) fix #114's blocking error immediately. D3-D6 need takod capabilities and follow.
6. **Reproduce the "both hosts end up in one mesh" symptom?** Current master already builds peers from environment setup targets (`deploy.go:395-408`). The symptom is most likely positional address overlap or the last-writer-wins `peers.json` on shared nodes, or pre-v0.14 behavior. Recommended: add the regression test in tasks 3.1 before changing code.
