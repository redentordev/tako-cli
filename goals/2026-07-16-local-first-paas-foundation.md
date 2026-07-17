# Goal: Local-First PaaS Foundation

Date: 2026-07-16
Status: in progress — Phases 1 through 3 are complete and adversarially
reviewed; Phase 4 is next.

## Objective

Make Tako a safe foundation for a PaaS installed on its first server. The
first node starts as the single control plane and normally also serves as a
worker, edge, and builder. Routine operations on that node use takod directly
over its Unix socket. Additional nodes can be enrolled as workers without
changing the application-facing workflow or moving existing workloads
implicitly.

The completed system must preserve legacy SSH configurations while adding:

- immutable cluster and node identity;
- identity-verified direct local takod access;
- separate runtime, provisioning, and image transport boundaries;
- explicit control-plane, worker, edge, and builder roles;
- protected first-node resources and durable operations;
- platform-owned cluster inventory and authenticated enrollment;
- sticky placement plus cordon, drain, rebalance, and removal;
- single-controller lease authority, target fencing, recovery, and backups;
- staged node upgrades independent from application deployment.

## Locked architecture decisions

1. **Transport, placement, and control authority are independent.** Running
   the CLI or PaaS worker on a node never makes that node a workload target by
   itself.
2. **IP equality is discovery, not identity.** Automatic local selection
   requires the local takod socket to report the configured immutable
   `clusterID` and `nodeID`. DNS, NAT, VIPs, interface aliases, and SSH failure
   cannot authorize local execution.
3. **Routine runtime work is structured takod traffic.** Local deployments do
   not gain a generic shell. An `AgentClient` reaches takod through either a
   direct Unix socket or an SSH-forwarded Unix socket. A separate privileged
   provisioning boundary is used only by explicit setup, join, repair, and
   upgrade operations.
4. **Legacy configurations remain SSH-first.** Identity-verified automatic
   local transport is enabled by platform enrollment, not retroactively for
   every existing `host` value.
5. **The first release is honestly single-controller.** Node 1 is the sole
   mutation authority. Workloads continue when it is down, but deploy, scale,
   and administrative mutations stop. Recovery copies are not described as
   active-active consensus.
6. **Node 1 is explicitly multi-role.** It begins as
   `control-plane + worker + edge + builder`; any role can move later without
   changing node identity. The create-once installation document records
   enrollment roles as audit facts; mutable current role assignments live in
   control-plane state and are reported separately.
7. **Placement is persisted and sticky.** Joining or reordering nodes does not
   move workloads. Drain and rebalance are explicit planned operations.
8. **Application deploys do not upgrade nodes.** Agent upgrades are separate,
   capability-checked, staged worker-first, and rollback-capable.
9. **The takod socket is a host-control boundary.** Only a dedicated trusted
   PaaS worker identity receives access. Tenant workloads never receive the
   socket or management resource access. The access group may connect to the
   socket but cannot mutate its parent directory, and direct-local clients
   verify the operating-system peer UID before trusting status.

## Target architecture

```text
PaaS API/UI
    |
    v
durable deployment worker on node 1
    |
    +-- AgentClient -- local Unix socket ----------> takod@node-1
    |
    +-- AgentClient -- SSH direct-streamlocal ----> takod@worker-N

control authority: node 1
placement authority: persisted desired assignments
node inventory: platform-owned cluster membership
```

The runtime client owns typed HTTP, streamed bodies, and upgraded PTY
connections. The provisioning client owns explicit host administration. The
image transport owns digest/platform-aware reuse and node-to-node transfer.

## Milestones

### Phase 1 — Installation identity and agent transport

Progress: complete. Both mandatory reviewers closed their architecture and
security findings after socket peer authentication, descriptor-relative
identity-file hardening, defensive resolver validation, bounded status probes,
and real SSH streamlocal integration coverage were added.

- Add a root-owned installation identity document with schema version,
  cluster ID, node ID, logical name, roles, and creation time.
- Creation is exclusive; normal metadata APIs cannot write or replace it.
- Expose validated installation identity through takod status separately from
  mutable project/environment metadata.
- Add a structured `AgentClient` over a generic Unix-socket dialer.
- Provide direct-local and SSH-forwarded dialers.
- Add identity matching and transport-decision types with explicit evidence.
- Preserve existing SSH request paths until consumers migrate deliberately.

Acceptance:

- A local socket is selected only when both immutable IDs match.
- Wrong, missing, or malformed identity never authorizes local execution.
- Existing SSH-only configurations behave as before.
- Routine structured requests work over both local and SSH socket dialers.
- Identity creation is atomic, mode-restricted, and cannot overwrite an
  enrolled identity.

### Phase 2 — First-node bootstrap

