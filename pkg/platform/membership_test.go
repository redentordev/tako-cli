package platform

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	cryptossh "golang.org/x/crypto/ssh"
)

const membershipWorkerID = "33333333-3333-4333-8333-333333333333"

func newTestMembershipStore(t *testing.T) (*MembershipStore, *nodeidentity.Installation, time.Time) {
	t.Helper()
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	configDir := filepath.Join(root, "etc")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := NewMembershipStore(DefaultMembershipPath(stateDir), filepath.Join(configDir, "inventory.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	installation, err := nodeidentity.New("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "node-1", firstNodeRoles, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.InitializeFirstNode(*installation, DefaultPlatformMeshCIDR, "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=", "node-1.example"); err != nil {
		t.Fatal(err)
	}
	return store, installation, now
}

func testEnrollment(t *testing.T, store *MembershipStore, token string, nodeID string, now time.Time) EnrollWorkerRequest {
	t.Helper()
	worker, err := nodeidentity.New("11111111-1111-4111-8111-111111111111", nodeID, "worker-2", []string{nodeidentity.RoleWorker}, now)
	if err != nil {
		t.Fatal(err)
	}
	hostKey, fingerprint := testMembershipHostKey(t)
	reservation, err := store.ReserveJoinToken(token, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	return EnrollWorkerRequest{
		Reservation: reservation.Reservation, NodeID: nodeID, NodeName: "worker-2", MeshIP: "10.210.0.2", MeshEndpoint: "worker-2.example", ControllerMeshEndpoint: "node-1.example",
		SSHHost: "203.0.113.20", SSHPort: 22, SSHUser: "root", SSHHostKeyType: "ssh-ed25519",
		SSHHostKey:            hostKey,
		SSHHostKeyFingerprint: fingerprint,
		ControllerSSHHost:     "203.0.113.10", ControllerSSHPort: 22, ControllerSSHUser: "root", ControllerSSHHostKeyType: "ssh-ed25519",
		ControllerSSHHostKey: hostKey, ControllerSSHHostKeyFingerprint: fingerprint,
		AllocationPublicKey: worker.AllocationPublicKey,
		MeshPublicKey:       "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI=",
	}
}

func testMembershipHostKey(t *testing.T) (string, string) {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := cryptossh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(key.Marshal()), cryptossh.FingerprintSHA256(key)
}

func TestValidateControllerRecoverySnapshotBindsIdentityMembershipAndInventory(t *testing.T) {
	store, controller, now := newTestMembershipStore(t)
	if err := ValidateControllerRecoverySnapshot(store.path, store.inventoryPath, controller); err != nil {
		t.Fatalf("valid controller snapshot rejected: %v", err)
	}
	worker, err := nodeidentity.New(controller.ClusterID, membershipWorkerID, "worker-2", []string{nodeidentity.RoleWorker}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateControllerRecoverySnapshot(store.path, store.inventoryPath, worker); err == nil || !strings.Contains(err.Error(), "controller identity") {
		t.Fatalf("worker snapshot authority accepted: %v", err)
	}
}

func TestMembershipJoinTokenIsBoundSingleUseAndWorkerStartsJoining(t *testing.T) {
	store, _, now := newTestMembershipStore(t)
	issued, err := store.CreateJoinToken(membershipWorkerID, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReserveJoinToken(issued.Token, "44444444-4444-4444-8444-444444444444"); err == nil || !strings.Contains(err.Error(), "another node") {
		t.Fatalf("wrong-node token error = %v", err)
	}
	request := testEnrollment(t, store, issued.Token, membershipWorkerID, now)
	node, err := store.EnrollWorker(request)
	if err != nil {
		t.Fatal(err)
	}
	if node.Lifecycle != nodeidentity.NodeLifecycleJoining || node.Schedulable || len(node.Roles) != 1 || node.Roles[0] != nodeidentity.RoleWorker {
		t.Fatalf("new worker was not safely unschedulable: %#v", node)
	}
	if _, err := store.EnrollWorker(request); err == nil {
		t.Fatal("join token replay was accepted")
	}
	state, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if state.Generation != 4 || len(state.JoinTokens) != 1 || state.JoinTokens[0].ConsumedAt.IsZero() {
		t.Fatalf("unexpected durable token state: %#v", state)
	}
	controller, ok := state.ActiveNode(state.ControllerNodeID)
	if !ok || controller.SSHHost == "" || controller.SSHUser == "" || controller.SSHHostKey == "" || controller.SSHHostKeyFingerprint == "" {
		t.Fatalf("first worker enrollment did not publish pinned controller access: %#v", controller)
	}
}

func TestMembershipLifecycleRevokesAllocationsAndRetainsTombstone(t *testing.T) {
	store, _, now := newTestMembershipStore(t)
	issued, _ := store.CreateJoinToken(membershipWorkerID, 10*time.Minute)
	request := testEnrollment(t, store, issued.Token, membershipWorkerID, now)
	if _, err := store.EnrollWorker(request); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkSchedulable(membershipWorkerID); err == nil {
		t.Fatal("joining worker skipped ready transition")
	}
	if _, err := store.MarkReady(membershipWorkerID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkSchedulable(membershipWorkerID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Cordon(membershipWorkerID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginDrain(membershipWorkerID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PrepareRemoval(membershipWorkerID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkRemovalPeersRevoked(membershipWorkerID); err != nil {
		t.Fatal(err)
	}
	tombstone, err := store.Remove(membershipWorkerID)
	if err != nil {
		t.Fatal(err)
	}
	if tombstone.RevokedMeshCredentialID == "" || tombstone.AllocationKeyFingerprint == "" {
		t.Fatalf("removal omitted revocation evidence: %#v", tombstone)
	}
	state, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if state.node(membershipWorkerID) != nil || !state.tombstoned(membershipWorkerID) {
		t.Fatalf("removed node was not permanently tombstoned: %#v", state)
	}
	if _, err := store.CreateJoinToken(membershipWorkerID, time.Minute); err == nil {
		t.Fatal("tombstoned node ID was reusable")
	}
}

func TestMembershipRefusesFinalControllerRemoval(t *testing.T) {
	store, installation, _ := newTestMembershipStore(t)
	if _, err := store.Cordon(installation.NodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginDrain(installation.NodeID); err == nil || !strings.Contains(err.Error(), "final controller") {
		t.Fatalf("final controller drain error = %v", err)
	}
}

func TestMembershipJoinTokenConsumptionIsAtomicAcrossStoreInstances(t *testing.T) {
	store, _, now := newTestMembershipStore(t)
	issued, _ := store.CreateJoinToken(membershipWorkerID, 10*time.Minute)
	second, err := NewMembershipStore(store.path, store.inventoryPath)
	if err != nil {
		t.Fatal(err)
	}
	second.now = store.now
	request := testEnrollment(t, store, issued.Token, membershipWorkerID, now)
	var successes int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, current := range []*MembershipStore{store, second} {
		wg.Add(1)
		go func(candidate *MembershipStore) {
			defer wg.Done()
			if _, err := candidate.EnrollWorker(request); err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}(current)
	}
	wg.Wait()
	if successes != 1 {
		t.Fatalf("concurrent enrollment successes = %d, want 1", successes)
	}
}

func TestMembershipExpiredTokenFailsClosed(t *testing.T) {
	store, _, now := newTestMembershipStore(t)
	issued, _ := store.CreateJoinToken(membershipWorkerID, time.Minute)
	store.now = func() time.Time { return now.Add(2 * time.Minute) }
	if _, err := store.ReserveJoinToken(issued.Token, membershipWorkerID); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired token error = %v", err)
	}
}

func TestMembershipRemovalPhasesResumeAfterTombstone(t *testing.T) {
	store, _, now := newTestMembershipStore(t)
	issued, _ := store.CreateJoinToken(membershipWorkerID, 10*time.Minute)
	_, _ = store.EnrollWorker(testEnrollment(t, store, issued.Token, membershipWorkerID, now))
	_, _ = store.MarkReady(membershipWorkerID)
	_, _ = store.Cordon(membershipWorkerID)
	_, _ = store.BeginDrain(membershipWorkerID)
	first, err := store.PrepareRemoval(membershipWorkerID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.PrepareRemoval(membershipWorkerID)
	if err != nil || second.Phase != first.Phase {
		t.Fatalf("prepare removal was not idempotent: %#v err=%v", second, err)
	}
	_, _ = store.MarkRemovalPeersRevoked(membershipWorkerID)
	if _, err := store.Remove(membershipWorkerID); err != nil {
		t.Fatal(err)
	}
	state, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	operation, ok := state.RemovalOperation(membershipWorkerID)
	if !ok || operation.Phase != RemovalPhaseCleanupTarget || operation.Node.SSHHost == "" {
		t.Fatalf("target cleanup was not resumable: %#v", operation)
	}
	if _, err := store.Remove(membershipWorkerID); err != nil {
		t.Fatalf("membership removal retry failed: %v", err)
	}
	if err := store.CompleteRemoval(membershipWorkerID); err != nil {
		t.Fatal(err)
	}
}
