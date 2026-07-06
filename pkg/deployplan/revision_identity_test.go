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

func TestEffectiveDeployStrategyDefaultsToRecreate(t *testing.T) {
	if got := EffectiveDeployStrategy(nil); got != config.DeployStrategyRecreate {
		t.Fatalf("EffectiveDeployStrategy(nil) = %q, want recreate", got)
	}
	if got := EffectiveDeployStrategy(&config.ServiceConfig{}); got != config.DeployStrategyRecreate {
		t.Fatalf("EffectiveDeployStrategy(empty service) = %q, want recreate", got)
	}
	service := &config.ServiceConfig{Deploy: config.DeployConfig{Strategy: config.DeployStrategyRolling}}
	if got := EffectiveDeployStrategy(service); got != config.DeployStrategyRolling {
		t.Fatalf("EffectiveDeployStrategy(rolling service) = %q, want rolling", got)
	}
}
