package takod

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/platform"
)

func TestMembershipReconcileStopsProxyAfterCordonRevokesRoute(t *testing.T) {
	root := useTempProxyPaths(t)
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()
	marker := filepath.Join(t.TempDir(), "proxy-running")
	if err := os.WriteFile(marker, []byte("running"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TAKO_FAKE_PROXY_RUNNING_MARKER", marker)
	controlDir, configDir := filepath.Join(root, "control"), filepath.Join(root, "etc")
	if err := os.MkdirAll(controlDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	controller, _ := nodeidentity.New("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "node-a", []string{nodeidentity.RoleBuilder, nodeidentity.RoleControlPlane, nodeidentity.RoleEdge, nodeidentity.RoleWorker}, time.Now())
	worker, _ := nodeidentity.New(controller.ClusterID, "33333333-3333-4333-8333-333333333333", "node-b", []string{nodeidentity.RoleWorker}, time.Now())
	membershipPath, inventoryPath := filepath.Join(controlDir, "membership.json"), filepath.Join(configDir, "inventory.json")
	store, _ := platform.NewMembershipStore(membershipPath, inventoryPath)
	if _, err := store.InitializeFirstNode(*controller, "10.42.0.0/24", "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=", "node-a.example"); err != nil {
		t.Fatal(err)
	}
	token, _ := store.CreateJoinToken(worker.NodeID, time.Minute)
	reservation, _ := store.ReserveJoinToken(token.Token, worker.NodeID)
	hostKey, hostFingerprint := testTakodSSHHostKey(t)
	if _, err := store.EnrollWorker(platform.EnrollWorkerRequest{
		Reservation: reservation.Reservation, NodeID: worker.NodeID, NodeName: worker.NodeName, MeshIP: "10.42.0.2", MeshEndpoint: "worker.example", ControllerMeshEndpoint: "node-a.example",
		SSHHost: "worker.example", SSHPort: 22, SSHUser: "root", SSHHostKeyType: "ssh-ed25519",
		SSHHostKey: hostKey, SSHHostKeyFingerprint: hostFingerprint, AllocationPublicKey: worker.AllocationPublicKey,
		ControllerSSHHost: "node-a.example", ControllerSSHPort: 22, ControllerSSHUser: "root", ControllerSSHHostKeyType: "ssh-ed25519",
		ControllerSSHHostKey: hostKey, ControllerSSHHostKeyFingerprint: hostFingerprint,
		MeshPublicKey: "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI=",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkReady(worker.NodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkSchedulable(worker.NodeID); err != nil {
		t.Fatal(err)
	}
	allocation := PortAllocationResponse{Kind: PortAllocationKindMeshUpstream, Project: "demo", Environment: "production", Service: "web", Slot: 1, HostIP: "10.42.0.2", HostPort: 20000, ContainerPort: 3000, Key: "mesh-upstream/demo/production/web/1", ClusterID: worker.ClusterID, NodeID: worker.NodeID, Generation: 1, IssuedAt: time.Now().UTC()}
	if err := SignPortAllocation(&allocation, worker); err != nil {
		t.Fatal(err)
	}
	url := "http://10.42.0.2:20000"
	manifest := ProxyRouteManifest{Version: 2, Project: allocation.Project, Environment: allocation.Environment, ClusterID: allocation.ClusterID, Routes: []ProxyRoute{{Service: allocation.Service, Domains: []string{"app.example.com"}, Upstreams: []string{url}, Destinations: []ProxyDestination{{Kind: ProxyDestinationMesh, URL: url, Project: allocation.Project, Environment: allocation.Environment, Service: allocation.Service, Slot: allocation.Slot, ClusterID: allocation.ClusterID, NodeID: allocation.NodeID, AllocationKey: allocation.Key, Generation: allocation.Generation, IssuedAt: allocation.IssuedAt, Signature: allocation.Signature, ContainerPort: allocation.ContainerPort, HostPort: allocation.HostPort, HostIP: allocation.HostIP}}}}}
	content, _ := json.Marshal(manifest)
	server := &Server{installation: controller, inventoryFile: inventoryPath, membershipFile: membershipPath, dataDir: root}
	if err := server.authorizeProxyManifestAllocations(string(content)); err == nil {
		t.Fatal("remote route authority unexpectedly enabled")
	}
	if err := os.MkdirAll(proxyRoutesDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proxyRoutesDir, "demo.json"), content, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Cordon(worker.NodeID); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.handleMembershipReconcile(recorder, httptest.NewRequest(http.MethodPost, "/v1/platform/membership/reconcile", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("reconcile status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response MembershipReconcileResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.ProxyStopped || response.InvalidRoutes != 1 {
		t.Fatalf("response=%#v", response)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("proxy marker survived revocation: %v", err)
	}
}
