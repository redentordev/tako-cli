package mesh

import (
	"strings"
	"testing"
)

func TestRenderConfigTemplateSingleNode(t *testing.T) {
	config, err := RenderConfigTemplate(Node{
		Name:    "node-a",
		Host:    "203.0.113.10",
		Address: "10.210.0.1/24",
	}, []Node{
		{Name: "node-a", Host: "203.0.113.10", Address: "10.210.0.1/24", PublicKey: "self"},
	}, WireGuardConfig{
		Enabled:      true,
		Interface:    "tako",
		ListenPort:   51820,
		NATTraversal: true,
	})
	if err != nil {
		t.Fatalf("RenderConfigTemplate returned error: %v", err)
	}

	if !strings.Contains(config, "Address = 10.210.0.1/24") {
		t.Fatalf("config did not include node address: %s", config)
	}
	if !strings.Contains(config, "PrivateKey = __TAKO_PRIVATE_KEY__") {
		t.Fatalf("config did not include private-key placeholder: %s", config)
	}
	if strings.Contains(config, "[Peer]") {
		t.Fatalf("single-node mesh should not render peer blocks: %s", config)
	}
}

func TestRenderConfigTemplatePeers(t *testing.T) {
	config, err := RenderConfigTemplate(Node{
		Name:    "node-a",
		Host:    "203.0.113.10",
		Address: "10.210.0.1/24",
	}, []Node{
		{Name: "node-b", Host: "2001:db8::2", Address: "10.210.0.2/24", PublicKey: "peer-b-key"},
		{Name: "node-a", Host: "203.0.113.10", Address: "10.210.0.1/24", PublicKey: "self-key"},
	}, WireGuardConfig{
		Enabled:      true,
		Interface:    "tako",
		ListenPort:   51820,
		NATTraversal: true,
	})
	if err != nil {
		t.Fatalf("RenderConfigTemplate returned error: %v", err)
	}

	for _, expected := range []string{
		"# node-b",
		"PublicKey = peer-b-key",
		"Endpoint = [2001:db8::2]:51820",
		"AllowedIPs = 10.210.0.2/32",
		"PersistentKeepalive = 25",
	} {
		if !strings.Contains(config, expected) {
			t.Fatalf("config missing %q: %s", expected, config)
		}
	}
	if strings.Contains(config, "self-key") {
		t.Fatalf("config should not render current node as a peer: %s", config)
	}
}

func TestRenderConfigTemplateRejectsUnsafeInterface(t *testing.T) {
	_, err := RenderConfigTemplate(Node{
		Name:    "node-a",
		Address: "10.210.0.1/24",
	}, nil, WireGuardConfig{
		Enabled:    true,
		Interface:  "tako;rm",
		ListenPort: 51820,
	})
	if err == nil {
		t.Fatal("expected unsafe interface name to be rejected")
	}
}
