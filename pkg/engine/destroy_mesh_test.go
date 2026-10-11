package engine

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takod"
)

// peerSetDestroyClient answers GET /v1/status with the given capabilities and
// records every request.
type peerSetDestroyClient struct {
	capabilities []string
	requests     []string
}

func (c *peerSetDestroyClient) RequestJSON(ctx context.Context, method, endpoint string, value any) (string, error) {
	return c.RequestJSONWithTimeout(ctx, method, endpoint, value, time.Second)
}

func (c *peerSetDestroyClient) RequestJSONWithTimeout(_ context.Context, method, endpoint string, _ any, _ time.Duration) (string, error) {
	c.requests = append(c.requests, method+" "+endpoint)
	switch {
	case method == "GET" && endpoint == "/v1/status":
		return fmt.Sprintf(`{"runtime":"takod","capabilities":["%s"]}`, strings.Join(c.capabilities, `","`)), nil
	case method == "DELETE" && strings.HasPrefix(endpoint, "/v1/mesh/peer-sets/"):
		return `{"applied":true}`, nil
	}
	return "", fmt.Errorf("unexpected %s %s", method, endpoint)
}

// 4.4: destroy removes the environment's peer set only on capable,
// non-enrolled members.
func TestDestroyRemovesMeshPeerSetOnCapableMembers(t *testing.T) {
	cfg := &config.Config{Project: config.ProjectConfig{Name: "demo"}}
	capable := &peerSetDestroyClient{capabilities: []string{takod.CapabilityMeshPeerSetsV1}}
	if err := RemoveMeshPeerSetContext(context.Background(), capable, cfg, config.ServerConfig{Host: "203.0.113.5"}, "preview"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(capable.requests, ","); got != "GET /v1/status,DELETE /v1/mesh/peer-sets/demo/preview" {
		t.Fatalf("requests = %s", got)
	}

	older := &peerSetDestroyClient{capabilities: []string{takod.CapabilityNodeUpgradeV1}}
	if err := RemoveMeshPeerSetContext(context.Background(), older, cfg, config.ServerConfig{Host: "203.0.113.5"}, "preview"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(older.requests, ","); got != "GET /v1/status" {
		t.Fatalf("older agent requests = %s", got)
	}

	enrolled := &peerSetDestroyClient{capabilities: []string{takod.CapabilityMeshPeerSetsV1}}
	if err := RemoveMeshPeerSetContext(context.Background(), enrolled, cfg, config.ServerConfig{Host: "203.0.113.5", ClusterID: "cluster-1", NodeID: "node-1"}, "preview"); err != nil {
		t.Fatal(err)
	}
	if len(enrolled.requests) != 0 {
		t.Fatalf("enrolled node requests = %v", enrolled.requests)
	}
}

func TestDestroyAppliesMeshPeerSetRemoval(t *testing.T) {
	data, err := os.ReadFile("destroy.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "RemoveMeshPeerSetContext(ctx, client, cfg, serverCfg, envName)") {
		t.Fatal("destroy does not remove the environment's mesh peer set")
	}
}
