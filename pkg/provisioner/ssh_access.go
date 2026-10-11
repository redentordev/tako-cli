package provisioner

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// sshFirewallComment marks the UFW allow rules setup creates for SSH. Rules
// carrying it are Tako-owned, which is how stale SSH allowances are found.
const sshFirewallComment = "SSH (fail2ban protected)"

// defaultSSHPort is always part of the SSH port set: it was allowed by every
// earlier setup, and keeping it preserves the operator's fallback path.
const defaultSSHPort = 22

// Firewall report values.
const (
	FirewallBackendUFW  = "ufw"
	FirewallBackendNone = "none"

	FirewallStatusConfigured = "configured"
	FirewallStatusSkipped    = "skipped"

	NativeFirewallFirewalld = "firewalld"
)

// FirewallOptions configures the setup firewall step.
type FirewallOptions struct {
	MeshListenPort int
	// SSHPorts is the resolved SSH port set (see ResolveSSHPorts). Empty
	// means the default SSH port only.
	SSHPorts []int
	// Probe opens a fresh SSH connection to the configured host and port.
	// It runs after UFW is enabled; a failure disables UFW again.
	Probe func() error
}

// FirewallReport describes what the firewall step did.
type FirewallReport struct {
	Backend string
	Status  string
	Reason  string
	// SSHPorts are the SSH ports allowed (configured) or that must be
	// allowed (skipped).
	SSHPorts []int
	// AllowedPorts lists port/protocol pairs actually applied to UFW. It is
	// empty when the step was skipped.
	AllowedPorts []string
	// RequiredPorts lists the port/protocol pairs Tako needs open, whether or
	// not it applied them.
	RequiredPorts []string
	// StaleSSHPorts are Tako SSH allow rules whose port is no longer in the
	// SSH port set. They are reported, never removed.
	StaleSSHPorts []int
	// NativeFirewall names an active host firewall Tako did not manage.
	NativeFirewall string
}

// Warnings returns the operator-facing warnings for the report: the skipped
// host-firewall notice and one line per stale SSH allowance.
func (r *FirewallReport) Warnings() []string {
	if r == nil {
		return nil
	}
	var warnings []string
	if r.Status == FirewallStatusSkipped {
		msg := fmt.Sprintf("Warning: Tako did not configure a host firewall (%s).", r.Reason)
		if r.NativeFirewall != "" {
			msg += fmt.Sprintf(" %s is active and was not modified by Tako.", r.NativeFirewall)
		}
		if len(r.RequiredPorts) > 0 {
			msg += fmt.Sprintf(" Make sure these ports are open in your provider or native firewall: %s", strings.Join(r.RequiredPorts, ", "))
		}
		warnings = append(warnings, msg)
	}
	for _, port := range r.StaleSSHPorts {
		warnings = append(warnings, fmt.Sprintf("Warning: UFW still allows %d/tcp from an earlier Tako SSH port. Once you no longer need it, remove it with: sudo ufw delete allow %d/tcp", port, port))
	}
	return warnings
}

// FormatTCPPorts renders ports as "<port>/tcp" strings.
func FormatTCPPorts(ports []int) []string {
	if len(ports) == 0 {
		return nil
	}
	out := make([]string, 0, len(ports))
	for _, port := range ports {
		out = append(out, fmt.Sprintf("%d/tcp", port))
	}
	return out
}

// parseSSHConnectionServerPort returns the server-side port (4th field) of
// an $SSH_CONNECTION value, or 0 when it is missing or invalid.
func parseSSHConnectionServerPort(value string) int {
	fields := strings.Fields(value)
	if len(fields) < 4 {
		return 0
	}
	port, err := strconv.Atoi(fields[3])
	if err != nil || port < 1 || port > 65535 {
		return 0
	}
	return port
}

