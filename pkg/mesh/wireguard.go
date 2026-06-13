package mesh

import (
	"encoding/base64"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/redentordev/tako-cli/pkg/ssh"
)

const (
	KeyDir                = "/etc/tako/wireguard"
	privateKeyPath        = KeyDir + "/privatekey"
	publicKeyPath         = KeyDir + "/publickey"
	privateKeyPlaceholder = "__TAKO_PRIVATE_KEY__"
)

type WireGuardConfig struct {
	Enabled      bool
	Interface    string
	ListenPort   int
	NATTraversal bool
}

type Node struct {
	Name      string
	Host      string
	Address   string
	PublicKey string
	Labels    map[string]string
}

type Manager struct {
	client  *ssh.Client
	config  WireGuardConfig
	verbose bool
}

type Status struct {
	Interface  string
	Up         bool
	PublicKey  string
	ListenPort string
	Peers      int
}

func NewManager(client *ssh.Client, config WireGuardConfig, verbose bool) *Manager {
	return &Manager{client: client, config: config, verbose: verbose}
}

func EnsureWireGuardTools(client *ssh.Client, verbose bool) error {
	if _, err := client.Execute("command -v wg >/dev/null 2>&1 && command -v wg-quick >/dev/null 2>&1"); err == nil {
		return nil
	}

	if verbose {
		fmt.Println("  Installing WireGuard tools...")
	}

	script := `set -eu
if command -v apt-get >/dev/null 2>&1; then
  apt-get update
  DEBIAN_FRONTEND=noninteractive apt-get install -y wireguard wireguard-tools iproute2
elif command -v dnf >/dev/null 2>&1; then
  dnf install -y wireguard-tools iproute
elif command -v yum >/dev/null 2>&1; then
  yum install -y wireguard-tools iproute
elif command -v zypper >/dev/null 2>&1; then
  zypper --non-interactive install wireguard-tools iproute2
elif command -v apk >/dev/null 2>&1; then
  apk add --no-cache wireguard-tools iproute2
else
  echo "no supported package manager found for WireGuard installation" >&2
  exit 1
fi
command -v wg >/dev/null 2>&1
command -v wg-quick >/dev/null 2>&1
`
	if _, err := client.Execute(runRootScript(script)); err != nil {
		return fmt.Errorf("failed to install WireGuard tools: %w", err)
	}
	return nil
}

func EnsureNodeKeys(client *ssh.Client, verbose bool) (string, error) {
	if err := EnsureWireGuardTools(client, verbose); err != nil {
		return "", err
	}

	script := fmt.Sprintf(`set -eu
install -d -m 700 %s
if [ ! -s %s ]; then
  umask 077
  wg genkey > %s
fi
chmod 600 %s
wg pubkey < %s > %s
chmod 644 %s
cat %s
`,
		shellQuote(KeyDir),
		shellQuote(privateKeyPath),
		shellQuote(privateKeyPath),
		shellQuote(privateKeyPath),
		shellQuote(privateKeyPath),
		shellQuote(publicKeyPath),
		shellQuote(publicKeyPath),
		shellQuote(publicKeyPath),
	)
	output, err := client.Execute(runRootScript(script))
	if err != nil {
		return "", fmt.Errorf("failed to ensure WireGuard node key: %w", err)
	}
	publicKey := strings.TrimSpace(output)
	if publicKey == "" {
		return "", fmt.Errorf("WireGuard public key is empty")
	}
	return publicKey, nil
}

func (m *Manager) Apply(node Node, peers []Node) error {
	if !m.config.Enabled {
		return nil
	}
	iface, err := validateInterfaceName(m.config.Interface)
	if err != nil {
		return err
	}
	if err := EnsureWireGuardTools(m.client, m.verbose); err != nil {
		return err
	}
	if _, err := EnsureNodeKeys(m.client, m.verbose); err != nil {
		return err
	}

	template, err := RenderConfigTemplate(node, peers, m.config)
	if err != nil {
		return err
	}
	tmpPath := fmt.Sprintf("/tmp/tako-wireguard-%s.conf", iface)
	confPath := wireGuardConfigPath(iface)
	if err := m.client.UploadReader(strings.NewReader(template), tmpPath, 0600); err != nil {
		return fmt.Errorf("failed to upload WireGuard config template: %w", err)
	}

	script := fmt.Sprintf(`set -eu
install -d -m 700 %s
private_key=$(cat %s)
sed "s#__TAKO_PRIVATE_KEY__#$private_key#g" %s > %s
chmod 600 %s
rm -f %s
`,
		shellQuote("/etc/wireguard"),
		shellQuote(privateKeyPath),
		shellQuote(tmpPath),
		shellQuote(confPath),
		shellQuote(confPath),
		shellQuote(tmpPath),
	)
	if _, err := m.client.Execute(runRootScript(script)); err != nil {
		return fmt.Errorf("failed to install WireGuard config: %w", err)
	}

	if _, err := m.client.Execute(fmt.Sprintf("command -v ufw >/dev/null 2>&1 && sudo ufw allow %d/udp comment 'Tako mesh' >/dev/null 2>&1 || true", m.config.ListenPort)); err != nil {
		return fmt.Errorf("failed to update WireGuard firewall rule: %w", err)
	}

	applyCmd := fmt.Sprintf(
		"sudo systemctl enable wg-quick@%[1]s >/dev/null 2>&1 || true; "+
			"if sudo wg show %[1]s >/dev/null 2>&1; then "+
			"sudo ip address replace %[2]s dev %[1]s; "+
			"sudo bash -lc 'wg syncconf %[1]s <(wg-quick strip %[1]s)'; "+
			"else sudo wg-quick up %[1]s; fi; "+
			"sudo wg show %[1]s >/dev/null",
		iface,
		shellQuote(node.Address),
	)
	if _, err := m.client.Execute(applyCmd); err != nil {
		return fmt.Errorf("failed to apply WireGuard interface %s: %w", m.config.Interface, err)
	}

	return nil
}

