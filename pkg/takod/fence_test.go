package takod

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/recovery"
)

const testOperationHolderToken = "test-operation-holder-token"

func bindTestOperationHolder(t *testing.T, fence *nodeidentity.OperationFence) string {
	t.Helper()
	hash, err := nodeidentity.OperationHolderTokenHash(testOperationHolderToken)
	if err != nil {
		t.Fatal(err)
	}
	fence.HolderTokenHash = hash
	return testOperationHolderToken
}

func TestControllerOperationAuthorityIsIdempotentExclusiveAndMonotonic(t *testing.T) {
	server := lifecycleTestServer(t, nodeidentity.NodeLifecycleSchedulable, true)
	nodeID := server.installation.NodeID
	request := LeaseRequest{Project: "demo", Environment: "production", Operation: "deploy", Who: "test", PID: 123, RequestID: "retry-key", TargetNodeIDs: []string{nodeID}, TTLSeconds: 60}
	first, err := server.acquireControllerOperationLease(context.Background(), request)
	if err != nil || !first.Acquired || first.Lease == nil || first.Lease.Fence == nil {
		t.Fatalf("first authority = %#v, %v", first, err)
	}
	if !strings.HasPrefix(first.Lease.ID, "op-") || first.Lease.ID == request.RequestID || first.Lease.Fence.Token != 1 {
		t.Fatalf("controller did not issue operation identity/token: %#v", first.Lease)
	}
	retry, err := server.acquireControllerOperationLease(context.Background(), request)
	if err != nil || !retry.Acquired || retry.Lease.ID != first.Lease.ID || retry.Lease.Fence.Token != first.Lease.Fence.Token {
		t.Fatalf("idempotent retry = %#v, %v", retry, err)
	}
	if !operationFencesEqual(*retry.Lease.Fence, *first.Lease.Fence) {
		t.Fatal("idempotent retry changed the exact signed fence")
	}
	changed := request
	changed.Operation = "placement-apply"
	if _, err := server.acquireControllerOperationLease(context.Background(), changed); err == nil || !strings.Contains(err.Error(), "immutable scope") {
		t.Fatalf("same request ID changed operation: %v", err)
	}
	contender := request
	contender.RequestID = "other-writer"
	blocked, err := server.acquireControllerOperationLease(context.Background(), contender)
	if err != nil || blocked.Acquired {
		t.Fatalf("partitioned writer was not blocked: %#v, %v", blocked, err)
	}
	if blocked.Lease != nil && blocked.Lease.Fence != nil {
		t.Fatal("blocked contender received the active signed fence")
	}
	status := httptest.NewRecorder()
	server.handleFenceStatus(status, httptest.NewRequest(http.MethodGet, "/v1/fence?project=demo&environment=production", nil))
	if strings.Contains(status.Body.String(), first.Lease.Fence.Signature) || strings.Contains(status.Body.String(), first.HolderToken) {
		t.Fatalf("operation status exposed bearer authority: %s", status.Body.String())
	}
	if _, err := server.releaseControllerOperationLease(context.Background(), LeaseRequest{Project: request.Project, Environment: request.Environment, ID: first.Lease.ID}); err == nil || !strings.Contains(err.Error(), "signed active") {
		t.Fatalf("unsigned release admitted: %v", err)
	}
	if _, err := server.releaseControllerOperationLease(context.Background(), LeaseRequest{Project: request.Project, Environment: request.Environment, ID: first.Lease.ID, Fence: first.Lease.Fence}); err == nil || !strings.Contains(err.Error(), "holder credential") {
		t.Fatalf("release without holder credential admitted: %v", err)
	}
	forgedFence := *first.Lease.Fence
	forgedFence.Operation = "placement-apply"
	if _, err := server.releaseControllerOperationLease(context.Background(), LeaseRequest{Project: request.Project, Environment: request.Environment, ID: first.Lease.ID, Fence: &forgedFence, HolderToken: first.HolderToken}); err == nil || !strings.Contains(err.Error(), "exact active") {
		t.Fatalf("forged release admitted: %v", err)
	}
	stillBlocked, err := server.acquireControllerOperationLease(context.Background(), contender)
	if err != nil || stillBlocked.Acquired {
		t.Fatalf("failed release cleared authority: %#v, %v", stillBlocked, err)
	}
	if _, err := server.releaseControllerOperationLease(context.Background(), LeaseRequest{Project: request.Project, Environment: request.Environment, ID: first.Lease.ID, Fence: first.Lease.Fence, HolderToken: first.HolderToken}); err != nil {
		t.Fatal(err)
	}
	if _, err := server.acquireControllerOperationLease(context.Background(), changed); err == nil || !strings.Contains(err.Error(), "durably bound") {
		t.Fatalf("completed request ID changed immutable scope: %v", err)
	}
	second, err := server.acquireControllerOperationLease(context.Background(), contender)
	if err != nil || !second.Acquired || second.Lease.Fence.Token != 2 || second.Lease.ID == first.Lease.ID {
		t.Fatalf("next authority = %#v, %v", second, err)
	}
}