Progress: complete. Both mandatory reviewers closed their architecture,
correctness, security, operations, and failure-mode findings after bootstrap
resume hardening, protected binary/account validation, durable operation
recovery, resource admission, background-work admission, bounded journal
recovery, systemd write-root protection, and stalled-upload handling were
added. The established `tako` socket group remains temporarily so existing
SSH deployments are not cut off before Phase 3 migrates all operation
producers to the durable worker. Phase 3 must introduce protected worker
ingress and only then revoke direct legacy socket access.

- Add an explicit platform-init workflow that creates the cluster and node
  identities and enrolls node 1 as control-plane, worker, edge, and builder.
- Run the deployment worker as a protected service with a durable operation
  journal and dedicated UID.
- Add socket authorization, audit records, protected management resources,
  controller reservations, build concurrency limits, and free-disk admission.
- Remove implicit agent installation/upgrades from application deployment.

### Phase 3 — Local runtime and image path

Progress: complete. Both mandatory reviewers closed their architecture,
correctness, security, operations, and failure-ordering findings after the
structured runtime migration, protected worker ingress, durable streamed
operations, immutable image attestation/transfer, fail-closed enrolled proxy
policy, and pre-mutation remote-route capability boundary were added. Local
runtime-alias proxy routes are enabled. Remote mesh routes remain explicitly
disabled on enrolled nodes until Phase 4 provides authoritative active
allocation generations and revocations.

- Migrate deploy, status, logs, exec, scale, rollback, jobs, state, backup,
  removal, leases, and reconciliation to `AgentClient`.
- Keep setup, join, repair, and upgrade behind the provisioning boundary.
- Reuse a local image only after digest, platform, and Docker-daemon identity
  agree; never treat a matching tag as sufficient.
- Transfer images only to remote targets that lack the exact digest.
- Bind every proxy upstream and dynamic-domain authorization destination to a
  project/environment service identity. Local destinations must match a Tako
  runtime alias. Enrolled nodes reject all remote IP/port destinations until
  Phase 4 can prove current allocation and inventory state; legacy un-enrolled
  SSH configurations retain version-1 compatibility. Reject loopback,
  link-local, metadata, userinfo, unrelated-service, and unproven raw-IP
  destinations.

### Phase 4 — Cluster inventory and node lifecycle

- Move node membership and credentials out of individual application intent.
- Add expiring single-use join tokens bound to cluster and expected node IDs.
- Pin SSH host keys during enrollment.
- Implement `joining -> ready -> schedulable -> cordoned -> draining -> removed`.
- Join workers unschedulable and without edge/builder roles.
- Revoke mesh credentials and retain node-ID tombstones on removal.
- Refuse removal of the final controller.

### Phase 5 — Sticky scheduling and explicit movement

- Persist replica-to-node assignments in desired state.
- Keep the initial singleton on the configured default worker.
- Scaling adds replicas without moving healthy assignments.
- Joining or reordering nodes causes no implicit rebalance.
- Add reviewed cordon, drain, rebalance, and persistent-volume movement plans.

### Phase 6 — Control authority, fencing, and recovery

- Make the control node authoritative for project/environment operation IDs,
  membership generations, and mutation leases.
- Require target-node fencing for every mutated node.
- Do not let an unavailable non-target worker block a local-only operation.
- Fail closed for unavailable targets and prevent two partitioned writers.
- Persist idempotent operation phases and reconcile incomplete operations.
- Back up membership, journal, PaaS data, encryption keys, environment bundles,
  certificate state, and persistent workload data outside node 1.

### Phase 7 — Upgrades and HA evolution

- Separate application and node lifecycle commands.
- Support N/N-1 capability compatibility and worker-first canary upgrades.
- Upgrade the controller last with atomic replacement and automatic rollback.
- Add protected self-upgrade handoff for the PaaS deployment worker.
- Prove passive controller promotion before proposing active-active control.

## Cross-cutting verification

Every phase requires unit and integration coverage, machine-interface goldens,
Linux and Windows builds, and the repository race and quality gates.

The milestone workflow is mandatory until every phase in this goal is
complete:

1. Implement one milestone on its dedicated branch and verify its intended
   contract.
2. Spawn two independent adversarial reviewer agents against the complete
   milestone diff. Give one architecture/correctness focus and the other a
   security/operations/failure focus.
3. Consolidate both reviews, inspect every finding against the code, and fix
   every valid issue before proceeding.
4. Re-run the milestone's targeted tests and full applicable quality gates
   after the fixes.
5. Commit the reviewed milestone with no unrelated worktree changes.
6. Only then start the next milestone. Repeat this workflow until all phases
   and final end-to-end acceptance scenarios are complete.

End-to-end scenarios must include identity forgery, NAT/VIP/container
namespaces, join-token replay, host-key changes, partitions, controller crashes
at each operation phase, resource exhaustion, interrupted upgrades, and
complete first-node recovery.

## Explicit non-goals

- No public takod TCP listener.
- No local fallback after SSH failure.
- No workload placement based on the caller's location.
- No tenant access to the takod socket.
- No active-active or quorum claims without a real consensus design.
- No automatic movement of persistent workloads.
