# Spec Delta

## Purpose

Defines which nodes an operation fences with remote leases or otherwise contacts. An environment's operations depend only on its own nodes and on nodes that still hold its prior assignments. Node-global mutations exclude every environment that shares the node.

## ADDED Requirements

### Requirement: Deploy leases cover only nodes the deploy can mutate
In a configuration without enrolled servers, `tako deploy -e ENV` SHALL acquire remote leases on exactly these nodes:
- the servers of `ENV`;
- the configured servers that hold prior desired assignments for `ENV`.

A server outside `ENV` SHALL NOT be treated as an edge, a builder or an image source for `ENV`. Configurations with enrolled servers SHALL keep their single controller lease and inventory roles.

#### Scenario: Independent environments on separate servers
- **WHEN** `preview` uses `servers: [preview]`, `production` uses `servers: [production]`, the production host is unreachable, and the operator runs `tako deploy -e preview` with a proxied, build-backed service
- **THEN** the deploy acquires a lease only on `preview`, makes no connection to `production`, and succeeds

#### Scenario: Prior owner outside the environment
- **WHEN** server `old-1` was removed from `preview.servers`, is still defined in `servers`, and still holds preview's prior desired assignments
- **THEN** `old-1` is leased
- **AND** if `old-1` is unreachable, the deploy fails with an error stating that `old-1` holds prior desired assignments for `preview`

### Requirement: Builds and image reuse stay inside the environment
In a configuration without enrolled servers, remote image builds and retained-image lookups for `ENV` SHALL use only the schedulable servers of `ENV` as builders and image sources.

#### Scenario: Build never runs in another environment
- **WHEN** `staging` uses `servers: [staging]`, `production` uses `servers: [app-prod]`, and a staging deploy needs a remote build
- **THEN** the image is built on `staging` and no request is sent to `app-prod`

#### Scenario: Rollback while another environment is down
- **WHEN** `production` is unreachable and `tako rollback -e preview` reuses a retained image
- **THEN** the rollback locates the image only on preview servers and succeeds

### Requirement: Servers omitted by environment scoping are not silently skipped
When environment scoping omits a server because it references unset variables, and that server holds prior desired assignments for `ENV`, the operation SHALL fail before acquiring any lease. The error SHALL name the server and the unset variables.

#### Scenario: Pruned prior owner
- **WHEN** server `old-1` holds prior desired assignments for `preview`, its host is `${OLD_1_HOST}`, the variable is unset, and the operator runs `tako deploy -e preview`
- **THEN** the deploy exits with code 2 before any lease, naming `old-1` and `OLD_1_HOST`

### Requirement: Every mutated node is leased
For deploy, run, rollback (single-service and project-wide), scale and promote, every node that receives a mutating takod request SHALL be in that operation's lease set.

#### Scenario: Mutation outside the lease set
- **WHEN** a code path would send a mutating request to a node not in the lease set
- **THEN** the conformance test suite fails

### Requirement: Unreachable in-scope nodes still block
With `onUnreachableNode: block`, a lease acquisition failure on any node in the scoped set SHALL fail the operation before mutation and SHALL release all acquired leases. Nodes outside the scoped set SHALL NOT be contacted for leases.

#### Scenario: Environment member down
- **WHEN** an environment has servers `a` and `b`, and `b` is unreachable
- **THEN** `tako deploy -e ENV` fails with a connectivity error naming `b`, and no lease remains held on `a`

### Requirement: Node maintenance lease excludes environment operations
`tako upgrade servers` for non-enrolled nodes, and `tako setup` converging a node whose takod is running, SHALL hold an exclusive node maintenance lease on each node while they mutate it. While a node maintenance lease is live, environment leases on that node SHALL fail. A node maintenance lease SHALL NOT be granted while any environment lease of any project is live on that node.

#### Scenario: Upgrade during a deploy on a shared server
- **WHEN** `tako deploy -e preview` holds a lease on shared server `s1` and `tako upgrade servers` targets `s1`
- **THEN** the upgrade exits with code 3, naming the holder `preview`, and `s1` is not modified

#### Scenario: Deploy during an upgrade
- **WHEN** `tako upgrade servers` holds the node maintenance lease on `s1` and `tako deploy -e production` targets `s1`
- **THEN** the deploy fails before mutation with a locked error naming the maintenance operation

#### Scenario: Lease survives the agent restart
- **WHEN** an upgrade restarts takod on `s1` while it holds the node maintenance lease
- **THEN** the restarted takod still refuses environment leases until the upgrade releases the lease or it expires

#### Scenario: Older takod without node leases
- **WHEN** a node's takod lacks `lease.node-maintenance-v1`
- **THEN** the operation acquires the selected environment's lease on that node instead
- **AND** warns that other environments and projects on the node are not fenced

### Requirement: Environments sharing a server do not block each other
Environment leases SHALL remain keyed per project and environment. Deploys of two environments that share a server MAY run concurrently.

#### Scenario: Concurrent deploys on a shared server
- **WHEN** `preview` and `production` both include server `s1` and are deployed at the same time
- **THEN** neither deploy fails because of the other's lease