func TestControllerOperationAuthoritySerializesDifferentProjectsGlobally(t *testing.T) {
	server := lifecycleTestServer(t, nodeidentity.NodeLifecycleSchedulable, true)
	target := []string{server.installation.NodeID}
	firstRequest := LeaseRequest{Project: "alpha", Environment: "production", Operation: "deploy", Who: "test", PID: 1, RequestID: "alpha-op", TargetNodeIDs: target, TTLSeconds: 60}
	first, err := server.acquireControllerOperationLease(context.Background(), firstRequest)
	if err != nil || first.Lease == nil || first.Lease.Fence == nil {
		t.Fatalf("first operation = %#v, %v", first, err)
	}
	secondRequest := LeaseRequest{Project: "beta", Environment: "production", Operation: "deploy", Who: "test", PID: 2, RequestID: "beta-op", TargetNodeIDs: target, TTLSeconds: 60}
	blocked, err := server.acquireControllerOperationLease(context.Background(), secondRequest)
	if err != nil || blocked.Acquired || blocked.Lease == nil || blocked.Lease.ID != first.Lease.ID {
		t.Fatalf("cross-project operation was not globally blocked: %#v, %v", blocked, err)
	}
	if _, err := server.releaseControllerOperationLease(context.Background(), LeaseRequest{Project: "alpha", Environment: "production", ID: first.Lease.ID, Fence: first.Lease.Fence, HolderToken: first.HolderToken}); err != nil {
		t.Fatal(err)
	}
	second, err := server.acquireControllerOperationLease(context.Background(), secondRequest)
	if err != nil || !second.Acquired {
		t.Fatalf("second operation after release = %#v, %v", second, err)
	}
}

func TestControllerRenewalDoesNotExtendBackingLeaseBeforeAuthorityValidation(t *testing.T) {
	server := lifecycleTestServer(t, nodeidentity.NodeLifecycleSchedulable, true)
	request := LeaseRequest{Project: "demo", Environment: "production", Operation: "deploy", Who: "test", PID: 1, RequestID: "renew-order", TargetNodeIDs: []string{server.installation.NodeID}, TTLSeconds: 60}
	first, err := server.acquireControllerOperationLease(context.Background(), request)
	if err != nil || first.Lease == nil || first.Lease.Fence == nil {
		t.Fatalf("first authority = %#v, %v", first, err)
	}
	before, err := ReadLease(context.Background(), server.dataDir, request)
	if err != nil || before.Lease == nil {
		t.Fatalf("backing lease before renewal = %#v, %v", before, err)
	}
	inventory, err := nodeidentity.ReadInventory(server.inventoryFile)
	if err != nil {
		t.Fatal(err)
	}
	inventory.Generation++
	inventory.UpdatedAt = time.Now().UTC()
	for index := range inventory.Nodes {
		inventory.Nodes[index].UpdatedAt = inventory.UpdatedAt
	}
	if err := nodeidentity.ReplaceInventory(server.inventoryFile, *inventory); err != nil {
		t.Fatal(err)
	}
	renew := request
	renew.ID, renew.Renew, renew.Fence, renew.HolderToken, renew.TTLSeconds = first.Lease.ID, true, first.Lease.Fence, first.HolderToken, 600
	if _, err := server.acquireControllerOperationLease(context.Background(), renew); err == nil {
		t.Fatal("membership-changing renewal was accepted")
	}
	after, err := ReadLease(context.Background(), server.dataDir, request)
	if err != nil || after.Lease == nil || !after.Lease.ExpiresAt.Equal(before.Lease.ExpiresAt) {
		t.Fatalf("failed renewal changed backing expiry: before=%#v after=%#v err=%v", before.Lease, after.Lease, err)
	}
}

