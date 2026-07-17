package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/platform"
	"github.com/redentordev/tako-cli/pkg/provisioner"
	"github.com/redentordev/tako-cli/pkg/setup"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/redentordev/tako-cli/pkg/updater"
	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"
)

var (
	upgradeForce              bool
	upgradeCheck              bool
	upgradeServersServer      string
	upgradeServersDryRun      bool
	upgradeServersTakodBinary string
	upgradeServersStatusWait  = 30 * time.Second
	upgradeServersStatusPoll  = 1 * time.Second
	upgradeServersStatusProbe = 3 * time.Second
	upgradeCoordinatorLockTTL = 4 * time.Hour
)

var upgradeCmd = &cobra.Command{
	Use:          "upgrade",
	Short:        "Upgrade Tako CLI to the latest version",
	SilenceUsage: true,
	Long: `Upgrade Tako CLI to the latest version from GitHub releases.

This command will:
  - Check for the latest version
  - Download the appropriate binary for your platform
  - Replace the current binary with the new version
  - Create a backup of the current version (just in case)

The upgrade is performed in-place and requires the same permissions
as the original installation.`,
	Example: `  # Check for updates without upgrading
  tako upgrade --check

  # Upgrade to the latest version
  tako upgrade

  # Force upgrade even if already on latest
  tako upgrade --force`,
	RunE: runUpgrade,
}

var upgradeServersCmd = &cobra.Command{
	Use:          "servers",
	Short:        "Upgrade server-side takod agents to this CLI version",
	SilenceUsage: true,
	Long: `Upgrade server-side takod agents in the authoritative enrolled cluster inventory.

This command patches stale or missing takod agents without changing application
services. It installs the matching Tako release binary, restarts the takod
systemd service, refreshes /etc/tako/version.json, and verifies /v1/status. An
enrolled cluster is upgraded worker-first and controller-last, independent of
application environment subsets. Legacy configurations retain environment
selection behavior.

Use --dry-run first to see current agent versions. Development builds must pass
--takod-binary with a Linux tako binary because there is no release asset for
version "dev".`,
	Example: `  # Show current server agent versions
  tako upgrade servers --dry-run

  # Patch every node in the authoritative enrolled cluster
  tako upgrade servers

  # Patch one server
  tako upgrade servers --server node-a

  # Development/testing with a locally built Linux binary
  tako upgrade servers --takod-binary ./dist/tako-linux-amd64`,
	RunE: runUpgradeServers,
}

func init() {
	rootCmd.AddCommand(upgradeCmd)
	upgradeCmd.Flags().BoolVarP(&upgradeCheck, "check", "c", false, "Only check for updates, don't upgrade")
	upgradeCmd.Flags().BoolVarP(&upgradeForce, "force", "f", false, "Force upgrade even if already on latest version")
	upgradeCmd.AddCommand(upgradeServersCmd)
	upgradeServersCmd.Flags().StringVarP(&upgradeServersServer, "server", "s", "", "Server to upgrade (default: authoritative enrolled cluster inventory; legacy: active environment)")
	upgradeServersCmd.Flags().BoolVar(&upgradeServersDryRun, "dry-run", false, "Report server agent versions without changing remote state")
	upgradeServersCmd.Flags().StringVar(&upgradeServersTakodBinary, "takod-binary", "", "Path to a Linux tako binary to install as takod (required for development builds)")
}

func runUpgrade(cmd *cobra.Command, args []string) error {
	fmt.Println("🐙 Tako CLI Upgrade Tool")
	fmt.Println()

	// Check for updates
	fmt.Printf("Current version: %s\n", Version)
	fmt.Println("Checking for updates...")

	available, latestVersion, err := updater.IsUpdateAvailable(Version)
	if err != nil {
		return fmt.Errorf("failed to check for updates: %w", err)
	}

	// If only checking
	if upgradeCheck {
		if available {
			fmt.Printf("\n✨ New version available: %s\n", latestVersion)
			fmt.Printf("   Run 'tako upgrade' to update\n")
		} else {
			fmt.Printf("\n✓ You are already on the latest version (%s)\n", Version)
		}
		return nil
	}

	// Check if update is available
	if !available && !upgradeForce {
		fmt.Printf("\n✓ You are already on the latest version (%s)\n", Version)
		fmt.Println("\nUse --force to reinstall the current version")
		return nil
	}

	if available {
		fmt.Printf("\n📦 New version available: %s → %s\n\n", Version, latestVersion)
	} else {
		fmt.Printf("\n🔄 Reinstalling version: %s\n\n", latestVersion)
	}

	// Perform the upgrade
	if err := updater.DownloadUpdate(latestVersion); err != nil {
		return fmt.Errorf("upgrade failed: %w", err)
	}

	fmt.Println()
	fmt.Println("🎉 Upgrade completed successfully!")
	fmt.Println()
	fmt.Println("Verify with: tako --version")

	return nil
}

