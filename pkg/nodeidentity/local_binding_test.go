package nodeidentity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocalBindingIsPublicAndIntegrityProtected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-node.json")
	binding := LocalBinding{APIVersion: InventoryAPIVersion, Kind: LocalBindingKind, ClusterID: "11111111-1111-4111-8111-111111111111", NodeID: "22222222-2222-4222-8222-222222222222", NodeName: "node-1", WorkerUID: 1001}
	if err := WriteLocalBinding(path, binding); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0644 {
		t.Fatalf("local binding mode = %04o", info.Mode().Perm())
	}
	loaded, err := ReadLocalBinding(path)
	if err != nil || loaded.NodeID != binding.NodeID || loaded.WorkerUID != binding.WorkerUID {
		t.Fatalf("local binding = %#v, %v", loaded, err)
	}
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLocalBinding(path); err == nil {
		t.Fatal("writable local binding was trusted")
	}
}
