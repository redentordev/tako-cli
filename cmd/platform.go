package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/platform"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
)

var (
	platformNodeName          string
	platformClusterID         string
	platformNodeID            string
	platformBinaryPath        string
	platformWorkerUser        string
	platformWorkerGroup       string
	platformMinFreeDisk       int64
	platformReservedDisk      int64
	platformReservedMemory    int64
	platformBuildLimit        int
	platformOperationLimit    int
	platformWorkerStateDir    string
	platformWorkerConfig      string
	platformWorkerSocket      string
	platformInspectIdentity   string
	platformInspectBinding    string
	platformInspectInventory  string
	platformInspectConfig     string
	platformInspectMembership string
	platformInspectWorkerUnit string
	platformInspectMeshKeyDir string
)

var platformCmd = &cobra.Command{
	Use:   "platform",
	Short: "Manage the local-first Tako platform foundation",
}

var platformInitCmd = &cobra.Command{
	Use:          "init",
	Short:        "Initialize this server as the first Tako platform node",
	SilenceUsage: true,
	RunE:         runPlatformInit,
}

var platformInspectCmd = &cobra.Command{
	Use:          "inspect",
	Short:        "Explain this server's existing platform enrollment and control authority",
	SilenceUsage: true,
	RunE:         runPlatformInspect,
}

var platformWorkerCmd = &cobra.Command{
	Use:    "worker",
	Short:  "Run internal platform worker commands",
	Hidden: true,
}

var platformWorkerRunCmd = &cobra.Command{
	Use:          "run",
	Short:        "Run the durable single-controller deployment worker",
	Hidden:       true,
	SilenceUsage: true,
	RunE:         runPlatformWorker,
}

func init() {
	rootCmd.AddCommand(platformCmd)
	platformCmd.AddCommand(platformInitCmd, platformInspectCmd, platformWorkerCmd)
	platformWorkerCmd.AddCommand(platformWorkerRunCmd)
	markHumanOnly(platformInitCmd)

	platformInitCmd.Flags().StringVar(&platformNodeName, "node", "", "Logical name for the first node (default: node-1; existing name on resume)")
	platformInitCmd.Flags().StringVar(&platformClusterID, "cluster-id", "", "Existing cluster UUID (generated when omitted)")
	platformInitCmd.Flags().StringVar(&platformNodeID, "node-id", "", "First-node UUID (generated when omitted)")
	platformInitCmd.Flags().StringVar(&platformBinaryPath, "binary", "", "Source Tako binary staged into a protected root-owned service path (default: current executable)")
	platformInitCmd.Flags().StringVar(&platformWorkerUser, "worker-user", platform.DefaultWorkerUser, "Dedicated platform worker system user")
	platformInitCmd.Flags().StringVar(&platformWorkerGroup, "worker-group", platform.DefaultWorkerGroup, "Dedicated platform worker system group")
	policy := platform.DefaultResourcePolicy()
	platformInitCmd.Flags().Int64Var(&platformMinFreeDisk, "minimum-free-disk-bytes", policy.MinimumFreeDiskBytes, "Reject platform operations below this free-disk floor")
	platformInitCmd.Flags().Int64Var(&platformReservedDisk, "reserved-disk-bytes", policy.ReservedDiskBytes, "Disk reserved for controller and management state")
	platformInitCmd.Flags().Int64Var(&platformReservedMemory, "reserved-memory-bytes", policy.ReservedMemoryBytes, "Memory reserved for controller and management services")
	platformInitCmd.Flags().IntVar(&platformBuildLimit, "max-concurrent-builds", policy.MaximumConcurrentBuilds, "Maximum concurrent image builds")
	platformInitCmd.Flags().IntVar(&platformOperationLimit, "max-concurrent-operations", policy.MaximumConcurrentOps, "Maximum concurrent platform operations")

	platformInspectCmd.Flags().StringVar(&platformInspectIdentity, "identity-file", nodeidentity.DefaultPath, "Protected create-once installation identity")
	platformInspectCmd.Flags().StringVar(&platformInspectBinding, "local-binding", nodeidentity.DefaultLocalBindingPath, "Root-published public local-node binding")
	platformInspectCmd.Flags().StringVar(&platformInspectInventory, "inventory", nodeidentity.DefaultInventoryPath, "Root-published trusted cluster inventory")
	platformInspectCmd.Flags().StringVar(&platformInspectConfig, "platform-config", platform.DefaultPlatformConfigPath, "Protected controller platform configuration")
	platformInspectCmd.Flags().StringVar(&platformInspectMembership, "membership", platform.DefaultMembershipPath(platform.DefaultStateDir), "Protected controller membership state")
	platformInspectCmd.Flags().StringVar(&platformInspectWorkerUnit, "worker-unit", platform.DefaultWorkerUnitPath, "Installed controller worker service unit")
	platformInspectCmd.Flags().StringVar(&platformInspectMeshKeyDir, "mesh-key-dir", platform.DefaultPlatformMeshKeyDir, "Protected platform WireGuard identity directory")

	platformWorkerRunCmd.Flags().StringVar(&platformWorkerStateDir, "state-dir", platform.DefaultStateDir, "Durable platform worker state directory")
	platformWorkerRunCmd.Flags().StringVar(&platformWorkerConfig, "config", filepath.Join(platform.DefaultConfigDir, "platform.json"), "Protected platform configuration")
	platformWorkerRunCmd.Flags().StringVar(&platformWorkerSocket, "socket", platform.DefaultSocket, "Local takod Unix socket")
}