func runUpgradeServers(cmd *cobra.Command, args []string) error {
	if err := validateUpgradeServersOptions(Version, upgradeServersTakodBinary, upgradeServersDryRun); err != nil {
		return err
	}

	cfg, err := loadDeployConfig(cfgFile)
	if err != nil {
		return err
	}
	if err := ensureDeployRuntimeSupported(cfg); err != nil {
		return err
	}

	envName := getEnvironmentName(cfg)
	targetServerNames, servers, err := upgradeTargetServers(cfg, envName, upgradeServersServer)
	if err != nil {
		return err
	}
	if len(targetServerNames) == 0 {
		return fmt.Errorf("no servers configured for environment %s", envName)
	}

	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()

	out := humanOut()
	fmt.Fprintln(out, "🐙 Tako server agent upgrade")
	fmt.Fprintf(out, "Environment: %s\n", envName)
	fmt.Fprintf(out, "Target CLI version: %s\n", Version)
	if upgradeServersDryRun {
		fmt.Fprintln(out, "Mode: dry-run")
	}

	result := engine.UpgradeServersResult{
		APIVersion:    takoapi.APIVersionCurrent,
		Kind:          engine.KindUpgradeServersResult,
		Project:       cfg.Project.Name,
		Environment:   envName,
		TargetVersion: Version,
		DryRun:        upgradeServersDryRun,
		Nodes:         []engine.UpgradeServersNodeOutcome{},
	}
	upgradeNodes := make([]engine.UpgradeNode, 0, len(targetServerNames))
	for _, name := range targetServerNames {
		upgradeNodes = append(upgradeNodes, engine.UpgradeNode{Name: name, Roles: append([]string(nil), servers[name].Roles...)})
	}
	plan, err := engine.PlanNodeUpgrade(upgradeNodes)
	if err != nil {
		return err
	}
	if !upgradeServersDryRun {
		if err := preflightStagedUpgrade(cfg, sshPool, plan, servers, upgradeServersServer); err != nil {
			return emitUpgradePreflightFailure(result, plan, servers, err)
		}
	}
	var coordinator *provisioner.Provisioner
	if !upgradeServersDryRun && plan[0].Stage != engine.UpgradeStageLegacy {
		coordinator, err = acquireUpgradeCoordinator(cfg, sshPool)
		if err != nil {
			return emitUpgradePreflightFailure(result, plan, servers, err)
		}
		defer func() { _ = coordinator.ReleaseTakodUpgradeCoordinatorLock() }()
	}
	blocked := false
	for _, planned := range plan {
		if blocked {
			result.Nodes = append(result.Nodes, engine.UpgradeServersNodeOutcome{
				Server: planned.Name, Host: servers[planned.Name].Host, ToVersion: Version,
				Stage: planned.Stage, Outcome: engine.UpgradeOutcomeBlocked,
				Error: "blocked because an earlier staged upgrade did not verify",
			})
			continue
		}
		if coordinator != nil {
			if err := coordinator.RefreshTakodUpgradeCoordinatorLock(upgradeCoordinatorLockTTL); err != nil {
				result.Nodes = append(result.Nodes, engine.UpgradeServersNodeOutcome{
					Server: planned.Name, Host: servers[planned.Name].Host, ToVersion: Version,
					Stage: planned.Stage, Outcome: engine.UpgradeOutcomeBlocked,
					Error: fmt.Sprintf("lost authoritative cluster upgrade lock: %v", err),
				})
				blocked = true
				continue
			}
		}
		if !upgradeServersDryRun && planned.Stage == engine.UpgradeStageController {
			if err := validateControllerLastWorkers(cfg, sshPool, planned.Name); err != nil {
				result.Nodes = append(result.Nodes, engine.UpgradeServersNodeOutcome{
					Server: planned.Name, Host: servers[planned.Name].Host, ToVersion: Version,
					Stage: planned.Stage, Outcome: engine.UpgradeOutcomeBlocked, Error: err.Error(),
				})
				blocked = true
				continue
			}
		}
		outcome := upgradeServerNode(cfg, sshPool, out, planned.Name, servers[planned.Name], planned.Stage)
		result.Nodes = append(result.Nodes, outcome)
		if planned.Stage != engine.UpgradeStageLegacy && (outcome.Outcome == engine.UpgradeOutcomeFailed || outcome.Outcome == engine.UpgradeOutcomeRolledBack) {
			blocked = true
		}
	}

	failed := 0
	for _, node := range result.Nodes {
		if node.Outcome == engine.UpgradeOutcomeFailed || node.Outcome == engine.UpgradeOutcomeRolledBack || node.Outcome == engine.UpgradeOutcomeBlocked || node.Outcome == engine.UpgradeOutcomeDowngradeBlocked {
			failed++
		}
	}

	var opErr error
	switch {
	case failed == 0:
		if upgradeServersDryRun {
			fmt.Fprintln(out, "\nDry-run complete. Apply with: tako upgrade servers")
		} else {
			fmt.Fprintln(out, "\nAll selected server agents upgraded and verified.")
			fmt.Fprintln(out, "Next:")
			fmt.Fprintf(out, "  tako state status -e %s\n", envName)
			fmt.Fprintf(out, "  tako deploy -e %s --yes\n", envName)
		}
	case failed == len(result.Nodes):
		opErr = fmt.Errorf("server agent upgrade failed on all %d node(s)", failed)
	default:
		opErr = &engine.AttentionError{Err: fmt.Errorf("server agent upgrade failed on %d of %d node(s)", failed, len(result.Nodes))}
	}
	if opErr != nil {
		result.Error = opErr.Error()
	}
	if emitErr := emitResultDocument(result); emitErr != nil && opErr == nil {
		opErr = emitErr
	}
	return opErr
}

