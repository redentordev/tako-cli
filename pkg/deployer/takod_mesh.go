package deployer

import (
	"fmt"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/mesh"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
)

// meshMembers returns the nodes whose mesh an operation reconciles. Without
// enrolled servers this is every schedulable member of the environment,
// sorted, independent of the operation's workload targets (D4 of
// scope-leases-and-mesh-to-environment). Enrolled clusters keep the
// operation's targets because the platform owns their mesh.
func (d *Deployer) meshMembers() ([]string, error) {
	if d.config.HasEnrolledServers() {
		return d.getTakodTargetServers()
	}
	servers, err := d.config.GetEnvironmentServers(d.environment)
	if err != nil {
		return nil, err
	}
	members, err := config.ResolveSchedulableEnvironmentTargets(d.config.Servers, servers, d.environment)
	if err != nil {
		return nil, err
	}
	members = append([]string(nil), members...)
	sort.Strings(members)
	return members, nil
}

func (d *Deployer) storeMeshStatus(serverName string, status takod.Status) {
	d.meshMu.Lock()
	defer d.meshMu.Unlock()
	if d.meshStatuses == nil {
		d.meshStatuses = make(map[string]takod.Status)
	}
	d.meshStatuses[serverName] = status
}

func (d *Deployer) meshStatus(serverName string) (takod.Status, bool) {
	d.meshMu.Lock()
	defer d.meshMu.Unlock()
	status, ok := d.meshStatuses[serverName]
	return status, ok
}

// Validator defaults for mesh.networkCIDR and mesh.subnetBits.
const (
	defaultMeshNetworkCIDR = "10.210.0.0/16"
	defaultMeshSubnetBits  = 24
)

func (d *Deployer) meshNetwork() (string, int) {
	if d.config.Mesh == nil {
		return defaultMeshNetworkCIDR, defaultMeshSubnetBits
	}
	cidr := d.config.Mesh.NetworkCIDR
	if strings.TrimSpace(cidr) == "" {
		cidr = defaultMeshNetworkCIDR
	}
	bits := d.config.Mesh.SubnetBits
	if bits <= 0 {
		bits = defaultMeshSubnetBits
	}
	return cidr, bits
}

// resolveTakodMeshAddresses resolves every member's address from the status
// documents captured by verifyTakodAgents and records the result for proxy
// upstreams. publicKeys holds the members' current WireGuard keys.
func (d *Deployer) resolveTakodMeshAddresses(members []string, publicKeys map[string]string) (map[string]string, error) {
	records := make(map[string]mesh.MemberRecord, len(members))
	for _, name := range members {
		status, _ := d.meshStatus(name)
		record := memberRecordFromStatus(status, publicKeys[name])
		record.PlatformAddress = strings.TrimSpace(d.config.Servers[name].MeshIP)
		records[name] = record
	}
	cidr, bits := d.meshNetwork()
	resolution, err := mesh.ResolveAddresses(cidr, bits, members, records)
	if err != nil {
		return nil, err
	}
	for _, warning := range resolution.Warnings {
		d.emitEvent(events.Event{Type: events.TypeWarning, Phase: events.PhaseDeploy, Level: events.LevelWarn, Message: fmt.Sprintf("  ⚠ %s\n", warning)})
	}
	d.meshMu.Lock()
	d.meshAddresses = resolution.Addresses
	d.meshMu.Unlock()
	return resolution.Addresses, nil
}

// ensureMeshAddresses returns the operation's resolved member addresses. An
// operation that did not prepare the mesh (for example a placement movement
// or a proxy reconcile) resolves them read-only from the members' status, so
// it uses exactly the addresses mesh apply recorded.
func (d *Deployer) ensureMeshAddresses() (map[string]string, error) {
	d.meshMu.Lock()
	addresses := d.meshAddresses
	d.meshMu.Unlock()
	if addresses != nil {
		return addresses, nil
	}
	members, err := d.meshMembers()
	if err != nil {
		return nil, err
	}
	if err := runTakodNodeActions(members, func(serverName string) error {
		if _, captured := d.meshStatus(serverName); captured {
			return nil
		}
		status, err := d.readTakodStatus(serverName)
		if err != nil {
			return fmt.Errorf("read recorded mesh address: %w", err)
		}
		d.storeMeshStatus(serverName, status)
		return nil
	}); err != nil {
		return nil, err
	}
	return d.resolveTakodMeshAddresses(members, nil)
}