func TestControllerRenewalRecoversCommittedFenceFromImmediatePredecessor(t *testing.T) {
	server := lifecycleTestServer(t, nodeidentity.NodeLifecycleSchedulable, true)
	request := LeaseRequest{Project: "demo", Environment: "production", Operation: "deploy", Who: "test", PID: 1, RequestID: "renew-recovery", TargetNodeIDs: []string{server.installation.NodeID}, TTLSeconds: 60}
	first, err := server.acquireControllerOperationLease(context.Background(), request)
	if err != nil || first.Lease == nil || first.Lease.Fence == nil {
		t.Fatalf("first authority = %#v, %v", first, err)
	}
	renew := request
	renew.ID, renew.Renew, renew.Fence, renew.HolderToken, renew.TTLSeconds = first.Lease.ID, true, first.Lease.Fence, first.HolderToken, 120
	renewed, err := server.acquireControllerOperationLease(context.Background(), renew)
	if err != nil || renewed.Lease == nil || renewed.Lease.Fence == nil || operationFencesEqual(*renewed.Lease.Fence, *first.Lease.Fence) {
		t.Fatalf("renewed authority = %#v, %v", renewed, err)
	}
	recovered, err := server.acquireControllerOperationLease(context.Background(), renew)
	if err != nil || recovered.Lease == nil || recovered.Lease.Fence == nil || !operationFencesEqual(*recovered.Lease.Fence, *renewed.Lease.Fence) {
		t.Fatalf("predecessor recovery = %#v, %v", recovered, err)
	}
	backing, err := ReadLease(context.Background(), server.dataDir, request)
	if err != nil || backing.Lease == nil || !backing.Lease.ExpiresAt.Equal(renewed.Lease.Fence.ExpiresAt) {
		t.Fatalf("reconciled backing lease = %#v, %v", backing, err)
	}
	if _, err := server.releaseControllerOperationLease(context.Background(), LeaseRequest{Project: request.Project, Environment: request.Environment, ID: first.Lease.ID, Fence: first.Lease.Fence, HolderToken: first.HolderToken}); err != nil {
		t.Fatalf("release with authenticated predecessor: %v", err)
	}
}

