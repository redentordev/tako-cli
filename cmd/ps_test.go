package cmd

import (
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestDesiredReplicasForSelection(t *testing.T) {
	envServers := []string{"node-a", "node-b", "node-c"}

	tests := []struct {
		name      string
		service   config.ServiceConfig
		selected  []string
		wantCount int
	}{
		{
			name: "spread counts only selected nodes",
			service: config.ServiceConfig{
				Replicas: 5,
			},
			selected:  []string{"node-a", "node-c"},
			wantCount: 3,
		},
		{
			name: "global maps one replica per environment node",
			service: config.ServiceConfig{
				Placement: &config.PlacementConfig{Strategy: "global"},
			},
			selected:  []string{"node-b"},
			wantCount: 1,
		},
		{
			name: "pinned ignores unselected nodes",
			service: config.ServiceConfig{
				Replicas: 3,
				Placement: &config.PlacementConfig{
					Strategy: "pinned",
					Servers:  []string{"node-b"},
				},
			},
			selected:  []string{"node-a"},
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := desiredReplicasForSelection(tt.service, envServers, tt.selected)
			if got != tt.wantCount {
				t.Fatalf("desiredReplicasForSelection() = %d, want %d", got, tt.wantCount)
			}
		})
	}
}