func upgradeTargetServers(cfg *config.Config, envName string, requested string) ([]string, map[string]config.ServerConfig, error) {
	enrolled := false
	for _, server := range cfg.Servers {
		if len(server.Roles) > 0 {
			enrolled = true
			break
		}
	}
	if !enrolled {
		return setupTargetServers(cfg, envName, requested)
	}
	if strings.TrimSpace(requested) != "" {
		server, ok := cfg.Servers[requested]
		if !ok {
			return nil, nil, fmt.Errorf("server %s not found in authoritative cluster inventory", requested)
		}
		if len(server.Roles) == 0 {
			return nil, nil, fmt.Errorf("server %s is not an enrolled cluster member", requested)
		}
		return []string{requested}, map[string]config.ServerConfig{requested: server}, nil
	}
	names := make([]string, 0, len(cfg.Servers))
	servers := make(map[string]config.ServerConfig, len(cfg.Servers))
	for name, server := range cfg.Servers {
		if len(server.Roles) == 0 {
			return nil, nil, fmt.Errorf("cannot mix legacy server %s into an enrolled cluster node upgrade", name)
		}
		names = append(names, name)
		servers[name] = server
	}
	sort.Strings(names)
	return names, servers, nil
}

func emitUpgradePreflightFailure(result engine.UpgradeServersResult, plan []engine.UpgradePlanNode, servers map[string]config.ServerConfig, cause error) error {
	result.Error = cause.Error()
	for _, planned := range plan {
		result.Nodes = append(result.Nodes, engine.UpgradeServersNodeOutcome{
			Server: planned.Name, Host: servers[planned.Name].Host, ToVersion: Version,
			Stage: planned.Stage, Outcome: engine.UpgradeOutcomeBlocked, Error: cause.Error(),
		})
	}
	if err := emitResultDocument(result); err != nil {
		return err
	}
	return cause
}