// memberRecordFromStatus converts one GET /v1/status response into the
// resolver's view of the member. On a node that keeps per-environment peer
// sets, peers.json holds only the last writer's peers, so the peers of every
// stored set are added: their addresses are in use on the interface.
//
// The node's own address comes from its peer sets when one of them peers
// with other nodes: takod renders the interface from those sets and requires
// them to agree on it, while node.json is rewritten by every metadata write
// (a co-located single-node project with a different mesh block, an older
// CLI, or an attempt the union rejected). Reading node.json there would make
// every environment on the node send an address the union rejects. A node
// without such a set keeps node.json, or else any set's address.
func memberRecordFromStatus(status takod.Status, publicKey string) mesh.MemberRecord {
	record := mesh.MemberRecordFromStatus(status.Node, status.Peers, publicKey)
	if address := unionNodeAddress(status.MeshPeerSets); address != "" {
		record.Recorded = address
	}
	for _, set := range status.MeshPeerSets {
		if record.Recorded == "" && strings.TrimSpace(set.Node.Address) != "" {
			record.Recorded = strings.TrimSpace(set.Node.Address)
		}
		for _, peer := range set.Peers {
			record.PeerAddresses = append(record.PeerAddresses, mesh.RecordedPeer{Name: peer.Name, Address: peer.Address, PublicKey: peer.PublicKey})
		}
	}
	return record
}

// unionNodeAddress returns the node address of the first stored peer set (in
// takod's sorted order) that has peers other than the node itself, which is
// the address takod's union render uses; "" when no set has other peers.
func unionNodeAddress(sets []takod.MeshPeerSetSummary) string {
	sorted := append([]takod.MeshPeerSetSummary(nil), sets...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Project+"/"+sorted[i].Environment < sorted[j].Project+"/"+sorted[j].Environment
	})
	for _, set := range sorted {
		address := strings.TrimSpace(set.Node.Address)
		if address == "" {
			continue
		}
		for _, peer := range set.Peers {
			if !isOwnPeerEntry(set.Node, peer) {
				return address
			}
		}
	}
	return ""
}

// isOwnPeerEntry mirrors takod's self filter for a stored set.
func isOwnPeerEntry(node, peer mesh.Node) bool {
	if key := strings.TrimSpace(node.PublicKey); key != "" && strings.TrimSpace(peer.PublicKey) == key {
		return true
	}
	return peer.Name == node.Name && meshAddressIP(peer.Address) == meshAddressIP(node.Address)
}

func meshAddressIP(address string) string {
	address = strings.TrimSpace(address)
	if index := strings.Index(address, "/"); index >= 0 {
		return address[:index]
	}
	return address
}

// meshPeerSetRequest is the PUT /v1/mesh/peer-sets body for one member.
func (d *Deployer) meshPeerSetRequest(node mesh.Node, peers []takodMeshPeer) takod.MeshPeerSetRequest {
	cidr, bits := d.meshNetwork()
	return takod.MeshPeerSetRequest{
		Project:     d.config.Project.Name,
		Environment: d.environment,
		NetworkCIDR: cidr,
		SubnetBits:  bits,
		Config:      d.wireGuardConfig(),
		Node:        node,
		Peers:       takodPeersToWireGuardNodes(peers),
	}
}

// memberSupportsMeshPeerSets reports whether the member's takod keeps one
// peer set per (project, environment). The choice is made per member, so a
// partially upgraded environment keeps working.
func (d *Deployer) memberSupportsMeshPeerSets(serverName string) bool {
	status, ok := d.meshStatus(serverName)
	return ok && statusHasCapability(status, takod.CapabilityMeshPeerSetsV1)
}

// warnLegacySharedMeshNodes warns once per member whose recorded node
// metadata belongs to another project or environment while its takod cannot
// keep one peer set per environment: the last deploy replaces its peers.
func (d *Deployer) warnLegacySharedMeshNodes(members []string) {
	project := d.config.Project.Name
	for _, name := range members {
		if strings.TrimSpace(d.config.Servers[name].ClusterID) != "" {
			continue
		}
		status, ok := d.meshStatus(name)
		if !ok || statusHasCapability(status, takod.CapabilityMeshPeerSetsV1) {
			continue
		}
		owner, environment := mesh.RecordedNodeOwner(status.Node)
		if owner == "" && environment == "" {
			continue
		}
		if owner == project && environment == d.environment {
			continue
		}
		d.emitEvent(events.Event{
			Type:    events.TypeWarning,
			Phase:   events.PhaseDeploy,
			Level:   events.LevelWarn,
			Node:    name,
			Message: fmt.Sprintf("  ⚠ server %s is shared with %s/%s; its mesh peers are replaced by whichever deploy runs last until takod is upgraded (run 'tako upgrade servers --server %s')\n", name, owner, environment, name),
			Data:    map[string]any{"server": name, "sharedWith": owner + "/" + environment},
		})
	}
}

// meshNodeClient returns the takod client for mesh key, metadata and mesh
// apply requests.
func (d *Deployer) meshNodeClient(serverName string) (any, error) {
	if d.meshClient != nil {
		return d.meshClient(serverName)
	}
	client, err := d.getRuntimeClient(serverName)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func statusHasCapability(status takod.Status, capability string) bool {
	for _, candidate := range status.Capabilities {
		if candidate == capability {
			return true
		}
	}
	return false
}
