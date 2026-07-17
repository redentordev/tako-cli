package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/platform"
)

func TestDecodeEnrollmentMarkerIgnoresNonProtocolOutput(t *testing.T) {
	var result platform.WorkerEnrollmentIdentity
	output := "sudo: notice\nTAKO_ENROLLMENT_IDENTITY={\"clusterId\":\"c\",\"nodeId\":\"n\",\"nodeName\":\"worker\",\"allocationPublicKey\":\"k\"}\n"
	if err := decodeEnrollmentMarker(output, "TAKO_ENROLLMENT_IDENTITY=", &result); err != nil {
		t.Fatal(err)
	}
	if result.NodeName != "worker" {
		t.Fatalf("decoded result = %#v", result)
	}
	if err := decodeEnrollmentMarker("{}", "TAKO_ENROLLMENT_IDENTITY=", &result); err == nil {
		t.Fatal("unframed enrollment output was accepted")
	}
}

func TestPlatformMeshTopologyComesOnlyFromActiveInventory(t *testing.T) {
	controller, err := nodeidentity.New("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "node-1", []string{nodeidentity.RoleControlPlane, nodeidentity.RoleWorker}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	controllerKey := "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
	workerKey := "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI="
	controllerCredential, _ := nodeidentity.MeshCredentialID(controllerKey)
	workerCredential, _ := nodeidentity.MeshCredentialID(workerKey)
	inventory := &nodeidentity.ClusterInventory{ClusterID: controller.ClusterID, Nodes: []nodeidentity.InventoryNode{
		{NodeID: controller.NodeID, NodeName: controller.NodeName, MeshIP: "10.210.0.1", MeshEndpoint: "node-1.example", MeshPublicKey: controllerKey, MeshCredentialID: controllerCredential, MeshCredentialStatus: nodeidentity.MeshCredentialActive},
		{NodeID: "33333333-3333-4333-8333-333333333333", NodeName: "worker-2", MeshIP: "10.210.0.2", MeshEndpoint: "worker.example", MeshPublicKey: workerKey, MeshCredentialID: workerCredential, MeshCredentialStatus: nodeidentity.MeshCredentialActive},
	}}
	node, peers, excluded, err := platformMeshTopology(controller, inventory, "")
	if err != nil {
		t.Fatal(err)
	}
	if excluded != "" || node.PublicKey != controllerKey || len(peers) != 1 || peers[0].PublicKey != workerKey || peers[0].Host != "worker.example" {
		t.Fatalf("unexpected membership-owned topology: node=%#v peers=%#v", node, peers)
	}
	inventory.Nodes = inventory.Nodes[:1]
	_, peers, _, err = platformMeshTopology(controller, inventory, "")
	if err != nil || len(peers) != 0 {
		t.Fatalf("removed peer remained in topology: peers=%#v err=%v", peers, err)
	}
}

func TestPlatformMeshTopologyExcludesPendingRemovalDurably(t *testing.T) {
	controller, err := nodeidentity.New("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "node-1", []string{nodeidentity.RoleControlPlane, nodeidentity.RoleWorker}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	controllerKey := "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
	workerID := "33333333-3333-4333-8333-333333333333"
	workerKey := "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI="
	controllerCredential, _ := nodeidentity.MeshCredentialID(controllerKey)
	workerCredential, _ := nodeidentity.MeshCredentialID(workerKey)
	inventory := &nodeidentity.ClusterInventory{ClusterID: controller.ClusterID, Nodes: []nodeidentity.InventoryNode{
		{NodeID: controller.NodeID, NodeName: controller.NodeName, MeshIP: "10.210.0.1", MeshEndpoint: "node-1.example", MeshPublicKey: controllerKey, MeshCredentialID: controllerCredential, MeshCredentialStatus: nodeidentity.MeshCredentialActive},
		{NodeID: workerID, NodeName: "worker-2", MeshIP: "10.210.0.2", MeshEndpoint: "worker.example", MeshPublicKey: workerKey, MeshCredentialID: workerCredential, MeshCredentialStatus: nodeidentity.MeshCredentialActive},
	}}
	_, peers, excluded, err := platformMeshTopology(controller, inventory, workerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 0 || excluded != workerKey {
		t.Fatalf("pending-removal peer was not excluded: peers=%#v excluded=%q", peers, excluded)
	}
}

func TestPlatformRemoteCommandsQuoteOperatorValues(t *testing.T) {
	quoted := shellQuotePlatform("worker'; touch /tmp/pwned; echo '")
	if !strings.HasPrefix(quoted, "'") || !strings.HasSuffix(quoted, "'") || !strings.Contains(quoted, "'\"'\"'") {
		t.Fatalf("unsafe shell quoting: %s", quoted)
	}
}

func TestPlatformLifecycleRejectsInventoryPathTakodDoesNotUse(t *testing.T) {
	if err := validatePlatformMembershipInventoryPath("/tmp/other-inventory.json"); err == nil {
		t.Fatal("custom lifecycle inventory path was accepted")
	}
	if err := validatePlatformMembershipInventoryPath(nodeidentity.DefaultInventoryPath); err != nil {
		t.Fatal(err)
	}
}

func TestHiddenWorkerSafetyCommandsRejectInventoryPathTakodDoesNotUse(t *testing.T) {
	previous := platformMembershipInventory
	platformMembershipInventory = "/tmp/other-inventory.json"
	t.Cleanup(func() { platformMembershipInventory = previous })
	if err := runPlatformWorkerReconcileMesh(platformWorkerReconcileMeshCmd, nil); err == nil || !strings.Contains(err.Error(), nodeidentity.DefaultInventoryPath) {
		t.Fatalf("worker reconcile custom inventory error = %v", err)
	}
	if err := runPlatformWorkerVerifyEnrollment(platformWorkerVerifyEnrollmentCmd, nil); err == nil || !strings.Contains(err.Error(), nodeidentity.DefaultInventoryPath) {
		t.Fatalf("worker verification custom inventory error = %v", err)
	}
}

func TestWorkerInventoryPublicationPreservesOperatorReadableMode(t *testing.T) {
	command := platformWorkerInventoryInstallCommand("/tmp/inventory", "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "")
	if !strings.Contains(command, "-m 0644") || strings.Contains(command, "-m 0600") {
		t.Fatalf("worker inventory publication mode is not readable: %s", command)
	}
	if !strings.Contains(command, "flock -x 9") || !strings.Contains(command, "chmod 0600 \"$lock_dir/.guard\"") || !strings.Contains(command, "cluster-inventory.json.lock") || !strings.Contains(command, "flock -x 8") {
		t.Fatalf("worker inventory publication does not join upgrade and inventory barriers: %s", command)
	}
}

func TestRemovedWorkerRevocationJoinsUpgradeAndInventoryBarriers(t *testing.T) {
	command := removedWorkerRevocationCommand("/tmp/revoked-inventory")
	for _, required := range []string{"flock -x 9", "chmod 0600 \"$lock_dir/.guard\"", "node lifecycle mutation blocked by active node upgrade lease", "exit 73", "cluster-inventory.json.lock", "flock -x 8", "install -o root -g root -m 0644", "systemctl stop takod.service"} {
		if !strings.Contains(command, required) {
			t.Fatalf("removed-worker revocation lacks %q: %s", required, command)
		}
	}
}
