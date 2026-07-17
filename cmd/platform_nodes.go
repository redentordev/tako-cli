package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/mesh"
	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/platform"
	takossh "github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
)

var (
	platformMembershipStateDir           string
	platformMembershipIdentity           string
	platformMembershipInventory          string
	platformJoinNodeID                   string
	platformJoinTTL                      time.Duration
	platformEnrollTokenEnv               string
	platformEnrollNodeID                 string
	platformEnrollNodeName               string
	platformEnrollMeshIP                 string
	platformEnrollHost                   string
	platformEnrollPort                   int
	platformEnrollUser                   string
	platformEnrollSSHKey                 string
	platformEnrollPasswordEnv            string
	platformEnrollHostKeyFingerprint     string
	platformControllerMeshHost           string
	platformControllerSSHHost            string
	platformControllerSSHPort            int
	platformControllerSSHUser            string
	platformControllerHostKeyFingerprint string
	platformLifecycleNodeID              string
	platformCandidateClusterID           string
	platformCandidateNodeID              string
	platformCandidateNodeName            string
	platformCandidateIdentityPath        string
	platformCandidateSocket              string
	platformMeshExcludeNodeID            string
)

var platformJoinTokenCmd = &cobra.Command{Use: "join-token", Short: "Manage expiring worker join tokens"}
var platformNodeCmd = &cobra.Command{Use: "node", Short: "Manage platform node membership and lifecycle"}

var platformJoinTokenCreateCmd = &cobra.Command{
	Use: "create", Short: "Create a single-use join token bound to an expected node ID", SilenceUsage: true,
	RunE: runPlatformJoinTokenCreate,
}

var platformNodeListCmd = &cobra.Command{
	Use: "list", Short: "List controller-owned node membership", SilenceUsage: true,
	RunE: runPlatformNodeList,
}

var platformNodeEnrollCmd = &cobra.Command{
	Use: "enroll", Short: "Enroll a prepared server as an unschedulable worker", SilenceUsage: true,
	RunE: runPlatformNodeEnroll,
}

var platformNodeReadyCmd = lifecycleCommand("ready", "Mark a joining worker ready but unschedulable", func(store *platform.MembershipStore, nodeID string) error {
	_, err := store.MarkReady(nodeID)
	return err
})
var platformNodeSchedulableCmd = lifecycleCommand("schedulable", "Allow a ready or cordoned worker to receive new placements", func(store *platform.MembershipStore, nodeID string) error {
	_, err := store.MarkSchedulable(nodeID)
	return err
})
var platformNodeCordonCmd = lifecycleCommand("cordon", "Stop new placements on a node", func(store *platform.MembershipStore, nodeID string) error {
	_, err := store.Cordon(nodeID)
	return err
})
var platformNodeDrainCmd = lifecycleCommand("drain", "Begin explicit workload drain after cordon", func(store *platform.MembershipStore, nodeID string) error {
	_, err := store.BeginDrain(nodeID)
	return err
})
var platformNodeRemoveCmd = lifecycleCommand("remove", "Revoke and remove a drained node while retaining its tombstone", nil)

var platformWorkerPrepareEnrollmentCmd = &cobra.Command{
	Use: "prepare-enrollment", Hidden: true, SilenceUsage: true,
	RunE: runPlatformWorkerPrepareEnrollment,
}

var platformWorkerVerifyEnrollmentCmd = &cobra.Command{
	Use: "verify-enrollment", Hidden: true, SilenceUsage: true,
	RunE: runPlatformWorkerVerifyEnrollment,
}

var platformWorkerReconcileMeshCmd = &cobra.Command{
	Use: "reconcile-mesh", Hidden: true, SilenceUsage: true,
	RunE: runPlatformWorkerReconcileMesh,
}