func runPlatformInspect(_ *cobra.Command, _ []string) error {
	inspection, err := platform.InspectEnrollment(platform.EnrollmentInspectionPaths{
		IdentityPath: platformInspectIdentity, LocalBindingPath: platformInspectBinding, InventoryPath: platformInspectInventory,
		ConfigPath: platformInspectConfig, MembershipPath: platformInspectMembership, WorkerUnitPath: platformInspectWorkerUnit, MeshKeyDir: platformInspectMeshKeyDir,
	})
	if err != nil {
		return fmt.Errorf("inspect platform enrollment: %w", err)
	}
	return deliverPlatformInspection(inspection)
}

func deliverPlatformInspection(inspection *platform.EnrollmentInspection) error {
	if inspection == nil {
		return fmt.Errorf("platform inspection returned no result")
	}
	if machineOutputEnabled() {
		if err := emitResultDocument(inspection); err != nil {
			return err
		}
	} else if err := renderPlatformInspection(inspection); err != nil {
		return err
	}
	if inspection.Status == platform.EnrollmentIncomplete {
		return &engine.AttentionError{Err: fmt.Errorf("platform enrollment is incomplete; follow nextAction %q before deploying or initializing", inspection.NextAction)}
	}
	return nil
}

func renderPlatformInspection(inspection *platform.EnrollmentInspection) error {
	if inspection == nil {
		return fmt.Errorf("platform inspection returned no result")
	}
	out := humanOut()
	switch inspection.Status {
	case platform.EnrollmentNotEnrolled:
		fmt.Fprintln(out, "No Tako platform enrollment detected.")
		fmt.Fprintln(out, "No protected identity, trusted inventory, controller config, membership, or worker-service artifacts were found.")
		fmt.Fprintf(out, "Next action: %s\nCommand: %s\n", inspection.NextAction, inspection.NextCommand)
		return nil
	case platform.EnrollmentIncomplete:
		fmt.Fprintln(out, "Incomplete Tako platform enrollment detected; no changes were made.")
		if inspection.ClusterID != "" || inspection.NodeID != "" {
			fmt.Fprintf(out, "Cluster: %s\nNode: %s (%s)\n", inspection.ClusterID, inspection.NodeName, inspection.NodeID)
		}
		fmt.Fprintf(out, "Problem: %s\n", inspection.Detail)
		fmt.Fprintf(out, "Next action: %s\n", inspection.NextAction)
		if inspection.NextCommand != "" {
			fmt.Fprintf(out, "Safe resume command: %s\n", inspection.NextCommand)
		} else {
			fmt.Fprintln(out, "Do not initialize over this state. Recover it from the existing controller or backup before removing protected artifacts.")
		}
		for _, artifact := range inspection.ResidualArtifacts {
			fmt.Fprintf(out, "Residual artifact: %s\n", artifact)
		}
		return nil
	case platform.EnrollmentEnrolled:
	default:
		return fmt.Errorf("platform inspection returned unsupported status %q", inspection.Status)
	}

	controller := inspection.PublishedControllerID
	if inspection.PublishedControllerName != "" {
		controller = fmt.Sprintf("%s (%s)", inspection.PublishedControllerName, inspection.PublishedControllerID)
	}
	fmt.Fprintln(out, "Existing Tako platform enrollment detected; no changes were made.")
	fmt.Fprintf(out, "Cluster: %s\nNode: %s (%s)\n", inspection.ClusterID, inspection.NodeName, inspection.NodeID)
	fmt.Fprintf(out, "Protected identity verification: %s\n", inspection.IdentityVerification)
	fmt.Fprintf(out, "Published roles: %s\nPublished lifecycle: %s\nPublished schedulable: %t\n", strings.Join(inspection.PublishedRoles, ","), inspection.PublishedLifecycle, inspection.PublishedSchedulable)
	updatedAt := "unknown"
	if inspection.InventoryUpdatedAt != nil {
		updatedAt = inspection.InventoryUpdatedAt.Format(time.RFC3339)
	}
	fmt.Fprintf(out, "Last trusted inventory: generation %d, updated %s\n", inspection.InventoryGeneration, updatedAt)
	fmt.Fprintf(out, "Last published controller: %s\n", controller)
	if inspection.LocalController {
		fmt.Fprintln(out, "Published authority: this node is the cluster's single-writer controller in the last trusted snapshot.")
		fmt.Fprintf(out, "Protected membership comparison: %s\n", inspection.MembershipComparison)
	} else {
		fmt.Fprintln(out, "Published authority: remote. Running Tako or a project here does not promote this node or let it authorize cluster mutations.")
		fmt.Fprintln(out, "Deploy, scale, and administrative mutations require the published controller to be reachable and still authoritative.")
	}
	if inspection.PublishedSchedulable {
		fmt.Fprintln(out, "Projects may run on this node under the existing cluster's placement and fencing rules.")
	} else {
		fmt.Fprintln(out, "This node cannot receive new workload placement in its current lifecycle state.")
	}
	for _, warning := range inspection.Warnings {
		fmt.Fprintf(out, "Warning: %s\n", warning)
	}
	fmt.Fprintln(out, "For an independent cluster, use another server or VM; Tako will not replace this node's identity automatically.")
	return nil
}

