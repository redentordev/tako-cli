//go:build !windows

package provisioner

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/recovery"
)

func TestProtectedInventoryCannotChangeBetweenUpgradeGuardAndPublication(t *testing.T) {
	root := t.TempDir()
	paths := takodUpgradePublicationPaths{
		dir: filepath.Join(root, "node-upgrade"), locks: filepath.Join(root, "node-upgrade", "locks"),
		candidate: filepath.Join(root, "node-upgrade", "candidate"),
		takod:     filepath.Join(root, "bin", "tako"), worker: filepath.Join(root, "lib", "tako"),
		manifest: filepath.Join(root, "etc", "version.json"),
		identity: filepath.Join(root, "etc", "identity.json"), inventory: filepath.Join(root, "etc", "inventory.json"),
		platformState: root,
	}
	for _, directory := range []string{paths.dir, paths.locks, filepath.Join(paths.locks, takodUpgradeNodeLockScope), filepath.Dir(paths.takod), filepath.Dir(paths.identity)} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeRecoveryFixture(t, paths.candidate, "new-agent", 0755)
	writeRecoveryFixture(t, paths.takod, "old-agent", 0755)
	writeRecoveryFixture(t, filepath.Join(paths.locks, takodUpgradeNodeLockScope, "owner"), "lease-owner\n", 0600)
	writeRecoveryFixture(t, filepath.Join(paths.locks, takodUpgradeNodeLockScope, "expiry"), strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)+"\n", 0600)

	installation, err := nodeidentity.New(
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
		"node-a", []string{nodeidentity.RoleWorker}, time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := nodeidentity.Create(paths.identity, *installation); err != nil {
		t.Fatal(err)
	}
	inventory := nodeidentity.ClusterInventory{
		APIVersion: nodeidentity.InventoryAPIVersion, Kind: nodeidentity.InventoryKind,
		ClusterID: installation.ClusterID, Generation: 7,
		Nodes: []nodeidentity.InventoryNode{{
			NodeID: installation.NodeID, Lifecycle: nodeidentity.NodeLifecycleSchedulable,
			Roles: []string{nodeidentity.RoleWorker}, Schedulable: true,
			AllocationPublicKey: installation.AllocationPublicKey,
		}},
	}
	if err := nodeidentity.CreateInventory(paths.inventory, inventory); err != nil {
		t.Fatal(err)
	}
	contract := &nodeidentity.UpgradeContract{
		ClusterID: installation.ClusterID, NodeID: installation.NodeID,
		MembershipGeneration: 7, Lifecycle: nodeidentity.NodeLifecycleSchedulable,
		Roles: []string{nodeidentity.RoleWorker},
	}
	validated := make(chan struct{})
	continuePublish := make(chan struct{})
	publishDone := make(chan error, 1)
	go func() {
		publishDone <- publishTakodUpgrade(paths, contract, "lease-owner", func() {
			close(validated)
			<-continuePublish
		})
	}()
	<-validated

	mutationDone := make(chan error, 1)
	go func() {
		inventory.Generation = 8
		mutationDone <- nodeidentity.ReplaceInventory(paths.inventory, inventory)
	}()
	snapshotMutationDone := make(chan error, 1)
	go func() {
		release, lockErr := recovery.AcquireMutationLock(root)
		if lockErr == nil {
			release()
		}
		snapshotMutationDone <- lockErr
	}()
	select {
	case err := <-mutationDone:
		t.Fatalf("inventory mutation crossed the publication barrier: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case err := <-snapshotMutationDone:
		t.Fatalf("takod data-root mutation crossed the publication barrier: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	data, err := os.ReadFile(paths.takod)
	if err != nil || string(data) != "old-agent" {
		t.Fatalf("binary changed before guarded publication resumed: %q, %v", data, err)
	}
	close(continuePublish)
	if err := <-publishDone; err != nil {
		t.Fatal(err)
	}
	if err := <-mutationDone; err != nil {
		t.Fatal(err)
	}
	if err := <-snapshotMutationDone; err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(paths.takod)
	if err != nil || string(data) != "new-agent" {
		t.Fatalf("guarded candidate was not published: %q, %v", data, err)
	}
}