func init() {
	platformCmd.AddCommand(platformJoinTokenCmd, platformNodeCmd)
	platformJoinTokenCmd.AddCommand(platformJoinTokenCreateCmd)
	platformNodeCmd.AddCommand(platformNodeListCmd, platformNodeEnrollCmd, platformNodeReadyCmd, platformNodeSchedulableCmd, platformNodeCordonCmd, platformNodeDrainCmd, platformNodeRemoveCmd)
	platformWorkerCmd.AddCommand(platformWorkerPrepareEnrollmentCmd, platformWorkerVerifyEnrollmentCmd, platformWorkerReconcileMeshCmd)

	for _, command := range []*cobra.Command{platformJoinTokenCreateCmd, platformNodeListCmd, platformNodeEnrollCmd, platformNodeReadyCmd, platformNodeSchedulableCmd, platformNodeCordonCmd, platformNodeDrainCmd, platformNodeRemoveCmd} {
		command.Flags().StringVar(&platformMembershipStateDir, "state-dir", platform.DefaultStateDir, "Protected controller platform state directory")
		command.Flags().StringVar(&platformMembershipIdentity, "identity-file", nodeidentity.DefaultPath, "Local immutable controller identity")
		command.Flags().StringVar(&platformMembershipInventory, "inventory-file", nodeidentity.DefaultInventoryPath, "Trusted cluster inventory snapshot")
		markHumanOnly(command)
	}
	platformJoinTokenCreateCmd.Flags().StringVar(&platformJoinNodeID, "node-id", "", "Expected immutable worker node UUID (required)")
	platformJoinTokenCreateCmd.Flags().DurationVar(&platformJoinTTL, "ttl", platform.DefaultJoinTokenTTL, "Token lifetime (1m to 24h)")
	_ = platformJoinTokenCreateCmd.MarkFlagRequired("node-id")

	platformNodeEnrollCmd.Flags().StringVar(&platformEnrollTokenEnv, "token-env", "TAKO_JOIN_TOKEN", "Environment variable containing the single-use join token")
	platformNodeEnrollCmd.Flags().StringVar(&platformEnrollNodeID, "node-id", "", "Expected immutable worker node UUID (required)")
	platformNodeEnrollCmd.Flags().StringVar(&platformEnrollNodeName, "node", "", "Logical worker name (required)")
	platformNodeEnrollCmd.Flags().StringVar(&platformEnrollMeshIP, "mesh-ip", "", "Worker address inside the platform mesh CIDR (required)")
	platformNodeEnrollCmd.Flags().StringVar(&platformEnrollHost, "host", "", "SSH host already pinned in known_hosts (required)")
	platformNodeEnrollCmd.Flags().IntVar(&platformEnrollPort, "port", 22, "SSH port")
	platformNodeEnrollCmd.Flags().StringVar(&platformEnrollUser, "user", "root", "SSH user")
	platformNodeEnrollCmd.Flags().StringVar(&platformEnrollSSHKey, "ssh-key", "", "SSH private key path")
	platformNodeEnrollCmd.Flags().StringVar(&platformEnrollPasswordEnv, "password-env", "TAKO_SSH_PASSWORD", "Environment variable containing the SSH password")
	platformNodeEnrollCmd.Flags().StringVar(&platformEnrollHostKeyFingerprint, "host-key-fingerprint", "", "Expected SHA256 SSH host-key fingerprint")
	platformNodeEnrollCmd.Flags().StringVar(&platformControllerMeshHost, "controller-mesh-host", "", "Worker-reachable node 1 WireGuard endpoint host (required)")
	platformNodeEnrollCmd.Flags().StringVar(&platformControllerSSHHost, "controller-host", "", "Worker-reachable node 1 SSH host already pinned in known_hosts (required)")
	platformNodeEnrollCmd.Flags().IntVar(&platformControllerSSHPort, "controller-port", 22, "Node 1 SSH port")
	platformNodeEnrollCmd.Flags().StringVar(&platformControllerSSHUser, "controller-user", "root", "Node 1 SSH user")
	platformNodeEnrollCmd.Flags().StringVar(&platformControllerHostKeyFingerprint, "controller-host-key-fingerprint", "", "Expected SHA256 node 1 SSH host-key fingerprint")
	for _, name := range []string{"node-id", "node", "mesh-ip", "host", "controller-mesh-host", "controller-host"} {
		_ = platformNodeEnrollCmd.MarkFlagRequired(name)
	}

	for _, command := range []*cobra.Command{platformNodeReadyCmd, platformNodeSchedulableCmd, platformNodeCordonCmd, platformNodeDrainCmd, platformNodeRemoveCmd} {
		command.Flags().StringVar(&platformLifecycleNodeID, "node-id", "", "Immutable node UUID (required)")
		command.Flags().StringVar(&platformEnrollSSHKey, "ssh-key", "", "SSH private key used to publish the lifecycle snapshot")
		command.Flags().StringVar(&platformEnrollPasswordEnv, "password-env", "TAKO_SSH_PASSWORD", "Environment variable containing the SSH password")
		command.Flags().StringVar(&platformCandidateSocket, "socket", takodclient.DefaultSocket, "Local controller takod Unix socket")
		_ = command.MarkFlagRequired("node-id")
	}

	for _, command := range []*cobra.Command{platformWorkerPrepareEnrollmentCmd, platformWorkerVerifyEnrollmentCmd, platformWorkerReconcileMeshCmd} {
		command.Flags().StringVar(&platformCandidateClusterID, "cluster-id", "", "Cluster UUID")
		command.Flags().StringVar(&platformCandidateNodeID, "node-id", "", "Node UUID")
		command.Flags().StringVar(&platformCandidateIdentityPath, "identity-file", nodeidentity.DefaultPath, "Installation identity path")
		_ = command.MarkFlagRequired("cluster-id")
		_ = command.MarkFlagRequired("node-id")
	}
	platformWorkerPrepareEnrollmentCmd.Flags().StringVar(&platformCandidateNodeName, "node", "", "Logical node name")
	_ = platformWorkerPrepareEnrollmentCmd.MarkFlagRequired("node")
	platformWorkerVerifyEnrollmentCmd.Flags().StringVar(&platformMembershipInventory, "inventory-file", nodeidentity.DefaultInventoryPath, "Trusted cluster inventory")
	platformWorkerVerifyEnrollmentCmd.Flags().StringVar(&platformCandidateSocket, "socket", takodclient.DefaultSocket, "Local takod Unix socket")
	platformWorkerReconcileMeshCmd.Flags().StringVar(&platformMembershipInventory, "inventory-file", nodeidentity.DefaultInventoryPath, "Trusted cluster inventory")
	platformWorkerReconcileMeshCmd.Flags().StringVar(&platformMeshExcludeNodeID, "exclude-node-id", "", "Pending-removal node UUID to exclude and verify absent")
}

func controllerMembership() (*platform.MembershipStore, *platform.MembershipState, error) {
	if err := validatePlatformMembershipInventoryPath(platformMembershipInventory); err != nil {
		return nil, nil, err
	}
	return platform.OpenControllerMembership(platformMembershipStateDir, platformMembershipIdentity, platformMembershipInventory)
}

