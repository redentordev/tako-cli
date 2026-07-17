package platform

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

func TestAuthorizeAllocationsUsesIndependentGenerationAndOmissionRevokes(t *testing.T) {
	store, controller, now := newTestMembershipStore(t)
	allocation := nodeidentity.ActiveAllocation{Kind: "mesh-upstream", Project: "demo", Environment: "production", Service: "web", Slot: 1, HostIP: "10.210.0.1", HostPort: 20000, ContainerPort: 3000, Key: "mesh-upstream/demo/production/web/1", ClusterID: controller.ClusterID, NodeID: controller.NodeID, Generation: 1, IssuedAt: now}
	allocation.Signature = signActiveAllocation(t, controller, allocation)
	before, _ := store.Read()
	authorized, err := store.AuthorizeAllocations("demo", "production", []nodeidentity.ActiveAllocation{allocation})
	if err != nil {
		t.Fatal(err)
	}
	if authorized.Generation != before.Generation || authorized.AllocationGeneration != before.AllocationGeneration+1 || len(authorized.ActiveAllocations) != 1 || authorized.ActiveAllocations[0].AuthorizedAt.IsZero() {
		t.Fatalf("authorization generations = %#v", authorized)
	}
	revoked, err := store.AuthorizeAllocations("demo", "production", nil)
	if err != nil {
		t.Fatal(err)
	}
	if revoked.Generation != before.Generation || revoked.AllocationGeneration != authorized.AllocationGeneration+1 || len(revoked.ActiveAllocations) != 0 {
		t.Fatalf("omission did not revoke allocation: %#v", revoked)
	}
	if _, err := store.AuthorizeAllocations("demo", "production", []nodeidentity.ActiveAllocation{allocation}); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("withdrawn proof replay was accepted: %v", err)
	}
	newer := allocation
	newer.Generation++
	newer.IssuedAt = now.Add(time.Second)
	newer.Signature = signActiveAllocation(t, controller, newer)
	if _, err := store.AuthorizeAllocations("demo", "production", []nodeidentity.ActiveAllocation{newer}); err != nil {
		t.Fatalf("new worker generation after revocation: %v", err)
	}
	forged := allocation
	forged.Generation++
	if _, err := store.AuthorizeAllocations("demo", "production", []nodeidentity.ActiveAllocation{forged}); err == nil {
		t.Fatal("forged allocation generation was authorized")
	}
}

func TestPreviewThenCommitPersistsExactEdgeAcknowledgedInventory(t *testing.T) {
	store, controller, now := newTestMembershipStore(t)
	allocation := nodeidentity.ActiveAllocation{Kind: "mesh-upstream", Project: "demo", Environment: "production", Service: "web", Slot: 1, HostIP: "10.210.0.1", HostPort: 20000, ContainerPort: 3000, Key: "mesh-upstream/demo/production/web/1", ClusterID: controller.ClusterID, NodeID: controller.NodeID, Generation: 1, IssuedAt: now}
	allocation.Signature = signActiveAllocation(t, controller, allocation)
	base, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := store.PreviewAllocations("demo", "production", []nodeidentity.ActiveAllocation{allocation})
	if err != nil {
		t.Fatal(err)
	}
	committed, err := store.CommitPreparedAllocations(prepared, base.Generation, base.AllocationGeneration)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(prepared.Inventory())
	got, _ := json.Marshal(committed.Inventory())
	if string(want) != string(got) {
		t.Fatalf("committed inventory differs from acknowledged proposal\nwant=%s\ngot=%s", want, got)
	}
	if _, err := store.CommitPreparedAllocations(prepared, base.Generation, base.AllocationGeneration); err == nil {
		t.Fatal("stale prepared base committed twice")
	}
}

func signActiveAllocation(t *testing.T, installation *nodeidentity.Installation, allocation nodeidentity.ActiveAllocation) string {
	t.Helper()
	evidence := struct {
		Kind          string    `json:"kind"`
		Project       string    `json:"project"`
		Environment   string    `json:"environment"`
		Service       string    `json:"service"`
		Revision      string    `json:"revision,omitempty"`
		Slot          int       `json:"slot"`
		HostIP        string    `json:"hostIp"`
		HostPort      int       `json:"hostPort"`
		ContainerPort int       `json:"containerPort"`
		Key           string    `json:"key"`
		ClusterID     string    `json:"clusterId,omitempty"`
		NodeID        string    `json:"nodeId,omitempty"`
		Generation    uint64    `json:"generation,omitempty"`
		IssuedAt      time.Time `json:"issuedAt,omitempty"`
		Signature     string    `json:"signature,omitempty"`
	}{allocation.Kind, allocation.Project, allocation.Environment, allocation.Service, allocation.Revision, allocation.Slot, allocation.HostIP, allocation.HostPort, allocation.ContainerPort, allocation.Key, allocation.ClusterID, allocation.NodeID, allocation.Generation, allocation.IssuedAt, ""}
	data, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := installation.SignAllocation(data)
	if err != nil {
		t.Fatal(err)
	}
	return signature
}
