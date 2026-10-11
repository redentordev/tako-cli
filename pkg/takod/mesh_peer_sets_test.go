package takod

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/redentordev/tako-cli/pkg/mesh"
)

func peerSetNode(name, address string) mesh.Node {
	return mesh.Node{Name: name, Host: name + ".example.test", Address: address, PublicKey: "wg-test-key-" + name}
}

func peerSetRequest(project, environment string, self mesh.Node, members ...mesh.Node) MeshPeerSetRequest {
	return MeshPeerSetRequest{
		Project: project, Environment: environment,
		NetworkCIDR: "10.210.0.0/16", SubnetBits: 24,
		Config: mesh.WireGuardConfig{Enabled: true, Interface: "tako", ListenPort: 51820, NATTraversal: true},
		Node:   self,
		Peers:  append([]mesh.Node{self}, members...),
	}
}

type recordedApply struct {
	node   mesh.Node
	peers  []mesh.Node
	config mesh.WireGuardConfig
}

// recordMeshApplies replaces the WireGuard apply with a recorder.
func recordMeshApplies(t *testing.T) *[]recordedApply {
	t.Helper()
	var mu sync.Mutex
	var applies []recordedApply
	previous := applyMeshConfig
	applyMeshConfig = func(_ context.Context, node mesh.Node, peers []mesh.Node, config mesh.WireGuardConfig, _ bool) (*mesh.Status, error) {
		mu.Lock()
		defer mu.Unlock()
		applies = append(applies, recordedApply{node: node, peers: append([]mesh.Node(nil), peers...), config: config})
		return &mesh.Status{Interface: config.Interface, Up: true, Peers: len(peers)}, nil
	}
	t.Cleanup(func() { applyMeshConfig = previous })
	return &applies
}

func renderedPeerNames(apply recordedApply) []string {
	names := make([]string, 0, len(apply.peers))
	for _, peer := range apply.peers {
		names = append(names, peer.Name+"="+meshIP(peer.Address))
	}
	sort.Strings(names)
	return names
}

func lastApply(t *testing.T, applies *[]recordedApply) recordedApply {
	t.Helper()
	if len(*applies) == 0 {
		t.Fatal("no mesh apply")
	}
	return (*applies)[len(*applies)-1]
}

// 4.1: preview [a, s] and production [b, s] share s. Deployed in either
// order, s peers with a and b, a and b never peer, and redeploying preview
// keeps b on s.
func TestSharedNodePeerSetsUnionBothEnvironments(t *testing.T) {
	a, b, s := peerSetNode("a", "10.210.0.1/24"), peerSetNode("b", "10.210.0.3/24"), peerSetNode("s", "10.210.0.2/24")
	preview := peerSetRequest("app", "preview", s, a)
	production := peerSetRequest("app", "production", s, b)
	for _, order := range [][]MeshPeerSetRequest{{preview, production}, {production, preview}} {
		applies := recordMeshApplies(t)
		sDir, aDir, bDir := t.TempDir(), t.TempDir(), t.TempDir()
		for _, request := range order {
			if _, err := PutMeshPeerSet(context.Background(), sDir, request); err != nil {
				t.Fatal(err)
			}
		}
		if got := renderedPeerNames(lastApply(t, applies)); strings.Join(got, ",") != "a=10.210.0.1,b=10.210.0.3" {
			t.Fatalf("s peers = %v, want a and b", got)
		}
		// a and b only receive their own environment's set.
		if _, err := PutMeshPeerSet(context.Background(), aDir, peerSetRequest("app", "preview", a, s)); err != nil {
			t.Fatal(err)
		}
		if got := renderedPeerNames(lastApply(t, applies)); strings.Join(got, ",") != "s=10.210.0.2" {
			t.Fatalf("a peers = %v, want only s", got)
		}
		if _, err := PutMeshPeerSet(context.Background(), bDir, peerSetRequest("app", "production", b, s)); err != nil {
			t.Fatal(err)
		}
		if got := renderedPeerNames(lastApply(t, applies)); strings.Join(got, ",") != "s=10.210.0.2" {
			t.Fatalf("b peers = %v, want only s", got)
		}
		// Redeploying preview keeps b on s.
		if _, err := PutMeshPeerSet(context.Background(), sDir, preview); err != nil {
			t.Fatal(err)
		}
		if got := renderedPeerNames(lastApply(t, applies)); strings.Join(got, ",") != "a=10.210.0.1,b=10.210.0.3" {
			t.Fatalf("s peers after preview redeploy = %v", got)
		}
	}
}

func TestPeerSetUnionDeduplicatesByKey(t *testing.T) {
	s, a := peerSetNode("s", "10.210.0.2/24"), peerSetNode("a", "10.210.0.1/24")
	union, err := renderMeshUnion(map[string]MeshPeerSet{
		"app/preview": {MeshPeerSetRequest: peerSetRequest("app", "preview", s, a)},
		"app/staging": {MeshPeerSetRequest: peerSetRequest("app", "staging", s, a)},
	}, "app/preview")
	if err != nil {
		t.Fatal(err)
	}
	if len(union.peers) != 1 || union.peers[0].Name != "a" {
		t.Fatalf("peers = %+v, want a once", union.peers)
	}
}

