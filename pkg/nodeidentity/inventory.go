package nodeidentity

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	cryptossh "golang.org/x/crypto/ssh"
)

const (
	InventoryAPIVersion  = "tako.io/v1"
	InventoryKind        = "ClusterInventory"
	DefaultInventoryPath = "/etc/tako/cluster-inventory.json"
	// DeploymentDenyFile is a durable, node-local safety latch. Lifecycle
	// commands set it on the target before committing a transition away from
	// schedulable, so a controller crash cannot leave a stale worker accepting
	// workload mutations indefinitely.
	DeploymentDenyFile   = "deployment-denied"
	maxInventoryFileSize = 8 << 20

	NodeLifecycleJoining     = "joining"
	NodeLifecycleReady       = "ready"
	NodeLifecycleSchedulable = "schedulable"
	NodeLifecycleCordoned    = "cordoned"
	NodeLifecycleDraining    = "draining"

	MeshCredentialActive  = "active"
	MeshCredentialRevoked = "revoked"
)

func DeploymentDenyPath(inventoryPath string) string {
	return filepath.Join(filepath.Dir(inventoryPath), DeploymentDenyFile)
}

func requiresRootPublishedOwner(path string) bool {
	clean := filepath.Clean(path)
	return clean == filepath.Clean(DefaultInventoryPath) || clean == filepath.Clean(DefaultLocalBindingPath)
}

func publishedOwnerAllowed(path string, ownerUID, effectiveUID uint32) bool {
	if requiresRootPublishedOwner(path) {
		return ownerUID == 0
	}
	return ownerUID == 0 || ownerUID == effectiveUID
}

var activeNodeLifecycles = map[string]struct{}{
	NodeLifecycleJoining: {}, NodeLifecycleReady: {}, NodeLifecycleSchedulable: {},
	NodeLifecycleCordoned: {}, NodeLifecycleDraining: {},
}

// ClusterInventory is a controller-published, read-only trust snapshot. Join
// token material and private credentials never appear here.
type ClusterInventory struct {
	APIVersion        string             `json:"apiVersion"`
	Kind              string             `json:"kind"`
	ClusterID         string             `json:"clusterId"`
	Generation        uint64             `json:"generation,omitempty"`
	ControllerNodeID  string             `json:"controllerNodeId,omitempty"`
	MeshCIDR          string             `json:"meshCidr,omitempty"`
	UpdatedAt         time.Time          `json:"updatedAt,omitempty"`
	Nodes             []InventoryNode    `json:"nodes"`
	Tombstones        []NodeTombstone    `json:"tombstones,omitempty"`
	ActiveAllocations []ActiveAllocation `json:"activeAllocations,omitempty"`
}

type InventoryNode struct {
	NodeID                string    `json:"nodeId"`
	NodeName              string    `json:"nodeName,omitempty"`
	Lifecycle             string    `json:"lifecycle,omitempty"`
	Roles                 []string  `json:"roles,omitempty"`
	Schedulable           bool      `json:"schedulable"`
	MeshIP                string    `json:"meshIp,omitempty"`
	MeshEndpoint          string    `json:"meshEndpoint,omitempty"`
	MeshCredentialID      string    `json:"meshCredentialId,omitempty"`
	MeshPublicKey         string    `json:"meshPublicKey,omitempty"`
	MeshCredentialStatus  string    `json:"meshCredentialStatus,omitempty"`
	SSHHost               string    `json:"sshHost,omitempty"`
	SSHPort               int       `json:"sshPort,omitempty"`
	SSHUser               string    `json:"sshUser,omitempty"`
	SSHHostKeyType        string    `json:"sshHostKeyType,omitempty"`
	SSHHostKey            string    `json:"sshHostKey,omitempty"`
	SSHHostKeyFingerprint string    `json:"sshHostKeyFingerprint,omitempty"`
	AllocationPublicKey   string    `json:"allocationPublicKey"`
	JoinedAt              time.Time `json:"joinedAt,omitempty"`
	UpdatedAt             time.Time `json:"updatedAt,omitempty"`
}