func validatePlatformMembershipInventoryPath(path string) error {
	if filepath.Clean(path) != filepath.Clean(nodeidentity.DefaultInventoryPath) {
		return fmt.Errorf("platform membership commands require the takod inventory path %s", nodeidentity.DefaultInventoryPath)
	}
	return nil
}

func runPlatformJoinTokenCreate(_ *cobra.Command, _ []string) error {
	store, _, err := controllerMembership()
	if err != nil {
		return err
	}
	issued, err := store.CreateJoinToken(platformJoinNodeID, platformJoinTTL)
	if err != nil {
		return err
	}
	fmt.Fprintf(humanOut(), "Join token (shown once): %s\nCluster: %s\nExpected node: %s\nExpires: %s\n", issued.Token, issued.ClusterID, issued.ExpectedNodeID, issued.ExpiresAt.Format(time.RFC3339))
	return nil
}

func runPlatformNodeList(_ *cobra.Command, _ []string) error {
	_, state, err := controllerMembership()
	if err != nil {
		return err
	}
	fmt.Fprintf(humanOut(), "Cluster %s generation %d\n", state.ClusterID, state.Generation)
	for _, node := range state.Nodes {
		fmt.Fprintf(humanOut(), "%s\t%s\t%s\tschedulable=%t\troles=%s\n", node.NodeName, node.NodeID, node.Lifecycle, node.Schedulable, strings.Join(node.Roles, ","))
	}
	for _, tombstone := range state.Tombstones {
		fmt.Fprintf(humanOut(), "%s\t%s\tremoved\tgeneration=%d\n", tombstone.NodeName, tombstone.NodeID, tombstone.RemovedGeneration)
	}
	return nil
}

func runPlatformNodeEnroll(cmd *cobra.Command, _ []string) error {
	if err := nodeidentity.ValidateNodeID(platformEnrollNodeID); err != nil {
		return err
	}
	store, state, err := controllerMembership()
	if err != nil {
		return err
	}
	node, exists := state.ActiveNode(platformEnrollNodeID)
	var recorded takossh.RecordedHostKey
	controllerRecorded, err := takossh.LookupRecordedHostKey(platformControllerSSHHost, platformControllerSSHPort)
	if err != nil {
		return err
	}
	if controllerRecorded == nil {
		return fmt.Errorf("controller SSH host key is not pinned; add the verified key to known_hosts before enrollment")
	}
	if platformControllerHostKeyFingerprint != "" && controllerRecorded.Fingerprint != platformControllerHostKeyFingerprint {
		return fmt.Errorf("pinned controller SSH host key fingerprint %s does not match expected %s", controllerRecorded.Fingerprint, platformControllerHostKeyFingerprint)
	}
	reservation := ""
	if exists {
		recorded = takossh.RecordedHostKey{Type: node.SSHHostKeyType, Key: node.SSHHostKey, Fingerprint: node.SSHHostKeyFingerprint}
	} else {
		token := strings.TrimSpace(os.Getenv(strings.TrimSpace(platformEnrollTokenEnv)))
		if token == "" {
			return fmt.Errorf("join token environment variable %s is empty", platformEnrollTokenEnv)
		}
		reserved, err := store.ReserveJoinToken(token, platformEnrollNodeID)
		if err != nil {
			return fmt.Errorf("reserve authenticated join request before candidate mutation: %w", err)
		}
		reservation = reserved.Reservation
		known, err := takossh.LookupRecordedHostKey(platformEnrollHost, platformEnrollPort)
		if err != nil {
			return err
		}
		if known == nil {
			return fmt.Errorf("worker SSH host key is not pinned; run setup or add the verified key to known_hosts before enrollment")
		}
		recorded = *known
	}
	if platformEnrollHostKeyFingerprint != "" && recorded.Fingerprint != platformEnrollHostKeyFingerprint {
		return fmt.Errorf("pinned SSH host key fingerprint %s does not match expected %s", recorded.Fingerprint, platformEnrollHostKeyFingerprint)
	}
	client, err := takossh.NewClientFromConfigPinned(takossh.ServerConfig{Host: platformEnrollHost, Port: platformEnrollPort, User: platformEnrollUser, SSHKey: platformEnrollSSHKey, Password: platformSSHPassword()}, recorded)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := client.ConnectContext(cmd.Context()); err != nil {
		return fmt.Errorf("connect to pinned worker: %w", err)
	}

	prepare := fmt.Sprintf("sudo /usr/local/bin/tako platform worker prepare-enrollment --cluster-id %s --node-id %s --node %s", shellQuotePlatform(state.ClusterID), shellQuotePlatform(platformEnrollNodeID), shellQuotePlatform(platformEnrollNodeName))
	output, err := client.ExecuteWithContext(cmd.Context(), prepare)
	if err != nil {
		return fmt.Errorf("prepare immutable worker identity: %w, output: %s", err, strings.TrimSpace(output))
	}
	var identity platform.WorkerEnrollmentIdentity
	if err := decodeEnrollmentMarker(output, "TAKO_ENROLLMENT_IDENTITY=", &identity); err != nil {
		return err
	}
	if identity.ClusterID != state.ClusterID || identity.NodeID != strings.ToLower(platformEnrollNodeID) || identity.NodeName != platformEnrollNodeName {
		return fmt.Errorf("candidate returned mismatched immutable identity")
	}

	if exists {
		controller, controllerOK := state.ActiveNode(state.ControllerNodeID)
		if !controllerOK || controller.MeshEndpoint != platformControllerMeshHost || controller.SSHHost != platformControllerSSHHost || controller.SSHPort != platformControllerSSHPort || controller.SSHUser != platformControllerSSHUser || controller.SSHHostKeyType != controllerRecorded.Type || controller.SSHHostKey != controllerRecorded.Key || controller.SSHHostKeyFingerprint != controllerRecorded.Fingerprint || node.NodeName != platformEnrollNodeName || node.SSHHost != platformEnrollHost || node.SSHPort != platformEnrollPort || node.SSHUser != platformEnrollUser || node.SSHHostKeyType != recorded.Type || node.SSHHostKey != recorded.Key || node.SSHHostKeyFingerprint != recorded.Fingerprint || node.AllocationPublicKey != identity.AllocationPublicKey || node.MeshPublicKey != identity.MeshPublicKey || node.MeshIP != platformEnrollMeshIP || node.MeshEndpoint != platformEnrollHost {
			return fmt.Errorf("active joining node differs from requested enrollment; remove it through the lifecycle instead of replacing identity")
		}
		if node.Lifecycle != nodeidentity.NodeLifecycleJoining && node.Lifecycle != nodeidentity.NodeLifecycleReady {
			return fmt.Errorf("node enrollment can resume only while joining or ready")
		}
	} else {
		node, err = store.EnrollWorker(platform.EnrollWorkerRequest{
			Reservation: reservation, NodeID: platformEnrollNodeID, NodeName: platformEnrollNodeName, MeshIP: platformEnrollMeshIP, MeshEndpoint: platformEnrollHost,
			SSHHost: platformEnrollHost, SSHPort: platformEnrollPort, SSHHostKeyType: recorded.Type,
			SSHUser:    platformEnrollUser,
			SSHHostKey: recorded.Key, SSHHostKeyFingerprint: recorded.Fingerprint, AllocationPublicKey: identity.AllocationPublicKey,
			MeshPublicKey: identity.MeshPublicKey, ControllerMeshEndpoint: platformControllerMeshHost,
			ControllerSSHHost: platformControllerSSHHost, ControllerSSHPort: platformControllerSSHPort, ControllerSSHUser: platformControllerSSHUser,
			ControllerSSHHostKeyType: controllerRecorded.Type, ControllerSSHHostKey: controllerRecorded.Key, ControllerSSHHostKeyFingerprint: controllerRecorded.Fingerprint,
		})
		if err != nil {
			return err
		}
	}
	if err := publishInventoryToWorker(cmd, client, state.ClusterID, node.NodeID); err != nil {
		return err
	}
	if node.Lifecycle == nodeidentity.NodeLifecycleJoining {
		if _, err := store.MarkReady(node.NodeID); err != nil {
			return err
		}
	}
	current, err := store.Read()
	if err != nil {
		return err
	}
	if err := reconcilePlatformMesh(cmd.Context(), platformMembershipIdentity, platformMembershipInventory, ""); err != nil {
		return fmt.Errorf("worker membership is committed but controller mesh reconciliation failed; rerun enrollment: %w", err)
	}
	if err := publishInventoryToActiveWorkers(cmd, current); err != nil {
		return fmt.Errorf("worker membership is committed but inventory publication is incomplete; rerun enrollment: %w", err)
	}
	fmt.Fprintf(humanOut(), "Worker %s enrolled as ready and unschedulable\nPinned host key: %s\nRun 'tako platform node schedulable --node-id %s' after readiness review.\n", node.NodeName, recorded.Fingerprint, node.NodeID)
	return nil
}