func TestMutationAndRevocationRequireExactCryptographicallyVerifiedFence(t *testing.T) {
	server := lifecycleTestServer(t, nodeidentity.NodeLifecycleSchedulable, true)
	request := LeaseRequest{Project: "demo", Environment: "production", Operation: "deploy", Who: "test", PID: 123, RequestID: "exact-fence", TargetNodeIDs: []string{server.installation.NodeID}, TTLSeconds: 60}
	lease, err := server.acquireControllerOperationLease(context.Background(), request)
	if err != nil || lease.Lease == nil || lease.Lease.Fence == nil {
		t.Fatalf("acquire: %#v %v", lease, err)
	}
	fence := *lease.Lease.Fence
	if _, err := server.activateWorkerFence(fence, "wrong-holder"); err == nil || !strings.Contains(err.Error(), "holder credential") {
		t.Fatalf("activation with wrong holder credential admitted: %v", err)
	}
	if _, err := server.activateWorkerFence(fence, lease.HolderToken); err != nil {
		t.Fatal(err)
	}
	validData, _ := json.Marshal(fence)
	missingHolder := httptest.NewRequest(http.MethodPost, "/v1/reconcile-service?project=demo&environment=production", nil)
	missingHolder.Header.Set(OperationFenceHeader, base64.RawURLEncoding.EncodeToString(validData))
	if _, err := server.validateMutationFence(missingHolder); err == nil || !strings.Contains(err.Error(), "holder credential") {
		t.Fatalf("mutation without holder credential admitted: %v", err)
	}
	forged := fence
	forged.Operation = "placement-apply"
	data, _ := json.Marshal(forged)
	httpRequest := httptest.NewRequest(http.MethodPost, "/v1/reconcile-service", nil)
	httpRequest.Header.Set(OperationFenceHeader, base64.RawURLEncoding.EncodeToString(data))
	httpRequest.Header.Set(OperationHolderHeader, lease.HolderToken)
	if _, err := server.validateMutationFence(httpRequest); err == nil || !strings.Contains(err.Error(), "exact active") {
		t.Fatalf("tampered operation admitted: %v", err)
	}
	if _, err := server.revokeWorkerFenceWithFence(forged, lease.HolderToken); err == nil || !strings.Contains(err.Error(), "exact active") {
		t.Fatalf("tampered revocation admitted: %v", err)
	}
	if _, err := server.revokeWorkerFenceWithFence(fence, "wrong-holder"); err == nil || !strings.Contains(err.Error(), "holder credential") {
		t.Fatalf("revocation with wrong holder credential admitted: %v", err)
	}
	data, _ = json.Marshal(fence)
	httpRequest = httptest.NewRequest(http.MethodPost, "/v1/reconcile-service?project=other&environment=production", nil)
	httpRequest.Header.Set(OperationFenceHeader, base64.RawURLEncoding.EncodeToString(data))
	httpRequest.Header.Set(OperationHolderHeader, lease.HolderToken)
	if _, err := server.validateMutationFence(httpRequest); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("cross-project query admitted: %v", err)
	}
}

func TestWorkerFenceRejectsReplayAfterHigherToken(t *testing.T) {
	server := lifecycleTestServer(t, nodeidentity.NodeLifecycleSchedulable, true)
	inventory, err := nodeidentity.ReadInventory(server.inventoryFile)
	if err != nil {
		t.Fatal(err)
	}
	makeFence := func(token uint64, operation string) nodeidentity.OperationFence {
		fence := nodeidentity.OperationFence{Kind: nodeidentity.OperationFenceKind, ClusterID: inventory.ClusterID, ControllerNodeID: inventory.ControllerNodeID, MembershipGeneration: inventory.Generation, Project: "demo", Environment: "production", OperationID: operation, Operation: "deploy", Token: token, TargetNodeIDs: []string{server.installation.NodeID}, IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
		bindTestOperationHolder(t, &fence)
		if err := nodeidentity.SignOperationFence(&fence, server.installation); err != nil {
			t.Fatal(err)
		}
		return fence
	}
	first, second := makeFence(1, "op-first"), makeFence(2, "op-second")
	if _, err := server.activateWorkerFence(first, testOperationHolderToken); err != nil {
		t.Fatal(err)
	}
	if _, err := server.revokeWorkerFenceWithFence(first, testOperationHolderToken); err != nil {
		t.Fatal(err)
	}
	if _, err := server.activateWorkerFence(first, testOperationHolderToken); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked fence replay error = %v", err)
	}
	if _, err := server.activateWorkerFence(second, testOperationHolderToken); err != nil {
		t.Fatal(err)
	}
	if _, err := server.activateWorkerFence(first, testOperationHolderToken); err == nil || !strings.Contains(err.Error(), "high-water") {
		t.Fatalf("stale fence replay error = %v", err)
	}
}