func TestPeerSetUnionConflicts(t *testing.T) {
	s := peerSetNode("s", "10.210.0.2/24")
	a, b := peerSetNode("a", "10.210.0.1/24"), peerSetNode("b", "10.210.0.1/24")
	movedA := a
	movedA.Address = "10.210.0.9/24"
	otherCIDR := peerSetRequest("other", "production", s, peerSetNode("c", "10.220.0.5/24"))
	otherCIDR.NetworkCIDR = "10.220.0.0/16"
	otherPort := peerSetRequest("other", "production", s, peerSetNode("c", "10.210.0.5/24"))
	otherPort.Config.ListenPort = 51821
	selfMoved := s
	selfMoved.Address = "10.210.0.8/24"
	tests := []struct {
		name  string
		other MeshPeerSetRequest
		want  string
	}{
		{"address held by two keys", peerSetRequest("other", "production", s, b), "mesh address 10.210.0.1 is held by a (app/preview) and b (other/production)"},
		{"key holding two addresses", peerSetRequest("other", "production", s, movedA), "WireGuard key of a (app/preview) is at 10.210.0.1"},
		{"CIDR mismatch", otherCIDR, "multi-node environments that share a server need the same mesh: block"},
		{"port mismatch", otherPort, "multi-node environments that share a server need the same mesh: block"},
		{"node address mismatch", peerSetRequest("other", "production", selfMoved, peerSetNode("c", "10.210.0.5/24")), "addresses this node (s) as 10.210.0.8"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := renderMeshUnion(map[string]MeshPeerSet{
				"app/preview":      {MeshPeerSetRequest: peerSetRequest("app", "preview", s, a)},
				"other/production": {MeshPeerSetRequest: test.other},
			}, "other/production")
			var conflict *MeshPeerSetConflictError
			if !errors.As(err, &conflict) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want conflict containing %q", err, test.want)
			}
		})
	}
}

// A rejected union stores nothing and leaves the interface unchanged.
func TestPeerSetConflictStoresNothing(t *testing.T) {
	applies := recordMeshApplies(t)
	dataDir := t.TempDir()
	s := peerSetNode("s", "10.210.0.2/24")
	if _, err := PutMeshPeerSet(context.Background(), dataDir, peerSetRequest("app", "preview", s, peerSetNode("a", "10.210.0.1/24"))); err != nil {
		t.Fatal(err)
	}
	before := len(*applies)
	_, err := PutMeshPeerSet(context.Background(), dataDir, peerSetRequest("other", "production", s, peerSetNode("b", "10.210.0.1/24")))
	var conflict *MeshPeerSetConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %v, want conflict", err)
	}
	if len(*applies) != before {
		t.Fatal("conflicting set was applied")
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, "mesh", "peer-sets", "other", "production.json")); !os.IsNotExist(statErr) {
		t.Fatalf("conflicting set was stored: %v", statErr)
	}
}

// Single-node environments add no peers and impose no interface settings,
// so co-located single-node projects with different mesh: blocks still work.
func TestSingleNodePeerSetImposesNoInterfaceSettings(t *testing.T) {
	applies := recordMeshApplies(t)
	dataDir := t.TempDir()
	s := peerSetNode("s", "10.210.0.2/24")
	if _, err := PutMeshPeerSet(context.Background(), dataDir, peerSetRequest("app", "production", s, peerSetNode("a", "10.210.0.1/24"))); err != nil {
		t.Fatal(err)
	}
	solo := peerSetRequest("blog", "production", mesh.Node{Name: "s", Address: "10.50.0.1/24", PublicKey: s.PublicKey})
	solo.NetworkCIDR = "10.50.0.0/16"
	solo.Config.ListenPort = 51900
	if _, err := PutMeshPeerSet(context.Background(), dataDir, solo); err != nil {
		t.Fatalf("single-node set rejected: %v", err)
	}
	apply := lastApply(t, applies)
	if apply.config.ListenPort != 51820 || apply.node.Address != "10.210.0.2/24" {
		t.Fatalf("single-node set changed the interface: %+v", apply)
	}
	if got := renderedPeerNames(apply); strings.Join(got, ",") != "a=10.210.0.1" {
		t.Fatalf("peers = %v", got)
	}
}

func TestDeletePeerSetRerendersUnion(t *testing.T) {
	applies := recordMeshApplies(t)
	dataDir := t.TempDir()
	s := peerSetNode("s", "10.210.0.2/24")
	if _, err := PutMeshPeerSet(context.Background(), dataDir, peerSetRequest("app", "preview", s, peerSetNode("a", "10.210.0.1/24"))); err != nil {
		t.Fatal(err)
	}
	if _, err := PutMeshPeerSet(context.Background(), dataDir, peerSetRequest("app", "production", s, peerSetNode("b", "10.210.0.3/24"))); err != nil {
		t.Fatal(err)
	}
	if _, err := DeleteMeshPeerSet(context.Background(), dataDir, "app", "preview"); err != nil {
		t.Fatal(err)
	}
	if got := renderedPeerNames(lastApply(t, applies)); strings.Join(got, ",") != "b=10.210.0.3" {
		t.Fatalf("peers after delete = %v", got)
	}
	summaries := meshPeerSetSummaries(dataDir)
	if len(summaries) != 1 || summaries[0].Environment != "production" {
		t.Fatalf("status sets = %+v", summaries)
	}
}

