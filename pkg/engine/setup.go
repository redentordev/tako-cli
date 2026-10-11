package engine

// KindSetupResult identifies a serialized setup result document.
const KindSetupResult = "SetupResult"

// Setup step keys carried in SetupStepOutcome.Step and setup.step.* events.
// Keys are contract-stable; titles are human prose and may change.
const (
	SetupStepOSCheck      = "os-check"
	SetupStepPackages     = "packages"
	SetupStepDocker       = "docker"
	SetupStepWireGuard    = "wireguard"
	SetupStepFirewall     = "firewall"
	SetupStepHardening    = "hardening"
	SetupStepAutoRecovery = "auto-recovery"
	SetupStepDeployUser   = "deploy-user"
	SetupStepMonitorAgent = "monitor-agent"
	SetupStepTakodInstall = "takod-install"
	SetupStepTakodService = "takod-service"
)

// Setup step statuses carried in SetupStepOutcome.Status.
const (
	SetupStepCompleted = "completed"
	SetupStepFailed    = "failed"
	SetupStepSkipped   = "skipped"
)

// Setup modes carried in SetupNodeResult.Mode: fresh provisions a new node,
// reapply upgrades an older setup version, converge refreshes a node already
// at the current setup version (only firewall, deploy access, and the takod
// runtime re-run; other steps report skipped).
const (
	SetupModeFresh    = "fresh"
	SetupModeReapply  = "reapply"
	SetupModeConverge = "converge"
)

// SetupStepOutcome reports one provisioning step on one node.
type SetupStepOutcome struct {
	Step   string `json:"step"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// SetupHostKey is the SSH host key recorded for the node during setup so
// callers can pin it (feeds --host-key-mode strict). Key is the
// base64-encoded public key; Fingerprint is its SHA256 form.
type SetupHostKey struct {
	Type        string `json:"type"`
	Key         string `json:"key"`
	Fingerprint string `json:"fingerprint"`
}

// Setup firewall report values.
const (
	SetupFirewallBackendUFW  = "ufw"
	SetupFirewallBackendNone = "none"

	SetupFirewallConfigured = "configured"
	SetupFirewallSkipped    = "skipped"
)

// SetupFirewall reports the firewall step's outcome. Backend is "ufw" or
// "none"; Status is "configured" or "skipped" (with Reason). SSHPorts are
// the SSH port/protocol pairs allowed (or, when skipped, that must be
// opened). StaleSSHPorts are Tako SSH allowances no longer in use; setup
// reports them and never removes them. NativeFirewall names an active host
// firewall (for example "firewalld") that Tako did not modify.
type SetupFirewall struct {
	Backend        string   `json:"backend"`
	Status         string   `json:"status"`
	Reason         string   `json:"reason,omitempty"`
	SSHPorts       []string `json:"sshPorts,omitempty"`
	StaleSSHPorts  []string `json:"staleSshPorts,omitempty"`
	NativeFirewall string   `json:"nativeFirewall,omitempty"`
}

// SetupNodeResult reports one node's provisioning outcome, including the
// facts a control plane needs to adopt the node: detected OS, installed
// docker/takod versions, firewall allowances, and the recorded host key.
// FirewallPorts lists only ports actually applied to the host firewall and
// is omitted when the firewall step was skipped.
type SetupNodeResult struct {
	Server        string             `json:"server"`
	Host          string             `json:"host,omitempty"`
	Mode          string             `json:"mode,omitempty"`
	OS            string             `json:"os,omitempty"`
	DockerVersion string             `json:"dockerVersion,omitempty"`
	TakodVersion  string             `json:"takodVersion,omitempty"`
	SetupVersion  string             `json:"setupVersion,omitempty"`
	FirewallPorts []string           `json:"firewallPorts,omitempty"`
	Firewall      *SetupFirewall     `json:"firewall,omitempty"`
	HostKey       *SetupHostKey      `json:"hostKey,omitempty"`
	Steps         []SetupStepOutcome `json:"steps,omitempty"`
	Error         string             `json:"error,omitempty"`
}

// SetupResult is the serializable outcome of `tako setup`. Setup aborts on
// the first failing node, so Nodes lists only the nodes attempted.
type SetupResult struct {
	APIVersion  string            `json:"apiVersion"`
	Kind        string            `json:"kind"`
	Project     string            `json:"project"`
	Environment string            `json:"environment"`
	Nodes       []SetupNodeResult `json:"nodes"`
	Error       string            `json:"error,omitempty"`
}