func TestWorkerFenceRenewalAcceptsPreviousGrantUntilRevocation(t *testing.T) {
	server := lifecycleTestServer(t, nodeidentity.NodeLifecycleSchedulable, true)
	inventory, err := nodeidentity.ReadInventory(server.inventoryFile)
	if err != nil {
		t.Fatal(err)
	}
	issued := time.Now().UTC().Add(-time.Second)
	first := nodeidentity.OperationFence{Kind: nodeidentity.OperationFenceKind, ClusterID: inventory.ClusterID, ControllerNodeID: inventory.ControllerNodeID, MembershipGeneration: inventory.Generation, Project: "demo", Environment: "production", OperationID: "op-renew", Operation: "deploy", Token: 1, TargetNodeIDs: []string{server.installation.NodeID}, IssuedAt: issued, ExpiresAt: issued.Add(2 * time.Second)}
	bindTestOperationHolder(t, &first)
	if err := nodeidentity.SignOperationFence(&first, server.installation); err != nil {
		t.Fatal(err)
	}
	if _, err := server.activateWorkerFence(first, testOperationHolderToken); err != nil {
		t.Fatal(err)
	}
	renewed := first
	renewed.IssuedAt = issued.Add(time.Second)
	renewed.ExpiresAt = issued.Add(2 * time.Minute)
	if err := nodeidentity.SignOperationFence(&renewed, server.installation); err != nil {
		t.Fatal(err)
	}
	if _, err := server.activateWorkerFence(renewed, testOperationHolderToken); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/reconcile-service?project=demo&environment=production", nil)
	data, _ := json.Marshal(first)
	request.Header.Set(OperationFenceHeader, base64.RawURLEncoding.EncodeToString(data))
	request.Header.Set(OperationHolderHeader, testOperationHolderToken)
	if _, err := server.validateMutationFence(request); err != nil {
		t.Fatalf("previous fence rejected during renewal overlap: %v", err)
	}
	if _, err := server.activateWorkerFence(first, testOperationHolderToken); err == nil || !strings.Contains(err.Error(), "monotonic") {
		t.Fatalf("older renewal admitted: %v", err)
	}
	if delay := time.Until(first.ExpiresAt) + 20*time.Millisecond; delay > 0 {
		time.Sleep(delay)
	}
	if _, err := server.revokeWorkerFenceWithFence(first, testOperationHolderToken); err != nil {
		t.Fatal(err)
	}
	if _, err := server.validateMutationFence(request); err == nil {
		t.Fatal("previous fence remained valid after revocation")
	}
}

