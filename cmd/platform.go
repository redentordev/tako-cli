package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/redentordev/tako-cli/pkg/platform"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
)

var (
	platformNodeName       string
	platformClusterID      string
	platformNodeID         string
	platformBinaryPath     string
	platformWorkerUser     string
	platformWorkerGroup    string
	platformMinFreeDisk    int64
	platformReservedDisk   int64
	platformReservedMemory int64
	platformBuildLimit     int
	platformOperationLimit int
	platformWorkerStateDir string
	platformWorkerConfig   string
	platformWorkerSocket   string
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
	platformCmd.AddCommand(platformInitCmd, platformWorkerCmd)
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

	platformWorkerRunCmd.Flags().StringVar(&platformWorkerStateDir, "state-dir", platform.DefaultStateDir, "Durable platform worker state directory")
	platformWorkerRunCmd.Flags().StringVar(&platformWorkerConfig, "config", filepath.Join(platform.DefaultConfigDir, "platform.json"), "Protected platform configuration")
	platformWorkerRunCmd.Flags().StringVar(&platformWorkerSocket, "socket", platform.DefaultSocket, "Local takod Unix socket")
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
