package projectbinding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testClusterID   = "11111111-1111-4111-8111-111111111111"
	testLocalNodeID = "22222222-2222-4222-8222-222222222222"
	testControlID   = "33333333-3333-4333-8333-333333333333"
)

func testPlatformContext() Context {
	return Context{
		ClusterID: testClusterID, LocalNodeID: testLocalNodeID, LocalNodeName: "node-2",
		ControllerNodeID: testControlID, ControllerNodeName: "node-1",
		InventoryGeneration: 7, InventoryUpdatedAt: time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC),
	}
}

func TestCreateReadAndMatchBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".tako", "platform-cluster.json")
	binding, err := New("demo", testPlatformContext(), time.Date(2026, 7, 17, 12, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	winner, err := Create(path, *binding)
	if err != nil {
		t.Fatal(err)
	}
	if winner.ClusterID != testClusterID {
		t.Fatalf("winner = %#v", winner)
	}
	read, err := ReadOptional(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := read.Matches("demo", testPlatformContext()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("binding mode = %04o, want 0600", info.Mode().Perm())
	}
}

func TestCreateDoesNotReplaceExistingBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".tako", "platform-cluster.json")
	first, _ := New("demo", testPlatformContext(), time.Now())
	if _, err := Create(path, *first); err != nil {
		t.Fatal(err)
	}
	otherContext := testPlatformContext()
	otherContext.ClusterID = "44444444-4444-4444-8444-444444444444"
	second, _ := New("demo", otherContext, time.Now())
	winner, err := Create(path, *second)
	if err != nil {
		t.Fatal(err)
	}
	if winner.ClusterID != first.ClusterID {
		t.Fatalf("existing binding was replaced: %#v", winner)
	}
}

func TestReadRejectsSymlinkAndUnknownFields(t *testing.T) {
	root := t.TempDir()
	realPath := filepath.Join(root, "real.json")
	if err := os.WriteFile(realPath, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(root, "binding.json")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadOptional(linkPath); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlink error = %v", err)
	}

	context := testPlatformContext()
	binding, _ := New("demo", context, time.Now())
	path := filepath.Join(root, "unknown.json")
	data := []byte(`{"apiVersion":"` + binding.APIVersion + `","kind":"` + binding.Kind + `","project":"demo","clusterId":"` + binding.ClusterID + `","localNodeId":"` + binding.LocalNodeID + `","localNodeName":"node-2","controllerNodeId":"` + binding.ControllerNodeID + `","controllerNodeName":"node-1","inventoryGeneration":7,"inventoryUpdatedAt":"2026-07-17T12:00:00Z","attachedAt":"2026-07-17T12:01:00Z","unexpected":true}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadOptional(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown-field error = %v", err)
	}
}

func TestReadRejectsSymlinkedOrWritableStateDirectory(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0700); err != nil {
		t.Fatal(err)
	}
	binding, _ := New("demo", testPlatformContext(), time.Now())
	realPath := filepath.Join(realDir, "platform-cluster.json")
	if _, err := Create(realPath, *binding); err != nil {
		t.Fatal(err)
	}
	linkedDir := filepath.Join(root, ".tako")
	if err := os.Symlink(realDir, linkedDir); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadOptional(filepath.Join(linkedDir, "platform-cluster.json")); err == nil || (!strings.Contains(err.Error(), "must be a directory") && !strings.Contains(err.Error(), "not a directory")) {
		t.Fatalf("symlinked directory error = %v", err)
	}
	if err := os.Remove(linkedDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(realDir, 0777); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadOptional(realPath); err == nil || !strings.Contains(err.Error(), "world-writable") {
		t.Fatalf("writable directory error = %v", err)
	}
}

func TestMatchesRejectsProjectAndClusterChanges(t *testing.T) {
	binding, _ := New("demo", testPlatformContext(), time.Now())
	if err := binding.Matches("other", testPlatformContext()); err == nil || !strings.Contains(err.Error(), "attached for project") {
		t.Fatalf("project mismatch = %v", err)
	}
	other := testPlatformContext()
	other.ClusterID = "44444444-4444-4444-8444-444444444444"
	if err := binding.Matches("demo", other); err == nil || !strings.Contains(err.Error(), "belongs to cluster") {
		t.Fatalf("cluster mismatch = %v", err)
	}
}