// sshPortSet returns {22} ∪ {configured} ∪ {live}, sorted and de-duplicated.
// A configured port of 0 means the default port; invalid live ports are
// ignored.
func sshPortSet(configured int, live int) []int {
	ports := []int{defaultSSHPort}
	if configured >= 1 && configured <= 65535 {
		ports = append(ports, configured)
	}
	if live >= 1 && live <= 65535 {
		ports = append(ports, live)
	}
	return normalizeSSHPorts(ports)
}

// normalizeSSHPorts sorts and de-duplicates ports and always includes 22.
func normalizeSSHPorts(ports []int) []int {
	seen := map[int]bool{defaultSSHPort: true}
	out := []int{defaultSSHPort}
	for _, port := range ports {
		if seen[port] {
			continue
		}
		seen[port] = true
		out = append(out, port)
	}
	sort.Ints(out)
	return out
}

func validateSSHPorts(ports []int) error {
	for _, port := range ports {
		if port < 1 || port > 65535 {
			return fmt.Errorf("SSH port %d must be between 1 and 65535", port)
		}
	}
	return nil
}

// ResolveSSHPorts returns the SSH port set setup must keep reachable: 22,
// the configured server port, and the server-side port of the live session
// as reported by $SSH_CONNECTION. A missing or unparsable $SSH_CONNECTION is
// not an error; the configured port still applies.
func (p *Provisioner) ResolveSSHPorts(configuredPort int) ([]int, error) {
	if configuredPort < 0 || configuredPort > 65535 {
		return nil, fmt.Errorf("SSH port %d must be between 1 and 65535", configuredPort)
	}
	live := 0
	if output, err := p.client.Execute("printf '%s' \"$SSH_CONNECTION\""); err == nil {
		live = parseSSHConnectionServerPort(output)
	}
	return sshPortSet(configuredPort, live), nil
}

// ufwAddedRule is one rule from `ufw show added`.
type ufwAddedRule struct {
	Action  string // allow, limit, deny, reject
	Port    int    // 0 when the rule has no single numeric port
	Proto   string // tcp, udp, or "" for both
	Comment string
	// AnySource is true when the rule does not restrict the source address.
	AnySource bool
}

// parseUFWAddedRules parses `ufw show added`, which lists user rules in
// command form even while UFW is inactive.
func parseUFWAddedRules(output string) []ufwAddedRule {
	var rules []ufwAddedRule
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ufw ") {
			continue
		}
		rule, ok := parseUFWAddedRule(strings.TrimPrefix(line, "ufw "))
		if ok {
			rules = append(rules, rule)
		}
	}
	return rules
}

func parseUFWAddedRule(line string) (ufwAddedRule, bool) {
	comment := ""
	if idx := strings.Index(line, " comment "); idx >= 0 {
		comment = strings.TrimSpace(line[idx+len(" comment "):])
		comment = strings.Trim(comment, "'\"")
		line = line[:idx]
	}
	tokens := strings.Fields(line)
	if len(tokens) == 0 {
		return ufwAddedRule{}, false
	}
	// "ufw route ..." rules forward traffic and never admit SSH to the host.
	if tokens[0] == "route" {
		return ufwAddedRule{}, false
	}
	rule := ufwAddedRule{Action: tokens[0], Comment: comment, AnySource: true}
	tokens = tokens[1:]
	// Optional direction, and optional "log"/"log-all".
	for len(tokens) > 0 && (tokens[0] == "in" || tokens[0] == "out" || tokens[0] == "log" || tokens[0] == "log-all") {
		if tokens[0] == "out" {
			return ufwAddedRule{}, false
		}
		tokens = tokens[1:]
	}
	if len(tokens) == 0 {
		return ufwAddedRule{}, false
	}
	// Optional "on <iface>" restricts the rule; it is not a host-wide allow.
	if tokens[0] == "on" {
		rule.AnySource = false
		if len(tokens) < 3 {
			return ufwAddedRule{}, false
		}
		tokens = tokens[2:]
	}

	if tokens[0] != "from" && tokens[0] != "to" && tokens[0] != "proto" {
		// Simple syntax: PORT[/PROTO] or a service name.
		portSpec := tokens[0]
		if slash := strings.Index(portSpec, "/"); slash >= 0 {
			rule.Proto = portSpec[slash+1:]
			portSpec = portSpec[:slash]
		}
		if port, err := strconv.Atoi(portSpec); err == nil {
			rule.Port = port
		}
		return rule, true
	}

	// Extended syntax: [proto P] [from SRC [port P]] [to DST [port P]] [proto P]
	for i := 0; i < len(tokens); i++ {
		switch tokens[i] {
		case "from":
			if i+1 < len(tokens) {
				src := tokens[i+1]
				if src != "any" && src != "0.0.0.0/0" && src != "::/0" {
					rule.AnySource = false
				}
				i++
			}
		case "to":
			i++
		case "port":
			// Only the destination port (the one after "to") matters; ufw
			// prints "from X port P" for source ports, which never applies
			// to inbound SSH.
			if i+1 < len(tokens) && i >= 2 && tokens[i-2] == "to" {
				if port, err := strconv.Atoi(tokens[i+1]); err == nil {
					rule.Port = port
				}
			}
			i++
		case "proto":
			if i+1 < len(tokens) {
				rule.Proto = tokens[i+1]
			}
			i++
		}
	}
	return rule, true
}

