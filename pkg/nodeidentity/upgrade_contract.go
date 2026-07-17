package nodeidentity

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// UpgradeContract binds privileged binary publication to the exact protected
// identity and inventory generation most recently attested over takod.
type UpgradeContract struct {
	ClusterID            string   `json:"clusterId"`
	NodeID               string   `json:"nodeId"`
	MembershipGeneration uint64   `json:"membershipGeneration"`
	Lifecycle            string   `json:"lifecycle"`
	Roles                []string `json:"roles"`
}

func (c UpgradeContract) Validate() error {
	if err := (Reference{ClusterID: c.ClusterID, NodeID: c.NodeID}).Validate(); err != nil {
		return err
	}
	if c.MembershipGeneration == 0 {
		return fmt.Errorf("membership generation must be positive")
	}
	if _, ok := activeNodeLifecycles[c.Lifecycle]; !ok {
		return fmt.Errorf("membership lifecycle %q is invalid", c.Lifecycle)
	}
	if len(c.Roles) == 0 {
		return fmt.Errorf("at least one current membership role is required")
	}
	seen := make(map[string]struct{}, len(c.Roles))
	for _, role := range c.Roles {
		if _, ok := allowedRoles[role]; !ok {
			return fmt.Errorf("membership role %q is invalid", role)
		}
		if _, ok := seen[role]; ok {
			return fmt.Errorf("membership role %q is duplicated", role)
		}
		seen[role] = struct{}{}
	}
	return nil
}

// VerifyUpgradeContract reopens the protected identity and inventory and
// proves they still match the last takod attestation. Call it from the staged
// candidate immediately before publishing that candidate.
func VerifyUpgradeContract(identityPath string, inventoryPath string, contract UpgradeContract) error {
	if err := contract.Validate(); err != nil {
		return err
	}
	installation, err := Read(identityPath)
	if err != nil {
		return fmt.Errorf("read protected installation identity: %w", err)
	}
	if !strings.EqualFold(installation.ClusterID, contract.ClusterID) || !strings.EqualFold(installation.NodeID, contract.NodeID) {
		return fmt.Errorf("protected installation identity changed before upgrade publication")
	}
	inventory, err := ReadInventory(inventoryPath)
	if err != nil {
		return fmt.Errorf("read protected cluster inventory: %w", err)
	}
	if !strings.EqualFold(inventory.ClusterID, contract.ClusterID) || inventory.Generation != contract.MembershipGeneration {
		return fmt.Errorf("protected membership generation changed before upgrade publication")
	}
	node, ok := inventory.Node(contract.NodeID)
	if !ok {
		return fmt.Errorf("node disappeared from protected cluster inventory before upgrade publication")
	}
	if node.Lifecycle != contract.Lifecycle || !sameUpgradeRoles(node.Roles, contract.Roles) {
		return fmt.Errorf("protected node lifecycle or roles changed before upgrade publication")
	}
	if node.AllocationPublicKey == "" || node.AllocationPublicKey != installation.AllocationPublicKey {
		return fmt.Errorf("protected node allocation identity changed before upgrade publication")
	}
	return nil
}

func sameUpgradeRoles(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	left = append([]string(nil), left...)
	right = append([]string(nil), right...)
	sort.Strings(left)
	sort.Strings(right)
	return slices.Equal(left, right)
}
