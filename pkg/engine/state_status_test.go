package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	localstate "github.com/redentordev/tako-cli/pkg/state"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takodstate"
)

func TestStateStatusBuildsBestKnownAndSyncRecommendation(t *testing.T) {
	base := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	oldRemote := stateStatusRemoteDeployment("deploy-old", base, "demo:v1")
	newRemote := stateStatusRemoteDeployment("deploy-new", base.Add(time.Hour), "demo:v2")
	olderHistory := &remotestate.DeploymentHistory{LastUpdated: base, Deployments: []*remotestate.DeploymentState{oldRemote}}
	newerHistory := &remotestate.DeploymentHistory{LastUpdated: base.Add(time.Hour), Deployments: []*remotestate.DeploymentState{newRemote}}

	result, err := New(Options{}).StateStatus(context.Background(), StateStatusRequest{
		Project:     "demo",
		Environment: "production",
		Local: StateStatusLocalInput{
			Path:   ".tako",
			Exists: true,
			Current: &localstate.DeploymentState{
				DeploymentID: "deploy-old",
				Timestamp:    base,
			},
		},
		Nodes: []StateStatusRemoteNodeInput{
			{
				Name:    "node-a",
				Host:    "10.0.0.1",
				History: olderHistory,
				Desired: &takodstate.DesiredRevision{RevisionID: "rev-a", CreatedAt: base, Services: map[string]takodstate.DesiredService{"web": {Name: "web"}}},
				Actual:  &takodstate.ActualSnapshot{Project: "demo", Environment: "production", CapturedAt: base, Services: map[string]takodstate.ActualService{"web": {Name: "web", Replicas: 1}}},
			},
			{
				Name:    "node-b",
				Host:    "10.0.0.2",
				History: newerHistory,
				Desired: &takodstate.DesiredRevision{RevisionID: "rev-b", CreatedAt: base.Add(time.Hour), Services: map[string]takodstate.DesiredService{"web": {Name: "web"}, "worker": {Name: "worker"}}},
				Actual:  &takodstate.ActualSnapshot{Project: "demo", Environment: "production", CapturedAt: base.Add(time.Hour), Services: map[string]takodstate.ActualService{"web": {Name: "web", Replicas: 1}, "worker": {Name: "worker", Replicas: 1}}},
			},
		},
	})
	if err != nil {
		t.Fatalf("StateStatus returned error: %v", err)
	}
	if result.APIVersion != takoapi.APIVersionCurrent || result.Kind != KindStateStatusResult || result.Project != "demo" || result.Environment != "production" {
		t.Fatalf("identity = %#v", result)
	}
	if result.Counts.ConfiguredNodes != 2 || result.Counts.ReachableNodes != 2 || result.Counts.RemoteHistoryNodes != 2 {
		t.Fatalf("counts = %#v", result.Counts)
	}
	if result.BestKnown.History == nil || result.BestKnown.History.Source != "node-b" || result.BestKnown.History.Latest.ID != "deploy-new" {
		t.Fatalf("best history = %#v", result.BestKnown.History)
	}
	if result.BestKnown.Desired == nil || result.BestKnown.Desired.Source != "node-b" || result.BestKnown.Desired.RevisionID != "rev-b" {
		t.Fatalf("best desired = %#v", result.BestKnown.Desired)
	}
	if result.BestKnown.Actual == nil || result.BestKnown.Actual.Source != "node-b" || result.BestKnown.Actual.ServiceCount != 2 {
		t.Fatalf("best actual = %#v", result.BestKnown.Actual)
	}
	joined := strings.Join(result.Sync.Recommendations, "\n")
	if !strings.Contains(joined, "Remote deployment history from node-b is newer") || !strings.Contains(joined, "Run 'tako state pull'") {
		t.Fatalf("sync recommendations = %q", joined)
	}
}

func TestStateStatusReturnsResultWithNoReachableError(t *testing.T) {
	result, err := New(Options{}).StateStatus(context.Background(), StateStatusRequest{
		Project:     "demo",
		Environment: "production",
		Nodes: []StateStatusRemoteNodeInput{
			{Name: "node-b", Host: "10.0.0.2", ConnectErr: errors.New("refused")},
			{Name: "node-a", Host: "10.0.0.1", ConnectErr: errors.New("timeout")},
		},
	})
	if err == nil {
		t.Fatal("StateStatus should return no-reachable error")
	}
	if result == nil {
		t.Fatal("StateStatus should return a result with the error")
	}
	if result.Counts.ReachableNodes != 0 || result.Counts.UnreachableNodes != 2 {
		t.Fatalf("counts = %#v", result.Counts)
	}
	if result.Error == "" || !strings.Contains(result.Error, "deploy will fail closed") || !strings.Contains(result.Error, "node-a: timeout; node-b: refused") {
		t.Fatalf("error = %q", result.Error)
	}
	if len(result.Remote.UnreachableGuidance) == 0 {
		t.Fatalf("expected unreachable guidance: %#v", result.Remote)
	}
}

func stateStatusRemoteDeployment(id string, timestamp time.Time, image string) *remotestate.DeploymentState {
	return &remotestate.DeploymentState{
		ID:        id,
		Timestamp: timestamp,
		Status:    remotestate.StatusSuccess,
		Services: map[string]remotestate.ServiceState{
			"web": {Name: "web", Image: image, Replicas: 1},
		},
	}
}