type NodeTombstone struct {
	NodeID                   string    `json:"nodeId"`
	NodeName                 string    `json:"nodeName,omitempty"`
	RemovedAt                time.Time `json:"removedAt"`
	RemovedGeneration        uint64    `json:"removedGeneration"`
	RevokedMeshCredentialID  string    `json:"revokedMeshCredentialId,omitempty"`
	RevokedMeshPublicKey     string    `json:"revokedMeshPublicKey,omitempty"`
	AllocationKeyFingerprint string    `json:"allocationKeyFingerprint,omitempty"`
}

// ActiveAllocation is the exact node-signed allocation generation that the
// single controller currently authorizes for routing. Omission is revocation.
type ActiveAllocation struct {
	Kind          string    `json:"kind"`
	Project       string    `json:"project"`
	Environment   string    `json:"environment"`
	Service       string    `json:"service"`
	Revision      string    `json:"revision,omitempty"`
	Slot          int       `json:"slot"`
	HostIP        string    `json:"hostIp"`
	HostPort      int       `json:"hostPort"`
	ContainerPort int       `json:"containerPort"`
	Key           string    `json:"key"`
	ClusterID     string    `json:"clusterId"`
	NodeID        string    `json:"nodeId"`
	Generation    uint64    `json:"generation"`
	IssuedAt      time.Time `json:"issuedAt"`
	Signature     string    `json:"signature"`
	AuthorizedAt  time.Time `json:"authorizedAt"`
}

