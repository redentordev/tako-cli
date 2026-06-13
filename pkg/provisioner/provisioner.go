package provisioner

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/mesh"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/utils"
)

const takodAccessGroup = "tako"
const takodActualRefreshInterval = "30s"

// Provisioner handles server provisioning
type Provisioner struct {
	client  *ssh.Client
	verbose bool
}

// NewProvisioner creates a new provisioner
func NewProvisioner(client *ssh.Client, verbose bool) *Provisioner {
	return &Provisioner{
		client:  client,
		verbose: verbose,
	}
}

// CheckRequirements checks if the server meets basic requirements
func (p *Provisioner) CheckRequirements() error {
	osInfo, err := DetectOS(p.client)
	if err != nil {
		return fmt.Errorf("failed to check OS: %w", err)
	}
	if !osInfo.IsSupported() {
		return fmt.Errorf("unsupported OS: %s", osInfo.String())
	}

	if p.verbose {
		fmt.Printf("  OS: %s\n", osInfo.String())
	}

	return nil
}

// UpdateSystem updates system packages
func (p *Provisioner) UpdateSystem() error {
	if p.verbose {
		fmt.Printf("  Updating system packages with detected package manager...\n")
	}
	if _, err := p.client.Execute(runRootScript(basePackageInstallScript())); err != nil {
		return fmt.Errorf("failed to update system packages: %w", err)
	}
	return nil
}

// InstallDocker installs and enables the container runtime used by takod.
func (p *Provisioner) InstallDocker() error {
	// Check if Docker is already installed
	if output, err := p.client.Execute("which docker"); err == nil && output != "" {
		if p.verbose {
			fmt.Printf("  Docker already installed, ensuring it's enabled on boot...\n")
		}
		// Make sure Docker is enabled to start on boot
		p.client.Execute("sudo systemctl enable docker")
		p.client.Execute("sudo systemctl enable containerd")
		p.client.Execute("sudo systemctl start docker")

		// Verify Docker is running
		if _, err := p.client.Execute("sudo systemctl is-active docker"); err != nil {
			if p.verbose {
				fmt.Printf("  Starting Docker service...\n")
			}
			p.client.Execute("sudo systemctl start docker")
		}
		return nil
	}

	if p.verbose {
		fmt.Printf("  Installing Docker from OS packages...\n")
	}
	if _, err := p.client.Execute(runRootScript(dockerInstallScript())); err != nil {
		return fmt.Errorf("failed to install Docker packages: %w", err)
	}

	_, _ = p.client.Execute("sudo usermod -aG docker $(id -un) 2>/dev/null || true")

	// Enable Docker to start on boot
	if p.verbose {
		fmt.Printf("  Enabling Docker to start on boot...\n")
	}
	enableCommands := []string{
		"sudo systemctl enable docker",
		"sudo systemctl enable containerd",
		"sudo systemctl start docker",
		"sudo systemctl start containerd",
	}

	for _, cmd := range enableCommands {
		if p.verbose {
			fmt.Printf("  Running: %s\n", cmd)
		}
		// Don't fail if containerd doesn't exist (older Docker versions)
		p.client.Execute(cmd)
	}

	// Verify Docker installation and is running
	if _, err := p.client.Execute("docker --version"); err != nil {
		return fmt.Errorf("docker installation verification failed: %w", err)
	}

	if _, err := p.client.Execute("sudo systemctl is-active docker"); err != nil {
		return fmt.Errorf("docker daemon is not running: %w", err)
	}

	return nil
}

