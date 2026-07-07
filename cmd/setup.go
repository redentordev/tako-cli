package cmd

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/provisioner"
	"github.com/redentordev/tako-cli/pkg/setup"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/spf13/cobra"
)

var (
	setupServer      string
	setupTakodBinary string
)

var setupCmd = &cobra.Command{
	Use:          "setup",
	Short:        "Set up server with takod, proxy, and security hardening",
	SilenceUsage: true,
	Long: `Setup configures your VPS server with all necessary components:
  - Container runtime prerequisites
  - WireGuard mesh networking
  - takod node-local runtime
  - tako-proxy for automatic SSL and load balancing
  - UFW firewall configuration
  - Security hardening (disable root login, fail2ban)
  - Deploy user creation and SSH key setup
  - Monitoring agent for system metrics

This only needs to be run once per server.`,
	RunE: runSetup,
}

func init() {
	rootCmd.AddCommand(setupCmd)
	setupCmd.Flags().StringVarP(&setupServer, "server", "s", "", "Server to set up (default: all servers)")
	setupCmd.Flags().StringVar(&setupTakodBinary, "takod-binary", "", "Path to a Linux tako binary to install as takod during setup (for development/testing)")
}

func runSetup(cmd *cobra.Command, args []string) error {
	// Load deployment configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Create SSH pool
	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()

	envName := getEnvironmentName(cfg)
	targetServerNames, servers, err := setupTargetServers(cfg, envName, setupServer)
	if err != nil {
		return err
	}

	result := engine.SetupResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindSetupResult,
		Project:     cfg.Project.Name,
		Environment: envName,
		Nodes:       []engine.SetupNodeResult{},
	}

	// Set up each server; abort on the first failing node.
	var opErr error
	for _, name := range targetServerNames {
		node, err := runSetupNode(cfg, sshPool, name, servers[name], envName)
		result.Nodes = append(result.Nodes, node)
		if err != nil {
			opErr = err
			break
		}
	}

	if opErr == nil {
		emitSetupLine("", "\nAll servers set up successfully!\n")
		emitSetupLine("", "\nNext step: Run 'tako deploy' to deploy your application\n")
	} else {
		result.Error = opErr.Error()
	}
	if emitErr := emitResultDocument(result); emitErr != nil && opErr == nil {
		opErr = emitErr
	}
	return opErr
}

// runSetupNode provisions one server and reports its per-step outcomes plus
// the adoption facts (OS, versions, firewall, recorded host key).
func runSetupNode(cfg *config.Config, sshPool *ssh.Pool, name string, server config.ServerConfig, envName string) (engine.SetupNodeResult, error) {
	node := engine.SetupNodeResult{Server: name, Host: server.Host}
	emitSetupLine(name, fmt.Sprintf("\n=== Setting up server: %s (%s) ===\n\n", name, server.Host))

	client, err := sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
	if err != nil {
		err = fmt.Errorf("failed to connect to server %s: %w", name, err)
		node.Error = err.Error()
		return node, err
	}

	prov := provisioner.NewProvisioner(client, verbose)
	prov.SetOutput(humanOut())
	meshListenPort := setupMeshListenPort(cfg)

	// Check if server is already set up and needs upgrade
	node.Mode = engine.SetupModeFresh
	var existingVersion *setup.ServerVersion
	if serverVersion, err := setup.DetectServerVersion(client); err == nil && serverVersion != nil {
		existingVersion = serverVersion
		if serverVersion.Version == setup.CurrentVersion {
			node.Mode = engine.SetupModeConverge
			emitSetupLine(name, fmt.Sprintf("→ Server is already at the latest version (v%s), refreshing firewall, deploy access, and takod runtime\n", serverVersion.Version))
		} else {
			node.Mode = engine.SetupModeReapply
			if serverVersion.IsUpgradeAvailable() {
				emitSetupLine(name, fmt.Sprintf("→ Server is at v%s, reapplying current setup v%s...\n", serverVersion.Version, setup.CurrentVersion))
			}
		}
	} else {
		emitSetupLine(name, "→ Setting up server from scratch...\n")
	}

	steps := setupNodeSteps(prov, cfg, name, server.User, meshListenPort, takodBinaryForSetup(), node.Mode == engine.SetupModeConverge)
	for _, step := range steps {
		if step.skip {
			node.Steps = append(node.Steps, engine.SetupStepOutcome{Step: step.key, Title: step.title, Status: engine.SetupStepSkipped})
			emitSetupStepEvent(events.TypeSetupStepSkipped, events.LevelInfo, name, "", step, nil)
			continue
		}
		emitSetupStepEvent(events.TypeSetupStepStarted, events.LevelInfo, name, fmt.Sprintf("→ %s...\n", step.title), step, nil)
		if err := step.fn(); err != nil {
			node.Steps = append(node.Steps, engine.SetupStepOutcome{Step: step.key, Title: step.title, Status: engine.SetupStepFailed, Error: err.Error()})
			emitSetupStepEvent(events.TypeSetupStepFailed, events.LevelError, name, "", step, err)
			err = fmt.Errorf("failed at step '%s' on server %s: %w", step.title, name, err)
			node.Error = err.Error()
			return node, err
		}
		node.Steps = append(node.Steps, engine.SetupStepOutcome{Step: step.key, Title: step.title, Status: engine.SetupStepCompleted})
		emitSetupStepEvent(events.TypeSetupStepCompleted, events.LevelInfo, name, fmt.Sprintf("  ✓ %s completed\n", step.title), step, nil)
	}

	// Write version file after successful provisioning. Converge preserves
	// the recorded install time; fresh/reapply restamp it.
	manifestBase := existingVersion
	if node.Mode != engine.SetupModeConverge {
		manifestBase = nil
	}
	if err := setup.WriteVersionFile(client, setupVersionManifest(manifestBase)); err != nil {
		err = setupVersionWriteError(name, err)
		node.Error = err.Error()
		return node, err
	}

	collectSetupNodeFacts(&node, client, cfg, server, meshListenPort)

	if node.Mode != engine.SetupModeConverge {
		emitSetupLine(name, fmt.Sprintf("\n✓ Server %s set up successfully!\n", name))
	}
	return node, nil
}

