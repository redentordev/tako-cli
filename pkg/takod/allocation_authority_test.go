package takod

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/platform"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

func TestAbandonedAllocationPublicationRecoversMonotonicallyBeforeNextPrepare(t *testing.T) {
	server := lifecycleTestServer(t, nodeidentity.NodeLifecycleSchedulable, true)
	if err := os.Remove(server.inventoryFile); err != nil {
		t.Fatal(err)
	}
	server.membershipFile = filepath.Join(server.dataDir, "control", platform.DefaultMembershipName)
	store, err := platform.NewMembershipStore(server.membershipFile, server.inventoryFile)
	if err != nil {
		t.Fatal(err)
	}
	installation := *server.installation
	installation.EnrollmentRoles = []string{nodeidentity.RoleControlPlane, nodeidentity.RoleWorker}
	if _, err := store.InitializeFirstNode(installation, "10.42.0.0/24", "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=", "worker.example"); err != nil {
		t.Fatal(err)
	}
	firstRequest := LeaseRequest{Project: "demo", Environment: "production", Operation: "deploy", Who: "test", PID: 1, RequestID: "allocation-first", TargetNodeIDs: []string{server.installation.NodeID}, TTLSeconds: 60}
	first, err := server.acquireControllerOperationLease(context.Background(), firstRequest)
	if err != nil || first.Lease == nil || first.Lease.Fence == nil {
		t.Fatalf("first operation = %#v, %v", first, err)
	}
	firstFence := *first.Lease.Fence
	if _, err := server.activateWorkerFence(firstFence, first.HolderToken); err != nil {
		t.Fatal(err)
	}
	firstContext := withOperationFence(context.Background(), firstFence)
	inventory, err := nodeidentity.ReadInventory(server.inventoryFile)
	if err != nil {
		t.Fatal(err)
	}
	node, _ := inventory.Node(server.installation.NodeID)
	proof := PortAllocationResponse{
		Kind: PortAllocationKindMeshUpstream, Project: "demo", Environment: "production", Service: "web", Slot: 1,
		HostIP: node.MeshIP, HostPort: 20000, ContainerPort: 3000, Key: "mesh-upstream/demo/production/web/1",
		ClusterID: server.installation.ClusterID, NodeID: server.installation.NodeID, Generation: 1, IssuedAt: firstFence.IssuedAt,
		OperationID: firstFence.OperationID, FenceToken: firstFence.Token,
	}
	if err := SignPortAllocation(&proof, server.installation); err != nil {
		t.Fatal(err)
	}
	allocation := nodeidentity.ActiveAllocation{
		Kind: proof.Kind, Project: proof.Project, Environment: proof.Environment, Service: proof.Service, Slot: proof.Slot,
		HostIP: proof.HostIP, HostPort: proof.HostPort, ContainerPort: proof.ContainerPort, Key: proof.Key,
		ClusterID: proof.ClusterID, NodeID: proof.NodeID, Generation: proof.Generation, IssuedAt: proof.IssuedAt,
		OperationID: proof.OperationID, FenceToken: proof.FenceToken, Signature: proof.Signature,
	}
	prepared, err := server.prepareAllocationAuthorization(store, AllocationAuthorizationRequest{Project: "demo", Environment: "production", Allocations: []nodeidentity.ActiveAllocation{allocation}}, firstFence)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.trackAllocationPublicationTarget(firstContext, AllocationAuthorizationRequest{Project: "demo", Environment: "production", ProposalID: prepared.ProposalID, TargetNodeID: server.installation.NodeID}); err != nil {
		t.Fatal(err)
	}
	// Simulate a client death after its durable publication intent (and
	// possibly the edge write) but before acknowledgement/controller commit.
	if _, err := server.revokeWorkerFenceWithFence(firstFence, first.HolderToken); err != nil {
		t.Fatal(err)
	}
	if _, err := server.releaseControllerOperationLease(context.Background(), LeaseRequest{Project: "demo", Environment: "production", ID: first.Lease.ID, Fence: first.Lease.Fence, HolderToken: first.HolderToken}); err != nil {
		t.Fatal(err)
	}
	secondRequest := firstRequest
	secondRequest.RequestID = "allocation-second"
	second, err := server.acquireControllerOperationLease(context.Background(), secondRequest)
	if err != nil || second.Lease == nil || second.Lease.Fence == nil {
		t.Fatalf("second operation = %#v, %v", second, err)
	}
	secondFence := *second.Lease.Fence
	if _, err := server.activateWorkerFence(secondFence, second.HolderToken); err != nil {
		t.Fatal(err)
	}
	secondContext := withOperationFence(context.Background(), secondFence)
	if _, err := server.prepareAllocationAuthorization(store, AllocationAuthorizationRequest{Project: "demo", Environment: "production"}, secondFence); err == nil || !strings.Contains(err.Error(), "must be recovered") {
		t.Fatalf("new proposal bypassed abandoned recovery: %v", err)
	}
	recovered, err := server.recoverAllocationAuthorization(secondContext, store)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.Recovered || recovered.Snapshot.Inventory.AllocationGeneration <= prepared.Snapshot.Inventory.AllocationGeneration || len(recovered.RecoveryTargetNodeIDs) != 1 {
		t.Fatalf("recovery = %#v", recovered)
	}
	if _, err := server.finalizeAllocationRecovery(secondContext, AllocationAuthorizationRequest{ProposalID: recovered.ProposalID}); err == nil || !strings.Contains(err.Error(), "unacknowledged") {
		t.Fatalf("unacknowledged recovery finalized: %v", err)
	}
	if _, err := server.acknowledgeAllocationRecovery(secondContext, AllocationAuthorizationRequest{ProposalID: recovered.ProposalID, TargetNodeID: server.installation.NodeID}); err != nil {
		t.Fatal(err)
	}
	if _, err := server.finalizeAllocationRecovery(secondContext, AllocationAuthorizationRequest{ProposalID: recovered.ProposalID}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(server.pendingAllocationAuthorizationPath("", "")); !os.IsNotExist(err) {
		t.Fatalf("pending recovery still exists: %v", err)
	}
}

func TestProxyManifestFilenameIsBoundToManifestOwnership(t *testing.T) {
	host := runtimeid.ContainerAlias("demo", "production", "web", 1)
	url := "http://" + host + ":3000"
	manifest := ProxyRouteManifest{Version: 2, Project: "demo", Environment: "production", Routes: []ProxyRoute{{
		Service: "web", Domains: []string{"app.example.com"}, Upstreams: []string{url}, Destinations: []ProxyDestination{{
			Kind: ProxyDestinationRuntimeAlias, URL: url, Project: "demo", Environment: "production", Service: "web", Slot: 1, ContainerPort: 3000, HostPort: 3000,
		}},
	}}}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{}
	if err := server.authorizeProxyFileCandidate(runtimeid.ProxyConfigFileName("other", "production"), string(data)); err == nil || !strings.Contains(err.Error(), "ownership") {
		t.Fatalf("cross-project proxy filename error = %v", err)
	}
	if err := server.authorizeProxyFileCandidate(runtimeid.ProxyConfigFileName("demo", "production"), string(data)); err != nil {
		t.Fatal(err)
	}
}