// allowsInboundTCP reports whether the rule admits inbound TCP on port from
// any source.
func (r ufwAddedRule) allowsInboundTCP(port int) bool {
	return r.Action == "allow" && r.AnySource && r.Port == port && (r.Proto == "" || r.Proto == "tcp")
}

// missingSSHPorts returns the SSH ports with no confirmed allow rule.
func missingSSHPorts(rules []ufwAddedRule, sshPorts []int) []int {
	var missing []int
	for _, port := range sshPorts {
		found := false
		for _, rule := range rules {
			if rule.allowsInboundTCP(port) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, port)
		}
	}
	return missing
}

// staleSSHPorts returns ports of Tako SSH allow rules that are no longer in
// the SSH port set, sorted and de-duplicated.
func staleSSHPorts(rules []ufwAddedRule, sshPorts []int) []int {
	current := make(map[int]bool, len(sshPorts))
	for _, port := range sshPorts {
		current[port] = true
	}
	seen := map[int]bool{}
	var stale []int
	for _, rule := range rules {
		if rule.Action != "allow" || rule.Comment != sshFirewallComment || rule.Port == 0 {
			continue
		}
		if current[rule.Port] || seen[rule.Port] {
			continue
		}
		seen[rule.Port] = true
		stale = append(stale, rule.Port)
	}
	sort.Ints(stale)
	return stale
}

// skippedFirewallReport describes a firewall step that left the host
// firewall to the provider or OS.
func skippedFirewallReport(osName string, nativeFirewall string, meshListenPort int, sshPorts []int) *FirewallReport {
	return &FirewallReport{
		Backend:        FirewallBackendNone,
		Status:         FirewallStatusSkipped,
		Reason:         fmt.Sprintf("UFW is not available on %s", osName),
		SSHPorts:       sshPorts,
		RequiredPorts:  FirewallAllowedPorts(meshListenPort, sshPorts),
		NativeFirewall: nativeFirewall,
	}
}

// parseNativeFirewall maps `systemctl is-active firewalld` output to the
// native firewall name, or "" when it is not active.
func parseNativeFirewall(output string) string {
	if strings.TrimSpace(output) == "active" {
		return NativeFirewallFirewalld
	}
	return ""
}

// HardeningOptions configures the setup hardening step.
type HardeningOptions struct {
	// SSHPorts is the resolved SSH port set; the fail2ban jail bans on it.
	SSHPorts []int
	// KeyOnlyProbe opens a fresh SSH connection using only key or agent
	// authentication. Password authentication is disabled only when it
	// succeeds.
	KeyOnlyProbe func() error
	// Warn receives operator-facing warnings (for example, password
	// authentication kept enabled). Nil discards them.
	Warn func(message string)
}