func upgradeSSHClient(pool *ssh.Pool, name string, server config.ServerConfig) (*ssh.Client, error) {
	if len(server.Roles) == 0 {
		return pool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
	}
	if server.ClusterID == "" || server.NodeID == "" || server.SSHHostKeyType == "" || server.SSHHostKey == "" || server.SSHHostKeyFingerprint == "" {
		return nil, fmt.Errorf("enrolled server %s lacks a complete controller-owned SSH identity pin", name)
	}
	return pool.GetOrCreateWithAuthPinned(server.Host, server.Port, server.User, server.SSHKey, server.Password, ssh.RecordedHostKey{
		Type: server.SSHHostKeyType, Key: server.SSHHostKey, Fingerprint: server.SSHHostKeyFingerprint,
	})
}

func acquireUpgradeCoordinator(cfg *config.Config, sshPool *ssh.Pool) (*provisioner.Provisioner, error) {
	controllerName := ""
	var controller config.ServerConfig
	for name, server := range cfg.Servers {
		if len(server.Roles) == 0 || !server.HasPlatformRole(nodeidentity.RoleControlPlane) {
			continue
		}
		if controllerName != "" {
			return nil, fmt.Errorf("authoritative cluster upgrade requires exactly one controller; found %s and %s", controllerName, name)
		}
		controllerName, controller = name, server
	}
	if controllerName == "" {
		return nil, fmt.Errorf("authoritative cluster upgrade requires exactly one enrolled controller")
	}
	client, err := upgradeSSHClient(sshPool, controllerName, controller)
	if err != nil {
		return nil, fmt.Errorf("connect to authoritative upgrade coordinator %s: %w", controllerName, err)
	}
	coordinator := provisioner.NewProvisioner(client, verbose)
	coordinator.SetOutput(humanOut())
	if err := coordinator.AcquireTakodUpgradeCoordinatorLock(upgradeCoordinatorLockTTL); err != nil {
		return nil, fmt.Errorf("acquire authoritative cluster upgrade lock on %s: %w", controllerName, err)
	}
	return coordinator, nil
}

func preflightStagedUpgrade(cfg *config.Config, sshPool *ssh.Pool, plan []engine.UpgradePlanNode, selected map[string]config.ServerConfig, requested string) error {
	enrolled := false
	for _, planned := range plan {
		if len(selected[planned.Name].Roles) > 0 {
			enrolled = true
			break
		}
	}
	if !enrolled {
		return nil
	}
	for _, planned := range plan {
		server := selected[planned.Name]
		client, err := upgradeSSHClient(sshPool, planned.Name, server)
		if err != nil {
			return fmt.Errorf("preflight staged upgrade connection to %s: %w", planned.Name, err)
		}
		status, err := probeTakodAgentStatus(client, cfg, upgradeServersStatusProbe)
		if err != nil {
			return fmt.Errorf("preflight staged upgrade status for %s: %w", planned.Name, err)
		}
		if err := engine.ValidateUpgradeCompatibility(status.Version, status.UpgradeProtocol, status.MinimumUpgradeProtocol); err != nil {
			return fmt.Errorf("preflight staged upgrade compatibility for %s: %w", planned.Name, err)
		}
		if err := validateUpgradeStatusIdentity(planned.Name, server, status); err != nil {
			return fmt.Errorf("preflight staged upgrade identity for %s: %w", planned.Name, err)
		}
		if err := validateUpgradeDoesNotDowngrade(Version, status.Version); err != nil {
			return fmt.Errorf("preflight staged upgrade version for %s: %w", planned.Name, err)
		}
	}

	// An explicitly targeted controller may be upgraded only after every other
	// enrolled worker already reports the target protocol and release.
	if len(plan) == 1 && plan[0].Stage == engine.UpgradeStageController && strings.TrimSpace(requested) != "" {
		return validateControllerLastWorkers(cfg, sshPool, plan[0].Name)
	}
	return nil
}

