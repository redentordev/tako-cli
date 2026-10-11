package cmd

import (
	"slices"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
)

// The legacy-agent step keeps its machine key, runs after takod-service and
// is never skipped, so converge also removes the agent.
func TestSetupLegacyMonitorAgentStepRunsAfterTakodServiceInEveryMode(t *testing.T) {
	for _, converge := range []bool{false, true} {
		steps := setupNodeSteps(&recordingSetupRefresher{}, &config.Config{}, "node-a", "deploy", 51820, "", converge, setupAccessOptions{})
		keys := make([]string, 0, len(steps))
		for _, step := range steps {
			keys = append(keys, step.key)
		}
		service := slices.Index(keys, engine.SetupStepTakodService)
		agent := slices.Index(keys, engine.SetupStepMonitorAgent)
		if service < 0 || agent != service+1 || agent != len(keys)-1 {
			t.Fatalf("converge=%v keys = %v, want monitor-agent right after takod-service", converge, keys)
		}
		step := steps[agent]
		if step.skip || step.title != "Removing legacy monitoring agent" {
			t.Fatalf("converge=%v monitor-agent step = %+v", converge, step)
		}
	}
}