func basePackageInstallScript() string {
	return `set -eu
if command -v apt-get >/dev/null 2>&1; then
  apt-get update
  DEBIAN_FRONTEND=noninteractive apt-get upgrade -y
  DEBIAN_FRONTEND=noninteractive apt-get install -y curl wget git build-essential ca-certificates
elif command -v dnf >/dev/null 2>&1; then
  dnf upgrade -y
  dnf install -y curl wget git gcc gcc-c++ make ca-certificates
elif command -v yum >/dev/null 2>&1; then
  yum update -y
  yum install -y curl wget git gcc gcc-c++ make ca-certificates
elif command -v zypper >/dev/null 2>&1; then
  zypper --non-interactive refresh
  zypper --non-interactive update -y
  zypper --non-interactive install -y curl wget git gcc gcc-c++ make ca-certificates
elif command -v apk >/dev/null 2>&1; then
  apk update
  apk upgrade
  apk add --no-cache curl wget git build-base ca-certificates
else
  echo "no supported package manager found" >&2
  exit 1
fi
`
}

func dockerInstallScript() string {
	return `set -eu
if command -v docker >/dev/null 2>&1; then
  exit 0
fi
if command -v apt-get >/dev/null 2>&1; then
  apt-get update
  DEBIAN_FRONTEND=noninteractive apt-get install -y docker.io containerd
elif command -v dnf >/dev/null 2>&1; then
  dnf install -y moby-engine containerd || dnf install -y docker containerd
elif command -v yum >/dev/null 2>&1; then
  yum install -y docker containerd || yum install -y moby-engine containerd
elif command -v zypper >/dev/null 2>&1; then
  zypper --non-interactive install -y docker containerd
elif command -v apk >/dev/null 2>&1; then
  apk add --no-cache docker docker-cli containerd
else
  echo "no supported package manager found for Docker installation" >&2
  exit 1
fi
command -v docker >/dev/null 2>&1
`
}

func (p *Provisioner) InstallWireGuard() error {
	return mesh.EnsureWireGuardTools(p.client, p.verbose)
}

func (p *Provisioner) InstallTakodBinary(version string) error {
	version, err := releaseVersionArg(version)
	if err != nil {
		existing, _ := p.client.Execute("command -v tako 2>/dev/null || true")
		if strings.TrimSpace(existing) != "" {
			if p.verbose {
				fmt.Printf("  Using existing server-side tako binary: %s\n", strings.TrimSpace(existing))
			}
			return nil
		}
		if p.verbose {
			fmt.Printf("  Skipping takod binary install: %v\n", err)
		}
		return nil
	}

	arch, err := p.detectLinuxArch()
	if err != nil {
		return err
	}

	binaryName := fmt.Sprintf("tako-linux-%s", arch)
	downloadURL := fmt.Sprintf("https://github.com/redentordev/tako-cli/releases/download/%s/%s", version, binaryName)
	script := fmt.Sprintf(`set -eu
tmp="$(mktemp)"
cleanup() { rm -f "$tmp"; }
trap cleanup EXIT
if command -v curl >/dev/null 2>&1; then
  curl -fL --retry 3 --connect-timeout 15 -o "$tmp" %s
elif command -v wget >/dev/null 2>&1; then
  wget -O "$tmp" %s
else
  echo "curl or wget is required to install takod binary" >&2
  exit 1
fi
install -m 0755 "$tmp" /usr/local/bin/tako
/usr/local/bin/tako --version >/dev/null
`,
		shellQuote(downloadURL),
		shellQuote(downloadURL),
	)
	if _, err := p.client.Execute(runRootScript(script)); err != nil {
		return fmt.Errorf("failed to install takod binary from release %s: %w", version, err)
	}
	return nil
}