func validateControllerLastWorkers(cfg *config.Config, sshPool *ssh.Pool, controller string) error {
	names := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		server := cfg.Servers[name]
		if name == controller || len(server.Roles) == 0 || !server.HasPlatformRole(nodeidentity.RoleWorker) || server.HasPlatformRole(nodeidentity.RoleControlPlane) {
			continue
		}
		client, err := upgradeSSHClient(sshPool, name, server)
		if err != nil {
			return fmt.Errorf("controller-last recheck connection to worker %s: %w", name, err)
		}
		status, err := probeTakodAgentStatus(client, cfg, upgradeServersStatusProbe)
		if err != nil {
			return fmt.Errorf("controller-last recheck requires live worker %s: %w", name, err)
		}
		if err := validateUpgradeStatusIdentity(name, server, status); err != nil {
			return fmt.Errorf("controller-last recheck identity mismatch for worker %s: %w", name, err)
		}
		if status.Version != Version || status.UpgradeProtocol != engine.UpgradeProtocolCurrent || status.MinimumUpgradeProtocol != engine.UpgradeProtocolCurrent || !slices.Contains(status.Capabilities, takod.CapabilityNodeUpgradeV1) {
			return fmt.Errorf("controller-last recheck requires worker %s at release %s and protocol %d", name, Version, engine.UpgradeProtocolCurrent)
		}
	}
	return nil
}

