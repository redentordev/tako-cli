package nodeidentity

import (
	"testing"
	"time"
)

func TestOperationFenceSignatureBindsTargetsAndToken(t *testing.T) {
	now := time.Now().UTC()
	controller, err := New("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "node-1", []string{RoleControlPlane, RoleWorker}, now)
	if err != nil {
		t.Fatal(err)
	}
	fence := OperationFence{
		Kind: OperationFenceKind, ClusterID: controller.ClusterID, ControllerNodeID: controller.NodeID,
		MembershipGeneration: 7, Project: "demo", Environment: "production", OperationID: "op-1", Operation: "deploy", Token: 4,
		TargetNodeIDs: []string{"33333333-3333-4333-8333-333333333333"}, IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	fence.HolderTokenHash, err = OperationHolderTokenHash("test-holder-token")
	if err != nil {
		t.Fatal(err)
	}
	if err := SignOperationFence(&fence, controller); err != nil {
		t.Fatal(err)
	}
	if err := VerifyOperationFence(fence, controller.AllocationPublicKey, now); err != nil {
		t.Fatal(err)
	}
	forged := fence
	forged.Token++
	if err := VerifyOperationFence(forged, controller.AllocationPublicKey, now); err == nil {
		t.Fatal("forged fencing token verified")
	}
}