func (p *Provisioner) InstallTakodService(socket string, dataDir string, nodeName string) error {
	binaryPath, _ := p.client.Execute("command -v tako 2>/dev/null || test -x /usr/local/bin/tako && echo /usr/local/bin/tako || true")
	binaryPath = strings.TrimSpace(binaryPath)
	if binaryPath == "" {
		if p.verbose {
			fmt.Printf("  Skipping takod service: no server-side tako binary found\n")
		}
		return nil
	}
	var err error
	if binaryPath, err = systemdPathArg(binaryPath, ""); err != nil {
		return fmt.Errorf("invalid server-side tako binary path: %w", err)
	}
	if socket, err = systemdPathArg(socket, "/run/tako/takod.sock"); err != nil {
		return fmt.Errorf("invalid takod socket path: %w", err)
	}
	if dataDir, err = systemdPathArg(dataDir, "/var/lib/tako"); err != nil {
		return fmt.Errorf("invalid takod data directory: %w", err)
	}
	if nodeName, err = systemdIdentifierArg(nodeName); err != nil {
		return fmt.Errorf("invalid takod node name: %w", err)
	}
	if err := p.ensureTakodAccessGroup(); err != nil {
		return err
	}

	unit := buildTakodSystemdUnit(binaryPath, socket, dataDir, nodeName, takodActualRefreshInterval)

	uploadServiceCmd := fmt.Sprintf("sudo tee /etc/systemd/system/takod.service > /dev/null << 'EOFSERVICE'\n%s\nEOFSERVICE", unit)
	if _, err := p.client.Execute(uploadServiceCmd); err != nil {
		return fmt.Errorf("failed to write takod service: %w", err)
	}

	commands := []string{
		"sudo systemctl daemon-reload",
		"sudo systemctl enable takod",
		"sudo systemctl restart takod",
	}
	for _, cmd := range commands {
		if _, err := p.client.Execute(cmd); err != nil {
			return fmt.Errorf("failed to run %s: %w", cmd, err)
		}
	}
	return nil
}

func buildTakodSystemdUnit(binaryPath string, socket string, dataDir string, nodeName string, actualRefreshInterval string) string {
	return fmt.Sprintf(`[Unit]
Description=Tako node agent
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
Type=simple
User=root
Group=%s
RuntimeDirectory=tako
RuntimeDirectoryMode=0770
UMask=0007
ExecStart=%s takod run --socket %s --data-dir %s --node %s --actual-refresh-interval %s
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`, takodAccessGroup, binaryPath, socket, dataDir, nodeName, actualRefreshInterval)
}

func (p *Provisioner) ensureTakodAccessGroup() error {
	script := fmt.Sprintf(`set -eu
if ! getent group %[1]s >/dev/null 2>&1; then
  if command -v groupadd >/dev/null 2>&1; then
    groupadd --system %[1]s 2>/dev/null || groupadd -r %[1]s 2>/dev/null || groupadd %[1]s
  elif command -v addgroup >/dev/null 2>&1; then
    addgroup -S %[1]s 2>/dev/null || addgroup --system %[1]s 2>/dev/null || addgroup %[1]s
  else
    echo "groupadd or addgroup is required to create the takod access group" >&2
    exit 1
  fi
fi
`, takodAccessGroup)
	if _, err := p.client.Execute(runRootScript(script)); err != nil {
		return fmt.Errorf("failed to ensure takod access group: %w", err)
	}
	return nil
}

func (p *Provisioner) detectLinuxArch() (string, error) {
	output, err := p.client.Execute("uname -m")
	if err != nil {
		return "", fmt.Errorf("failed to detect server architecture: %w", err)
	}
	return normalizeLinuxArch(strings.TrimSpace(output))
}

func normalizeLinuxArch(machine string) (string, error) {
	switch strings.ToLower(machine) {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported Linux architecture %q", machine)
	}
}

func releaseVersionArg(version string) (string, error) {
	trimmed := strings.TrimSpace(version)
	if trimmed != version {
		return "", fmt.Errorf("release version must not contain leading or trailing whitespace")
	}
	if version == "" || version == "dev" || version == "unknown" {
		return "", fmt.Errorf("release version is not available for this build")
	}
	for _, r := range version {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return "", fmt.Errorf("release version contains unsupported characters")
	}
	return version, nil
}

func systemdPathArg(value string, fallback string) (string, error) {
	if value == "" {
		value = fallback
	}
	if value == "" {
		return "", fmt.Errorf("path is required")
	}
	if !strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("path must be absolute")
	}
	if strings.ContainsAny(value, " \t\r\n") {
		return "", fmt.Errorf("path must not contain whitespace")
	}
	return value, nil
}