// upgradeServerNode upgrades (or, in dry-run, assesses) one node's takod
// agent. Failures are recorded in the outcome so remaining nodes still run.
func upgradeServerNode(cfg *config.Config, sshPool *ssh.Pool, out io.Writer, name string, server config.ServerConfig, stage string) engine.UpgradeServersNodeOutcome {
	node := engine.UpgradeServersNodeOutcome{Server: name, Host: server.Host, ToVersion: Version, Stage: stage}
	fail := func(err error) engine.UpgradeServersNodeOutcome {
		node.Outcome = engine.UpgradeOutcomeFailed
		node.Error = err.Error()
		fmt.Fprintf(out, "✗ %v\n", err)
		return node
	}

	fmt.Fprintf(out, "\n=== Server: %s (%s) ===\n", name, server.Host)
	client, err := upgradeSSHClient(sshPool, name, server)
	if err != nil {
		return fail(fmt.Errorf("failed to connect to server %s: %w", name, err))
	}

	status, statusErr := probeTakodAgentStatus(client, cfg, upgradeServersStatusProbe)
	current := "unavailable"
	var downgradeErr error
	if statusErr == nil {
		current = status.Version
		node.FromVersion = status.Version
		node.Protocol = status.UpgradeProtocol
		if err := engine.ValidateUpgradeCompatibility(status.Version, status.UpgradeProtocol, status.MinimumUpgradeProtocol); err != nil {
			return fail(fmt.Errorf("server %s is not rolling-upgrade compatible: %w", name, err))
		}
		if len(server.Roles) > 0 {
			if err := validateUpgradeStatusIdentity(name, server, status); err != nil {
				return fail(fmt.Errorf("server %s mutation-time identity revalidation failed: %w", name, err))
			}
		}
		downgradeErr = validateUpgradeDoesNotDowngrade(Version, status.Version)
		if downgradeErr != nil && !upgradeServersDryRun {
			return fail(fmt.Errorf("server %s: %w", name, downgradeErr))
		}
	} else if len(server.Roles) > 0 {
		return fail(fmt.Errorf("server %s mutation-time identity revalidation requires live status: %w", name, statusErr))
	}
	fmt.Fprintf(out, "Current takod: %s\n", current)
	if statusErr != nil {
		fmt.Fprintf(out, "Status check: %v\n", statusErr)
	}

	serverVersion, versionErr := setup.DetectServerVersion(client)
	if upgradeServersDryRun {
		if versionErr != nil {
			fmt.Fprintf(out, "Setup manifest: unavailable (%v)\n", versionErr)
			fmt.Fprintf(out, "Action: setup required (%s -> %s)\n", current, Version)
			node.Outcome = engine.UpgradeOutcomeSetupRequired
			return node
		}
		action := "current"
		node.Outcome = engine.UpgradeOutcomeCurrent
		if statusErr != nil {
			action = "status unavailable"
			node.Outcome = engine.UpgradeOutcomeStatusUnavailable
		} else if downgradeErr != nil {
			action = "downgrade blocked"
			node.Outcome = engine.UpgradeOutcomeDowngradeBlocked
			node.Error = downgradeErr.Error()
		} else if current != Version || serverVersion.TakoCLIVersion != Version {
			action = "upgrade needed"
			node.Outcome = engine.UpgradeOutcomeUpgradeNeeded
		}
		fmt.Fprintf(out, "Setup manifest: v%s (CLI %s)\n", serverVersion.Version, serverVersion.TakoCLIVersion)
		fmt.Fprintf(out, "Action: %s (%s -> %s)\n", action, current, Version)
		return node
	}
	if versionErr != nil {
		return fail(fmt.Errorf("server %s is not set up; run 'tako setup --server %s' first: %w", name, name, versionErr))
	}
	prov := provisioner.NewProvisioner(client, verbose)
	prov.SetOutput(humanOut())
	var revalidate func() (*nodeidentity.UpgradeContract, error)
	if len(server.Roles) > 0 {
		revalidate = func() (*nodeidentity.UpgradeContract, error) {
			mutationStatus, err := probeTakodAgentStatus(client, cfg, upgradeServersStatusProbe)
			if err != nil {
				return nil, fmt.Errorf("immediate pre-publication status probe: %w", err)
			}
			if err := engine.ValidateUpgradeCompatibility(mutationStatus.Version, mutationStatus.UpgradeProtocol, mutationStatus.MinimumUpgradeProtocol); err != nil {
				return nil, fmt.Errorf("immediate pre-publication compatibility changed: %w", err)
			}
			if err := validateUpgradeStatusIdentity(name, server, mutationStatus); err != nil {
				return nil, fmt.Errorf("immediate pre-publication identity changed: %w", err)
			}
			if err := validateUpgradeDoesNotDowngrade(Version, mutationStatus.Version); err != nil {
				return nil, fmt.Errorf("immediate pre-publication version changed: %w", err)
			}
			return &nodeidentity.UpgradeContract{
				ClusterID: server.ClusterID, NodeID: server.NodeID,
				MembershipGeneration: mutationStatus.MembershipGeneration,
				Lifecycle:            mutationStatus.Membership.Lifecycle,
				Roles:                append([]string(nil), mutationStatus.Membership.Roles...),
			}, nil
		}
	}
	if err := prov.BeginTakodUpgrade(Version, upgradeServersTakodBinary, revalidate); err != nil {
		return fail(fmt.Errorf("failed to stage takod upgrade on server %s: %w", name, err))
	}
	rollback := func(cause error) engine.UpgradeServersNodeOutcome {
		rollbackErr := prov.RollbackTakodUpgrade()
		if manifestErr := setup.WriteVersionFile(client, serverVersion); rollbackErr == nil && manifestErr != nil {
			rollbackErr = fmt.Errorf("restore setup version manifest: %w", manifestErr)
		}
		if rollbackErr == nil && node.FromVersion != "" && node.FromVersion != "unavailable" {
			_, rollbackErr = waitForTakodAgentVersion(client, cfg, node.FromVersion, upgradeServersStatusWait, upgradeServersStatusPoll, upgradeServersStatusProbe)
		}
		if rollbackErr != nil {
			return fail(fmt.Errorf("%v; automatic rollback also failed: %w", cause, rollbackErr))
		}
		node.Outcome = engine.UpgradeOutcomeRolledBack
		node.RolledBack = true
		node.Error = cause.Error()
		fmt.Fprintf(out, "↩ %v; restored takod %s\n", cause, node.FromVersion)
		return node
	}
	if err := prov.ActivateTakodUpgrade(); err != nil {
		return rollback(fmt.Errorf("failed to activate takod upgrade on server %s: %w", name, err))
	}
	verified, err := waitForTakodAgentVersion(client, cfg, Version, upgradeServersStatusWait, upgradeServersStatusPoll, upgradeServersStatusProbe)
	if err != nil {
		return rollback(fmt.Errorf("server %s takod upgrade did not verify: %w", name, err))
	}
	if verified.UpgradeProtocol != engine.UpgradeProtocolCurrent || verified.MinimumUpgradeProtocol != engine.UpgradeProtocolCurrent || !slices.Contains(verified.Capabilities, takod.CapabilityNodeUpgradeV1) {
		return rollback(fmt.Errorf("server %s reported an incomplete node-upgrade capability contract", name))
	}
	if len(server.Roles) > 0 && !slices.Contains(verified.Capabilities, takod.CapabilityPlatformWorkerHandoffV1) {
		return rollback(fmt.Errorf("server %s did not attest protected platform-worker handoff support", name))
	}
	if err := prov.VerifyTakodUpgradeServices(stage == engine.UpgradeStageController); err != nil {
		return rollback(fmt.Errorf("server %s protected service handoff did not verify: %w", name, err))
	}
	if stage == engine.UpgradeStageController {
		workerStatus, err := waitForTakodAgentVersionAtSocket(client, platform.DefaultWorkerSocket, Version, upgradeServersStatusWait, upgradeServersStatusPoll, upgradeServersStatusProbe)
		if err != nil || workerStatus.Version != Version || workerStatus.Identity == nil || workerStatus.Identity.ClusterID != server.ClusterID || workerStatus.Identity.NodeID != server.NodeID {
			if err == nil {
				err = fmt.Errorf("protected worker ingress returned the wrong version or immutable identity")
			}
			return rollback(fmt.Errorf("server %s protected worker ingress did not verify: %w", name, err))
		}
	}
	if err := setup.WriteVersionFile(client, setupVersionManifest(serverVersion)); err != nil {
		return rollback(setupVersionWriteError(name, err))
	}
	if err := prov.CommitTakodUpgrade(); err != nil {
		return rollback(fmt.Errorf("server %s node-upgrade commit failed: %w", name, err))
	}
	node.Outcome = engine.UpgradeOutcomeUpgraded
	node.ToVersion = verified.Version
	node.Protocol = verified.UpgradeProtocol
	fmt.Fprintf(out, "✓ Upgraded takod: %s -> %s\n", current, verified.Version)
	return node
}