func publishInventoryToWorker(cmd *cobra.Command, client *takossh.Client, clusterID, nodeID string) error {
	return publishInventoryToWorkerWithMeshExclusion(cmd, client, clusterID, nodeID, "")
}

func publishInventoryToWorkerWithMeshExclusion(cmd *cobra.Command, client *takossh.Client, clusterID, nodeID, excludeMeshNodeID string) error {
	file, err := os.Open(platformMembershipInventory)
	if err != nil {
		return fmt.Errorf("open trusted inventory: %w", err)
	}
	defer file.Close()
	remoteTemp, cleanup, err := client.UploadReaderPrivateTemp(cmd.Context(), file, 0600)
	if err != nil {
		return fmt.Errorf("upload trusted inventory: %w", err)
	}
	defer cleanup()
	install := platformWorkerInventoryInstallCommand(remoteTemp, clusterID, nodeID, excludeMeshNodeID)
	output, err := client.ExecuteWithContext(cmd.Context(), install)
	if err != nil {
		return fmt.Errorf("publish worker inventory: %w, output: %s", err, strings.TrimSpace(output))
	}
	verify := fmt.Sprintf("sudo /usr/local/bin/tako platform worker verify-enrollment --cluster-id %s --node-id %s", shellQuotePlatform(clusterID), shellQuotePlatform(nodeID))
	output, err = client.ExecuteWithContext(cmd.Context(), verify)
	if err != nil {
		return fmt.Errorf("attest worker enrollment: %w, output: %s", err, strings.TrimSpace(output))
	}
	var result platform.WorkerEnrollmentIdentity
	if err := decodeEnrollmentMarker(output, "TAKO_ENROLLMENT_VERIFIED=", &result); err != nil {
		return err
	}
	if result.ClusterID != clusterID || result.NodeID != nodeID {
		return fmt.Errorf("worker enrollment attestation returned mismatched identity")
	}
	return nil
}