func systemdIdentifierArg(value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("identifier is required")
	}
	if len(value) > 63 {
		return "", fmt.Errorf("identifier is too long")
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return "", fmt.Errorf("identifier contains unsupported characters")
	}
	return value, nil
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

// Note: tako-proxy is handled per-deployment by the deployer.
// No system-wide reverse proxy installation is needed during server setup

// ConfigureFirewall configures UFW firewall
func (p *Provisioner) ConfigureFirewall() error {
	// Check if UFW is already active
	output, _ := p.client.Execute("sudo ufw status | grep -i 'Status: active'")
	isActive := strings.TrimSpace(output) != ""

	if isActive && p.verbose {
		fmt.Printf("  UFW already active, updating rules...\n")
	}

	// Install UFW if not present
	if p.verbose {
		fmt.Printf("  Running: sudo apt-get install -y ufw\n")
	}
	if _, err := p.client.Execute("sudo apt-get install -y ufw"); err != nil {
		return fmt.Errorf("failed to install ufw: %w", err)
	}

	// Disable UFW temporarily to safely update rules
	if isActive {
		if p.verbose {
			fmt.Printf("  Temporarily disabling UFW to update rules\n")
		}
		p.client.Execute("sudo ufw --force disable")
	}

	// Reset UFW to clean state (only if not active before)
	if !isActive {
		if p.verbose {
			fmt.Printf("  Running: sudo ufw --force reset\n")
		}
		p.client.Execute("sudo ufw --force reset")
	}

	// Set default policies
	commands := []string{
		"sudo ufw --force default deny incoming",
		"sudo ufw --force default allow outgoing",
	}

	for _, cmd := range commands {
		if p.verbose {
			fmt.Printf("  Running: %s\n", cmd)
		}
		if _, err := p.client.Execute(cmd); err != nil {
			return fmt.Errorf("command failed '%s': %w", cmd, err)
		}
	}

	// Allow essential services with rate limiting for SSH (use || true to ignore "Skipping adding existing rule" errors)
	allowCommands := []string{
		// SSH with rate limiting (max 10 connections per 30 seconds per IP)
		"sudo ufw limit 22/tcp comment 'SSH with rate limiting' || true",

		// HTTP/HTTPS.
		"sudo ufw allow 80/tcp comment 'HTTP' || true",
		"sudo ufw allow 443/tcp comment 'HTTPS' || true",

		"sudo ufw allow 51820/udp comment 'Tako mesh' || true",
	}

	for _, cmd := range allowCommands {
		if p.verbose {
			fmt.Printf("  Running: %s\n", cmd)
		}
		// Execute but don't fail on "rule already exists" errors
		p.client.Execute(cmd)
	}

	// Enable UFW
	if p.verbose {
		fmt.Printf("  Running: sudo ufw --force enable\n")
	}
	if _, err := p.client.Execute("sudo ufw --force enable"); err != nil {
		return fmt.Errorf("failed to enable ufw: %w", err)
	}

	// Show status
	if p.verbose {
		output, _ := p.client.Execute("sudo ufw status verbose")
		fmt.Printf("\n  UFW Status:\n%s\n", output)
	}

	return nil
}

