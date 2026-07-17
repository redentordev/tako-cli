package nodeidentity

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInventoryIsIntegrityProtectedAndOperatorReadable(t *testing.T) {
	installation, err := New("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "node-1", []string{RoleWorker}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "inventory.json")
	inventory := ClusterInventory{APIVersion: InventoryAPIVersion, Kind: InventoryKind, ClusterID: installation.ClusterID, Nodes: []InventoryNode{{NodeID: installation.NodeID, AllocationPublicKey: installation.AllocationPublicKey}}}
	if err := CreateInventory(path, inventory); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0644 {
		t.Fatalf("inventory mode = %04o, want 0644", info.Mode().Perm())
	}
	inventory.Nodes[0].NodeName = "ignored-in-compatibility-form"
	if err := ReplaceInventory(path, inventory); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInventory(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0664); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInventory(path); err == nil {
		t.Fatal("group-writable inventory was trusted")
	}
}

func TestDefaultPublishedTrustFilesRequireRootOwnership(t *testing.T) {
	for _, path := range []string{DefaultInventoryPath, DefaultLocalBindingPath} {
		if !requiresRootPublishedOwner(path) {
			t.Fatalf("default trust path %s did not require root ownership", path)
		}
		if publishedOwnerAllowed(path, 501, 501) {
			t.Fatalf("default trust path %s accepted an operator-owned file or directory", path)
		}
		if !publishedOwnerAllowed(path, 0, 501) {
			t.Fatalf("default trust path %s rejected root ownership", path)
		}
	}
	if requiresRootPublishedOwner(filepath.Join(t.TempDir(), "inventory.json")) {
		t.Fatal("isolated test fixture unexpectedly required root ownership")
	}
}