// Peer names are comments plus the renderer's self filter; a peer of another
// project named like this node must not be dropped.
func TestPeerSetUnionQualifiesCollidingNames(t *testing.T) {
	s := peerSetNode("web-1", "10.210.0.2/24")
	other := mesh.Node{Name: "web-1", Address: "10.210.0.7/24", PublicKey: "wg-test-key-elsewhere"}
	union, err := renderMeshUnion(map[string]MeshPeerSet{
		"blog/production": {MeshPeerSetRequest: peerSetRequest("blog", "production", s, other)},
	}, "blog/production")
	if err != nil {
		t.Fatal(err)
	}
	if len(union.peers) != 1 || union.peers[0].Name != "blog.production.web-1" {
		t.Fatalf("peers = %+v", union.peers)
	}
}

// 4.3: an existing tagged peers.json becomes that environment's set, and an
// old CLI (PUT /v1/metadata, then POST /v1/mesh/apply) against a new takod
// keeps the other environment's peers.
func TestLegacyMeshMigrationAndOldCLIApplyPreserveOtherEnvironments(t *testing.T) {
	applies := recordMeshApplies(t)
	dataDir := t.TempDir()
	s, a, b := peerSetNode("s", "10.210.0.2/24"), peerSetNode("a", "10.210.0.1/24"), peerSetNode("b", "10.210.0.3/24")
	writeLegacyMetadata := func(environment string, peers ...mesh.Node) {
		t.Helper()
		_, err := WriteMetadata(context.Background(), dataDir, MetadataRequest{
			Node: map[string]any{"project": "app", "environment": environment, "node": "s", "host": s.Host,
				"mesh": map[string]any{"address": s.Address, "publicKey": s.PublicKey, "subnetBits": 24, "networkCIDR": "10.210.0.0/16", "interface": "tako", "listenPort": 51820}},
			Peers: map[string]any{"project": "app", "environment": environment, "networkCIDR": "10.210.0.0/16", "interface": "tako", "listenPort": 51820, "peers": peers},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	legacyApply := func(peers ...mesh.Node) {
		t.Helper()
		if _, err := reconcileLegacyMeshApply(context.Background(), dataDir, MeshApplyRequest{
			Config: mesh.WireGuardConfig{Enabled: true, Interface: "tako", ListenPort: 51820, NATTraversal: true},
			Node:   s, Peers: peers,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Before this takod: preview was the last legacy deploy.
	writeLegacyMetadata("preview", s, a)
	// A new CLI deploys production with peer sets; preview is migrated.
	if _, err := PutMeshPeerSet(context.Background(), dataDir, peerSetRequest("app", "production", s, b)); err != nil {
		t.Fatal(err)
	}
	if got := renderedPeerNames(lastApply(t, applies)); strings.Join(got, ",") != "a=10.210.0.1,b=10.210.0.3" {
		t.Fatalf("after migration peers = %v", got)
	}
	// An old CLI redeploys preview through the legacy endpoints.
	writeLegacyMetadata("preview", s, a)
	legacyApply(s, a)
	if got := renderedPeerNames(lastApply(t, applies)); strings.Join(got, ",") != "a=10.210.0.1,b=10.210.0.3" {
		t.Fatalf("old CLI apply dropped production peers: %v", got)
	}
	sets := meshPeerSetSummaries(dataDir)
	if len(sets) != 2 {
		t.Fatalf("stored sets = %+v", sets)
	}
}

func TestUntaggedLegacyPeersBecomeLegacySetUntilNextWrite(t *testing.T) {
	recordMeshApplies(t)
	dataDir := t.TempDir()
	s, a := peerSetNode("s", "10.210.0.2/24"), peerSetNode("a", "10.210.0.1/24")
	if _, err := WriteMetadata(context.Background(), dataDir, MetadataRequest{
		Node:  map[string]any{"node": "s", "mesh": map[string]any{"address": s.Address, "publicKey": s.PublicKey}},
		Peers: map[string]any{"peers": []mesh.Node{s, a}},
	}); err != nil {
		t.Fatal(err)
	}
	meshPeerSetsMu.Lock()
	err := migrateLegacyMeshLocked(dataDir)
	meshPeerSetsMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if sets := meshPeerSetSummaries(dataDir); len(sets) != 1 || sets[0].Project != legacyPeerSetKey {
		t.Fatalf("sets after migration = %+v", sets)
	}
	if _, err := PutMeshPeerSet(context.Background(), dataDir, peerSetRequest("app", "production", s, peerSetNode("b", "10.210.0.3/24"))); err != nil {
		t.Fatal(err)
	}
	if sets := meshPeerSetSummaries(dataDir); len(sets) != 1 || sets[0].Project != "app" {
		t.Fatalf("legacy set not dropped: %+v", sets)
	}
}