// HardenSecurity applies comprehensive security hardening
func (p *Provisioner) HardenSecurity() error {
	if p.verbose {
		fmt.Printf("  Installing and configuring security tools...\n")
	}

	// Install security packages
	installCommands := []string{
		"sudo apt-get install -y fail2ban",
		"sudo apt-get install -y unattended-upgrades",
		"sudo apt-get install -y ufw",
	}

	for _, cmd := range installCommands {
		if p.verbose {
			fmt.Printf("  Running: %s\n", cmd)
		}
		p.client.Execute(cmd)
	}

	// Configure fail2ban with custom jail for SSH
	fail2banConfig := `[sshd]
enabled = true
port = ssh
filter = sshd
logpath = /var/log/auth.log
maxretry = 5
findtime = 600
bantime = 3600
`

	if p.verbose {
		fmt.Printf("  Configuring fail2ban jail for SSH...\n")
	}

	// Write fail2ban jail config
	fail2banCmd := fmt.Sprintf("sudo tee /etc/fail2ban/jail.d/sshd.local > /dev/null << 'EOF'\n%s\nEOF", fail2banConfig)
	p.client.Execute(fail2banCmd)

	// Enable and start fail2ban
	p.client.Execute("sudo systemctl enable fail2ban")
	p.client.Execute("sudo systemctl restart fail2ban")

	// Configure SSH hardening
	if p.verbose {
		fmt.Printf("  Hardening SSH configuration...\n")
	}

	sshHardeningCommands := []string{
		// Increase connection limits to prevent lockouts
		"sudo sed -i 's/^#MaxStartups.*/MaxStartups 100:30:100/' /etc/ssh/sshd_config",
		"sudo grep -q '^MaxStartups' /etc/ssh/sshd_config || echo 'MaxStartups 100:30:100' | sudo tee -a /etc/ssh/sshd_config",

		"sudo sed -i 's/^#MaxSessions.*/MaxSessions 100/' /etc/ssh/sshd_config",
		"sudo grep -q '^MaxSessions' /etc/ssh/sshd_config || echo 'MaxSessions 100' | sudo tee -a /etc/ssh/sshd_config",

		// Keep connections alive
		"sudo sed -i 's/^#ClientAliveInterval.*/ClientAliveInterval 60/' /etc/ssh/sshd_config",
		"sudo grep -q '^ClientAliveInterval' /etc/ssh/sshd_config || echo 'ClientAliveInterval 60' | sudo tee -a /etc/ssh/sshd_config",

		"sudo sed -i 's/^#ClientAliveCountMax.*/ClientAliveCountMax 10/' /etc/ssh/sshd_config",
		"sudo grep -q '^ClientAliveCountMax' /etc/ssh/sshd_config || echo 'ClientAliveCountMax 10' | sudo tee -a /etc/ssh/sshd_config",

		// Increase login grace time
		"sudo sed -i 's/^#LoginGraceTime.*/LoginGraceTime 120/' /etc/ssh/sshd_config",
		"sudo grep -q '^LoginGraceTime' /etc/ssh/sshd_config || echo 'LoginGraceTime 120' | sudo tee -a /etc/ssh/sshd_config",

		// Disable password authentication (key-based only)
		"sudo sed -i 's/^PasswordAuthentication yes/PasswordAuthentication no/' /etc/ssh/sshd_config",
		"sudo sed -i 's/^#PasswordAuthentication yes/PasswordAuthentication no/' /etc/ssh/sshd_config",

		// Keep PermitRootLogin yes for Tako deployments (we use keys, not passwords)
		// This is needed for Tako to deploy applications
		"sudo sed -i 's/^PermitRootLogin no/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config",
		"sudo sed -i 's/^#PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config",
		"sudo grep -q '^PermitRootLogin' /etc/ssh/sshd_config || echo 'PermitRootLogin prohibit-password' | sudo tee -a /etc/ssh/sshd_config",
	}

	for _, cmd := range sshHardeningCommands {
		p.client.Execute(cmd)
	}

	// Configure automatic security updates
	if p.verbose {
		fmt.Printf("  Enabling automatic security updates...\n")
	}
	p.client.Execute("sudo dpkg-reconfigure -plow unattended-upgrades")

	// Enable and restart SSH service
	if p.verbose {
		fmt.Printf("  Enabling and restarting SSH service...\n")
	}

	// CRITICAL: Enable SSH to start on boot
	if _, err := p.client.Execute("sudo systemctl enable ssh"); err != nil {
		if p.verbose {
			fmt.Printf("  Warning: Failed to enable SSH service: %v\n", err)
		}
	}

	// Restart SSH service to apply changes (try both ssh and sshd)
	p.client.Execute("sudo systemctl restart ssh")
	p.client.Execute("sudo systemctl restart sshd")

	// Verify SSH is running
	output, err := p.client.Execute("sudo systemctl is-active ssh")
	if err != nil || strings.TrimSpace(output) != "active" {
		if p.verbose {
			fmt.Printf("  Warning: SSH service may not be running properly\n")
		}
	}

	if p.verbose {
		fmt.Printf("  ✓ Security hardening completed\n")
		fmt.Printf("  - fail2ban: enabled (5 retries in 10min = 1hr ban)\n")
		fmt.Printf("  - SSH: hardened (key-based auth only)\n")
		fmt.Printf("  - Auto-updates: enabled\n")
		fmt.Printf("  - SSH service: enabled on boot\n")
	}

	return nil
}

