package cmd

import (
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestMeshNodeIPsUsesEnvironmentOrder(t *testing.T) {
	cfg := &config.Config{
		Mesh: &config.MeshConfig{NetworkCIDR: "10.210.0.0/16"},
	}
	ips, err := meshNodeIPs(cfg, []string{"node-a", "node-b"})
	if err != nil {
		t.Fatalf("meshNodeIPs returned error: %v", err)
	}
	if ips["node-a"] != "10.210.0.1" || ips["node-b"] != "10.210.0.2" {
		t.Fatalf("unexpected mesh IPs: %#v", ips)
	}
}

func TestMeshRTTErrReportsPacketLoss(t *testing.T) {
	err := meshRTTErr([]meshRTTRow{{
		Source:      "node-a",
		Target:      "node-b",
		LossPercent: 100,
	}})
	if err == nil || !strings.Contains(err.Error(), "node-a->node-b") {
		t.Fatalf("expected packet loss error, got %v", err)
	}
}
