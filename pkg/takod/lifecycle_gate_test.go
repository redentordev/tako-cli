package takod

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	server := &Server{installation: installation, inventoryFile: path}
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
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/reconcile-service", nil))
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
	return &Server{installation: installation, inventoryFile: path}
}
