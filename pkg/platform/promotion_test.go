package platform

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

func TestVerifyPassivePromotionProvesColdSingleController(t *testing.T) {
	store, controller, now := newTestMembershipStore(t)
	root := securePromotionTestDir(t)
	configDir := filepath.Join(root, "etc", "tako")
	controlDir := filepath.Join(root, "var", "lib", "tako", DefaultMembershipDirName)
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(controlDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := copyPromotionFixture(store.path, filepath.Join(controlDir, DefaultMembershipName)); err != nil {
		t.Fatal(err)
	}
	if err := copyPromotionFixture(store.inventoryPath, filepath.Join(configDir, filepath.Base(nodeidentity.DefaultInventoryPath))); err != nil {
		t.Fatal(err)
	}
	if err := nodeidentity.Create(filepath.Join(configDir, filepath.Base(nodeidentity.DefaultPath)), *controller); err != nil {
		t.Fatal(err)
	}
	document := ConfigDocument{State: BootstrapState{
		APIVersion: APIVersion, Kind: BootstrapKind, ClusterID: controller.ClusterID, NodeID: controller.NodeID, NodeName: controller.NodeName,
		ControllerMode: "single-writer", EnrollmentRoles: append([]string(nil), firstNodeRoles...), IdentityPath: nodeidentity.DefaultPath, InventoryPath: nodeidentity.DefaultInventoryPath,
		MembershipPath: DefaultMembershipPath(DefaultStateDir), StateDir: DefaultStateDir, AuditDir: DefaultAuditDir, SocketPath: DefaultSocket, WorkerSocketPath: DefaultWorkerSocket,
		DockerDataRoot: "/var/lib/docker", SocketGroup: DefaultSocketGroup, ServiceBinaryPath: DefaultBinaryPath, WorkerUser: DefaultWorkerUser, WorkerGroup: DefaultWorkerGroup,
		WorkerUID: 1001, WorkerGID: 1001, SocketGroupGID: 1002, InitializedAt: now,
	}, Policy: DefaultResourcePolicy()}
	data, _ := json.Marshal(document)
	if err := os.WriteFile(filepath.Join(configDir, "platform.json"), data, 0600); err != nil {
		t.Fatal(err)
	}

	fingerprint, err := ControllerRecoveryKeyFingerprint(controller.AllocationPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := VerifyPassivePromotion(root, controller.ClusterID, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if proof.ActiveActive || proof.ControllerMode != "passive-recovery" || proof.ControllerNodeID != controller.NodeID || proof.MembershipGeneration == 0 {
		t.Fatalf("unexpected proof %#v", proof)
	}
	if _, err := VerifyPassivePromotion(root, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", fingerprint); err == nil || !strings.Contains(err.Error(), "required cluster") {
		t.Fatalf("cluster mismatch error = %v", err)
	}
	if _, err := VerifyPassivePromotion(root, controller.ClusterID, "SHA256:not-the-controller"); err == nil || !strings.Contains(err.Error(), "externally trusted") {
		t.Fatalf("untrusted controller key accepted: %v", err)
	}
	linkedRoot := securePromotionTestDir(t)
	if err := os.Symlink(filepath.Join(root, "etc"), filepath.Join(linkedRoot, "etc")); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyPassivePromotion(linkedRoot, controller.ClusterID, fingerprint); err == nil {
		t.Fatalf("symlinked staging tree accepted: %v", err)
	}
}

func TestVerifyPassivePromotionRejectsInvalidResourcePolicy(t *testing.T) {
	store, controller, now := newTestMembershipStore(t)
	root := securePromotionTestDir(t)
	configDir := filepath.Join(root, "etc", "tako")
	controlDir := filepath.Join(root, "var", "lib", "tako", DefaultMembershipDirName)
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(controlDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := copyPromotionFixture(store.path, filepath.Join(controlDir, DefaultMembershipName)); err != nil {
		t.Fatal(err)
	}
	if err := copyPromotionFixture(store.inventoryPath, filepath.Join(configDir, filepath.Base(nodeidentity.DefaultInventoryPath))); err != nil {
		t.Fatal(err)
	}
	if err := nodeidentity.Create(filepath.Join(configDir, filepath.Base(nodeidentity.DefaultPath)), *controller); err != nil {
		t.Fatal(err)
	}
	document := ConfigDocument{State: BootstrapState{
		APIVersion: APIVersion, Kind: BootstrapKind, ClusterID: controller.ClusterID, NodeID: controller.NodeID, NodeName: controller.NodeName,
		ControllerMode: "single-writer", EnrollmentRoles: append([]string(nil), firstNodeRoles...), IdentityPath: nodeidentity.DefaultPath, InventoryPath: nodeidentity.DefaultInventoryPath,
		MembershipPath: DefaultMembershipPath(DefaultStateDir), StateDir: DefaultStateDir, AuditDir: DefaultAuditDir, SocketPath: DefaultSocket, WorkerSocketPath: DefaultWorkerSocket,
		DockerDataRoot: "/var/lib/docker", SocketGroup: DefaultSocketGroup, ServiceBinaryPath: DefaultBinaryPath, WorkerUser: DefaultWorkerUser, WorkerGroup: DefaultWorkerGroup,
		WorkerUID: 1001, WorkerGID: 1001, SocketGroupGID: 1002, InitializedAt: now,
	}, Policy: DefaultResourcePolicy()}
	document.Policy.ReservedMemoryBytes = -1
	data, _ := json.Marshal(document)
	if err := os.WriteFile(filepath.Join(configDir, "platform.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	fingerprint, err := ControllerRecoveryKeyFingerprint(controller.AllocationPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyPassivePromotion(root, controller.ClusterID, fingerprint); err == nil || !strings.Contains(err.Error(), "memory reservation") {
		t.Fatalf("invalid staged resource policy accepted: %v", err)
	}
}

func securePromotionTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", ".tako-promotion-test-")
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatal(err)
	}
	if err := os.Chmod(abs, 0700); err != nil {
		_ = os.RemoveAll(abs)
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(abs) })
	return abs
}

func copyPromotionFixture(source, destination string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(destination, data, 0600)
}
