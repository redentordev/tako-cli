package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func lifecycleMutationTestConfig() *config.Config {
	return &config.Config{
		Project: config.ProjectConfig{Name: "demo", Version: "1"},
		Servers: map[string]config.ServerConfig{
			"active": {Host: "127.0.0.1", User: "root", Password: "$SSH_PASSWORD", Lifecycle: "schedulable"},
			"ready":  {Host: "127.0.0.2", User: "root", Password: "$SSH_PASSWORD", Lifecycle: "ready"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {Servers: []string{"active", "ready"}, Services: map[string]config.ServiceConfig{}},
		},
	}
}

func TestDestroyFailsBeforePartialFanoutWhenEnvironmentContainsReadyNode(t *testing.T) {
	if _, _, err := DestroyEnvironmentTargets(lifecycleMutationTestConfig(), "production"); err == nil || !strings.Contains(err.Error(), "ready") {
		t.Fatalf("destroy lifecycle admission error = %v", err)
	}
}

func TestRemoveFailsBeforePartialFanoutWhenEnvironmentContainsReadyNode(t *testing.T) {
	engine := &Engine{}
	if _, err := engine.PlanRemove(context.Background(), RemoveRequest{Config: lifecycleMutationTestConfig(), Environment: "production"}); err == nil || !strings.Contains(err.Error(), "ready") {
		t.Fatalf("remove lifecycle admission error = %v", err)
	}
}