func TestControllerOperationJournalUsesSnapshotAuthorityPath(t *testing.T) {
	server := lifecycleTestServer(t, nodeidentity.NodeLifecycleSchedulable, true)
	request := LeaseRequest{Project: "demo", Environment: "production", Operation: "deploy", Who: "test", PID: 1, RequestID: "snapshot-path", TargetNodeIDs: []string{server.installation.NodeID}, TTLSeconds: 60}
	lease, err := server.acquireControllerOperationLease(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	path, err := controlOperationStatePath(server.dataDir, request.Project, request.Environment)
	if err != nil || path != filepath.Join(server.dataDir, "control", "control-operation.json") {
		t.Fatalf("operation journal path = %q, %v", path, err)
	}
	unlock, err := recovery.AcquireSnapshotLock(server.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.EnsureNoActiveControllerOperation(server.dataDir); err == nil {
		unlock()
		t.Fatal("snapshot did not observe the real controller operation journal")
	}
	unlock()
	if _, err := server.releaseControllerOperationLease(context.Background(), LeaseRequest{Project: request.Project, Environment: request.Environment, ID: lease.Lease.ID, Fence: lease.Lease.Fence, HolderToken: lease.HolderToken}); err != nil {
		t.Fatal(err)
	}
}

func TestActiveWorkerFenceBlocksNodeGlobalMaintenance(t *testing.T) {
	server := lifecycleTestServer(t, nodeidentity.NodeLifecycleSchedulable, true)
	inventory, err := nodeidentity.ReadInventory(server.inventoryFile)
	if err != nil {
		t.Fatal(err)
	}
	fence := nodeidentity.OperationFence{Kind: nodeidentity.OperationFenceKind, ClusterID: inventory.ClusterID, ControllerNodeID: inventory.ControllerNodeID, MembershipGeneration: inventory.Generation, Project: "demo", Environment: "production", OperationID: "op-maintenance", Operation: "deploy", Token: 1, TargetNodeIDs: []string{server.installation.NodeID}, IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Minute)}
	bindTestOperationHolder(t, &fence)
	if err := nodeidentity.SignOperationFence(&fence, server.installation); err != nil {
		t.Fatal(err)
	}
	if _, err := server.activateWorkerFence(fence, testOperationHolderToken); err != nil {
		t.Fatal(err)
	}
	acquired := make(chan func(), 1)
	go func() {
		unlock, err := recovery.AcquireMaintenanceBarrier(server.dataDir)
		if err != nil {
			acquired <- nil
			return
		}
		acquired <- unlock
	}()
	select {
	case unlock := <-acquired:
		if unlock != nil {
			unlock()
		}
		t.Fatal("node-global maintenance overlapped active worker operation")
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := server.revokeWorkerFenceWithFence(fence, testOperationHolderToken); err != nil {
		t.Fatal(err)
	}
	select {
	case unlock := <-acquired:
		if unlock == nil {
			t.Fatal("maintenance barrier acquisition failed")
		}
		unlock()
	case <-time.After(time.Second):
		t.Fatal("maintenance barrier did not release after operation revocation")
	}
}

func TestControllerOperationStatusReconcilesExpiredRecord(t *testing.T) {
	server := lifecycleTestServer(t, nodeidentity.NodeLifecycleSchedulable, true)
	path, err := controlOperationStatePath(server.dataDir, "demo", "production")
	if err != nil {
		t.Fatal(err)
	}
	state := controlOperationState{SchemaVersion: 1, NextToken: 4, Active: &controlOperationRecord{RequestID: "retry", Fence: nodeidentity.OperationFence{OperationID: "op-expired", ExpiresAt: time.Now().Add(-time.Minute)}, Phase: "mutating"}}
	if err := writeJSONAtomic(path, state, 0600); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.handleFenceStatus(recorder, httptest.NewRequest(http.MethodGet, "/v1/fence?project=demo&environment=production", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "expired-reconciled") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	persisted, err := readControlOperationState(filepath.Clean(path))
	if err != nil || persisted.Active != nil || len(persisted.History) != 1 {
		t.Fatalf("reconciled state = %#v, %v", persisted, err)
	}
}

func TestSignedInventoryAcceptsAllocationAdvanceAndRejectsRollback(t *testing.T) {
	server := lifecycleTestServer(t, nodeidentity.NodeLifecycleSchedulable, true)
	current, err := nodeidentity.ReadInventory(server.inventoryFile)
	if err != nil {
		t.Fatal(err)
	}
	advanced := *current
	advanced.AllocationGeneration = 1
	snapshot := nodeidentity.SignedInventorySnapshot{Kind: nodeidentity.SignedInventoryKind, Inventory: advanced, IssuedAt: time.Now().UTC()}
	if err := nodeidentity.SignInventorySnapshot(&snapshot, server.installation); err != nil {
		t.Fatal(err)
	}
	if err := server.acceptSignedInventory(snapshot); err != nil {
		t.Fatal(err)
	}
	rollback := nodeidentity.SignedInventorySnapshot{Kind: nodeidentity.SignedInventoryKind, Inventory: *current, IssuedAt: time.Now().UTC()}
	if err := nodeidentity.SignInventorySnapshot(&rollback, server.installation); err != nil {
		t.Fatal(err)
	}
	if err := server.acceptSignedInventory(rollback); err == nil || !strings.Contains(err.Error(), "does not advance") {
		t.Fatalf("allocation authority rollback error = %v", err)
	}
}
