package mesh

import (
	"context"
	"fmt"
	"os"
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

func TestRenderConfigTemplateRejectsInvalidNodeAddress(t *testing.T) {
	_, err := RenderConfigTemplate(Node{
		Name:    "node-a",
		Address: "10.210.0.1\nPostUp = rm -rf /",
	}, nil, WireGuardConfig{
		Enabled:    true,
		Interface:  "tako",
		ListenPort: 51820,
	})
	if err == nil || !strings.Contains(err.Error(), "node mesh address") {
		t.Fatalf("expected invalid node address error, got %v", err)
	}
}

func TestRenderConfigTemplateRejectsInjectedPeerFields(t *testing.T) {
	tests := []struct {
		name string
		peer Node
	}{
		{
			name: "peer name",
			peer: Node{Name: "node-b\n[Peer]", Host: "203.0.113.11", Address: "10.210.0.2/24", PublicKey: "peer-b-key"},
		},
		{
			name: "peer public key",
			peer: Node{Name: "node-b", Host: "203.0.113.11", Address: "10.210.0.2/24", PublicKey: "peer-b-key\nPostUp = bad"},
		},
		{
			name: "peer host",
			peer: Node{Name: "node-b", Host: "203.0.113.11\nEndpoint = attacker:51820", Address: "10.210.0.2/24", PublicKey: "peer-b-key"},
		},
		{
			name: "peer address",
			peer: Node{Name: "node-b", Host: "203.0.113.11", Address: "10.210.0.2/24\nAllowedIPs = 0.0.0.0/0", PublicKey: "peer-b-key"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := RenderConfigTemplate(Node{
				Name:    "node-a",
				Host:    "203.0.113.10",
				Address: "10.210.0.1/24",
			}, []Node{tt.peer}, WireGuardConfig{
				Enabled:    true,
				Interface:  "tako",
				ListenPort: 51820,
			})
			if err == nil {
				t.Fatal("expected injected peer field to be rejected")
			}
		})
	}
}

func TestRunRootScriptUsesRootOwnedTempFile(t *testing.T) {
	cmd := runRootScript("echo ready\n")
	for _, expected := range []string{
		"sudo sh -c",
		"mktemp /tmp/tako-root-script.",
		"base64 -d > \"$tmp\"",
		"chmod 700 \"$tmp\"",
		"<<'TAKO_ROOT_SCRIPT'",
	} {
		if !strings.Contains(cmd, expected) {
			t.Fatalf("root script command missing %q: %s", expected, cmd)
		}
	}
	if strings.Contains(cmd, "| sudo sh") || strings.Contains(cmd, "echo '") {
		t.Fatalf("root script command should not pipe decoded script into sudo sh: %s", cmd)
	}
}

func TestApplyLocalWithRunnerWritesConfigWithoutSudo(t *testing.T) {
	runner := &fakeWireGuardRunner{
		files: map[string][]byte{
			privateKeyPath: []byte("self-private\n"),
		},
		writes: make(map[string][]byte),
		modes:  make(map[string]os.FileMode),
	}

	status, err := applyLocalWithRunner(context.Background(), runner, Node{
		Name:    "node-a",
		Host:    "203.0.113.10",
		Address: " 10.210.0.1/24 ",
	}, []Node{
		{Name: "node-b", Host: "203.0.113.11", Address: "10.210.0.2/24", PublicKey: "peer-b-key"},
	}, WireGuardConfig{
		Enabled:      true,
		Interface:    "tako",
		ListenPort:   51820,
		NATTraversal: true,
	}, false)
	if err != nil {
		t.Fatalf("applyLocalWithRunner returned error: %v", err)
	}
	if status == nil || !status.Up || status.PublicKey != "self-public" || status.Peers != 1 {
		t.Fatalf("unexpected status: %#v", status)
	}

	configPath := wireGuardConfigPath("tako")
	written := string(runner.writes[configPath])
	if written == "" {
		t.Fatalf("expected WireGuard config to be written to %s", configPath)
	}
	for _, expected := range []string{
		"PrivateKey = self-private",
		"# node-b",
		"PublicKey = peer-b-key",
		"AllowedIPs = 10.210.0.2/32",
	} {
		if !strings.Contains(written, expected) {
			t.Fatalf("written config missing %q: %s", expected, written)
		}
	}
	if strings.Contains(written, privateKeyPlaceholder) {
		t.Fatalf("private key placeholder was not replaced: %s", written)
	}
	if runner.modes[configPath] != 0600 {
		t.Fatalf("expected config mode 0600, got %v", runner.modes[configPath])
	}
	for _, command := range runner.commands {
		if strings.Contains(command, "sudo ") {
			t.Fatalf("local takod mesh command should not require sudo: %s", command)
		}
	}
	applyCommand := findCommandWithPrefix(runner.commands, "systemctl enable wg-quick@")
	if applyCommand == "" {
		t.Fatalf("expected wg-quick apply command, got %v", runner.commands)
	}
	for _, expected := range []string{"wg-quick@'tako'", "wg show 'tako'", "ip -o -4 address show dev", "ip address del", "ip address replace", "10.210.0.1/24", "dev \"$iface\"", "wg-quick up 'tako'"} {
		if !strings.Contains(applyCommand, expected) {
			t.Fatalf("apply command missing %q: %s", expected, applyCommand)
		}
	}
}

type fakeWireGuardRunner struct {
	commands []string
	files    map[string][]byte
	writes   map[string][]byte
	modes    map[string]os.FileMode
	dirs     []string
}

func (f *fakeWireGuardRunner) Run(ctx context.Context, command string) (string, error) {
	f.commands = append(f.commands, command)
	switch {
	case command == "command -v wg >/dev/null 2>&1 && command -v wg-quick >/dev/null 2>&1":
		return "", nil
	case strings.Contains(command, "wg pubkey <"):
		return "self-public\n", nil
	case strings.HasPrefix(command, "systemctl enable wg-quick@"):
		return "", nil
	case command == "wg show 'tako' >/dev/null 2>&1":
		return "", nil
	case command == "wg show 'tako' public-key":
		return "self-public\n", nil
	case command == "wg show 'tako' listen-port":
		return "51820\n", nil
	case command == "wg show 'tako' peers | wc -l":
		return "1\n", nil
	default:
		return "", nil
	}
}

func (f *fakeWireGuardRunner) ReadFile(path string) ([]byte, error) {
	data, ok := f.files[path]
	if !ok {
		return nil, fmt.Errorf("missing file %s", path)
	}
	return data, nil
}

func (f *fakeWireGuardRunner) WriteFile(path string, data []byte, mode os.FileMode) error {
	f.writes[path] = append([]byte(nil), data...)
	f.modes[path] = mode
	return nil
}

func (f *fakeWireGuardRunner) MkdirAll(path string, mode os.FileMode) error {
	f.dirs = append(f.dirs, path)
	return nil
}

func findCommandWithPrefix(commands []string, prefix string) string {
	for _, command := range commands {
		if strings.HasPrefix(command, prefix) {
			return command
		}
	}
	return ""
}