// SetupDeployUser ensures deploy user exists and has proper permissions
func (p *Provisioner) SetupDeployUser(username string) error {
	// Defense-in-depth: validate username before using in shell commands
	if !utils.IsValidUnixUsername(username) {
		return fmt.Errorf("invalid username %q: must be a valid POSIX username", username)
	}
	if err := p.ensureTakodAccessGroup(); err != nil {
		return err
	}

	// Check if user exists
	output, err := p.client.Execute(fmt.Sprintf("id -u %s", username))
	if err != nil || output == "" {
		// User doesn't exist, create it
		commands := []string{
			fmt.Sprintf("sudo useradd -m -s /bin/bash %s", username),
		}

		for _, cmd := range commands {
			if p.verbose {
				fmt.Printf("  Running: %s\n", cmd)
			}
			if _, err := p.client.Execute(cmd); err != nil {
				// May fail if user already exists, that's okay
				if p.verbose {
					fmt.Printf("  Warning: %v\n", err)
				}
			}
		}
	} else {
		if p.verbose {
			fmt.Printf("  User %s already exists\n", username)
		}
	}

	// Runtime access is mediated by takod's Unix socket, not broad sudo or Docker group membership.
	if username != "root" {
		if _, err := p.client.Execute(fmt.Sprintf("sudo usermod -aG %s %s", takodAccessGroup, username)); err != nil {
			return fmt.Errorf("failed to grant takod socket access to %s: %w", username, err)
		}
		_, _ = p.client.Execute(fmt.Sprintf("sudo rm -f /etc/sudoers.d/tako-%s", username))
	}

	return nil
}

// VerifyAutoRecovery verifies that critical services are enabled for auto-recovery
func (p *Provisioner) VerifyAutoRecovery() error {
	if p.verbose {
		fmt.Printf("→ Verifying auto-recovery configuration...\n")
	}

	// Check if critical services are enabled
	services := []string{"docker", "containerd", "ssh"}

	for _, service := range services {
		output, err := p.client.Execute(fmt.Sprintf("sudo systemctl is-enabled %s 2>/dev/null || echo 'not-found'", service))
		status := strings.TrimSpace(output)

		if err != nil || (status != "enabled" && status != "static") {
			if p.verbose {
				fmt.Printf("  ⚠ %s is not enabled, enabling now...\n", service)
			}
			p.client.Execute(fmt.Sprintf("sudo systemctl enable %s", service))
		} else {
			if p.verbose {
				fmt.Printf("  ✓ %s is enabled on boot\n", service)
			}
		}
	}

	// Verify services are running
	for _, service := range services {
		output, _ := p.client.Execute(fmt.Sprintf("sudo systemctl is-active %s 2>/dev/null", service))
		status := strings.TrimSpace(output)

		if status != "active" {
			if p.verbose {
				fmt.Printf("  ⚠ %s is not running, starting...\n", service)
			}
			p.client.Execute(fmt.Sprintf("sudo systemctl start %s", service))
		} else {
			if p.verbose {
				fmt.Printf("  ✓ %s is running\n", service)
			}
		}
	}

	if p.verbose {
		fmt.Printf("  ✓ Auto-recovery verification complete\n")
	}

	return nil
}

