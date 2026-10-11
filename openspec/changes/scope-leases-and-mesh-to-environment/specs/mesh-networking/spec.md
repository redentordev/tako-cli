# Spec Delta

## Purpose

Defines WireGuard mesh membership and addressing for non-enrolled Tako nodes:
- Membership is scoped per project and environment.
- Addresses are stable across operations.
- Narrow operations leave the mesh alone.
- Servers shared between environments or projects carry the union of their peer sets.

## ADDED Requirements

### Requirement: Mesh peers are the environment's members
Every mesh apply for environment `ENV` SHALL list all schedulable servers of `ENV` as peers, whatever subset of those servers the operation mutates. Nodes that belong only to different environments SHALL NOT be configured as peers of each other.

#### Scenario: Separate environments never peer
- **WHEN** `preview` uses `servers: [preview]`, `production` uses `servers: [production]`, and both are deployed
- **THEN** neither node lists the other as a WireGuard peer

#### Scenario: Subset deploy keeps the full peer list
- **WHEN** an environment has servers `a`, `b` and `c`, and a deploy's workloads are assigned only to `c`
- **THEN** every node's mesh configuration still lists `a`, `b` and `c`

### Requirement: Narrow operations do not rewrite the mesh
`tako run` SHALL NOT change any node's mesh address, mesh peers or takod node metadata.

#### Scenario: One-off run on a multi-node environment
- **WHEN** an environment has servers `a`, `b` and `c`, and the operator runs `tako run` on `b`
- **THEN** `b` keeps its mesh address and its peers `a` and `c`

### Requirement: Mesh addresses come from recorded state
A node SHALL keep the mesh address recorded in its takod node metadata when that address lies inside `mesh.networkCIDR` and no member whose name sorts earlier holds the same address. Any other node SHALL get the lowest free host address in `mesh.networkCIDR`. A free address is one that no member records for itself or lists in its peers. Unrecorded nodes SHALL be assigned in sorted name order.

#### Scenario: Existing mesh keeps its addresses
- **WHEN** nodes already record mesh addresses from an earlier Tako version and the environment is deployed with this version
- **THEN** every node keeps its recorded address

#### Scenario: Duplicate address repaired
- **WHEN** members `a` and `b` both record the same mesh address
- **THEN** `a` keeps it, `b` receives the lowest free address, both nodes are re-applied in the same operation, and a warning names both nodes

### Requirement: Mesh addresses do not depend on the operation
A resolved mesh address SHALL NOT depend on any of these:
- the operation's target list;
- the order of servers in the configuration;
- servers outside the environment, including servers omitted by environment scoping.

Mesh apply, proxy upstream addresses and `tako domains hosts --address mesh` SHALL use the same resolved addresses.

#### Scenario: Declaration order does not swap addresses
- **WHEN** an environment declares `servers: [b, a]`, is deployed, and is then scaled
- **THEN** `a` and `b` keep the addresses the deploy gave them

#### Scenario: Pruned servers do not shift addresses
- **WHEN** `tako deploy -e preview` runs once with every server's host variable set, and once with the production host variables unset so that environment scoping omits those servers
- **THEN** both runs resolve the same addresses for every preview node

#### Scenario: Shared server address agrees across environments
- **WHEN** `preview` and `production` both include server `s`, and `s` records a mesh address
- **THEN** both environments resolve that same address for `s`

#### Scenario: Hosts file matches the mesh
- **WHEN** the operator runs `tako domains hosts -e production --address mesh`
- **THEN** the printed addresses equal the proxy nodes' recorded mesh addresses
- **AND** an unreachable proxy node produces an error instead of a guessed address

### Requirement: Shared servers use the union of peer sets
takod advertising `mesh.peer-sets-v1` SHALL store one peer set per project and environment, and SHALL configure its WireGuard interface from the union of all stored sets. A deploy of one environment SHALL NOT remove peers that another environment or project contributed. Destroying an environment SHALL remove its set.

#### Scenario: Two environments share a server
- **WHEN** `preview: [a, s]` and `production: [b, s]` are deployed in either order
- **THEN** `s` peers with `a` and `b`, `a` does not peer with `b`, and redeploying `preview` leaves `s`'s peer `b` in place

#### Scenario: Environment destroyed
- **WHEN** `tako destroy -e preview` completes
- **THEN** the preview peer set is removed from its nodes, and `s` keeps its production peers

#### Scenario: Single-node projects with different mesh settings share a node
- **WHEN** two single-server projects with different `mesh:` blocks deploy to the same server
- **THEN** both deploys succeed, and neither adds peers to the node

### Requirement: Peer-set conflicts fail closed
takod SHALL refuse a peer set whose union with the stored sets would do any of the following:
- map one address to more than one public key;
- map one public key to more than one address;
- combine sets that have peers but disagree on network CIDR, subnet bits, listen port, or the node's own address.

When it refuses, takod SHALL report the conflicting project, environment and server entries, and SHALL leave both the stored sets and the interface unchanged.

#### Scenario: Pre-existing collision
- **WHEN** a node's union would contain the same address for `a` (preview) and `b` (production)
- **THEN** the mesh apply fails, naming both peers and their environments
- **AND** the node's stored peer sets and WireGuard configuration are unchanged

### Requirement: Existing meshes migrate and old components interoperate
When takod advertising `mesh.peer-sets-v1` finds a legacy peers file, it SHALL convert the file into the peer set named by the project and environment recorded in it. It SHALL key a legacy mesh apply the same way. When a target node lacks `mesh.peer-sets-v1`, the CLI SHALL use the legacy behavior. If that node's recorded metadata belongs to another project or environment, the CLI SHALL also warn that the last deploy replaces the node's peers.

#### Scenario: Upgrade of an existing mesh
- **WHEN** takod is upgraded on a node whose legacy peers file is tagged `myapp/production`
- **THEN** the file becomes the `myapp/production` peer set
- **AND** the next deploy keeps every node's address

#### Scenario: Older CLI against a new takod
- **WHEN** an older CLI deploys `preview` to shared server `s`, which already stores a `production` peer set
- **THEN** `s` keeps the production peers and adds the preview peers

#### Scenario: Old agent on a shared server
- **WHEN** a shared server's takod lacks `mesh.peer-sets-v1`
- **THEN** the deploy proceeds with a warning recommending `tako upgrade servers`
