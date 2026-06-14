package deployer

import (
	"slices"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestPlanTakodAssignmentsPinsToExistingLocalVolume(t *testing.T) {
	deploy := &Deployer{
		config:      testTakodDeployConfig([]string{"node-a", "node-b"}),
		environment: "production",
		volumeInspector: func(serverName string, volumeNames []string) (map[string]bool, error) {
			found := make(map[string]bool, len(volumeNames))
			for _, volumeName := range volumeNames {
				found[volumeName] = serverName == "node-b"
			}
			return found, nil
		},
	}

	assignments, err := deploy.planTakodAssignments("web", &config.ServiceConfig{
		Replicas: 1,
		Volumes:  []string{"data:/data"},
	})
	if err != nil {
		t.Fatalf("planTakodAssignments returned error: %v", err)
	}

	if got, want := assignmentServers(assignments), []string{"node-b"}; !slices.Equal(got, want) {
		t.Fatalf("assignment servers = %#v, want %#v", got, want)
	}
}

func TestPlanTakodAssignmentsRejectsLocalVolumeAcrossMultipleNodes(t *testing.T) {
	deploy := &Deployer{
		config:          testTakodDeployConfig([]string{"node-a", "node-b"}),
		environment:     "production",
		volumeInspector: noExistingVolumes,
	}

	_, err := deploy.planTakodAssignments("web", &config.ServiceConfig{
		Replicas: 2,
		Volumes:  []string{"data:/data"},
	})
	if err == nil {
		t.Fatal("expected local volume multi-node placement to be rejected")
	}
	if !strings.Contains(err.Error(), "local volume") || !strings.Contains(err.Error(), "multiple nodes") {
		t.Fatalf("error = %q, want local volume multi-node guidance", err.Error())
	}
}

func TestPlanTakodAssignmentsAllowsReplicatedVolumeAcrossNodes(t *testing.T) {
	cfg := testTakodDeployConfig([]string{"node-a", "node-b"})
	cfg.Volumes = map[string]config.VolumeConfig{
		"data": {Replicated: true},
	}
	deploy := &Deployer{
		config:      cfg,
		environment: "production",
		volumeInspector: func(serverName string, volumeNames []string) (map[string]bool, error) {
			t.Fatalf("replicated volumes should not be inspected as node-local")
			return nil, nil
		},
	}

	assignments, err := deploy.planTakodAssignments("web", &config.ServiceConfig{
		Replicas: 2,
		Volumes:  []string{"data:/data"},
	})
	if err != nil {
		t.Fatalf("planTakodAssignments returned error: %v", err)
	}

	if got, want := assignmentServers(assignments), []string{"node-a", "node-b"}; !slices.Equal(got, want) {
		t.Fatalf("assignment servers = %#v, want %#v", got, want)
	}
}

func TestPlanTakodAssignmentsCoLocatesSharedLocalVolume(t *testing.T) {
	deploy := &Deployer{
		config:          testTakodDeployConfig([]string{"node-a", "node-b"}),
		environment:     "production",
		volumeInspector: noExistingVolumes,
	}

	first, err := deploy.planTakodAssignments("web", &config.ServiceConfig{
		Replicas: 1,
		Volumes:  []string{"data:/data"},
	})
	if err != nil {
		t.Fatalf("first planTakodAssignments returned error: %v", err)
	}
	if got, want := assignmentServers(first), []string{"node-a"}; !slices.Equal(got, want) {
		t.Fatalf("first assignment servers = %#v, want %#v", got, want)
	}

	second, err := deploy.planTakodAssignments("worker", &config.ServiceConfig{
		Replicas: 1,
		Volumes:  []string{"data:/data"},
	})
	if err != nil {
		t.Fatalf("second planTakodAssignments returned error: %v", err)
	}
	if got, want := assignmentServers(second), []string{"node-a"}; !slices.Equal(got, want) {
		t.Fatalf("second assignment servers = %#v, want %#v", got, want)
	}
}

func TestPlanTakodAssignmentsGlobalAllowsLocalVolumeOnEveryNode(t *testing.T) {
	deploy := &Deployer{
		config:          testTakodDeployConfig([]string{"node-a", "node-b"}),
		environment:     "production",
		volumeInspector: noExistingVolumes,
	}

	assignments, err := deploy.planTakodAssignments("web", &config.ServiceConfig{
		Volumes: []string{"data:/data"},
		Placement: &config.PlacementConfig{
			Strategy: "global",
		},
	})
	if err != nil {
		t.Fatalf("planTakodAssignments returned error: %v", err)
	}

	if got, want := assignmentServers(assignments), []string{"node-a", "node-b"}; !slices.Equal(got, want) {
		t.Fatalf("assignment servers = %#v, want %#v", got, want)
	}
}

func assignmentServers(assignments []takodAssignment) []string {
	out := make([]string, 0, len(assignments))
	for _, assignment := range assignments {
		out = append(out, assignment.ServerName)
	}
	return out
}

func noExistingVolumes(serverName string, volumeNames []string) (map[string]bool, error) {
	found := make(map[string]bool, len(volumeNames))
	for _, volumeName := range volumeNames {
		found[volumeName] = false
	}
	return found, nil
}