func platformWorkerInventoryInstallCommand(remoteTemp, clusterID, nodeID, excludeMeshNodeID string) string {
	reconcile := fmt.Sprintf("sudo /usr/local/bin/tako platform worker reconcile-mesh --cluster-id %s --node-id %s", shellQuotePlatform(clusterID), shellQuotePlatform(nodeID))
	if excludeMeshNodeID != "" {
		reconcile += " --exclude-node-id " + shellQuotePlatform(excludeMeshNodeID)
	}
	return fmt.Sprintf("sudo install -d -m 0755 /etc/tako && sudo install -o root -g root -m 0644 %s /etc/tako/cluster-inventory.json && %s && sudo systemctl restart takod.service", shellQuotePlatform(remoteTemp), reconcile)
}

func decodeEnrollmentMarker(output, marker string, destination any) error {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), marker) {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(line), marker)), destination); err != nil {
				return fmt.Errorf("decode worker enrollment response: %w", err)
			}
			return nil
		}
	}
	return fmt.Errorf("worker enrollment response marker is missing")
}

func shellQuotePlatform(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func lifecycleCommand(use, short string, action func(*platform.MembershipStore, string) error) *cobra.Command {
	return &cobra.Command{Use: use, Short: short, SilenceUsage: true, RunE: func(cmd *cobra.Command, _ []string) error {
		store, state, err := controllerMembership()
		if err != nil {
			return err
		}
		node, active := state.ActiveNode(platformLifecycleNodeID)
		operation, removing := state.RemovalOperation(platformLifecycleNodeID)
		if use == "remove" && !active && !removing && state.IsTombstoned(platformLifecycleNodeID) {
			fmt.Fprintf(humanOut(), "Node %s is already removed\n", platformLifecycleNodeID)
			return nil
		}
		if !active && !(use == "remove" && removing) {
			return fmt.Errorf("node %s is not active and has no resumable removal", platformLifecycleNodeID)
		}
		if node == nil && operation != nil {
			node = &operation.Node
		}
		controllerAgent, err := takodclient.NewLocalAgentClient(platformCandidateSocket)
		if err != nil {
			return err
		}
		defer controllerAgent.CloseIdleConnections()
		controllerStatus, err := controllerAgent.Status(cmd.Context())
		if err != nil {
			return fmt.Errorf("controller takod must be ready before lifecycle mutation: %w", err)
		}
		if controllerStatus.Identity == nil || controllerStatus.Identity.ClusterID != state.ClusterID || controllerStatus.Identity.NodeID != state.ControllerNodeID {
			return fmt.Errorf("local takod is not the authoritative membership controller")
		}
		if !slices.Contains(controllerStatus.Capabilities, takod.CapabilityNodeMembershipV1) {
			return fmt.Errorf("local takod does not support authoritative membership reconciliation; upgrade the controller first")
		}
		if (use == "cordon" || use == "drain" || use == "remove") && active {
			if err := setLifecycleDeploymentDeny(cmd, state, node, true); err != nil {
				return fmt.Errorf("refusing lifecycle mutation because the target deny latch was not attested: %w", err)
			}
		}
		if use == "remove" {
			if err := resumeNodeRemoval(cmd, store, state, node); err != nil {
				return err
			}
		} else if err := action(store, platformLifecycleNodeID); err != nil {
			return err
		}
		reconcileOutput, err := controllerAgent.RequestJSON(cmd.Context(), "POST", "/v1/platform/membership/reconcile", nil)
		if err != nil {
			if stopErr := stopLocalProxyFailClosed(cmd); stopErr != nil {
				return fmt.Errorf("lifecycle committed but controller proxy revocation barrier failed: %v; emergency proxy stop also failed: %w", err, stopErr)
			}
			return fmt.Errorf("lifecycle committed and proxy was stopped fail-closed because controller reconciliation failed: %w", err)
		}
		var reconciled struct {
			Generation uint64 `json:"generation"`
		}
		if err := json.Unmarshal([]byte(reconcileOutput), &reconciled); err != nil || reconciled.Generation == 0 {
			return fmt.Errorf("controller returned invalid membership reconciliation evidence")
		}
		current, err := store.Read()
		if err != nil {
			return err
		}
		// Publish the changed lifecycle to its target before any unrelated peer.
		// The durable deny latch above already blocks stale direct mutations if
		// this command or controller crashes between commit and publication.
		if target, ok := current.ActiveNode(platformLifecycleNodeID); ok && target.NodeID != current.ControllerNodeID {
			if err := publishInventoryToMembershipNode(cmd, current, target, ""); err != nil {
				return fmt.Errorf("lifecycle committed with the target deny latch active, but target inventory attestation is incomplete; retry this command: %w", err)
			}
		}
		if err := reconcilePlatformMesh(cmd.Context(), platformMembershipIdentity, platformMembershipInventory, ""); err != nil {
			return fmt.Errorf("lifecycle committed but controller mesh reconciliation is incomplete; retry this command: %w", err)
		}
		if err := publishInventoryToActiveWorkersExcept(cmd, current, platformLifecycleNodeID); err != nil {
			return fmt.Errorf("lifecycle committed but inventory publication is incomplete; retry this command: %w", err)
		}
		if use == "schedulable" {
			if target, ok := current.ActiveNode(platformLifecycleNodeID); ok {
				if err := setLifecycleDeploymentDeny(cmd, current, target, false); err != nil {
					return fmt.Errorf("node is schedulable in membership but remains fail-closed until its deny latch can be cleared: %w", err)
				}
			}
		}
		if use == "remove" {
			if operation, pending := current.RemovalOperation(platformLifecycleNodeID); pending {
				client, err := connectLifecycleWorker(cmd, &operation.Node)
				if err != nil {
					return fmt.Errorf("membership removal is committed; retry target cleanup: %w", err)
				}
				if client != nil {
					defer client.Close()
					if err := revokeRemovedWorker(cmd, client); err != nil {
						return fmt.Errorf("membership removal is committed; retry target cleanup: %w", err)
					}
				}
				if err := store.CompleteRemoval(platformLifecycleNodeID); err != nil {
					return err
				}
			}
		}
		fmt.Fprintf(humanOut(), "Node %s transitioned via %s\n", platformLifecycleNodeID, use)
		return nil
	}}
}

func resumeNodeRemoval(cmd *cobra.Command, store *platform.MembershipStore, state *platform.MembershipState, node *platform.MembershipNode) error {
	operation, pending := state.RemovalOperation(platformLifecycleNodeID)
	if !pending {
		var err error
		operation, err = store.PrepareRemoval(platformLifecycleNodeID)
		if err != nil {
			return err
		}
		state, err = store.Read()
		if err != nil {
			return err
		}
	}
	if node == nil {
		node = &operation.Node
	}
	if operation.Phase == platform.RemovalPhaseRevokePeers {
		if err := revokeMeshPeerEverywhere(cmd, state, node); err != nil {
			return fmt.Errorf("removal is persisted and awaiting mesh peer revocation; retry this command: %w", err)
		}
		if _, err := store.MarkRemovalPeersRevoked(node.NodeID); err != nil {
			return err
		}
		operation.Phase = platform.RemovalPhaseRemoveMember
	}
	if operation.Phase == platform.RemovalPhaseRemoveMember {
		if _, err := store.Remove(node.NodeID); err != nil {
			return err
		}
	}
	return nil
}

func revokeMeshPeerEverywhere(cmd *cobra.Command, state *platform.MembershipState, removed *platform.MembershipNode) error {
	if removed == nil || removed.MeshPublicKey == "" {
		return fmt.Errorf("removed node has no bound mesh public key")
	}
	if err := reconcilePlatformMesh(cmd.Context(), platformMembershipIdentity, platformMembershipInventory, removed.NodeID); err != nil {
		return fmt.Errorf("durably revoke mesh peer on controller: %w", err)
	}
	for index := range state.Nodes {
		peer := &state.Nodes[index]
		if peer.NodeID == removed.NodeID {
			continue
		}
		if peer.NodeID == state.ControllerNodeID {
			continue
		}
		client, err := connectLifecycleWorker(cmd, peer)
		if err != nil {
			return fmt.Errorf("connect remaining peer %s: %w", peer.NodeID, err)
		}
		if client == nil {
			return fmt.Errorf("remaining peer %s has no provisioning transport", peer.NodeID)
		}
		executeErr := publishInventoryToWorkerWithMeshExclusion(cmd, client, state.ClusterID, peer.NodeID, removed.NodeID)
		_ = client.Close()
		if executeErr != nil {
			return fmt.Errorf("durably revoke mesh peer on %s: %w", peer.NodeID, executeErr)
		}
	}
	return nil
}

func publishInventoryToActiveWorkers(cmd *cobra.Command, state *platform.MembershipState) error {
	return publishInventoryToActiveWorkersExcept(cmd, state, "")
}

func publishInventoryToActiveWorkersExcept(cmd *cobra.Command, state *platform.MembershipState, excludedNodeID string) error {
	for index := range state.Nodes {
		node := &state.Nodes[index]
		if node.NodeID == state.ControllerNodeID || node.NodeID == excludedNodeID {
			continue
		}
		if err := publishInventoryToMembershipNode(cmd, state, node, ""); err != nil {
			return err
		}
	}
	return nil
}

func publishInventoryToMembershipNode(cmd *cobra.Command, state *platform.MembershipState, node *platform.MembershipNode, excludeMeshNodeID string) error {
	client, err := connectLifecycleWorker(cmd, node)
	if err != nil {
		return err
	}
	if client == nil {
		return fmt.Errorf("worker %s has no provisioning transport", node.NodeID)
	}
	defer client.Close()
	return publishInventoryToWorkerWithMeshExclusion(cmd, client, state.ClusterID, node.NodeID, excludeMeshNodeID)
}

func setLifecycleDeploymentDeny(cmd *cobra.Command, state *platform.MembershipState, node *platform.MembershipNode, denied bool) error {
	if node == nil {
		return fmt.Errorf("lifecycle target is required")
	}
	path := nodeidentity.DeploymentDenyPath(nodeidentity.DefaultInventoryPath)
	var command string
	if denied {
		command = fmt.Sprintf("sudo install -d -o root -g root -m 0755 %s && sudo sh -c %s && sudo test -f %s", shellQuotePlatform(filepath.Dir(path)), shellQuotePlatform("umask 077; : > "+path), shellQuotePlatform(path))
	} else {
		command = fmt.Sprintf("sudo rm -f -- %s && sudo test ! -e %s", shellQuotePlatform(path), shellQuotePlatform(path))
	}
	if node.NodeID == state.ControllerNodeID {
		output, err := exec.CommandContext(cmd.Context(), "sh", "-c", strings.ReplaceAll(command, "sudo ", "")).CombinedOutput()
		if err != nil {
			return fmt.Errorf("set local deployment deny latch: %w, output: %s", err, strings.TrimSpace(string(output)))
		}
		return nil
	}
	client, err := connectLifecycleWorker(cmd, node)
	if err != nil {
		return err
	}
	if client == nil {
		return fmt.Errorf("worker %s has no provisioning transport", node.NodeID)
	}
	defer client.Close()
	output, err := client.ExecuteWithContext(cmd.Context(), command)
	if err != nil {
		return fmt.Errorf("set deployment deny latch on %s: %w, output: %s", node.NodeID, err, strings.TrimSpace(output))
	}
	return nil
}

func stopLocalProxyFailClosed(cmd *cobra.Command) error {
	output, err := exec.CommandContext(cmd.Context(), "docker", "ps", "--filter", "name=^tako-proxy$", "--format", "{{.Names}}").CombinedOutput()
	if err != nil {
		return fmt.Errorf("inspect local proxy: %w, output: %s", err, strings.TrimSpace(string(output)))
	}
	if strings.TrimSpace(string(output)) == "tako-proxy" {
		output, err = exec.CommandContext(cmd.Context(), "docker", "rm", "-f", "tako-proxy").CombinedOutput()
		if err != nil {
			return fmt.Errorf("remove local proxy: %w, output: %s", err, strings.TrimSpace(string(output)))
		}
	}
	output, err = exec.CommandContext(cmd.Context(), "docker", "ps", "--filter", "name=^tako-proxy$", "--format", "{{.Names}}").CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) == "tako-proxy" {
		return fmt.Errorf("local proxy could not be verified stopped")
	}
	return nil
}

