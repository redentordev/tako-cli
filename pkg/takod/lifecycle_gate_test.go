package takod

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

func TestEnrolledLifecycleGateHonorsDurableDeploymentDenyLatch(t *testing.T) {
	server := lifecycleTestServer(t, nodeidentity.NodeLifecycleSchedulable, true)
	if err := os.WriteFile(nodeidentity.DeploymentDenyPath(server.inventoryFile), []byte("cordon pending\n"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	handler := server.enrolledLifecycleHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/reconcile-service", nil))
	if recorder.Code != http.StatusConflict || calls != 0 {
		t.Fatalf("durable deny latch was bypassed: status=%d calls=%d", recorder.Code, calls)
	}
}

func TestPlacementFenceOnDrainingNodeAllowsCleanupButNotCreation(t *testing.T) {
	server := lifecycleTestServer(t, nodeidentity.NodeLifecycleDraining, false)
	inventory, err := nodeidentity.ReadInventory(server.inventoryFile)
	if err != nil {
		t.Fatal(err)
	}
	fence := nodeidentity.OperationFence{Kind: nodeidentity.OperationFenceKind, ClusterID: inventory.ClusterID, ControllerNodeID: inventory.ControllerNodeID, MembershipGeneration: inventory.Generation, Project: "demo", Environment: "production", OperationID: "op-move", Operation: "placement-apply", Token: 1, TargetNodeIDs: []string{server.installation.NodeID}, IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	bindTestOperationHolder(t, &fence)
	if err := nodeidentity.SignOperationFence(&fence, server.installation); err != nil {
		t.Fatal(err)
	}
	if _, err := server.activateWorkerFence(fence, testOperationHolderToken); err != nil {
		t.Fatal(err)
	}
	fenceJSON, _ := json.Marshal(fence)
	body := `{"project":"demo","environment":"production","service":"web","image":"demo/web:1","network":"tako-demo-production","containers":[{"name":"new-container"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/reconcile-service", bytes.NewBufferString(body))
	request.Header.Set(OperationFenceHeader, base64.RawURLEncoding.EncodeToString(fenceJSON))
	request.Header.Set(OperationHolderHeader, testOperationHolderToken)
	recorder := httptest.NewRecorder()
	server.enrolledLifecycleHandler(http.HandlerFunc(server.handleReconcileService)).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "cannot create containers") {
		t.Fatalf("draining placement creation status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestEnrolledLifecycleGateBlocksWorkloadMutationUntilSchedulable(t *testing.T) {
	installation, err := nodeidentity.New("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "worker-1", []string{nodeidentity.RoleWorker}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "inventory.json")
	now := time.Now().UTC()
	meshKey := "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
	meshCredentialID, _ := nodeidentity.MeshCredentialID(meshKey)
	inventory := nodeidentity.ClusterInventory{
		APIVersion: nodeidentity.InventoryAPIVersion, Kind: nodeidentity.InventoryKind, ClusterID: installation.ClusterID,
		Generation: 1, ControllerNodeID: installation.NodeID, MeshCIDR: "10.42.0.0/24", UpdatedAt: now,
		Nodes: []nodeidentity.InventoryNode{{
			NodeID: installation.NodeID, NodeName: installation.NodeName, Lifecycle: nodeidentity.NodeLifecycleReady,
			Roles: []string{nodeidentity.RoleControlPlane, nodeidentity.RoleWorker}, Schedulable: false,
			MeshIP: "10.42.0.2", MeshEndpoint: "worker.example", MeshCredentialID: meshCredentialID, MeshPublicKey: meshKey, MeshCredentialStatus: nodeidentity.MeshCredentialActive,
			AllocationPublicKey: installation.AllocationPublicKey, JoinedAt: now, UpdatedAt: now,
		}},
	}
	if err := nodeidentity.CreateInventory(path, inventory); err != nil {
		t.Fatal(err)
	}
	server := &Server{installation: installation, inventoryFile: path, dataDir: t.TempDir()}
	calls := 0
	handler := server.enrolledLifecycleHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/reconcile-service", nil))
	if recorder.Code != http.StatusConflict || calls != 0 {
		t.Fatalf("ready mutation status=%d calls=%d", recorder.Code, calls)
	}
	inventory.Generation++
	inventory.UpdatedAt = now.Add(time.Second)
	inventory.Nodes[0].Lifecycle = nodeidentity.NodeLifecycleSchedulable
	inventory.Nodes[0].Schedulable = true
	inventory.Nodes[0].UpdatedAt = inventory.UpdatedAt
	if err := nodeidentity.ReplaceInventory(path, inventory); err != nil {
		t.Fatal(err)
	}
	fence := nodeidentity.OperationFence{
		Kind: nodeidentity.OperationFenceKind, ClusterID: installation.ClusterID, ControllerNodeID: installation.NodeID,
		MembershipGeneration: inventory.Generation, Project: "demo", Environment: "production", OperationID: "op-test", Operation: "deploy", Token: 1,
		TargetNodeIDs: []string{installation.NodeID}, IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	bindTestOperationHolder(t, &fence)
	if err := nodeidentity.SignOperationFence(&fence, installation); err != nil {
		t.Fatal(err)
	}
	if _, err := server.activateWorkerFence(fence, testOperationHolderToken); err != nil {
		t.Fatal(err)
	}
	fenceJSON, _ := json.Marshal(fence)
	recorder = httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/reconcile-service", nil)
	request.Header.Set(OperationFenceHeader, base64.RawURLEncoding.EncodeToString(fenceJSON))
	request.Header.Set(OperationHolderHeader, testOperationHolderToken)
	handler.ServeHTTP(recorder, request)
	if calls != 1 {
		t.Fatalf("schedulable mutation calls=%d, want 1", calls)
	}
}

func TestEnrolledLifecycleGateFailsClosedForEveryRegisteredMutation(t *testing.T) {
	server := lifecycleTestServer(t, nodeidentity.NodeLifecycleDraining, false)
	for _, route := range server.registeredRoutes() {
		t.Run(route.path, func(t *testing.T) {
			calls := 0
			handler := server.enrolledLifecycleHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, route.path, nil))
			_, safe := lifecycleSafeMutationPaths[route.path]
			if safe && calls != 1 {
				t.Fatalf("lifecycle-safe mutation was blocked: status=%d", recorder.Code)
			}
			if !safe && (calls != 0 || recorder.Code != http.StatusConflict) {
				t.Fatalf("mutation bypassed gate: calls=%d status=%d", calls, recorder.Code)
			}
		})
	}
}

func TestEnrolledLifecycleGateAllowsEveryRegisteredRead(t *testing.T) {
	server := lifecycleTestServer(t, nodeidentity.NodeLifecycleJoining, false)
	for _, route := range server.registeredRoutes() {
		calls := 0
		handler := server.enrolledLifecycleHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, route.path, nil))
		if calls != 1 {
			t.Fatalf("read %s was unexpectedly blocked", route.path)
		}
	}
}

func lifecycleTestServer(t *testing.T, lifecycle string, schedulable bool) *Server {
	t.Helper()
	installation, err := nodeidentity.New("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "worker-1", []string{nodeidentity.RoleWorker}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "inventory.json")
	meshKey := "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
	meshCredentialID, _ := nodeidentity.MeshCredentialID(meshKey)
	inventory := nodeidentity.ClusterInventory{
		APIVersion: nodeidentity.InventoryAPIVersion, Kind: nodeidentity.InventoryKind, ClusterID: installation.ClusterID,
		Generation: 1, ControllerNodeID: installation.NodeID, MeshCIDR: "10.42.0.0/24", UpdatedAt: now,
		Nodes: []nodeidentity.InventoryNode{{
			NodeID: installation.NodeID, NodeName: installation.NodeName, Lifecycle: lifecycle,
			Roles: []string{nodeidentity.RoleControlPlane, nodeidentity.RoleWorker}, Schedulable: schedulable,
			MeshIP: "10.42.0.2", MeshEndpoint: "worker.example", MeshCredentialID: meshCredentialID, MeshPublicKey: meshKey, MeshCredentialStatus: nodeidentity.MeshCredentialActive,
			AllocationPublicKey: installation.AllocationPublicKey, JoinedAt: now, UpdatedAt: now,
		}},
	}
	if err := nodeidentity.CreateInventory(path, inventory); err != nil {
		t.Fatal(err)
	}
	server := &Server{installation: installation, inventoryFile: path, dataDir: t.TempDir()}
	t.Cleanup(server.releaseAnyOperationBarrier)
	return server
}
