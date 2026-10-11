# Spec Delta

## Purpose

Defines which nodes an operation fences with remote leases. An environment's operations must depend only on nodes they mutate, and node-global mutations must exclude every environment that shares the node.

## ADDED Requirements

### Requirement: Deploy leases cover only nodes the deploy mutates
`tako deploy -e ENV` SHALL acquire remote leases on exactly these nodes:
- the servers of `ENV`;
- nodes holding prior desired assignments for `ENV`;
- the environment's resolved proxy targets when any service is proxied;
- the builder selected by the build plan when a remote build is needed;
- the controller in enrolled mode.

A server that has no `roles` and is not a member of `ENV` SHALL NOT be treated as an edge or builder for `ENV`.

#### Scenario: Independent environments on separate servers
- **WHEN** `preview` uses `servers: [preview]`, `production` uses `servers: [production]`, the production host is unreachable, and the operator runs `tako deploy -e preview`
- **THEN** the deploy acquires a lease only on `preview` and succeeds without connecting to `production`

#### Scenario: Explicit proxy target outside the environment
- **WHEN** `environments.preview.proxy` places the proxy on server `edge1`, which is not in `preview.servers`
- **THEN** `edge1` is leased, and if it is unreachable the deploy fails with an error stating that `edge1` is a proxy target for `preview`

#### Scenario: Shared explicit builder
- **WHEN** server `builder1` declares `roles: [builder]`, is not in the environment, and the build plan selects it
- **THEN** `builder1` is leased; other builders that are not selected are not

### Requirement: Every mutated node is leased
For deploy, rollback (single-service and project-wide), scale and promote, every node that receives a mutating takod request SHALL be in that operation's lease set.

#### Scenario: Mutation outside the lease set
- **WHEN** a code path would send a mutating request to a node not in the lease set
- **THEN** the conformance test suite fails

### Requirement: Unreachable in-scope nodes still block
With `onUnreachableNode: block`, a lease acquisition failure on any node in the scoped set SHALL fail the operation before mutation and release all acquired leases. Nodes outside the scoped set SHALL NOT be contacted for leases.

#### Scenario: Environment member down
- **WHEN** an environment has servers `a` and `b`, and `b` is unreachable
- **THEN** `tako deploy -e ENV` fails with a connectivity error naming `b`, and no lease remains held on `a`

### Requirement: Node maintenance lease excludes environment operations
Operations that change node-global state SHALL hold an exclusive node maintenance lease on each node they touch. These operations are takod upgrade, server setup/provisioning, and node-global state repair. While a node maintenance lease is live, environment leases on that node SHALL fail. A node maintenance lease SHALL NOT be granted while any environment lease of any project is live on that node.

#### Scenario: Upgrade during a deploy on a shared server
- **WHEN** `tako deploy -e preview` holds a lease on shared server `s1`, and `tako upgrade` targets `s1`
- **THEN** the upgrade fails with a locked error naming the holder `preview`, and `s1` is not modified

#### Scenario: Deploy during an upgrade
- **WHEN** `tako upgrade` holds the node maintenance lease on `s1` and `tako deploy -e production` targets `s1`
- **THEN** the deploy fails before mutation with a locked error naming the maintenance operation

#### Scenario: Older takod without node leases
- **WHEN** a node's takod lacks `lease.node-maintenance-v1`
- **THEN** the node-global operation instead acquires the environment lease for every environment in the config that includes the node
- **AND** the operation warns that other projects on the node are not fenced

### Requirement: Environments sharing a server do not block each other
Environment leases SHALL remain keyed per project and environment. Deploys of two environments that share a server MAY run concurrently.

#### Scenario: Concurrent deploys on a shared server
- **WHEN** `preview` and `production` both include server `s1` and are deployed at the same time
- **THEN** neither deploy fails because of the other's lease