func runPlatformInit(cmd *cobra.Command, _ []string) error {
	binary := platformBinaryPath
	if binary == "" {
		current, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolve current Tako binary: %w", err)
		}
		binary, err = filepath.Abs(current)
		if err != nil {
			return err
		}
	}
	bootstrapper, err := platform.NewBootstrapper(platform.LocalHost{})
	if err != nil {
		return err
	}
	result, err := bootstrapper.Bootstrap(cmd.Context(), platform.BootstrapConfig{
		NodeName: platformNodeName, ClusterID: platformClusterID, NodeID: platformNodeID,
		BinaryPath: binary, WorkerUser: platformWorkerUser, WorkerGroup: platformWorkerGroup,
		WorkerUserExplicit: cmd.Flags().Changed("worker-user"), WorkerGroupExplicit: cmd.Flags().Changed("worker-group"),
		PolicyExplicit: cmd.Flags().Changed("minimum-free-disk-bytes") || cmd.Flags().Changed("reserved-disk-bytes") || cmd.Flags().Changed("reserved-memory-bytes") || cmd.Flags().Changed("max-concurrent-builds") || cmd.Flags().Changed("max-concurrent-operations"),
		RequireRoot:    true,
		Policy: platform.ResourcePolicy{
			APIVersion: platform.APIVersion, Kind: platform.PolicyKind,
			ReservedMemoryBytes: platformReservedMemory, ReservedDiskBytes: platformReservedDisk,
			MinimumFreeDiskBytes: platformMinFreeDisk, MaximumConcurrentBuilds: platformBuildLimit,
			MaximumConcurrentOps: platformOperationLimit,
		},
	})
	if err != nil {
		return err
	}
	verb := "initialized"
	if result.Resumed {
		verb = "verified"
	}
	fmt.Fprintf(humanOut(), "Tako platform %s on %s\n", verb, result.State.NodeName)
	fmt.Fprintf(humanOut(), "Cluster: %s\nNode: %s\nRoles: %v\nController: %s\n", result.State.ClusterID, result.State.NodeID, result.State.EnrollmentRoles, result.State.ControllerMode)
	fmt.Fprintf(humanOut(), "\nAdd this enrolled node to tako.yaml (replace host/user as needed):\n")
	fmt.Fprintf(humanOut(), "servers:\n  %s:\n    host: <NODE_IP_OR_HOSTNAME>\n    user: root\n    transport: auto\n    clusterId: %s\n    nodeId: %s\n    workerUid: %d\n", result.State.NodeName, result.State.ClusterID, result.State.NodeID, result.State.WorkerUID)
	fmt.Fprintf(humanOut(), "\ntransport: auto uses the protected local worker ingress only when this process is running on that exact enrolled node; otherwise it uses identity-verified SSH.\n")
	return nil
}

func runPlatformWorker(cmd *cobra.Command, _ []string) error {
	agent, err := takodclient.NewLocalAgentClient(platformWorkerSocket)
	if err != nil {
		return err
	}
	defer agent.CloseIdleConnections()
	worker, err := platform.NewWorker(platformWorkerConfig, platformWorkerStateDir, platformWorkerSocket, agent, platform.OSDiskProbe{})
	if err != nil {
		return err
	}
	err = worker.Run(cmd.Context())
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}
