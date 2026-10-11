# Spec Delta

## Purpose

Defines WireGuard mesh membership and addressing for Tako-managed nodes. Membership is scoped per project and environment, addresses are stable, and servers shared between environments carry the union of their environments' peers.

## ADDED Requirements

### Requirement: Mesh membership is scoped to the environment
A deploy of environment `ENV` SHALL contact, and configure as mesh peers, only the servers that `ENV` targets. Nodes that belong only to different environments SHALL NOT be configured as peers of each other.

#### Scenario: Separate environments never peer
- **WHEN** `preview` uses `servers: [preview]` and `production` uses `servers: [production]`, and both are deployed
- **THEN** neither node lists the other as a WireGuard peer

#### Scenario: Preview deploy does not contact production
- **WHEN** `tako deploy -e preview` runs while the production host is unreachable
- **THEN** mesh preparation succeeds without attempting a connection to production

### Requirement: Shared servers use the union of environment peer sets
takod SHALL store one peer set per project and environment, and SHALL configure its WireGuard interface from the union of all stored sets. Deploying one environment SHALL NOT remove peers that another environment contributed.

#### Scenario: Two environments share a server
- **WHEN** `preview: [a, s]` and `production: [b, s]` are deployed in either order
- **THEN** `s` peers with `a` and `b`, `a` does not peer with `b`, and redeploying `preview` leaves `s`'s peer `b` in place

#### Scenario: Environment destroyed
- **WHEN** `tako destroy -e preview` completes
- **THEN** the preview peer set is removed from its nodes, and `s` keeps its production peers

### Requirement: Mesh addresses are stable and conflict-checked
A node's mesh address SHALL be resolved in this order:
1. a pinned address (`servers.<name>.meshIP` or the platform-assigned address);
2. the address already recorded on the node;
3. a deterministic config-only allocation that does not depend on which servers the current operation targets.

takod SHALL refuse to apply a union in which one address maps to more than one public key, or one key to more than one address. When it refuses, it SHALL report the conflicting entries and change nothing.

#### Scenario: Narrow operation does not renumber
- **WHEN** an environment has servers `a`, `b` and `c`, and an operation targets only `c`
- **THEN** `c` keeps the same mesh address it had after a full deploy

#### Scenario: Shared server address agrees across environments
- **WHEN** `preview` and `production` both compute the address of shared server `s`
- **THEN** both compute the same address

#### Scenario: Pre-existing collision
- **WHEN** an upgraded node's union would contain the same address for `a` and `b`
- **THEN** mesh apply fails, naming both peers and their environments and suggesting `meshIP`
- **AND** the node's existing WireGuard configuration is left unchanged

### Requirement: Existing meshes migrate without renumbering
When takod supporting `mesh.peer-sets-v1` finds a legacy single peers file, it SHALL convert the file into a peer set keyed by the project and environment recorded in it, and SHALL keep the node's current address. When a target node lacks `mesh.peer-sets-v1`, the CLI SHALL use the legacy behavior. In that case, if the node is shared by several environments of the config, the CLI SHALL emit a warning that the last deploy replaces the node's peers.

#### Scenario: Upgrade of an existing single-environment mesh
- **WHEN** takod is upgraded on a node that has a legacy peers file tagged `myapp/production`
- **THEN** the file becomes the `myapp/production` peer set
- **AND** the next deploy keeps every node's address

#### Scenario: Old agent on a shared server
- **WHEN** a shared server's takod lacks `mesh.peer-sets-v1`
- **THEN** the deploy proceeds with a warning recommending `tako upgrade`
