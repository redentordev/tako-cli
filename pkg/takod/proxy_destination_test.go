package takod

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

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

func TestEnrolledProxyManifestRejectsRemoteMeshUntilAuthoritativeInventory(t *testing.T) {
	installation, err := nodeidentity.New("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "node-b", []string{nodeidentity.RoleWorker}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	inventoryPath := t.TempDir() + "/inventory.json"
	if err := nodeidentity.CreateInventory(inventoryPath, nodeidentity.ClusterInventory{
		APIVersion: nodeidentity.InventoryAPIVersion, Kind: nodeidentity.InventoryKind, ClusterID: installation.ClusterID, MeshCIDR: "10.42.0.0/24",
		Nodes: []nodeidentity.InventoryNode{{NodeID: installation.NodeID, MeshIP: "10.42.0.2", AllocationPublicKey: installation.AllocationPublicKey}},
	}); err != nil {
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
	if err := validateEnrolledProxyRouteManifest(string(content), installation.ClusterID, inventoryPath); err == nil || !strings.Contains(err.Error(), "authoritative allocation inventory") {
		t.Fatalf("remote allocation was not rejected fail-closed: %v", err)
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