func (i ClusterInventory) Validate() error {
	if i.APIVersion != InventoryAPIVersion || i.Kind != InventoryKind {
		return fmt.Errorf("cluster inventory apiVersion/kind is invalid")
	}
	if err := ValidateClusterID(i.ClusterID); err != nil {
		return err
	}
	if i.ControllerNodeID != "" {
		if err := ValidateNodeID(i.ControllerNodeID); err != nil {
			return fmt.Errorf("controller node ID is invalid: %w", err)
		}
		if i.Generation == 0 || i.UpdatedAt.IsZero() {
			return fmt.Errorf("authoritative cluster inventory requires generation and updatedAt")
		}
	}
	var prefix netip.Prefix
	var err error
	if i.MeshCIDR != "" {
		prefix, err = netip.ParsePrefix(i.MeshCIDR)
		if err != nil {
			return fmt.Errorf("cluster inventory mesh CIDR is invalid")
		}
		prefix = prefix.Masked()
	}
	seen := make(map[string]struct{}, len(i.Nodes)+len(i.Tombstones))
	seenNames := make(map[string]struct{}, len(i.Nodes))
	seenMeshIPs := make(map[string]struct{}, len(i.Nodes))
	seenMeshKeys := make(map[string]struct{}, len(i.Nodes))
	controllerCount := 0
	for _, node := range i.Nodes {
		if err := validateInventoryNode(node, prefix, i.ControllerNodeID != ""); err != nil {
			return err
		}
		if _, ok := seen[node.NodeID]; ok {
			return fmt.Errorf("cluster inventory node %s is duplicated", node.NodeID)
		}
		seen[node.NodeID] = struct{}{}
		if node.NodeName != "" {
			name := strings.ToLower(node.NodeName)
			if _, ok := seenNames[name]; ok {
				return fmt.Errorf("cluster inventory node name %s is duplicated", node.NodeName)
			}
			seenNames[name] = struct{}{}
		}
		if node.MeshIP != "" {
			if _, ok := seenMeshIPs[node.MeshIP]; ok {
				return fmt.Errorf("cluster inventory mesh IP %s is duplicated", node.MeshIP)
			}
			seenMeshIPs[node.MeshIP] = struct{}{}
		}
		if node.MeshPublicKey != "" {
			if _, ok := seenMeshKeys[node.MeshPublicKey]; ok {
				return fmt.Errorf("cluster inventory mesh public key is duplicated")
			}
			seenMeshKeys[node.MeshPublicKey] = struct{}{}
		}
		if hasRole(node.Roles, RoleControlPlane) {
			controllerCount++
		}
	}
	if i.ControllerNodeID != "" {
		controller, ok := i.Node(i.ControllerNodeID)
		if !ok || !hasRole(controller.Roles, RoleControlPlane) {
			return fmt.Errorf("authoritative controller is not an active control-plane node")
		}
		if controllerCount == 0 {
			return fmt.Errorf("cluster inventory has no active controller")
		}
	}
	for _, tombstone := range i.Tombstones {
		if err := ValidateNodeID(tombstone.NodeID); err != nil {
			return fmt.Errorf("cluster inventory tombstone node ID is invalid: %w", err)
		}
		if tombstone.RemovedAt.IsZero() || tombstone.RemovedGeneration == 0 || tombstone.RemovedGeneration > i.Generation {
			return fmt.Errorf("cluster inventory tombstone %s has invalid removal evidence", tombstone.NodeID)
		}
		if _, ok := seen[tombstone.NodeID]; ok {
			return fmt.Errorf("cluster inventory node %s is both active and tombstoned", tombstone.NodeID)
		}
		if tombstone.RevokedMeshCredentialID != "" && tombstone.RevokedMeshPublicKey == "" {
			return fmt.Errorf("cluster inventory tombstone %s lacks revoked mesh-key identity", tombstone.NodeID)
		}
		if tombstone.RevokedMeshPublicKey != "" {
			if err := ValidateMeshPublicKey(tombstone.RevokedMeshPublicKey); err != nil {
				return fmt.Errorf("cluster inventory tombstone %s has invalid revoked mesh key", tombstone.NodeID)
			}
			expectedCredentialID, _ := MeshCredentialID(tombstone.RevokedMeshPublicKey)
			if tombstone.RevokedMeshCredentialID != expectedCredentialID {
				return fmt.Errorf("cluster inventory tombstone %s revoked mesh identity is inconsistent", tombstone.NodeID)
			}
		}
		seen[tombstone.NodeID] = struct{}{}
	}
	allocationKeys := make(map[string]struct{}, len(i.ActiveAllocations))
	allocationPorts := make(map[string]struct{}, len(i.ActiveAllocations))
	for _, allocation := range i.ActiveAllocations {
		if err := validateActiveAllocation(i, allocation); err != nil {
			return err
		}
		compound := allocation.NodeID + "\x00" + allocation.Key
		if _, ok := allocationKeys[compound]; ok {
			return fmt.Errorf("active allocation %s on node %s is duplicated", allocation.Key, allocation.NodeID)
		}
		allocationKeys[compound] = struct{}{}
		portKey := allocation.NodeID + "\x00" + allocation.HostIP + "\x00" + fmt.Sprint(allocation.HostPort)
		if _, ok := allocationPorts[portKey]; ok {
			return fmt.Errorf("active allocation host port %s:%d on node %s is duplicated", allocation.HostIP, allocation.HostPort, allocation.NodeID)
		}
		allocationPorts[portKey] = struct{}{}
	}
	return nil
}