func (o HardeningOptions) warn(message string) {
	if o.Warn != nil {
		o.Warn(message)
	}
}

type sshdHardeningOptions struct {
	DisablePasswordAuth bool
}

const sshdConfigPath = "/etc/ssh/sshd_config"
const sshdConfigBackupPath = "/etc/ssh/sshd_config.tako-bak"

// sshdHardeningScript returns a root script that backs up sshd_config,
// applies setup's sshd edits, and validates the result with `sshd -t`. On
// validation failure it restores the backup, prints the validator output,
// and exits non-zero without touching the running sshd.
func sshdHardeningScript(opts sshdHardeningOptions) string {
	var b strings.Builder
	b.WriteString(`set -eu
cfg=` + sshdConfigPath + `
backup=` + sshdConfigBackupPath + `
cp -p "$cfg" "$backup"

# Connection limits, keepalives, and login grace time.
sed -i 's/^#MaxStartups.*/MaxStartups 100:30:100/' "$cfg"
grep -q '^MaxStartups' "$cfg" || echo 'MaxStartups 100:30:100' >> "$cfg"
sed -i 's/^#MaxSessions.*/MaxSessions 100/' "$cfg"
grep -q '^MaxSessions' "$cfg" || echo 'MaxSessions 100' >> "$cfg"
sed -i 's/^#ClientAliveInterval.*/ClientAliveInterval 60/' "$cfg"
grep -q '^ClientAliveInterval' "$cfg" || echo 'ClientAliveInterval 60' >> "$cfg"
sed -i 's/^#ClientAliveCountMax.*/ClientAliveCountMax 10/' "$cfg"
grep -q '^ClientAliveCountMax' "$cfg" || echo 'ClientAliveCountMax 10' >> "$cfg"
sed -i 's/^#LoginGraceTime.*/LoginGraceTime 120/' "$cfg"
grep -q '^LoginGraceTime' "$cfg" || echo 'LoginGraceTime 120' >> "$cfg"

# Block root password login only when no PermitRootLogin is set; an
# operator's explicit value (including "no") is left alone.
if ! grep -Eq '^[[:space:]]*PermitRootLogin[[:space:]]' "$cfg"; then
  if grep -q '^#PermitRootLogin' "$cfg"; then
    sed -i 's/^#PermitRootLogin.*/PermitRootLogin prohibit-password/' "$cfg"
  else
    echo 'PermitRootLogin prohibit-password' >> "$cfg"
  fi
fi
`)
	if opts.DisablePasswordAuth {
		b.WriteString(`
# Key-based access was verified; disable password authentication.
sed -i 's/^PasswordAuthentication yes/PasswordAuthentication no/' "$cfg"
sed -i 's/^#PasswordAuthentication yes/PasswordAuthentication no/' "$cfg"
`)
	}
	b.WriteString(`
# Validate before any reload; restore the previous file on failure.
sshd_bin=$(command -v sshd || true)
[ -n "$sshd_bin" ] || sshd_bin=/usr/sbin/sshd
[ -d /run/sshd ] || mkdir -p /run/sshd
if ! sshd_out=$("$sshd_bin" -t 2>&1); then
  cp -p "$backup" "$cfg"
  echo "sshd -t rejected the hardened sshd_config; restored the previous configuration:"
  printf '%s\n' "$sshd_out"
  exit 1
fi
exit 0
`)
	return b.String()
}

// sshdHardeningError wraps a failed hardening script run with its output,
// which carries the `sshd -t` validator message.
func sshdHardeningError(output string, err error) error {
	output = strings.TrimSpace(output)
	if output == "" {
		return fmt.Errorf("sshd hardening failed; sshd was not reloaded: %w", err)
	}
	return fmt.Errorf("sshd hardening failed; sshd was not reloaded: %w\n%s", err, output)
}

// sshdReloadCommand reloads sshd without dropping existing sessions.
const sshdReloadCommand = "sudo systemctl reload ssh || sudo systemctl reload sshd"
