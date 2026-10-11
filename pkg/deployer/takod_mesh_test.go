package deployer

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/mesh"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
)

// freshMeshStatus is a member that has never recorded mesh state.
func freshMeshStatus(string) (takod.Status, error) {
	return takod.Status{Runtime: "takod"}, nil
}

// recordedMeshStatuses returns a status reader whose members recorded the
// given node.mesh.address values.
func recordedMeshStatuses(recorded map[string]string) func(string) (takod.Status, error) {
	return func(serverName string) (takod.Status, error) {
		status := takod.Status{Runtime: "takod"}
		if address, ok := recorded[serverName]; ok {
			status.Node = map[string]any{
				"project":     "demo",
				"environment": "production",
				"node":        serverName,
				"mesh":        map[string]any{"address": address, "publicKey": testMeshKey(serverName)},
			}
		}
		return status, nil
	}
}

func testMeshKey(serverName string) string {
	return "test-wg-key-" + serverName
}

type meshRequest struct {
	server   string
	method   string
	endpoint string
	body     []byte
}

// meshRecorder fakes the takod mesh endpoints of every node and records the
// requests in order.
type meshRecorder struct {
	mu       sync.Mutex
	requests []meshRequest
}

func (r *meshRecorder) client(serverName string) (any, error) {
	return &meshRecorderClient{recorder: r, server: serverName}, nil
}

func (r *meshRecorder) applies() map[string]takod.MeshApplyRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]takod.MeshApplyRequest{}
	for _, request := range r.requests {
		if request.endpoint != "/v1/mesh/apply" {
			continue
		}
		var apply takod.MeshApplyRequest
		if err := json.Unmarshal(request.body, &apply); err != nil {
			panic(err)
		}
		out[request.server] = apply
	}
	return out
}

func (r *meshRecorder) peerSets() map[string]takod.MeshPeerSetRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]takod.MeshPeerSetRequest{}
	for _, request := range r.requests {
		if request.endpoint != "/v1/mesh/peer-sets" {
			continue
		}
		var set takod.MeshPeerSetRequest
		if err := json.Unmarshal(request.body, &set); err != nil {
			panic(err)
		}
		out[request.server] = set
	}
	return out
}

func (r *meshRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

type meshRecorderClient struct {
	recorder *meshRecorder
	server   string
}

func (c *meshRecorderClient) RequestJSON(ctx context.Context, method, endpoint string, value any) (string, error) {
	return c.RequestJSONWithTimeout(ctx, method, endpoint, value, time.Second)
}

func (c *meshRecorderClient) RequestJSONWithTimeout(_ context.Context, method, endpoint string, value any, _ time.Duration) (string, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	c.recorder.mu.Lock()
	c.recorder.requests = append(c.recorder.requests, meshRequest{server: c.server, method: method, endpoint: endpoint, body: body})
	c.recorder.mu.Unlock()
	switch endpoint {
	case "/v1/mesh/key":
		data, _ := json.Marshal(takod.MeshKeyResponse{PublicKey: testMeshKey(c.server)})
		return string(data), nil
	case "/v1/metadata", "/v1/mesh/apply", "/v1/mesh/peer-sets":
		return "{}", nil
	}
	return "", fmt.Errorf("unexpected request %s %s", method, endpoint)
}

func meshTestDeployer(t *testing.T, servers []string, targets []string, reader func(string) (takod.Status, error)) (*Deployer, *meshRecorder) {
	t.Helper()
	recorder := &meshRecorder{}
	d := &Deployer{
		config:           testTakodDeployConfig(servers),
		environment:      "production",
		targetServers:    targets,
		meshStatusReader: reader,
		meshClient:       recorder.client,
	}
	return d, recorder
}

func appliedAddresses(t *testing.T, applies map[string]takod.MeshApplyRequest) map[string]string {
	t.Helper()
	out := map[string]string{}
	for server, apply := range applies {
		out[server] = apply.Node.Address
	}
	return out
}

func peerSummary(apply takod.MeshApplyRequest) []string {
	out := make([]string, 0, len(apply.Peers))
	for _, peer := range apply.Peers {
		out = append(out, peer.Name+"="+peer.Address)
	}
	sort.Strings(out)
	return out
}

// 2.1/2.3: a deploy whose workloads touch one node still sends the full
// environment peer list to every member.
func TestSubsetDeploySendsFullPeerListToEveryMember(t *testing.T) {
	d, recorder := meshTestDeployer(t, []string{"a", "b", "c"}, []string{"c"}, freshMeshStatus)
	if err := d.SetupTakodRuntime(); err != nil {
		t.Fatal(err)
	}
	applies := recorder.applies()
	if len(applies) != 3 {
		t.Fatalf("mesh apply sent to %d nodes, want every member: %v", len(applies), applies)
	}
	want := []string{"a=10.210.0.1/24", "b=10.210.0.2/24", "c=10.210.0.3/24"}
	for server, apply := range applies {
		if got := peerSummary(apply); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s peers = %v, want %v", server, got, want)
		}
	}
}