// InstallMonitoringAgent installs the lightweight monitoring agent
func (p *Provisioner) InstallMonitoringAgent() error {
	if p.verbose {
		fmt.Printf("→ Installing monitoring agent...\n")
	}

	// Check if already installed and running (but allow updates)
	output, err := p.client.Execute("systemctl is-active tako-monitor 2>/dev/null")
	alreadyRunning := err == nil && strings.TrimSpace(output) == "active"

	if alreadyRunning && p.verbose {
		fmt.Printf("  Updating monitoring agent...\n")
	}

	// Read the agent script from embedded file
	agentScript := `#!/bin/bash
# Tako CLI Monitoring Agent
set -euo pipefail

INTERVAL=${MONITOR_INTERVAL:-60}
STATE_DIR="/var/lib/tako/metrics"
METRICS_FILE="$STATE_DIR/current.json"

mkdir -p "$STATE_DIR"

get_cpu_usage() {
    local cpu_line=$(grep '^cpu ' /proc/stat)
    local cpu_times=($cpu_line)
    local user=${cpu_times[1]}
    local nice=${cpu_times[2]}
    local system=${cpu_times[3]}
    local idle=${cpu_times[4]}
    local iowait=${cpu_times[5]}
    local irq=${cpu_times[6]}
    local softirq=${cpu_times[7]}
    local steal=${cpu_times[8]:-0}

    local total=$((user + nice + system + idle + iowait + irq + softirq + steal))
    local busy=$((total - idle - iowait))

    local prev_file="$STATE_DIR/cpu_prev"
    if [ -f "$prev_file" ]; then
        local prev_values=$(cat "$prev_file")
        local prev_total=$(echo "$prev_values" | cut -d' ' -f1)
        local prev_busy=$(echo "$prev_values" | cut -d' ' -f2)
        local total_delta=$((total - prev_total))
        local busy_delta=$((busy - prev_busy))
        if [ $total_delta -gt 0 ]; then
            local cpu_pct=$((busy_delta * 10000 / total_delta))
            echo "scale=2; $cpu_pct / 100" | bc
        else
            echo "0.00"
        fi
    else
        echo "0.00"
    fi
    echo "$total $busy" > "$prev_file"
}

get_memory_usage() {
    local mem_total=$(grep '^MemTotal:' /proc/meminfo | awk '{print $2}')
    local mem_available=$(grep '^MemAvailable:' /proc/meminfo | awk '{print $2}')
    local swap_total=$(grep '^SwapTotal:' /proc/meminfo | awk '{print $2}')
    local swap_free=$(grep '^SwapFree:' /proc/meminfo | awk '{print $2}')
    local mem_used=$((mem_total - mem_available))
    local mem_total_mb=$((mem_total / 1024))
    local mem_used_mb=$((mem_used / 1024))
    local mem_available_mb=$((mem_available / 1024))
    local swap_total_mb=$((swap_total / 1024))
    local swap_used_mb=$(((swap_total - swap_free) / 1024))
    local mem_pct=$(echo "scale=2; $mem_used * 100 / $mem_total" | bc)
    echo "{\"total_mb\":$mem_total_mb,\"used_mb\":$mem_used_mb,\"available_mb\":$mem_available_mb,\"percent\":\"$mem_pct\",\"swap_total_mb\":$swap_total_mb,\"swap_used_mb\":$swap_used_mb}"
}

get_disk_usage() {
    local disk_info=$(df -BM / | tail -1)
    local total=$(echo "$disk_info" | awk '{print $2}' | sed 's/M//')
    local used=$(echo "$disk_info" | awk '{print $3}' | sed 's/M//')
    local available=$(echo "$disk_info" | awk '{print $4}' | sed 's/M//')
    local percent=$(echo "$disk_info" | awk '{print $5}' | sed 's/%//')
    echo "{\"total_mb\":$total,\"used_mb\":$used,\"available_mb\":$available,\"percent\":\"$percent\"}"
}

get_uptime() {
    cat /proc/uptime | awk '{print int($1)}'
}

get_load_average() {
    local loadavg=$(cat /proc/loadavg)
    local load1=$(echo "$loadavg" | awk '{print $1}')
    local load5=$(echo "$loadavg" | awk '{print $2}')
    local load15=$(echo "$loadavg" | awk '{print $3}')
    echo "{\"1min\":\"$load1\",\"5min\":\"$load5\",\"15min\":\"$load15\"}"
}

collect_metrics() {
    local timestamp=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
    local cpu=$(get_cpu_usage)
    local memory=$(get_memory_usage)
    local disk=$(get_disk_usage)
    local uptime=$(get_uptime)
    local load=$(get_load_average)

    cat > "$METRICS_FILE" <<EOF
{
  "timestamp": "$timestamp",
  "cpu_percent": "$cpu",
  "memory": $memory,
  "disk": $disk,
  "uptime_seconds": $uptime,
  "load_average": $load
}
EOF

    if [ "${OUTPUT_STDOUT:-0}" = "1" ]; then
        cat "$METRICS_FILE"
    fi
}

main() {
    while true; do
        collect_metrics
        sleep "$INTERVAL"
    done
}

trap 'exit 0' SIGTERM SIGINT

if [ "${1:-}" = "once" ]; then
    collect_metrics
    exit 0
fi

main
`

	systemdService := `[Unit]
Description=Tako CLI Monitoring Agent
After=network.target

[Service]
Type=simple
User=root
ExecStart=/usr/local/bin/tako-monitor.sh
Restart=always
RestartSec=10
Environment="MONITOR_INTERVAL=60"
StandardOutput=journal
StandardError=journal
SyslogIdentifier=tako-monitor

[Install]
WantedBy=multi-user.target
`

	// Install bc (required for floating point calculations)
	if p.verbose {
		fmt.Printf("  Installing bc (calculator)...\n")
	}
	_, err = p.client.Execute("sudo apt-get install -y bc")
	if err != nil {
		return fmt.Errorf("failed to install bc: %w", err)
	}

	// Upload agent script
	if p.verbose {
		fmt.Printf("  Uploading monitoring agent script...\n")
	}
	scriptPath := "/usr/local/bin/tako-monitor.sh"
	uploadCmd := fmt.Sprintf("sudo tee %s > /dev/null << 'EOFSCRIPT'\n%s\nEOFSCRIPT", scriptPath, agentScript)
	_, err = p.client.Execute(uploadCmd)
	if err != nil {
		return fmt.Errorf("failed to upload agent script: %w", err)
	}

	// Make script executable
	_, err = p.client.Execute(fmt.Sprintf("sudo chmod +x %s", scriptPath))
	if err != nil {
		return fmt.Errorf("failed to make script executable: %w", err)
	}

	// Upload systemd service
	if p.verbose {
		fmt.Printf("  Creating systemd service...\n")
	}
	servicePath := "/etc/systemd/system/tako-monitor.service"
	uploadServiceCmd := fmt.Sprintf("sudo tee %s > /dev/null << 'EOFSERVICE'\n%s\nEOFSERVICE", servicePath, systemdService)
	_, err = p.client.Execute(uploadServiceCmd)
	if err != nil {
		return fmt.Errorf("failed to create systemd service: %w", err)
	}

	// Reload systemd, enable and start service
	if p.verbose {
		fmt.Printf("  Starting monitoring service...\n")
	}
	commands := []string{
		"sudo systemctl daemon-reload",
		"sudo systemctl enable tako-monitor",
		"sudo systemctl restart tako-monitor",
	}

	for _, cmd := range commands {
		if _, err := p.client.Execute(cmd); err != nil {
			return fmt.Errorf("failed to setup systemd service: %w", err)
		}
	}

	// Verify service is running
	output, err = p.client.Execute("systemctl is-active tako-monitor")
	if err != nil || strings.TrimSpace(output) != "active" {
		return fmt.Errorf("monitoring service failed to start")
	}

	if p.verbose {
		fmt.Printf("  ✓ Monitoring agent installed and running\n")
	}

	return nil
}