func connectLifecycleWorker(cmd *cobra.Command, node *platform.MembershipNode) (*takossh.Client, error) {
	if node == nil || node.SSHHost == "" {
		return nil, nil
	}
	recorded := takossh.RecordedHostKey{Type: node.SSHHostKeyType, Key: node.SSHHostKey, Fingerprint: node.SSHHostKeyFingerprint}
	client, err := takossh.NewClientFromConfigPinned(takossh.ServerConfig{Host: node.SSHHost, Port: node.SSHPort, User: node.SSHUser, SSHKey: platformEnrollSSHKey, Password: platformSSHPassword()}, recorded)
	if err != nil {
		return nil, err
	}
	if err := client.ConnectContext(cmd.Context()); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("connect to pinned worker before lifecycle mutation: %w", err)
	}
	return client, nil
}

func revokeRemovedWorker(cmd *cobra.Command, client *takossh.Client) error {
	file, err := os.Open(platformMembershipInventory)
	if err != nil {
		return err
	}
	defer file.Close()
	remoteTemp, cleanupTemp, err := client.UploadReaderPrivateTemp(cmd.Context(), file, 0600)
	if err != nil {
		return err
	}
	defer cleanupTemp()
	cleanup := fmt.Sprintf("sudo install -o root -g root -m 0644 %s /etc/tako/cluster-inventory.json && sudo systemctl stop takod.service && sudo sh -c 'for f in /etc/wireguard/tako*.conf; do test -e \"$f\" || continue; wg-quick down \"$f\" >/dev/null 2>&1 || true; rm -f -- \"$f\"; done; rm -rf -- /etc/tako/wireguard'", shellQuotePlatform(remoteTemp))
	output, err := client.ExecuteWithContext(cmd.Context(), cleanup)
	if err != nil {
		return fmt.Errorf("revoke remote mesh identity and stop takod: %w, output: %s", err, strings.TrimSpace(output))
	}
	return nil
}