func validateUpgradeServersOptions(cliVersion string, takodBinary string, dryRun bool) error {
	if dryRun {
		return nil
	}
	version := strings.TrimSpace(cliVersion)
	if cliVersionRequiresTakodBinary(version) && strings.TrimSpace(takodBinary) == "" {
		return fmt.Errorf("server upgrades from a development or non-release CLI build require --takod-binary with a Linux tako binary")
	}
	return nil
}

func validateUpgradeDoesNotDowngrade(targetVersion string, runningVersion string) error {
	target := canonicalUpgradeSemver(targetVersion)
	running := canonicalUpgradeSemver(runningVersion)
	if target == "" || running == "" {
		return nil
	}
	if semver.Compare(target, running) < 0 {
		return fmt.Errorf("refusing protected node downgrade from %s to %s; restore an older release only through the disaster-recovery workflow", runningVersion, targetVersion)
	}
	return nil
}

func canonicalUpgradeSemver(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return ""
	}
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	version = strings.TrimSuffix(version, "-dirty")
	parts := strings.Split(version, "-")
	for index := 1; index+1 < len(parts); index++ {
		if allASCIIBytes(parts[index], isASCIIDigit) && len(parts[index+1]) > 1 && parts[index+1][0] == 'g' && allASCIIBytes(parts[index+1][1:], isASCIIHex) {
			version = strings.Join(parts[:index], "-")
			break
		}
	}
	if !semver.IsValid(version) {
		return ""
	}
	return version
}

