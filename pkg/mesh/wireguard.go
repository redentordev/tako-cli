package mesh

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/redentordev/tako-cli/pkg/fileutil"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

const (
	KeyDir                = "/etc/tako/wireguard"
	privateKeyPath        = KeyDir + "/privatekey"
	publicKeyPath         = KeyDir + "/publickey"
	privateKeyPlaceholder = "__TAKO_PRIVATE_KEY__"
)

type WireGuardConfig struct {
	Enabled      bool   `json:"enabled"`
	Interface    string `json:"interface"`
	ListenPort   int    `json:"listenPort"`
	NATTraversal bool   `json:"natTraversal"`
}

type Node struct {
	Name      string            `json:"name"`
	Host      string            `json:"host"`
	Address   string            `json:"address"`
	PublicKey string            `json:"publicKey,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type Status struct {
	Interface  string `json:"interface"`
	Up         bool   `json:"up"`
	PublicKey  string `json:"publicKey,omitempty"`
	ListenPort string `json:"listenPort,omitempty"`
	Peers      int    `json:"peers"`
}

func EnsureWireGuardTools(client *ssh.Client, verbose bool) error {
	if _, err := client.Execute("command -v wg >/dev/null 2>&1 && command -v wg-quick >/dev/null 2>&1"); err == nil {
		return nil
	}

	if verbose {
		fmt.Println("  Installing WireGuard tools...")
	}

	if _, err := client.Execute(runRootScript(wireGuardInstallScript())); err != nil {
		return fmt.Errorf("failed to install WireGuard tools: %w", err)
	}
	return nil
}

func EnsureWireGuardToolsLocal(ctx context.Context, verbose bool) error {
	return ensureWireGuardToolsWithRunner(ctx, localWireGuardRunner{}, verbose)
}

func wireGuardInstallScript() string {
	return `set -eu
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
}

func EnsureNodeKeysLocal(ctx context.Context, verbose bool) (string, error) {
	return ensureNodeKeysWithRunner(ctx, localWireGuardRunner{}, verbose)
}