// collectSetupNodeFacts fills the adoption facts a control plane reads from
// the result document. Probe failures leave fields empty rather than failing
// a setup that already succeeded.
func collectSetupNodeFacts(node *engine.SetupNodeResult, client *ssh.Client, cfg *config.Config, server config.ServerConfig, meshListenPort int) {
	node.SetupVersion = setup.CurrentVersion
	node.FirewallPorts = provisioner.FirewallAllowedPorts(meshListenPort)
	if osInfo, err := provisioner.DetectOS(client); err == nil && osInfo != nil {
		node.OS = osInfo.String()
	}
	if info, err := provisioner.DetectDockerRuntime(client); err == nil && info != nil {
		node.DockerVersion = info.ServerVersion
	}
	if status, err := probeTakodAgentStatus(client, cfg, upgradeServersStatusProbe); err == nil && status != nil {
		node.TakodVersion = status.Version
	}
	if hostKey, err := ssh.LookupRecordedHostKey(server.Host, server.Port); err == nil && hostKey != nil {
		node.HostKey = &engine.SetupHostKey{Type: hostKey.Type, Key: hostKey.Key, Fingerprint: hostKey.Fingerprint}
	}
}

// setupProvisionStep is one provisioning step with its contract-stable key.
type setupProvisionStep struct {
	key   string
	title string
	fn    func() error
	skip  bool
}

// setupNodeProvisioner is the provisioner surface node setup drives.
type setupNodeProvisioner interface {
	currentSetupRefresher
	CheckRequirements() error
	UpdateSystem() error
	InstallDocker() error
	InstallWireGuard() error
	HardenSecurity() error
	VerifyAutoRecovery() error
	InstallMonitoringAgent() error
}

// setupNodeSteps returns the full provisioning sequence. Converge re-runs
// only firewall, deploy access, and the takod runtime; the other steps are
// marked skip so the result document reports them as skipped.
func setupNodeSteps(prov setupNodeProvisioner, cfg *config.Config, nodeName string, username string, meshListenPort int, takodBinary string, converge bool) []setupProvisionStep {
	return []setupProvisionStep{
		{engine.SetupStepOSCheck, "Checking system requirements", prov.CheckRequirements, converge},
		{engine.SetupStepPackages, "Updating system packages", prov.UpdateSystem, converge},
		{engine.SetupStepDocker, "Installing Docker", prov.InstallDocker, converge},
		{engine.SetupStepWireGuard, "Installing WireGuard", prov.InstallWireGuard, converge},
		{engine.SetupStepFirewall, "Configuring firewall (UFW)", func() error { return prov.ConfigureFirewall(meshListenPort) }, false},
		{engine.SetupStepHardening, "Hardening security", prov.HardenSecurity, converge},
		{engine.SetupStepAutoRecovery, "Verifying auto-recovery", prov.VerifyAutoRecovery, converge},
		{engine.SetupStepDeployUser, "Setting up deploy user", func() error { return prov.SetupDeployUser(username) }, false},
		{engine.SetupStepMonitorAgent, "Installing monitoring agent", prov.InstallMonitoringAgent, converge},
		{engine.SetupStepTakodInstall, "Installing takod binary", func() error { return installTakodBinary(prov, takodBinary) }, false},
		{engine.SetupStepTakodService, "Configuring takod service", func() error { return installTakodService(prov, cfg, nodeName) }, false},
	}
}

