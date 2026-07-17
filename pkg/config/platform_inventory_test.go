package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	cryptossh "golang.org/x/crypto/ssh"
)

func TestMaterializePlatformInventoryOwnsTargetsPlacementAndCredentials(t *testing.T) {
	now := time.Now().UTC()
	controller, err := nodeidentity.New("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "node-1", []string{nodeidentity.RoleControlPlane, nodeidentity.RoleWorker}, now)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := nodeidentity.New(controller.ClusterID, "33333333-3333-4333-8333-333333333333", "node-2", []string{nodeidentity.RoleWorker}, now)
	if err != nil {
		t.Fatal(err)
	}
	joining, err := nodeidentity.New(controller.ClusterID, "44444444-4444-4444-8444-444444444444", "node-3", []string{nodeidentity.RoleWorker}, now)
	if err != nil {
		t.Fatal(err)
	}
	mesh1, mesh2, mesh3 := "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=", "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI=", "AwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwM="
	id1, _ := nodeidentity.MeshCredentialID(mesh1)
	id2, _ := nodeidentity.MeshCredentialID(mesh2)
	id3, _ := nodeidentity.MeshCredentialID(mesh3)
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostKey, err := cryptossh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	hostKeyBody := base64.StdEncoding.EncodeToString(hostKey.Marshal())
	hostKeyFingerprint := cryptossh.FingerprintSHA256(hostKey)
	inventory := &nodeidentity.ClusterInventory{
		APIVersion: nodeidentity.InventoryAPIVersion, Kind: nodeidentity.InventoryKind, ClusterID: controller.ClusterID,
		Generation: 9, ControllerNodeID: controller.NodeID, MeshCIDR: "10.210.0.0/16", UpdatedAt: now,
		Nodes: []nodeidentity.InventoryNode{
			{NodeID: controller.NodeID, NodeName: controller.NodeName, Lifecycle: nodeidentity.NodeLifecycleSchedulable, Roles: []string{nodeidentity.RoleControlPlane, nodeidentity.RoleWorker}, Schedulable: true, MeshIP: "10.210.0.1", MeshEndpoint: "node-1.example", MeshCredentialID: id1, MeshPublicKey: mesh1, MeshCredentialStatus: nodeidentity.MeshCredentialActive, SSHHost: "203.0.113.21", SSHPort: 22, SSHUser: "root", SSHHostKeyType: hostKey.Type(), SSHHostKey: hostKeyBody, SSHHostKeyFingerprint: hostKeyFingerprint, AllocationPublicKey: controller.AllocationPublicKey, JoinedAt: now, UpdatedAt: now},
			{NodeID: worker.NodeID, NodeName: worker.NodeName, Lifecycle: nodeidentity.NodeLifecycleSchedulable, Roles: []string{nodeidentity.RoleWorker}, Schedulable: true, MeshIP: "10.210.0.2", MeshEndpoint: "node-2.example", MeshCredentialID: id2, MeshPublicKey: mesh2, MeshCredentialStatus: nodeidentity.MeshCredentialActive, SSHHost: "203.0.113.22", SSHPort: 22, SSHUser: "root", SSHHostKeyType: hostKey.Type(), SSHHostKey: hostKeyBody, SSHHostKeyFingerprint: hostKeyFingerprint, AllocationPublicKey: worker.AllocationPublicKey, JoinedAt: now, UpdatedAt: now},
			{NodeID: joining.NodeID, NodeName: joining.NodeName, Lifecycle: nodeidentity.NodeLifecycleJoining, Roles: []string{nodeidentity.RoleWorker}, Schedulable: false, MeshIP: "10.210.0.3", MeshEndpoint: "node-3.example", MeshCredentialID: id3, MeshPublicKey: mesh3, MeshCredentialStatus: nodeidentity.MeshCredentialActive, SSHHost: "203.0.113.23", SSHPort: 22, SSHUser: "root", SSHHostKeyType: hostKey.Type(), SSHHostKey: hostKeyBody, SSHHostKeyFingerprint: hostKeyFingerprint, AllocationPublicKey: joining.AllocationPublicKey, JoinedAt: now, UpdatedAt: now},
		},
	}
	cfg := &Config{
		Servers: map[string]ServerConfig{"old-worker-alias": {Host: "attacker.example", User: "app-user", Password: "app-password", NodeID: worker.NodeID}},
		Environments: map[string]EnvironmentConfig{"production": {
			Servers: []string{worker.NodeID},
			Proxy:   &EnvironmentProxyConfig{Placement: &PlacementConfig{Strategy: "pinned", Servers: []string{"old-worker-alias"}}},
			Services: map[string]ServiceConfig{
				"web": {Image: "nginx", Placement: &PlacementConfig{Strategy: "pinned", Servers: []string{"old-worker-alias"}}},
			},
		}},
	}
	if err := MaterializePlatformInventory(cfg, inventory, controller.NodeID, 1234, "/secure/platform-key", "platform-password"); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 3 {
		t.Fatalf("materialized servers = %#v", cfg.Servers)
	}
	if local := cfg.Servers[controller.NodeName]; local.Transport != "local" || local.WorkerUID != 1234 || local.MeshIP != "10.210.0.1" {
		t.Fatalf("local controller materialization = %#v", local)
	}
	remote := cfg.Servers[worker.NodeName]
	if remote.Host != "203.0.113.22" || remote.User != "root" || remote.SSHKey != "/secure/platform-key" || remote.Password != "platform-password" || remote.SSHHostKeyFingerprint != hostKeyFingerprint {
		t.Fatalf("remote membership materialization = %#v", remote)
	}
	if got := cfg.Environments["production"].Servers; len(got) != 1 || got[0] != worker.NodeName {
		t.Fatalf("environment targets = %v", got)
	}
	if got := cfg.Environments["production"].Proxy.Placement.Servers; len(got) != 1 || got[0] != worker.NodeName {
		t.Fatalf("proxy placement targets = %v", got)
	}
	if got := cfg.Environments["production"].Services["web"].Placement.Servers; len(got) != 1 || got[0] != worker.NodeName {
		t.Fatalf("service placement targets = %v", got)
	}
	if candidate, ok := cfg.Servers[joining.NodeName]; !ok || candidate.Lifecycle != nodeidentity.NodeLifecycleJoining || candidate.Transport != "ssh" {
		t.Fatalf("unschedulable member was lost as a connectivity target: %#v", candidate)
	}

	root := t.TempDir()
	inventoryPath := filepath.Join(root, "inventory.json")
	bindingPath := filepath.Join(root, "local-node.json")
	if err := nodeidentity.CreateInventory(inventoryPath, *inventory); err != nil {
		t.Fatal(err)
	}
	if err := nodeidentity.WriteLocalBinding(bindingPath, nodeidentity.LocalBinding{APIVersion: nodeidentity.InventoryAPIVersion, Kind: nodeidentity.LocalBindingKind, ClusterID: controller.ClusterID, NodeID: controller.NodeID, NodeName: controller.NodeName, WorkerUID: 1234}); err != nil {
		t.Fatal(err)
	}
	fromPublicFiles := &Config{Environments: map[string]EnvironmentConfig{"production": {Services: map[string]ServiceConfig{"web": {Image: "nginx"}}}}}
	if err := materializePlatformInventoryFromFiles(fromPublicFiles, inventoryPath, bindingPath, "", ""); err != nil {
		t.Fatalf("public binding materialization required private identity/config: %v", err)
	}
	if fromPublicFiles.Servers[controller.NodeName].Transport != "local" {
		t.Fatalf("public binding did not resolve local transport: %#v", fromPublicFiles.Servers)
	}

	localOnly := minimalValidConfigWithService(ServiceConfig{Image: "nginx"})
	localOnly.Environments["production"] = EnvironmentConfig{Services: map[string]ServiceConfig{"web": {Image: "nginx"}}}
	localInventory := *inventory
	localInventory.Nodes = localInventory.Nodes[:1]
	if err := MaterializePlatformInventory(localOnly, &localInventory, controller.NodeID, 1234, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := ValidateConfig(localOnly); err != nil {
		t.Fatalf("single-node local materialization required SSH credentials: %v", err)
	}

	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(keyPath, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	inverse := minimalValidConfigWithService(ServiceConfig{Image: "nginx"})
	inverse.Environments["production"] = EnvironmentConfig{Services: map[string]ServiceConfig{"web": {Image: "nginx"}}}
	if err := MaterializePlatformInventory(inverse, inventory, worker.NodeID, 0, keyPath, ""); err != nil {
		t.Fatal(err)
	}
	if err := ValidateConfig(inverse); err != nil {
		t.Fatalf("worker-side materialized inventory is unusable: %v", err)
	}
	for name, server := range inverse.Servers {
		if server.Transport == "ssh" && (server.Host == "" || server.User == "" || server.SSHHostKey == "" || server.SSHHostKeyFingerprint == "") {
			t.Fatalf("worker-side SSH server %s is not completely pinned: %#v", name, server)
		}
	}
}