func RenderConfigTemplate(node Node, peers []Node, config WireGuardConfig) (string, error) {
	if node.Address == "" {
		return "", fmt.Errorf("node mesh address is required")
	}
	if _, err := validateInterfaceName(config.Interface); err != nil {
		return "", err
	}
	if config.ListenPort <= 0 || config.ListenPort > 65535 {
		return "", fmt.Errorf("mesh listen port must be between 1 and 65535")
	}

	var b strings.Builder
	b.WriteString("[Interface]\n")
	b.WriteString("Address = ")
	b.WriteString(node.Address)
	b.WriteString("\n")
	b.WriteString("ListenPort = ")
	b.WriteString(strconv.Itoa(config.ListenPort))
	b.WriteString("\n")
	b.WriteString("PrivateKey = ")
	b.WriteString(privateKeyPlaceholder)
	b.WriteString("\n")
	b.WriteString("SaveConfig = false\n")

	filtered := peerNodes(node.Name, peers)
	for _, peer := range filtered {
		if peer.PublicKey == "" {
			return "", fmt.Errorf("mesh peer %s has no public key", peer.Name)
		}
		allowedIP, err := wireGuardAllowedIP(peer.Address)
		if err != nil {
			return "", fmt.Errorf("mesh peer %s has invalid address: %w", peer.Name, err)
		}

		b.WriteString("\n[Peer]\n")
		b.WriteString("# ")
		b.WriteString(peer.Name)
		b.WriteString("\n")
		b.WriteString("PublicKey = ")
		b.WriteString(peer.PublicKey)
		b.WriteString("\n")
		if peer.Host != "" {
			b.WriteString("Endpoint = ")
			b.WriteString(net.JoinHostPort(peer.Host, strconv.Itoa(config.ListenPort)))
			b.WriteString("\n")
		}
		b.WriteString("AllowedIPs = ")
		b.WriteString(allowedIP)
		b.WriteString("\n")
		if config.NATTraversal {
			b.WriteString("PersistentKeepalive = 25\n")
		}
	}

	return b.String(), nil
}

func ReadStatus(client *ssh.Client, interfaceName string) (*Status, error) {
	iface, err := validateInterfaceName(interfaceName)
	if err != nil {
		return nil, err
	}

	if _, err := client.Execute(fmt.Sprintf("sudo wg show %s >/dev/null 2>&1", iface)); err != nil {
		return &Status{Interface: iface, Up: false}, nil
	}

	publicKey, err := client.Execute(fmt.Sprintf("sudo wg show %s public-key", iface))
	if err != nil {
		return nil, fmt.Errorf("failed to read WireGuard public key: %w", err)
	}
	listenPort, err := client.Execute(fmt.Sprintf("sudo wg show %s listen-port", iface))
	if err != nil {
		return nil, fmt.Errorf("failed to read WireGuard listen port: %w", err)
	}
	peerCountOutput, err := client.Execute(fmt.Sprintf("sudo wg show %s peers | wc -l", iface))
	if err != nil {
		return nil, fmt.Errorf("failed to read WireGuard peers: %w", err)
	}
	peerCount, _ := strconv.Atoi(strings.TrimSpace(peerCountOutput))

	return &Status{
		Interface:  iface,
		Up:         true,
		PublicKey:  strings.TrimSpace(publicKey),
		ListenPort: strings.TrimSpace(listenPort),
		Peers:      peerCount,
	}, nil
}

func peerNodes(currentNode string, peers []Node) []Node {
	filtered := make([]Node, 0, len(peers))
	for _, peer := range peers {
		if peer.Name == currentNode {
			continue
		}
		filtered = append(filtered, peer)
	}
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].Name < filtered[j].Name
	})
	return filtered
}

func wireGuardAllowedIP(address string) (string, error) {
	ip, _, err := net.ParseCIDR(address)
	if err != nil {
		parsed := net.ParseIP(address)
		if parsed == nil {
			return "", err
		}
		ip = parsed
	}
	if ip.To4() != nil {
		return ip.String() + "/32", nil
	}
	return ip.String() + "/128", nil
}

func wireGuardConfigPath(interfaceName string) string {
	return fmt.Sprintf("/etc/wireguard/%s.conf", interfaceName)
}

func validateInterfaceName(value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("mesh interface is required")
	}
	escaped := shellEscapeIdentifier(value)
	if escaped != value {
		return "", fmt.Errorf("mesh interface contains unsupported characters")
	}
	return escaped, nil
}

func runRootScript(script string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(script))
	return fmt.Sprintf("echo '%s' | base64 -d | sudo sh", encoded)
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func shellEscapeIdentifier(value string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			return r
		}
		return -1
	}, value)
}
