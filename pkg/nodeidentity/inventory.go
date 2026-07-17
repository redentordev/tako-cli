package nodeidentity

import (
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"sort"
	"strings"
)

const (
	InventoryAPIVersion  = "tako.io/v1"
	InventoryKind        = "ClusterInventory"
	DefaultInventoryPath = "/etc/tako/cluster-inventory.json"
)

type ClusterInventory struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	ClusterID  string          `json:"clusterId"`
	MeshCIDR   string          `json:"meshCidr,omitempty"`
	Nodes      []InventoryNode `json:"nodes"`
}

type InventoryNode struct {
	NodeID              string `json:"nodeId"`
	MeshIP              string `json:"meshIp,omitempty"`
	AllocationPublicKey string `json:"allocationPublicKey"`
}

func (i ClusterInventory) Validate() error {
	if i.APIVersion != InventoryAPIVersion || i.Kind != InventoryKind {
		return fmt.Errorf("cluster inventory apiVersion/kind is invalid")
	}
	if err := ValidateClusterID(i.ClusterID); err != nil {
		return err
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
	seen := make(map[string]struct{}, len(i.Nodes))
	for _, node := range i.Nodes {
		if err := ValidateNodeID(node.NodeID); err != nil {
			return err
		}
		if _, ok := seen[node.NodeID]; ok {
			return fmt.Errorf("cluster inventory node %s is duplicated", node.NodeID)
		}
		seen[node.NodeID] = struct{}{}
		if err := ValidateAllocationPublicKey(node.AllocationPublicKey); err != nil {
			return fmt.Errorf("cluster inventory node %s allocation key is invalid", node.NodeID)
		}
		if node.MeshIP != "" {
			address, err := netip.ParseAddr(node.MeshIP)
			if err != nil || !prefix.IsValid() || !prefix.Contains(address) {
				return fmt.Errorf("cluster inventory node %s mesh IP is outside the trusted mesh CIDR", node.NodeID)
			}
		}
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

func CreateInventory(path string, inventory ClusterInventory) error {
	sort.Slice(inventory.Nodes, func(a, b int) bool { return inventory.Nodes[a].NodeID < inventory.Nodes[b].NodeID })
	if err := inventory.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(inventory, "", "  ")
	if err != nil {
		return err
	}
	return createSecureFile(path, append(data, '\n'))
}

func ReadInventory(path string) (*ClusterInventory, error) {
	data, err := readSecureFile(path, maxFileSize)
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
