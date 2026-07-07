package deployer

import (
	"errors"
	"fmt"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
)

func registryDeployerFixture(sink events.Sink) *Deployer {
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "demo"},
		Registries: map[string]config.RegistryConfig{
			"ghcr.io":   {Username: "octocat", Password: "gh-token"},
			"docker.io": {Username: "acme", Password: "hub-token"},
		},
	}
	d := NewDeployer(nil, cfg, "production", false)
	if sink != nil {
		d.SetEventSink(sink)
	}
	return d
}

func TestRegistryAuthsSortedByHost(t *testing.T) {
	d := registryDeployerFixture(nil)
	auths := d.registryAuths()
	if len(auths) != 2 {
		t.Fatalf("auths = %+v, want 2", auths)
	}
	if auths[0].Registry != "docker.io" || auths[1].Registry != "ghcr.io" {
		t.Fatalf("auths not sorted: %+v", auths)
	}
	if auths[1].Username != "octocat" || auths[1].Password != "gh-token" {
		t.Fatalf("ghcr auth = %+v", auths[1])
	}
}

func TestRegistryAuthsEmptyWithoutConfig(t *testing.T) {
	d := NewDeployer(nil, &config.Config{Project: config.ProjectConfig{Name: "demo"}}, "production", false)
	if auths := d.registryAuths(); auths != nil {
		t.Fatalf("auths = %+v, want nil", auths)
	}
}

func TestWrapRegistryAuthErrorTypedAndEmitsEvent(t *testing.T) {
	sink := &capturingSink{}
	d := registryDeployerFixture(sink)

	plain := fmt.Errorf("failed to pull image: exit status 1: manifest unknown")
	if err := d.wrapRegistryAuthError("node-a", plain); err != plain {
		t.Fatalf("non-auth error was wrapped: %v", err)
	}
	if len(sink.events) != 0 {
		t.Fatalf("event emitted for non-auth error: %+v", sink.events)
	}

	authErr := fmt.Errorf("failed to pull image: %s: unauthorized", takod.RegistryAuthFailedMarker)
	wrapped := d.wrapRegistryAuthError("node-a", authErr)
	var typed *RegistryAuthError
	if !errors.As(wrapped, &typed) || typed.Node != "node-a" {
		t.Fatalf("wrapped = %v, want RegistryAuthError for node-a", wrapped)
	}
	if len(sink.events) != 1 || sink.events[0].Type != events.TypeImagePullAuthFailed {
		t.Fatalf("events = %+v, want one image.pull.auth_failed", sink.events)
	}
}