func validateInventoryNode(node InventoryNode, prefix netip.Prefix, authoritative bool) error {
	if err := ValidateNodeID(node.NodeID); err != nil {
		return err
	}
	if err := ValidateAllocationPublicKey(node.AllocationPublicKey); err != nil {
		return fmt.Errorf("cluster inventory node %s allocation key is invalid", node.NodeID)
	}
	if node.MeshIP != "" {
		address, err := netip.ParseAddr(node.MeshIP)
		if err != nil || !prefix.IsValid() || !prefix.Contains(address) {
			return fmt.Errorf("cluster inventory node %s mesh IP is outside the trusted mesh CIDR", node.NodeID)
		}
	}
	if authoritative && node.MeshEndpoint == "" {
		return fmt.Errorf("cluster inventory node %s lacks a platform mesh endpoint", node.NodeID)
	}
	if !authoritative {
		return nil
	}
	if !nodeNamePattern.MatchString(node.NodeName) {
		return fmt.Errorf("cluster inventory node %s has invalid node name", node.NodeID)
	}
	if _, ok := activeNodeLifecycles[node.Lifecycle]; !ok {
		return fmt.Errorf("cluster inventory node %s has invalid lifecycle %q", node.NodeID, node.Lifecycle)
	}
	if len(node.Roles) == 0 || !sort.StringsAreSorted(node.Roles) {
		return fmt.Errorf("cluster inventory node %s roles must be non-empty and sorted", node.NodeID)
	}
	seenRoles := map[string]struct{}{}
	for _, role := range node.Roles {
		if _, ok := allowedRoles[role]; !ok {
			return fmt.Errorf("cluster inventory node %s has invalid role %q", node.NodeID, role)
		}
		if _, ok := seenRoles[role]; ok {
			return fmt.Errorf("cluster inventory node %s has duplicated role %q", node.NodeID, role)
		}
		seenRoles[role] = struct{}{}
	}
	if node.Schedulable != (node.Lifecycle == NodeLifecycleSchedulable) {
		return fmt.Errorf("cluster inventory node %s schedulable flag does not match lifecycle", node.NodeID)
	}
	if node.MeshCredentialID == "" || node.MeshCredentialStatus != MeshCredentialActive {
		return fmt.Errorf("cluster inventory node %s lacks an active mesh credential", node.NodeID)
	}
	if err := ValidateMeshPublicKey(node.MeshPublicKey); err != nil {
		return fmt.Errorf("cluster inventory node %s mesh public key is invalid: %w", node.NodeID, err)
	}
	expectedCredentialID, _ := MeshCredentialID(node.MeshPublicKey)
	if node.MeshCredentialID != expectedCredentialID {
		return fmt.Errorf("cluster inventory node %s mesh credential ID does not bind its public key", node.NodeID)
	}
	if node.JoinedAt.IsZero() || node.UpdatedAt.IsZero() || node.UpdatedAt.Before(node.JoinedAt) {
		return fmt.Errorf("cluster inventory node %s has invalid lifecycle timestamps", node.NodeID)
	}
	if node.SSHHost != "" {
		if node.SSHPort < 1 || node.SSHPort > 65535 || node.SSHUser == "" || node.SSHHostKeyType == "" || node.SSHHostKey == "" || node.SSHHostKeyFingerprint == "" {
			return fmt.Errorf("cluster inventory node %s has incomplete pinned SSH host identity", node.NodeID)
		}
		keyBytes, err := base64.StdEncoding.DecodeString(node.SSHHostKey)
		if err != nil {
			return fmt.Errorf("cluster inventory node %s SSH host key is invalid", node.NodeID)
		}
		key, err := cryptossh.ParsePublicKey(keyBytes)
		if err != nil || key.Type() != node.SSHHostKeyType || cryptossh.FingerprintSHA256(key) != node.SSHHostKeyFingerprint {
			return fmt.Errorf("cluster inventory node %s SSH host-key fields are inconsistent", node.NodeID)
		}
	}
	return nil
}

// ValidateMeshPublicKey accepts the canonical WireGuard key representation:
// base64 of exactly 32 non-zero Curve25519 public-key bytes.
func ValidateMeshPublicKey(value string) error {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil || len(decoded) != 32 {
		return fmt.Errorf("mesh public key must be base64-encoded 32 bytes")
	}
	nonZero := byte(0)
	for _, value := range decoded {
		nonZero |= value
	}
	if nonZero == 0 {
		return fmt.Errorf("mesh public key cannot be all zeroes")
	}
	return nil
}

func MeshCredentialID(publicKey string) (string, error) {
	if err := ValidateMeshPublicKey(publicKey); err != nil {
		return "", err
	}
	decoded, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(publicKey))
	digest := sha256.Sum256(decoded)
	return "wg-sha256:" + hex.EncodeToString(digest[:]), nil
}