// 2.1: scale with servers listed as [b, a] resolves the same addresses as a
// full deploy.
func TestScaleTargetOrderDoesNotRenumber(t *testing.T) {
	deploy, deployRecorder := meshTestDeployer(t, []string{"a", "b"}, nil, freshMeshStatus)
	if err := deploy.SetupTakodRuntime(); err != nil {
		t.Fatal(err)
	}
	scale, scaleRecorder := meshTestDeployer(t, []string{"b", "a"}, []string{"b", "a"}, freshMeshStatus)
	if err := scale.SetupTakodRuntime(); err != nil {
		t.Fatal(err)
	}
	deployAddresses := appliedAddresses(t, deployRecorder.applies())
	scaleAddresses := appliedAddresses(t, scaleRecorder.applies())
	if !reflect.DeepEqual(deployAddresses, scaleAddresses) {
		t.Fatalf("scale addresses %v differ from deploy addresses %v", scaleAddresses, deployAddresses)
	}
	if deployAddresses["a"] != "10.210.0.1/24" || deployAddresses["b"] != "10.210.0.2/24" {
		t.Fatalf("fresh addresses = %v", deployAddresses)
	}
}

// 2.2/2.3 migration: addresses recorded by the positional scheme survive the
// switch to the resolver, even where positional order would now differ.
func TestMigrationPreservesRecordedMeshAddresses(t *testing.T) {
	recorded := map[string]string{"a": "10.210.0.2/24", "b": "10.210.0.1/24", "c": "10.210.0.7/24"}
	d, recorder := meshTestDeployer(t, []string{"a", "b", "c", "d"}, nil, recordedMeshStatuses(recorded))
	if err := d.SetupTakodRuntime(); err != nil {
		t.Fatal(err)
	}
	got := appliedAddresses(t, recorder.applies())
	want := map[string]string{"a": "10.210.0.2/24", "b": "10.210.0.1/24", "c": "10.210.0.7/24", "d": "10.210.0.3/24"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("addresses = %v, want %v", got, want)
	}
	// Proxy upstreams use the same resolution.
	ip, err := d.meshHostIPForServer("c")
	if err != nil {
		t.Fatal(err)
	}
	if ip != "10.210.0.7" {
		t.Fatalf("proxy upstream mesh IP for c = %s, want 10.210.0.7", ip)
	}
}

// Proxy reconciles that never ran mesh apply read the recorded addresses.
func TestMeshHostIPReadsRecordedAddressWithoutMeshApply(t *testing.T) {
	d, recorder := meshTestDeployer(t, []string{"a", "b"}, nil, recordedMeshStatuses(map[string]string{"a": "10.210.0.9/24", "b": "10.210.0.4/24"}))
	ip, err := d.meshHostIPForServer("a")
	if err != nil {
		t.Fatal(err)
	}
	if ip != "10.210.0.9" {
		t.Fatalf("mesh IP = %s, want recorded 10.210.0.9", ip)
	}
	if recorder.count() != 0 {
		t.Fatalf("resolving proxy upstreams wrote to nodes: %d requests", recorder.count())
	}
	if _, err := d.meshHostIPForServer("elsewhere"); err == nil {
		t.Fatal("a server outside the environment resolved a mesh IP")
	}
}

// 2.4: tako run only verifies agents; it never writes metadata or mesh.
func TestVerifyTakodRuntimeSendsNoMeshRequests(t *testing.T) {
	d, recorder := meshTestDeployer(t, []string{"a", "b"}, []string{"a"}, freshMeshStatus)
	if err := d.VerifyTakodRuntime(); err != nil {
		t.Fatal(err)
	}
	if recorder.count() != 0 {
		t.Fatalf("run sent %d node requests, want none", recorder.count())
	}
}