func platformSSHPassword() string {
	name := strings.TrimSpace(platformEnrollPasswordEnv)
	if name == "" {
	}
	return os.Getenv(name)
}

func runPlatformWorkerPrepareEnrollment(_ *cobra.Command, _ []string) error {
	if !platform.RunningAsRoot() {
		return fmt.Errorf("worker enrollment identity must be prepared as root")
	}
	identity, _, err := platform.PrepareWorkerIdentity(platformCandidateIdentityPath, platformCandidateClusterID, platformCandidateNodeID, platformCandidateNodeName, time.Now())
	if err != nil {
		return err
	}
	meshPublicKey, err := platform.EnsureMeshPublicKey(platform.DefaultPlatformMeshKeyDir)
	if err != nil {
		return err
	}
	identity.MeshPublicKey = meshPublicKey
	if err := nodeidentity.WriteLocalBinding(nodeidentity.DefaultLocalBindingPath, nodeidentity.LocalBinding{
		APIVersion: nodeidentity.InventoryAPIVersion, Kind: nodeidentity.LocalBindingKind,
		ClusterID: identity.ClusterID, NodeID: identity.NodeID, NodeName: identity.NodeName,
	}); err != nil {
		return fmt.Errorf("publish worker local node binding: %w", err)
	}
	data, _ := json.Marshal(identity)
	fmt.Fprintf(humanOut(), "TAKO_ENROLLMENT_IDENTITY=%s\n", data)
	return nil
}

func runPlatformWorkerReconcileMesh(cmd *cobra.Command, _ []string) error {
	if err := validatePlatformMembershipInventoryPath(platformMembershipInventory); err != nil {
		return err
	}
	if !platform.RunningAsRoot() {
		return fmt.Errorf("platform mesh reconciliation must run as root")
	}
	installation, err := nodeidentity.Read(platformCandidateIdentityPath)
	if err != nil {
		return err
	}
	if !installation.Matches(platformCandidateClusterID, platformCandidateNodeID) {
		return fmt.Errorf("local immutable identity does not match requested mesh member")
	}
	return reconcilePlatformMesh(cmd.Context(), platformCandidateIdentityPath, platformMembershipInventory, platformMeshExcludeNodeID)
}

