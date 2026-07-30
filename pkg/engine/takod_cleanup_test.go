package engine

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

func TestRemoveCleanupRequestProtectsOwnerPolicyBeforeFirstReconcile(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "demo"},
		Volumes: map[string]config.VolumeConfig{"data": {Owner: "1000:1000"}},
	}
	request := RemoveCleanupRequest(cfg, "production", nil)
	want := cfg.GetVolumeName("data", "production")
	if !slices.Contains(request.ExternalVolumes, want) {
		t.Fatalf("owner-policy volume %q is not protected before reconciliation: %#v", want, request.ExternalVolumes)
	}
}

type cleanupTimeoutClient struct {
	timeout time.Duration
	body    string
}

func (c *cleanupTimeoutClient) RequestJSON(ctx context.Context, method string, endpoint string, value any) (string, error) {
	return c.RequestJSONWithTimeout(ctx, method, endpoint, value, takodclient.JSONRequestTimeout)
}

func (c *cleanupTimeoutClient) RequestJSONWithTimeout(_ context.Context, _ string, _ string, _ any, timeout time.Duration) (string, error) {
	c.timeout = timeout
	if c.body != "" {
		return c.body, nil
	}
	return `{"project":"demo","environment":"production"}`, nil
}

func TestCleanupViaTakodAllowsMaximumGracefulDrain(t *testing.T) {
	client := &cleanupTimeoutClient{}
	_, err := CleanupViaTakodContext(context.Background(), client, &config.Config{}, takod.CleanupRequest{
		Project: "demo", Environment: "production", RemoveContainers: true,
	})
	if err != nil {
		t.Fatalf("CleanupViaTakodContext returned error: %v", err)
	}
	if want := 48*time.Hour + 10*time.Minute; client.timeout != want {
		t.Fatalf("cleanup timeout = %s, want %s", client.timeout, want)
	}
}

func TestCleanupViaTakodKeepsDefaultTimeoutWithoutContainerRemoval(t *testing.T) {
	client := &cleanupTimeoutClient{}
	_, err := CleanupViaTakodContext(context.Background(), client, &config.Config{}, takod.CleanupRequest{Project: "demo"})
	if err != nil {
		t.Fatalf("CleanupViaTakodContext returned error: %v", err)
	}
	if client.timeout != takodclient.JSONRequestTimeout {
		t.Fatalf("cleanup timeout = %s, want %s", client.timeout, takodclient.JSONRequestTimeout)
	}
}

func TestCleanupViaTakodAllowsSchedulerDrainForStateRemoval(t *testing.T) {
	client := &cleanupTimeoutClient{}
	_, err := CleanupViaTakodContext(context.Background(), client, &config.Config{}, takod.CleanupRequest{Project: "demo", RemoveTakodState: true})
	if err != nil {
		t.Fatalf("CleanupViaTakodContext returned error: %v", err)
	}
	if want := 24*time.Hour + 5*time.Minute; client.timeout != want {
		t.Fatalf("state cleanup timeout = %s, want %s", client.timeout, want)
	}
}

func TestRequireTakodCapabilityContextRejectsMissingCapability(t *testing.T) {
	client := &cleanupTimeoutClient{body: `{"capabilities":["container.runtime-controls-v1"]}`}
	err := RequireTakodCapabilityContext(context.Background(), client, &config.Config{}, takod.CapabilityContainerGracefulStopV1, "graceful container stop")
	if err == nil {
		t.Fatal("expected missing graceful-stop capability to fail")
	}
}