// 2.6: a node shared with another environment whose takod lacks peer sets
// gets one warning naming the sharing environment.
func TestLegacySharedNodeWarning(t *testing.T) {
	reader := func(serverName string) (takod.Status, error) {
		status := takod.Status{Runtime: "takod"}
		if serverName == "b" {
			status.Node = map[string]any{"project": "other", "environment": "staging", "mesh": map[string]any{"address": "10.210.0.1/24"}}
		}
		return status, nil
	}
	d, _ := meshTestDeployer(t, []string{"a", "b"}, nil, reader)
	sink := &capturingSink{}
	d.SetEventSink(sink)
	if err := d.SetupTakodRuntime(); err != nil {
		t.Fatal(err)
	}
	var warnings []string
	for _, event := range sink.events {
		if event.Type == events.TypeWarning && strings.Contains(event.Message, "shared with") {
			warnings = append(warnings, event.Message)
		}
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "server b is shared with other/staging") || !strings.Contains(warnings[0], "tako upgrade servers --server b") {
		t.Fatalf("shared node warnings = %v", warnings)
	}

	// A capable takod is not warned about.
	capable := func(serverName string) (takod.Status, error) {
		status, _ := reader(serverName)
		status.Capabilities = []string{takod.CapabilityMeshPeerSetsV1}
		return status, nil
	}
	d, _ = meshTestDeployer(t, []string{"a", "b"}, nil, capable)
	sink = &capturingSink{}
	d.SetEventSink(sink)
	if err := d.SetupTakodRuntime(); err != nil {
		t.Fatal(err)
	}
	for _, event := range sink.events {
		if strings.Contains(event.Message, "shared with") {
			t.Fatalf("capable node warned: %s", event.Message)
		}
	}
}

// Members outside the environment never influence its addresses.
func TestMeshMembersIgnoreOtherEnvironments(t *testing.T) {
	cfg := testTakodDeployConfig([]string{"a", "b"})
	cfg.Servers["z-staging"] = config.ServerConfig{Host: "z.example.test", User: "root"}
	cfg.Environments["staging"] = config.EnvironmentConfig{Servers: []string{"z-staging"}}
	d := &Deployer{config: cfg, environment: "production", meshStatusReader: freshMeshStatus}
	members, err := d.meshMembers()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(members, []string{"a", "b"}) {
		t.Fatalf("members = %v", members)
	}
}

// 4.4: each member is reconciled through peer sets when its own takod has
// them and through the legacy mesh apply otherwise.
func TestMeshPeerSetsChosenPerMember(t *testing.T) {
	reader := func(serverName string) (takod.Status, error) {
		status := takod.Status{Runtime: "takod"}
		if serverName == "a" {
			status.Capabilities = []string{takod.CapabilityMeshPeerSetsV1}
		}
		return status, nil
	}
	d, recorder := meshTestDeployer(t, []string{"a", "b"}, nil, reader)
	if err := d.SetupTakodRuntime(); err != nil {
		t.Fatal(err)
	}
	sets := recorder.peerSets()
	applies := recorder.applies()
	if _, ok := sets["a"]; !ok || len(sets) != 1 {
		t.Fatalf("peer sets sent to %v, want only a", sets)
	}
	if _, ok := applies["b"]; !ok || len(applies) != 1 {
		t.Fatalf("legacy mesh apply sent to %v, want only b", applies)
	}
	set := sets["a"]
	if set.Project != "demo" || set.Environment != "production" || set.NetworkCIDR != "10.210.0.0/16" || set.SubnetBits != 24 {
		t.Fatalf("peer set identity = %+v", set)
	}
	if set.Node.Address != "10.210.0.1/24" || set.Node.Name != "a" {
		t.Fatalf("peer set node = %+v", set.Node)
	}
	var peers []string
	for _, peer := range set.Peers {
		peers = append(peers, peer.Name+"="+peer.Address)
	}
	sort.Strings(peers)
	if want := []string{"a=10.210.0.1/24", "b=10.210.0.2/24"}; !reflect.DeepEqual(peers, want) {
		t.Fatalf("peer set peers = %v, want %v", peers, want)
	}
}

// 4.4: addresses used by another environment's peer set on a shared node are
// never handed out, and the node keeps the address its sets agree on.
func TestMeshAddressesAvoidOtherPeerSets(t *testing.T) {
	reader := func(serverName string) (takod.Status, error) {
		status := takod.Status{Runtime: "takod", Capabilities: []string{takod.CapabilityMeshPeerSetsV1}}
		if serverName == "a" {
			status.MeshPeerSets = []takod.MeshPeerSetSummary{{
				Project:     "other",
				Environment: "staging",
				Node:        mesh.Node{Name: "a", Address: "10.210.0.5/24", PublicKey: testMeshKey("a")},
				Peers: []mesh.Node{
					mesh.Node{Name: "a", Address: "10.210.0.5/24", PublicKey: testMeshKey("a")},
					mesh.Node{Name: "z", Address: "10.210.0.1/24", PublicKey: "test-wg-key-other-z"},
				},
			}}
		}
		return status, nil
	}
	d, recorder := meshTestDeployer(t, []string{"a", "b"}, nil, reader)
	if err := d.SetupTakodRuntime(); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for server, set := range recorder.peerSets() {
		got[server] = set.Node.Address
	}
	want := map[string]string{"a": "10.210.0.5/24", "b": "10.210.0.2/24"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("addresses = %v, want %v", got, want)
	}
}