func validateUpgradeStatusIdentity(name string, server config.ServerConfig, status *takod.Status) error {
	if len(server.Roles) == 0 {
		return nil
	}
	if status == nil || status.Identity == nil || status.Identity.ClusterID != server.ClusterID || status.Identity.NodeID != server.NodeID {
		return fmt.Errorf("immutable identity does not match enrolled node %s", name)
	}
	if err := status.Identity.Validate(); err != nil || !slices.Contains(status.Capabilities, nodeidentity.Capability) {
		return fmt.Errorf("immutable identity for enrolled node %s is not a valid takod attestation", name)
	}
	if status.Membership == nil || status.MembershipGeneration == 0 || status.Membership.NodeID != server.NodeID {
		return fmt.Errorf("current membership attestation does not match enrolled node %s", name)
	}
	if err := nodeidentity.ValidateAllocationPublicKey(status.Membership.AllocationPublicKey); err != nil || status.Membership.AllocationPublicKey != status.Identity.AllocationPublicKey {
		return fmt.Errorf("current membership allocation identity does not match enrolled node %s", name)
	}
	if !equalUpgradeRoles(status.Membership.Roles, server.Roles) {
		return fmt.Errorf("current membership roles for %s are %v, expected %v", name, status.Membership.Roles, server.Roles)
	}
	if server.Lifecycle != "" && status.Membership.Lifecycle != server.Lifecycle {
		return fmt.Errorf("current membership lifecycle for %s is %s, expected %s", name, status.Membership.Lifecycle, server.Lifecycle)
	}
	return nil
}

func equalUpgradeRoles(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy := append([]string(nil), left...)
	rightCopy := append([]string(nil), right...)
	sort.Strings(leftCopy)
	sort.Strings(rightCopy)
	return slices.Equal(leftCopy, rightCopy)
}

func cliVersionRequiresTakodBinary(version string) bool {
	version = strings.TrimSpace(version)
	if version == "" || version == "dev" || version == "unknown" {
		return true
	}
	return isGitDescribeSnapshot(version)
}

func isGitDescribeSnapshot(version string) bool {
	if strings.Contains(version, "-dirty") {
		return true
	}
	parts := strings.Split(version, "-")
	if len(parts) < 3 {
		return false
	}
	for i := 1; i < len(parts)-1; i++ {
		if !allASCIIBytes(parts[i], isASCIIDigit) {
			continue
		}
		next := parts[i+1]
		if len(next) > 1 && next[0] == 'g' && allASCIIBytes(next[1:], isASCIIHex) {
			return true
		}
	}
	return false
}

func allASCIIBytes(value string, valid func(byte) bool) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if !valid(value[i]) {
			return false
		}
	}
	return true
}

func isASCIIDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

func isASCIIHex(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

func probeTakodAgentStatus(client takodclient.RequestExecutor, cfg *config.Config, timeout time.Duration) (*takod.Status, error) {
	return probeTakodAgentStatusAtSocket(client, takodSocketFromConfig(cfg), timeout)
}

func probeTakodAgentStatusAtSocket(client takodclient.RequestExecutor, socket string, timeout time.Duration) (*takod.Status, error) {
	output, err := takodclient.RequestJSONWithTimeout(client, socket, "GET", "/v1/status", nil, timeout)
	if err != nil {
		return nil, err
	}
	var status takod.Status
	if err := json.Unmarshal([]byte(output), &status); err != nil {
		return nil, fmt.Errorf("failed to parse takod status: %w", err)
	}
	return &status, nil
}

func waitForTakodAgentVersion(client takodclient.RequestExecutor, cfg *config.Config, wantVersion string, wait time.Duration, poll time.Duration, probe time.Duration) (*takod.Status, error) {
	return waitForTakodAgentVersionAtSocket(client, takodSocketFromConfig(cfg), wantVersion, wait, poll, probe)
}

func waitForTakodAgentVersionAtSocket(client takodclient.RequestExecutor, socket string, wantVersion string, wait time.Duration, poll time.Duration, probe time.Duration) (*takod.Status, error) {
	if wait <= 0 {
		wait = 30 * time.Second
	}
	if poll <= 0 {
		poll = time.Second
	}
	deadline := time.Now().Add(wait)
	var lastErr error
	var lastVersion string
	for {
		status, err := probeTakodAgentStatusAtSocket(client, socket, probe)
		if err == nil {
			lastVersion = status.Version
			if status.Version == wantVersion {
				return status, nil
			}
			lastErr = fmt.Errorf("reported version %s", status.Version)
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(poll)
	}
	if lastVersion != "" {
		return nil, fmt.Errorf("expected version %s, last %s", wantVersion, lastVersion)
	}
	return nil, fmt.Errorf("expected version %s, status unavailable: %w", wantVersion, lastErr)
}
