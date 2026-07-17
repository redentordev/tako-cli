package takod

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/platform"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
	cryptossh "golang.org/x/crypto/ssh"
)

func testTakodSSHHostKey(t *testing.T) (string, string) {
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

func TestProxyManifestV2BindsLocalAliasToWorkloadIdentity(t *testing.T) {
	alias := runtimeid.ContainerAlias("demo", "production", "web", 1)
	url := "http://" + alias + ":3000"
	manifest := ProxyRouteManifest{
		Version: 2, Project: "demo", Environment: "production", ClusterID: "11111111-1111-4111-8111-111111111111",
		Routes: []ProxyRoute{{
			Service: "web", Domains: []string{"app.example.com"}, Upstreams: []string{url},
			Destinations: []ProxyDestination{{Kind: ProxyDestinationRuntimeAlias, URL: url, Project: "demo", Environment: "production", Service: "web", Slot: 1, ContainerPort: 3000, HostPort: 3000}},
		}},
	}
	if err := validateProxyRouteManifest(&manifest); err != nil {
		t.Fatalf("valid local destination proof rejected: %v", err)
	}
	manifest.Routes[0].Destinations[0].Service = "admin"
	if err := validateProxyRouteManifest(&manifest); err == nil || !strings.Contains(err.Error(), "service/revision") {
		t.Fatalf("unrelated service proof error = %v", err)
	}
}

func TestControllerKeepsSignedRemoteAllocationFailClosedUntilFenced(t *testing.T) {
	root := t.TempDir()
	controlDir := filepath.Join(root, "control")
	configDir := filepath.Join(root, "etc")
	if err := os.MkdirAll(controlDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	controller, err := nodeidentity.New("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "node-a", []string{nodeidentity.RoleBuilder, nodeidentity.RoleControlPlane, nodeidentity.RoleEdge, nodeidentity.RoleWorker}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	worker, err := nodeidentity.New(controller.ClusterID, "33333333-3333-4333-8333-333333333333", "node-b", []string{nodeidentity.RoleWorker}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	membershipPath := filepath.Join(controlDir, "membership.json")
	inventoryPath := filepath.Join(configDir, "inventory.json")
	store, _ := platform.NewMembershipStore(membershipPath, inventoryPath)
	if _, err := store.InitializeFirstNode(*controller, "10.42.0.0/24", "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=", "node-a.example"); err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateJoinToken(worker.NodeID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := store.ReserveJoinToken(token.Token, worker.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	hostKey, hostFingerprint := testTakodSSHHostKey(t)
	if _, err := store.EnrollWorker(platform.EnrollWorkerRequest{
		Reservation: reservation.Reservation, NodeID: worker.NodeID, NodeName: worker.NodeName, MeshIP: "10.42.0.2", MeshEndpoint: "worker.example", ControllerMeshEndpoint: "node-a.example",
		SSHHost: "worker.example", SSHPort: 22, SSHUser: "root", SSHHostKeyType: "ssh-ed25519",
		SSHHostKey: hostKey, SSHHostKeyFingerprint: hostFingerprint,
		ControllerSSHHost: "node-a.example", ControllerSSHPort: 22, ControllerSSHUser: "root", ControllerSSHHostKeyType: "ssh-ed25519",
		ControllerSSHHostKey: hostKey, ControllerSSHHostKeyFingerprint: hostFingerprint,
		AllocationPublicKey: worker.AllocationPublicKey, MeshPublicKey: "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI=",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkReady(worker.NodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkSchedulable(worker.NodeID); err != nil {
		t.Fatal(err)
	}
	allocation := PortAllocationResponse{
		Kind: PortAllocationKindMeshUpstream, Project: "demo", Environment: "production", Service: "web", Slot: 1,
		HostIP: "10.42.0.2", HostPort: 20000, ContainerPort: 3000, Key: "mesh-upstream/demo/production/web/1",
		ClusterID: worker.ClusterID, NodeID: worker.NodeID, Generation: 1, IssuedAt: time.Now().UTC(),
	}
	if err := SignPortAllocation(&allocation, worker); err != nil {
		t.Fatal(err)
	}
	url := "http://10.42.0.2:20000"
	manifest := ProxyRouteManifest{Version: 2, Project: allocation.Project, Environment: allocation.Environment, ClusterID: allocation.ClusterID, Routes: []ProxyRoute{{
		Service: allocation.Service, Domains: []string{"app.example.com"}, Upstreams: []string{url}, Destinations: []ProxyDestination{{
			Kind: ProxyDestinationMesh, URL: url, Project: allocation.Project, Environment: allocation.Environment, Service: allocation.Service,
			Slot: allocation.Slot, ClusterID: allocation.ClusterID, NodeID: allocation.NodeID, AllocationKey: allocation.Key,
			Generation: allocation.Generation, IssuedAt: allocation.IssuedAt, Signature: allocation.Signature,
			ContainerPort: allocation.ContainerPort, HostPort: allocation.HostPort, HostIP: allocation.HostIP,
		}},
	}}}
	content, _ := json.Marshal(manifest)
	server := &Server{installation: controller, inventoryFile: inventoryPath, membershipFile: membershipPath}
	if server.supportsAuthoritativeRemoteMeshRoutes() {
		t.Fatal("controller advertised remote routes before observed-allocation fencing exists")
	}
	if err := server.authorizeProxyManifestAllocations(string(content)); err == nil || !strings.Contains(err.Error(), "disabled until controller-observed") {
		t.Fatalf("self-presented remote allocation was accepted: %v", err)
	}
	if err := validateEnrolledProxyRouteManifest(string(content), controller.ClusterID, inventoryPath); err == nil || !strings.Contains(err.Error(), "disabled until controller-observed") {
		t.Fatalf("enrolled route validator accepted remote allocation: %v", err)
	}
}

func TestControllerRejectsForgedAllocationBeforeAuthorityMutation(t *testing.T) {
	// Signature verification is deliberately tested independently from the
	// inventory lookup so a forged generation can never become controller state.
	worker, _ := nodeidentity.New("11111111-1111-4111-8111-111111111111", "33333333-3333-4333-8333-333333333333", "node-b", []string{nodeidentity.RoleWorker}, time.Now())
	response := PortAllocationResponse{Kind: PortAllocationKindMeshUpstream, Project: "demo", Environment: "production", Service: "web", Slot: 1, HostIP: "10.42.0.2", HostPort: 20000, ContainerPort: 3000, Key: "mesh-upstream/demo/production/web/1", ClusterID: worker.ClusterID, NodeID: worker.NodeID, Generation: 1, IssuedAt: time.Now().UTC(), Signature: "forged"}
	if err := VerifyPortAllocation(response, worker.AllocationPublicKey); err == nil {
		t.Fatal("forged allocation signature verified")
	}
}

func TestEnrolledProxyManifestRequiresCurrentControllerAuthorization(t *testing.T) {
	installation, err := nodeidentity.New("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "node-b", []string{nodeidentity.RoleWorker}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	inventoryPath := t.TempDir() + "/inventory.json"
	controller, err := nodeidentity.New(installation.ClusterID, "33333333-3333-4333-8333-333333333333", "node-a", []string{nodeidentity.RoleControlPlane, nodeidentity.RoleWorker}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	allocation := PortAllocationResponse{
		Kind: PortAllocationKindMeshUpstream, Project: "demo", Environment: "production", Service: "web", Slot: 1,
		HostIP: "10.42.0.2", HostPort: 20000, ContainerPort: 3000, Key: "mesh-upstream/demo/production/web/1",
		ClusterID: installation.ClusterID, NodeID: installation.NodeID, Generation: 7, IssuedAt: time.Now().UTC(),
	}
	if err := SignPortAllocation(&allocation, installation); err != nil {
		t.Fatal(err)
	}
	url := "http://10.42.0.2:20000"
	manifest := ProxyRouteManifest{
		Version: 2, Project: "demo", Environment: "production", ClusterID: installation.ClusterID,
		Routes: []ProxyRoute{{Service: "web", Domains: []string{"app.example.com"}, Upstreams: []string{url}, Destinations: []ProxyDestination{{
			Kind: ProxyDestinationMesh, URL: url, Project: allocation.Project, Environment: allocation.Environment, Service: allocation.Service,
			Slot: allocation.Slot, ClusterID: allocation.ClusterID, NodeID: allocation.NodeID, AllocationKey: allocation.Key,
			Generation: allocation.Generation, IssuedAt: allocation.IssuedAt, Signature: allocation.Signature,
			ContainerPort: allocation.ContainerPort, HostPort: allocation.HostPort, HostIP: allocation.HostIP,
		}}}},
	}
	content, _ := json.Marshal(manifest)
	now := time.Now().UTC()
	controllerMeshKey := "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
	workerMeshKey := "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI="
	controllerMeshID, _ := nodeidentity.MeshCredentialID(controllerMeshKey)
	workerMeshID, _ := nodeidentity.MeshCredentialID(workerMeshKey)
	inventory := nodeidentity.ClusterInventory{
		APIVersion: nodeidentity.InventoryAPIVersion, Kind: nodeidentity.InventoryKind, ClusterID: installation.ClusterID,
		Generation: 5, ControllerNodeID: controller.NodeID, MeshCIDR: "10.42.0.0/24", UpdatedAt: now,
		Nodes: []nodeidentity.InventoryNode{
			{NodeID: controller.NodeID, NodeName: controller.NodeName, Lifecycle: nodeidentity.NodeLifecycleSchedulable, Roles: []string{nodeidentity.RoleControlPlane, nodeidentity.RoleWorker}, Schedulable: true, MeshIP: "10.42.0.1", MeshEndpoint: "node-a.example", MeshCredentialID: controllerMeshID, MeshPublicKey: controllerMeshKey, MeshCredentialStatus: nodeidentity.MeshCredentialActive, AllocationPublicKey: controller.AllocationPublicKey, JoinedAt: now, UpdatedAt: now},
			{NodeID: installation.NodeID, NodeName: installation.NodeName, Lifecycle: nodeidentity.NodeLifecycleSchedulable, Roles: []string{nodeidentity.RoleWorker}, Schedulable: true, MeshIP: "10.42.0.2", MeshEndpoint: "worker.example", MeshCredentialID: workerMeshID, MeshPublicKey: workerMeshKey, MeshCredentialStatus: nodeidentity.MeshCredentialActive, AllocationPublicKey: installation.AllocationPublicKey, JoinedAt: now, UpdatedAt: now},
		},
	}
	if err := nodeidentity.CreateInventory(inventoryPath, inventory); err != nil {
		t.Fatal(err)
	}
	if err := validateEnrolledProxyRouteManifest(string(content), installation.ClusterID, inventoryPath); err == nil || !strings.Contains(err.Error(), "disabled until controller-observed") {
		t.Fatalf("remote allocation was not rejected fail-closed: %v", err)
	}
	inventory.Generation++
	inventory.UpdatedAt = now.Add(time.Second)
	inventory.ActiveAllocations = []nodeidentity.ActiveAllocation{{
		Kind: allocation.Kind, Project: allocation.Project, Environment: allocation.Environment, Service: allocation.Service,
		Revision: allocation.Revision, Slot: allocation.Slot, HostIP: allocation.HostIP, HostPort: allocation.HostPort,
		ContainerPort: allocation.ContainerPort, Key: allocation.Key, ClusterID: allocation.ClusterID, NodeID: allocation.NodeID,
		Generation: allocation.Generation, IssuedAt: allocation.IssuedAt, Signature: allocation.Signature, AuthorizedAt: now,
	}}
	if err := nodeidentity.ReplaceInventory(inventoryPath, inventory); err != nil {
		t.Fatal(err)
	}
	if err := validateEnrolledProxyRouteManifest(string(content), installation.ClusterID, inventoryPath); err == nil || !strings.Contains(err.Error(), "disabled until controller-observed") {
		t.Fatalf("inventory injection bypassed deferred remote-route fencing: %v", err)
	}
	if err := VerifyPortAllocation(allocation, installation.AllocationPublicKey); err != nil {
		t.Fatalf("allocation generation signature did not verify independently: %v", err)
	}
}

func TestEnrolledStartupQuarantinesStoredRemoteRoutesAndActivatesPolicy(t *testing.T) {
	root := useTempProxyPaths(t)
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()
	if err := os.MkdirAll(proxyRoutesDir, 0755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"version":1,"project":"demo","environment":"production","routes":[{"service":"web","domains":["app.example.com"],"upstreams":["http://10.42.0.2:20000"]}]}`
	legacyPath := filepath.Join(proxyRoutesDir, "demo-production.json")
	if err := os.WriteFile(legacyPath, []byte(legacy), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(proxyCaddyfilePath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(proxyCaddyfilePath, []byte("reverse_proxy 10.42.0.2:20000\n"), 0644); err != nil {
		t.Fatal(err)
	}
	installation, err := nodeidentity.New("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "node-a", []string{nodeidentity.RoleWorker}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{dataDir: root, installation: installation, inventoryFile: filepath.Join(root, "inventory.json")}
	deactivate, err := server.enforceEnrolledProxyPolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer deactivate()
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy remote route was not withdrawn: %v", err)
	}
	quarantined, err := os.ReadDir(filepath.Join(root, "proxy", "quarantine"))
	if err != nil || len(quarantined) != 1 {
		t.Fatalf("quarantined routes = %d, %v", len(quarantined), err)
	}
	caddyfile, err := os.ReadFile(proxyCaddyfilePath)
	if err != nil || strings.Contains(string(caddyfile), "10.42.0.2:20000") {
		t.Fatalf("unsafe proxy configuration remained active: %q, %v", caddyfile, err)
	}
	if err := os.WriteFile(legacyPath, []byte(legacy), 0644); err != nil {
		t.Fatal(err)
	}
	if err := renderAndWriteCaddyfile(context.Background()); err == nil || !strings.Contains(err.Error(), "violates enrolled-node policy") {
		t.Fatalf("active enrolled policy did not reject out-of-band remote route: %v", err)
	}
}

func TestEnrolledProxyStopAndReloadFailuresAreVerified(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()
	marker := filepath.Join(t.TempDir(), "proxy-running")
	if err := os.WriteFile(marker, []byte("running"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TAKO_FAKE_PROXY_RUNNING_MARKER", marker)
	t.Setenv("TAKO_FAKE_DOCKER_RM_ERROR", "daemon refused removal")
	if err := stopProxyAndVerifyAbsent(context.Background()); err == nil || !strings.Contains(err.Error(), "daemon refused removal") {
		t.Fatalf("proxy removal failure = %v", err)
	}
	t.Setenv("TAKO_FAKE_DOCKER_EXEC_ERROR", "reload rejected")
	if err := reloadEnrolledProxyIfRunning(context.Background()); err == nil || !strings.Contains(err.Error(), "reload rejected") || !strings.Contains(err.Error(), "daemon refused removal") {
		t.Fatalf("reload plus fail-closed stop error = %v", err)
	}
	t.Setenv("TAKO_FAKE_DOCKER_RM_ERROR", "")
	if err := stopProxyAndVerifyAbsent(context.Background()); err != nil {
		t.Fatalf("verified proxy stop failed: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("proxy running marker survived verified stop: %v", err)
	}
}

func TestEnrolledStartupStopsInMemoryProxyWithoutRouteFiles(t *testing.T) {
	root := useTempProxyPaths(t)
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()
	marker := filepath.Join(t.TempDir(), "proxy-running")
	if err := os.WriteFile(marker, []byte("running"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TAKO_FAKE_PROXY_RUNNING_MARKER", marker)
	installation, err := nodeidentity.New("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "node-a", []string{nodeidentity.RoleWorker}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{dataDir: root, installation: installation, inventoryFile: filepath.Join(root, "inventory.json")}
	deactivate, err := server.enforceEnrolledProxyPolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer deactivate()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("in-memory proxy survived fileless enrolled startup: %v", err)
	}
}

func TestProxyManifestV2RejectsUnprovenAndSpecialIPDestinations(t *testing.T) {
	base := ProxyRouteManifest{
		Version: 2, Project: "demo", Environment: "production", ClusterID: "11111111-1111-4111-8111-111111111111",
		Routes: []ProxyRoute{{Service: "web", Domains: []string{"app.example.com"}, Upstreams: []string{"http://169.254.169.254:20000"}}},
	}
	if err := validateProxyRouteManifest(&base); err == nil || !strings.Contains(err.Error(), "requires destination identity proof") {
		t.Fatalf("unproven IP error = %v", err)
	}
	proof := ProxyDestination{
		Kind: ProxyDestinationMesh, URL: base.Routes[0].Upstreams[0], Project: "demo", Environment: "production", Service: "web", Slot: 1,
		ClusterID: base.ClusterID, NodeID: "22222222-2222-4222-8222-222222222222", ContainerPort: 3000, HostPort: 20000, HostIP: "169.254.169.254",
	}
	proof.AllocationKey = portAllocationKey(PortAllocationKindMeshUpstream, proof.Project, proof.Environment, proof.Service, proof.Revision, proof.Slot)
	base.Routes[0].Destinations = []ProxyDestination{proof}
	if err := validateProxyRouteManifest(&base); err == nil || !strings.Contains(err.Error(), "non-special IP") {
		t.Fatalf("metadata IP error = %v", err)
	}
}