func reconcilePlatformMesh(ctx context.Context, identityPath, inventoryPath, excludeNodeID string) error {
	installation, err := nodeidentity.Read(identityPath)
	if err != nil {
		return err
	}
	inventory, err := nodeidentity.ReadInventory(inventoryPath)
	if err != nil {
		return err
	}
	node, peers, excludedPublicKey, err := platformMeshTopology(installation, inventory, excludeNodeID)
	if err != nil {
		return err
	}
	_, err = mesh.ApplyLocal(ctx, node, peers, mesh.WireGuardConfig{Enabled: true, Interface: "tako", ListenPort: 51820, NATTraversal: true}, false)
	if err != nil || excludedPublicKey == "" {
		return err
	}
	configData, err := os.ReadFile("/etc/wireguard/tako.conf")
	if err != nil || strings.Contains(string(configData), excludedPublicKey) {
		return fmt.Errorf("durable WireGuard config still contains revoked peer")
	}
	output, err := exec.CommandContext(ctx, "wg", "show", "tako", "peers").CombinedOutput()
	if err != nil || strings.Contains(string(output), excludedPublicKey) {
		return fmt.Errorf("live WireGuard state still contains revoked peer")
	}
	return nil
}

func platformMeshTopology(installation *nodeidentity.Installation, inventory *nodeidentity.ClusterInventory, excludeNodeID string) (mesh.Node, []mesh.Node, string, error) {
	if installation == nil || inventory == nil {
		return mesh.Node{}, nil, "", fmt.Errorf("platform mesh identity and inventory are required")
	}
	local, ok := inventory.Node(installation.NodeID)
	if inventory.ClusterID != installation.ClusterID || !ok || local.MeshCredentialStatus != nodeidentity.MeshCredentialActive {
		return mesh.Node{}, nil, "", fmt.Errorf("local node is absent or revoked in platform mesh inventory")
	}
	excludeNodeID = strings.ToLower(strings.TrimSpace(excludeNodeID))
	excludedPublicKey := ""
	if excludeNodeID != "" {
		if excludeNodeID == local.NodeID {
			return mesh.Node{}, nil, "", fmt.Errorf("cannot reconcile a node while excluding its own identity")
		}
		excluded, exists := inventory.Node(excludeNodeID)
		if !exists {
			return mesh.Node{}, nil, "", fmt.Errorf("excluded mesh node is absent from inventory")
		}
		excludedPublicKey = excluded.MeshPublicKey
	}
	node := mesh.Node{Name: local.NodeName, Host: local.MeshEndpoint, Address: local.MeshIP, PublicKey: local.MeshPublicKey}
	peers := make([]mesh.Node, 0, len(inventory.Nodes)-1)
	for _, peer := range inventory.Nodes {
		if peer.NodeID == local.NodeID || peer.NodeID == excludeNodeID {
			continue
		}
		peers = append(peers, mesh.Node{Name: peer.NodeName, Host: peer.MeshEndpoint, Address: peer.MeshIP, PublicKey: peer.MeshPublicKey})
	}
	return node, peers, excludedPublicKey, nil
}

func runPlatformWorkerVerifyEnrollment(cmd *cobra.Command, _ []string) error {
	if err := validatePlatformMembershipInventoryPath(platformMembershipInventory); err != nil {
		return err
	}
	if !platform.RunningAsRoot() {
		return fmt.Errorf("worker enrollment verification must run as root")
	}
	installation, err := nodeidentity.Read(platformCandidateIdentityPath)
	if err != nil {
		return err
	}
	if !installation.Matches(platformCandidateClusterID, platformCandidateNodeID) {
		return fmt.Errorf("worker immutable identity does not match enrollment")
	}
	inventory, err := nodeidentity.ReadInventory(platformMembershipInventory)
	if err != nil {
		return err
	}
	node, ok := inventory.Node(installation.NodeID)
	if inventory.ClusterID != installation.ClusterID || !ok || node.AllocationPublicKey != installation.AllocationPublicKey || node.MeshCredentialStatus != nodeidentity.MeshCredentialActive {
		return fmt.Errorf("worker is absent or revoked in trusted controller inventory")
	}
	meshPublicKey, err := platform.EnsureMeshPublicKey(platform.DefaultPlatformMeshKeyDir)
	if err != nil || meshPublicKey != node.MeshPublicKey {
		return fmt.Errorf("worker mesh private key does not match controller membership")
	}
	agent, err := takodclient.NewLocalAgentClient(platformCandidateSocket)
	if err != nil {
		return err
	}
	defer agent.CloseIdleConnections()
	status, err := agent.Status(cmd.Context())
	if err != nil {
		return fmt.Errorf("worker takod is not ready: %w", err)
	}
	if status.Identity == nil || status.Identity.ClusterID != installation.ClusterID || status.Identity.NodeID != installation.NodeID {
		return fmt.Errorf("worker takod reported mismatched immutable identity")
	}
	if status.Membership == nil || status.Membership.NodeID != installation.NodeID || status.Membership.Lifecycle != node.Lifecycle || status.MembershipGeneration != inventory.Generation || !slices.Contains(status.Capabilities, takod.CapabilityNodeLifecycleV1) {
		return fmt.Errorf("worker takod did not attest the published lifecycle generation")
	}
	identity := platform.WorkerEnrollmentIdentity{ClusterID: installation.ClusterID, NodeID: installation.NodeID, NodeName: installation.NodeName, AllocationPublicKey: installation.AllocationPublicKey, MeshPublicKey: node.MeshPublicKey}
	data, _ := json.Marshal(identity)
	fmt.Fprintf(humanOut(), "TAKO_ENROLLMENT_VERIFIED=%s\n", data)
	return nil
}