func wireGuardKeyScript() string {
	return fmt.Sprintf(`set -eu
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
}

func ApplyLocal(ctx context.Context, node Node, peers []Node, config WireGuardConfig, verbose bool) (*Status, error) {
	return applyLocalWithRunner(ctx, localWireGuardRunner{}, node, peers, config, verbose)
}

func RenderConfigTemplate(node Node, peers []Node, config WireGuardConfig) (string, error) {
	if node.Address == "" {
		return "", fmt.Errorf("node mesh address is required")
	}
	nodeAddress, err := wireGuardInterfaceAddress(node.Address)
	if err != nil {
		return "", fmt.Errorf("node mesh address is invalid: %w", err)
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
	b.WriteString(nodeAddress)
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
		peerName, err := wireGuardConfigValue("mesh peer name", peer.Name)
		if err != nil {
			return "", fmt.Errorf("mesh peer has invalid name: %w", err)
		}
		publicKey, err := wireGuardConfigValue("mesh peer public key", peer.PublicKey)
		if err != nil {
			return "", fmt.Errorf("mesh peer %s has invalid public key: %w", peer.Name, err)
		}
		if publicKey == "" {
			return "", fmt.Errorf("mesh peer %s has no public key", peer.Name)
		}
		allowedIP, err := wireGuardAllowedIP(peer.Address)
		if err != nil {
			return "", fmt.Errorf("mesh peer %s has invalid address: %w", peer.Name, err)
		}

		b.WriteString("\n[Peer]\n")
		b.WriteString("# ")
		b.WriteString(peerName)
		b.WriteString("\n")
		b.WriteString("PublicKey = ")
		b.WriteString(publicKey)
		b.WriteString("\n")
		if peer.Host != "" {
			host, err := wireGuardConfigValue("mesh peer host", peer.Host)
			if err != nil {
				return "", fmt.Errorf("mesh peer %s has invalid host: %w", peer.Name, err)
			}
			b.WriteString("Endpoint = ")
			b.WriteString(net.JoinHostPort(host, strconv.Itoa(config.ListenPort)))
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

func ReadStatusLocal(ctx context.Context, interfaceName string) (*Status, error) {
	return readStatusWithRunner(ctx, localWireGuardRunner{}, interfaceName)
}

type wireGuardRunner interface {
	Run(ctx context.Context, command string) (string, error)
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte, mode os.FileMode) error
	MkdirAll(path string, mode os.FileMode) error
}

type localWireGuardRunner struct{}

func (localWireGuardRunner) Run(ctx context.Context, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func (localWireGuardRunner) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (localWireGuardRunner) WriteFile(path string, data []byte, mode os.FileMode) error {
	return fileutil.WriteFileAtomic(path, data, mode)
}

func (localWireGuardRunner) MkdirAll(path string, mode os.FileMode) error {
	return os.MkdirAll(path, mode)
}

func ensureWireGuardToolsWithRunner(ctx context.Context, runner wireGuardRunner, verbose bool) error {
	if _, err := runner.Run(ctx, "command -v wg >/dev/null 2>&1 && command -v wg-quick >/dev/null 2>&1"); err == nil {
		return nil
	}

	if verbose {
		fmt.Println("  Installing WireGuard tools...")
	}
	if _, err := runner.Run(ctx, wireGuardInstallScript()); err != nil {
		return fmt.Errorf("failed to install WireGuard tools: %w", err)
	}
	return nil
}

func ensureNodeKeysWithRunner(ctx context.Context, runner wireGuardRunner, verbose bool) (string, error) {
	if err := ensureWireGuardToolsWithRunner(ctx, runner, verbose); err != nil {
		return "", err
	}
	output, err := runner.Run(ctx, wireGuardKeyScript())
	if err != nil {
		return "", fmt.Errorf("failed to ensure WireGuard node key: %w", err)
	}
	publicKey := strings.TrimSpace(output)
	if publicKey == "" {
		return "", fmt.Errorf("WireGuard public key is empty")
	}
	return publicKey, nil
}

func applyLocalWithRunner(ctx context.Context, runner wireGuardRunner, node Node, peers []Node, config WireGuardConfig, verbose bool) (*Status, error) {
	if !config.Enabled {
		return &Status{Interface: config.Interface, Up: false}, nil
	}
	iface, err := validateInterfaceName(config.Interface)
	if err != nil {
		return nil, err
	}
	if err := ensureWireGuardToolsWithRunner(ctx, runner, verbose); err != nil {
		return nil, err
	}
	if _, err := ensureNodeKeysWithRunner(ctx, runner, verbose); err != nil {
		return nil, err
	}

	template, err := RenderConfigTemplate(node, peers, config)
	if err != nil {
		return nil, err
	}
	privateKey, err := runner.ReadFile(privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read WireGuard private key: %w", err)
	}
	privateKeyValue := strings.TrimSpace(string(privateKey))
	if privateKeyValue == "" {
		return nil, fmt.Errorf("WireGuard private key is empty")
	}
	configContent := strings.ReplaceAll(template, privateKeyPlaceholder, privateKeyValue)

	confPath := wireGuardConfigPath(iface)
	if err := runner.MkdirAll("/etc/wireguard", 0700); err != nil {
		return nil, fmt.Errorf("failed to create WireGuard config directory: %w", err)
	}
	if err := runner.WriteFile(confPath, []byte(configContent), 0600); err != nil {
		return nil, fmt.Errorf("failed to write WireGuard config: %w", err)
	}

	firewallCmd := fmt.Sprintf("command -v ufw >/dev/null 2>&1 && ufw allow %d/udp comment 'Tako mesh' >/dev/null 2>&1 || true", config.ListenPort)
	if _, err := runner.Run(ctx, firewallCmd); err != nil {
		return nil, fmt.Errorf("failed to update WireGuard firewall rule: %w", err)
	}

	quotedIface := shellQuote(iface)
	syncCmd := shellQuote(fmt.Sprintf("wg syncconf %s <(wg-quick strip %s)", iface, iface))
	applyCmd := fmt.Sprintf(
		"systemctl enable wg-quick@%[1]s >/dev/null 2>&1 || true; "+
			"if wg show %[1]s >/dev/null 2>&1; then "+
			"ip address replace %[2]s dev %[1]s; "+
			"bash -lc %[3]s; "+
			"else wg-quick up %[1]s; fi; "+
			"wg show %[1]s >/dev/null",
		quotedIface,
		shellQuote(node.Address),
		syncCmd,
	)
	if _, err := runner.Run(ctx, applyCmd); err != nil {
		return nil, fmt.Errorf("failed to apply WireGuard interface %s: %w", config.Interface, err)
	}

	return readStatusWithRunner(ctx, runner, iface)
}

func readStatusWithRunner(ctx context.Context, runner wireGuardRunner, interfaceName string) (*Status, error) {
	iface, err := validateInterfaceName(interfaceName)
	if err != nil {
		return nil, err
	}

	quotedIface := shellQuote(iface)
	if _, err := runner.Run(ctx, fmt.Sprintf("wg show %s >/dev/null 2>&1", quotedIface)); err != nil {
		return &Status{Interface: iface, Up: false}, nil
	}

	publicKey, err := runner.Run(ctx, fmt.Sprintf("wg show %s public-key", quotedIface))
	if err != nil {
		return nil, fmt.Errorf("failed to read WireGuard public key: %w", err)
	}
	listenPort, err := runner.Run(ctx, fmt.Sprintf("wg show %s listen-port", quotedIface))
	if err != nil {
		return nil, fmt.Errorf("failed to read WireGuard listen port: %w", err)
	}
	peerCountOutput, err := runner.Run(ctx, fmt.Sprintf("wg show %s peers | wc -l", quotedIface))
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
	address, err := wireGuardConfigValue("mesh peer address", address)
	if err != nil {
		return "", err
	}
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

func wireGuardInterfaceAddress(address string) (string, error) {
	address, err := wireGuardConfigValue("node mesh address", address)
	if err != nil {
		return "", err
	}
	if _, _, err := net.ParseCIDR(address); err != nil {
		return "", err
	}
	return address, nil
}

func wireGuardConfigValue(name string, value string) (string, error) {
	value = strings.TrimSpace(value)
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%s must not contain control characters", name)
		}
	}
	return value, nil
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
	return fmt.Sprintf(`sudo sh -c 'set -eu
tmp=$(mktemp /tmp/tako-root-script.XXXXXX)
trap '\''rm -f "$tmp"'\'' EXIT
base64 -d > "$tmp"
chmod 700 "$tmp"
sh "$tmp"' <<'TAKO_ROOT_SCRIPT'
%s
TAKO_ROOT_SCRIPT`, encoded)
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
