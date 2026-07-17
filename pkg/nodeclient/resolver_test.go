package nodeclient

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

const (
	clusterID = "11111111-1111-4111-8111-111111111111"
	nodeID    = "22222222-2222-4222-8222-222222222222"
)

type fakeIdentityProbe struct {
	status *takodclient.AgentStatus
	err    error
	calls  int
}

func (p *fakeIdentityProbe) Status(context.Context) (*takodclient.AgentStatus, error) {
	p.calls++
	return p.status, p.err
}

func enrolledStatus(t *testing.T, observedClusterID string, observedNodeID string) *takodclient.AgentStatus {
	t.Helper()
	identity, err := nodeidentity.New(observedClusterID, observedNodeID, "node-1", []string{nodeidentity.RoleWorker}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return &takodclient.AgentStatus{Runtime: "takod", Capabilities: []string{nodeidentity.Capability}, Identity: &identity.Identity}
}

func TestResolvePreservesLegacySSHWithoutLocalProbe(t *testing.T) {
	probe := &fakeIdentityProbe{err: errors.New("must not be called")}
	decision, err := Resolve(context.Background(), "", nodeidentity.Reference{}, probe)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if decision.Transport != TransportSSH || decision.Evidence != EvidenceLegacySSHDefault {
		t.Fatalf("decision = %#v", decision)
	}
	if probe.calls != 0 {
		t.Fatalf("legacy SSH unexpectedly probed local identity %d times", probe.calls)
	}
}

func TestResolveExplicitSSHNeverProbesLocalIdentity(t *testing.T) {
	probe := &fakeIdentityProbe{err: errors.New("must not be called")}
	decision, err := Resolve(context.Background(), PolicySSH, nodeidentity.Reference{ClusterID: clusterID, NodeID: nodeID}, probe)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if decision.Transport != TransportSSH || decision.Evidence != EvidenceExplicitSSH {
		t.Fatalf("decision = %#v", decision)
	}
	if probe.calls != 0 {
		t.Fatalf("explicit SSH unexpectedly probed local identity %d times", probe.calls)
	}
}

func TestResolveAutoSelectsLocalOnlyForImmutableIdentityMatch(t *testing.T) {
	probe := &fakeIdentityProbe{status: enrolledStatus(t, clusterID, nodeID)}
	decision, err := Resolve(context.Background(), PolicyAuto, nodeidentity.Reference{ClusterID: clusterID, NodeID: nodeID}, probe)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if decision.Transport != TransportLocal || decision.Evidence != EvidenceInstallationMatch || decision.NodeID != nodeID {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestResolveAutoFallsBackToPredecidedSSHOnMismatch(t *testing.T) {
	wrongNodeID := "33333333-3333-4333-8333-333333333333"
	probe := &fakeIdentityProbe{status: enrolledStatus(t, clusterID, wrongNodeID)}
	decision, err := Resolve(context.Background(), PolicyAuto, nodeidentity.Reference{ClusterID: clusterID, NodeID: nodeID}, probe)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if decision.Transport != TransportSSH || decision.Evidence != EvidenceLocalIdentityWrong {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestResolveAutoFallsBackToPredecidedSSHWhenProbeFails(t *testing.T) {
	probe := &fakeIdentityProbe{err: errors.New("socket denied")}
	decision, err := Resolve(context.Background(), PolicyAuto, nodeidentity.Reference{ClusterID: clusterID, NodeID: nodeID}, probe)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if decision.Transport != TransportSSH || decision.Evidence != EvidenceLocalProbeFailed {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestResolveAutoRejectsNilOrMalformedStatusWithoutPanicking(t *testing.T) {
	valid := enrolledStatus(t, clusterID, nodeID)
	malformed := *valid
	malformed.Identity = &nodeidentity.Identity{
		APIVersion: nodeidentity.APIVersion,
		Kind:       nodeidentity.Kind,
		ClusterID:  clusterID,
		NodeID:     nodeID,
		NodeName:   "../forged",
		CreatedAt:  time.Now(),
	}
	tests := []struct {
		name  string
		probe *fakeIdentityProbe
	}{
		{name: "nil status", probe: &fakeIdentityProbe{}},
		{name: "malformed matching identity", probe: &fakeIdentityProbe{status: &malformed}},
		{name: "missing capability", probe: &fakeIdentityProbe{status: &takodclient.AgentStatus{Runtime: "takod", Identity: valid.Identity}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision, err := Resolve(context.Background(), PolicyAuto, nodeidentity.Reference{ClusterID: clusterID, NodeID: nodeID}, test.probe)
			if err != nil {
				t.Fatalf("Resolve returned error: %v", err)
			}
			if decision.Transport != TransportSSH || decision.Evidence != EvidenceLocalProbeFailed {
				t.Fatalf("decision = %#v, want failed probe SSH decision", decision)
			}
		})
	}
}

func TestResolveLocalFailsClosedWithoutMatchingIdentity(t *testing.T) {
	tests := []struct {
		name  string
		probe *fakeIdentityProbe
	}{
		{name: "probe failure", probe: &fakeIdentityProbe{err: errors.New("socket denied")}},
		{name: "missing identity", probe: &fakeIdentityProbe{status: &takodclient.AgentStatus{Runtime: "takod"}}},
		{name: "wrong identity", probe: &fakeIdentityProbe{status: enrolledStatus(t, clusterID, "33333333-3333-4333-8333-333333333333")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Resolve(context.Background(), PolicyLocal, nodeidentity.Reference{ClusterID: clusterID, NodeID: nodeID}, test.probe)
			if err == nil || !strings.Contains(err.Error(), "identity verification failed") {
				t.Fatalf("Resolve error = %v", err)
			}
		})
	}
}

func TestResolveIdentityPolicyRequiresValidReferenceBeforeProbe(t *testing.T) {
	probe := &fakeIdentityProbe{status: enrolledStatus(t, clusterID, nodeID)}
	_, err := Resolve(context.Background(), PolicyAuto, nodeidentity.Reference{ClusterID: "bad", NodeID: nodeID}, probe)
	if err == nil || !strings.Contains(err.Error(), "valid node reference") {
		t.Fatalf("Resolve error = %v", err)
	}
	if probe.calls != 0 {
		t.Fatalf("invalid reference unexpectedly probed local identity %d times", probe.calls)
	}
}

func TestResolveRejectsUnknownPolicy(t *testing.T) {
	_, err := Resolve(context.Background(), Policy("magic"), nodeidentity.Reference{}, nil)
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("Resolve error = %v", err)
	}
}