// emitSetupLine emits setup prose exactly as the command historically
// printed it, riding the event stream so machine modes keep stdout clean.
func emitSetupLine(node string, message string) {
	cliEngine().EventStream().Emit(events.Event{
		Type:    events.TypeLogLine,
		Phase:   events.PhaseSetup,
		Level:   events.LevelInfo,
		Node:    node,
		Message: message,
	})
}

func emitSetupStepEvent(eventType string, level events.Level, node string, message string, step setupProvisionStep, stepErr error) {
	data := map[string]any{"step": step.key, "title": step.title}
	if stepErr != nil {
		data["error"] = stepErr.Error()
	}
	cliEngine().EventStream().Emit(events.Event{
		Type:    eventType,
		Phase:   events.PhaseSetup,
		Level:   level,
		Node:    node,
		Message: message,
		Data:    data,
	})
}

func setupVersionWriteError(serverName string, err error) error {
	return fmt.Errorf("server %s setup completed but failed to write setup version metadata: %w", serverName, err)
}

func setupVersionManifest(existing *setup.ServerVersion) *setup.ServerVersion {
	return setupVersionManifestAt(existing, time.Now())
}

func setupVersionManifestAt(existing *setup.ServerVersion, now time.Time) *setup.ServerVersion {
	installedAt := now
	lastUpgrade := time.Time{}
	components := make(map[string]string)

	if existing != nil {
		installedAt = existing.InstalledAt
		lastUpgrade = now
		for key, value := range existing.Components {
			components[key] = value
		}
	}

	return &setup.ServerVersion{
		Version:        setup.CurrentVersion,
		InstalledAt:    installedAt,
		LastUpgrade:    lastUpgrade,
		TakoCLIVersion: Version,
		Components:     components,
		Features:       []string{"docker", "wireguard-mesh", "tako-proxy", "firewall", "monitoring"},
	}
}

func setupTargetServers(cfg *config.Config, envName string, requestedServer string) ([]string, map[string]config.ServerConfig, error) {
	serverNames, err := statePullServerNames(cfg, envName, requestedServer)
	if err != nil {
		return nil, nil, err
	}
	servers := make(map[string]config.ServerConfig, len(serverNames))
	for _, serverName := range serverNames {
		server, ok := cfg.Servers[serverName]
		if !ok {
			return nil, nil, fmt.Errorf("server %s not found in config", serverName)
		}
		servers[serverName] = server
	}
	return serverNames, servers, nil
}

func setupMeshListenPort(cfg *config.Config) int {
	if cfg.Mesh != nil && cfg.Mesh.ListenPort > 0 {
		return cfg.Mesh.ListenPort
	}
	return 51820
}

type setupRuntimeInstaller interface {
	InstallTakodBinaryFromFile(string) error
	InstallTakodBinary(string) error
	InstallTakodService(socket string, dataDir string, nodeName string) error
}

type currentSetupRefresher interface {
	setupRuntimeInstaller
	ConfigureFirewall(meshListenPort int) error
	SetupDeployUser(username string) error
}

func ensureTakodRuntimeForSetup(prov setupRuntimeInstaller, cfg *config.Config, nodeName string) error {
	return ensureTakodRuntimeWithBinary(prov, cfg, nodeName, takodBinaryForSetup())
}

func takodBinaryForSetup() string {
	if binary := strings.TrimSpace(setupTakodBinary); binary != "" {
		return binary
	}
	return strings.TrimSpace(os.Getenv("TAKO_TAKOD_BINARY"))
}

func ensureTakodRuntimeWithBinary(prov setupRuntimeInstaller, cfg *config.Config, nodeName string, takodBinary string) error {
	if err := installTakodBinary(prov, takodBinary); err != nil {
		return err
	}
	return installTakodService(prov, cfg, nodeName)
}

func installTakodBinary(prov setupRuntimeInstaller, takodBinary string) error {
	if takodBinary != "" {
		return prov.InstallTakodBinaryFromFile(takodBinary)
	}
	return prov.InstallTakodBinary(Version)
}

func installTakodService(prov setupRuntimeInstaller, cfg *config.Config, nodeName string) error {
	socket := ""
	dataDir := ""
	if cfg.Runtime != nil && cfg.Runtime.Agent != nil {
		socket = cfg.Runtime.Agent.Socket
		dataDir = cfg.Runtime.Agent.DataDir
	}
	return prov.InstallTakodService(socket, dataDir, nodeName)
}

// consoleLogger implements the setup.Logger interface for console output
type consoleLogger struct{}

func (l *consoleLogger) Log(format string, args ...interface{}) {
	fmt.Printf("  "+format+"\n", args...)
}
