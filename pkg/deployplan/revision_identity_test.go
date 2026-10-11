package deployplan

import (
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestServiceRevisionIDStable(t *testing.T) {
	service := config.ServiceConfig{
		Image: "web:stable",
		Deploy: config.DeployConfig{
			Strategy: config.DeployStrategyRolling,
		},
	}

	got := ServiceRevisionID("demo", "production", "web", "registry.example.com/demo/web@sha256:abcdef", service)
	const want = "dff30b30111c"
	if got != want {
		t.Fatalf("ServiceRevisionID() = %q, want %q", got, want)
	}
}

func TestEffectiveDeployStrategyDelegatesToDefaultSelection(t *testing.T) {
	if got := EffectiveDeployStrategy(nil); got != config.DeployStrategyRecreate {
		t.Fatalf("EffectiveDeployStrategy(nil) = %q, want recreate", got)
	}
	if got := EffectiveDeployStrategy(&config.ServiceConfig{}); got != config.DeployStrategyRecreate {
		t.Fatalf("EffectiveDeployStrategy(portless service) = %q, want recreate", got)
	}
	if got := EffectiveDeployStrategy(&config.ServiceConfig{Port: 3000}); got != config.DeployStrategyRolling {
		t.Fatalf("EffectiveDeployStrategy(stateless port service) = %q, want rolling", got)
	}
	if got := EffectiveDeployStrategy(&config.ServiceConfig{Port: 5432, Persistent: true, Volumes: []string{"data:/var/lib/postgresql/data"}}); got != config.DeployStrategyRecreate {
		t.Fatalf("EffectiveDeployStrategy(persistent service) = %q, want recreate", got)
	}
	if got := EffectiveDeployStrategy(&config.ServiceConfig{Port: 3000, Deploy: config.DeployConfig{Strategy: config.DeployStrategyRecreate}}); got != config.DeployStrategyRecreate {
		t.Fatalf("EffectiveDeployStrategy(explicit recreate) = %q, want recreate", got)
	}
	service := &config.ServiceConfig{Deploy: config.DeployConfig{Strategy: config.DeployStrategyRolling}}
	if got := EffectiveDeployStrategy(service); got != config.DeployStrategyRolling {
		t.Fatalf("EffectiveDeployStrategy(rolling service) = %q, want rolling", got)
	}
}
