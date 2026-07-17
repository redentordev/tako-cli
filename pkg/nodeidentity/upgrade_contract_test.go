package nodeidentity

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVerifyUpgradeContractRejectsProtectedInventoryDrift(t *testing.T) {
	installation, err := New(
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
		"node-a",
		[]string{RoleWorker},
		time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	identityPath := filepath.Join(root, "identity.json")
	inventoryPath := filepath.Join(root, "inventory.json")
	if err := Create(identityPath, *installation); err != nil {
		t.Fatal(err)
	}
	inventory := ClusterInventory{
		APIVersion: InventoryAPIVersion, Kind: InventoryKind,
		ClusterID: installation.ClusterID, Generation: 7,
		Nodes: []InventoryNode{{
			NodeID: installation.NodeID, Lifecycle: NodeLifecycleSchedulable,
			Roles: []string{RoleWorker}, Schedulable: true,
			AllocationPublicKey: installation.AllocationPublicKey,
		}},
	}
	if err := CreateInventory(inventoryPath, inventory); err != nil {
		t.Fatal(err)
	}
	contract := UpgradeContract{
		ClusterID: installation.ClusterID, NodeID: installation.NodeID,
		MembershipGeneration: 7, Lifecycle: NodeLifecycleSchedulable,
		Roles: []string{RoleWorker},
	}
	if err := VerifyUpgradeContract(identityPath, inventoryPath, contract); err != nil {
		t.Fatalf("current protected contract rejected: %v", err)
	}
	inventory.Generation++
	if err := ReplaceInventory(inventoryPath, inventory); err != nil {
		t.Fatal(err)
	}
	if err := VerifyUpgradeContract(identityPath, inventoryPath, contract); err == nil || !strings.Contains(err.Error(), "generation changed") {
		t.Fatalf("stale membership generation accepted: %v", err)
	}
}