func validateActiveAllocation(inventory ClusterInventory, allocation ActiveAllocation) error {
	if inventory.ControllerNodeID == "" {
		return fmt.Errorf("active allocations require an authoritative inventory")
	}
	if allocation.ClusterID != inventory.ClusterID || allocation.Generation == 0 || allocation.IssuedAt.IsZero() || allocation.AuthorizedAt.IsZero() || allocation.Signature == "" || allocation.Key == "" {
		return fmt.Errorf("active allocation %s has incomplete authority evidence", allocation.Key)
	}
	node, ok := inventory.Node(allocation.NodeID)
	if !ok || node.MeshCredentialStatus != MeshCredentialActive || node.Lifecycle != NodeLifecycleSchedulable {
		return fmt.Errorf("active allocation %s belongs to a non-schedulable node", allocation.Key)
	}
	if allocation.HostIP != node.MeshIP || allocation.HostPort < 1 || allocation.HostPort > 65535 || allocation.ContainerPort < 1 || allocation.ContainerPort > 65535 || allocation.Slot < 1 {
		return fmt.Errorf("active allocation %s has invalid destination coordinates", allocation.Key)
	}
	return nil
}

func (i ClusterInventory) Node(nodeID string) (InventoryNode, bool) {
	for _, node := range i.Nodes {
		if strings.EqualFold(node.NodeID, strings.TrimSpace(nodeID)) {
			return node, true
		}
	}
	return InventoryNode{}, false
}

func (i ClusterInventory) IsTombstoned(nodeID string) bool {
	for _, tombstone := range i.Tombstones {
		if strings.EqualFold(tombstone.NodeID, strings.TrimSpace(nodeID)) {
			return true
		}
	}
	return false
}

func (i ClusterInventory) Allocation(nodeID, key string, generation uint64) (ActiveAllocation, bool) {
	for _, allocation := range i.ActiveAllocations {
		if allocation.NodeID == nodeID && allocation.Key == key && allocation.Generation == generation {
			return allocation, true
		}
	}
	return ActiveAllocation{}, false
}

func CreateInventory(path string, inventory ClusterInventory) error {
	data, err := marshalInventory(inventory)
	if err != nil {
		return err
	}
	if err := createSecureFile(path, data); err != nil {
		return err
	}
	return publishInventoryPermissions(path)
}

// ReplaceInventory atomically publishes a new controller snapshot in the
// already protected inventory directory. It validates the complete snapshot
// before replacing the previous inode.
func ReplaceInventory(path string, inventory ClusterInventory) error {
	data, err := marshalInventory(inventory)
	if err != nil {
		return err
	}
	if err := replaceSecureFile(path, data); err != nil {
		return err
	}
	return publishInventoryPermissions(path)
}

func marshalInventory(inventory ClusterInventory) ([]byte, error) {
	sort.Slice(inventory.Nodes, func(a, b int) bool { return inventory.Nodes[a].NodeID < inventory.Nodes[b].NodeID })
	for index := range inventory.Nodes {
		sort.Strings(inventory.Nodes[index].Roles)
	}
	sort.Slice(inventory.Tombstones, func(a, b int) bool { return inventory.Tombstones[a].NodeID < inventory.Tombstones[b].NodeID })
	sort.Slice(inventory.ActiveAllocations, func(a, b int) bool {
		if inventory.ActiveAllocations[a].NodeID != inventory.ActiveAllocations[b].NodeID {
			return inventory.ActiveAllocations[a].NodeID < inventory.ActiveAllocations[b].NodeID
		}
		return inventory.ActiveAllocations[a].Key < inventory.ActiveAllocations[b].Key
	})
	if err := inventory.Validate(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(inventory, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func ReadInventory(path string) (*ClusterInventory, error) {
	data, err := readPublishedInventoryFile(path, maxInventoryFileSize)
	if err != nil {
		return nil, err
	}
	var inventory ClusterInventory
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&inventory); err != nil {
		return nil, fmt.Errorf("decode cluster inventory: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("decode cluster inventory: multiple JSON values are not allowed")
	}
	if err := inventory.Validate(); err != nil {
		return nil, err
	}
	return &inventory, nil
}

func ReadInventoryOptional(path string) (*ClusterInventory, error) {
	inventory, err := ReadInventory(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return inventory, err
}

func hasRole(roles []string, wanted string) bool {
	for _, role := range roles {
		if role == wanted {
			return true
		}
	}
	return false
}
