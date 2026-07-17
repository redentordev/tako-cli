package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/fileutil"
	"github.com/redentordev/tako-cli/pkg/scheduler"
	"github.com/spf13/cobra"
)

const maxPlacementPlanBytes = 4 << 20

var (
	placementPlanNode string
	placementPlanFile string
	placementVerifyID string
)

var placementCmd = &cobra.Command{Use: "placement", Short: "Plan explicit workload placement changes"}
var placementPlanCmd = &cobra.Command{Use: "plan", Short: "Create digest-bound placement movement plans"}

var placementPlanCordonCmd = &cobra.Command{
	Use: "cordon", Short: "Plan the replica impact of excluding one node from new placement", SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlacementPlan(cmd, scheduler.MovementModeCordon)
	},
}

var placementPlanDrainCmd = &cobra.Command{
	Use: "drain", Short: "Plan moving stateless replicas off one node", SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlacementPlan(cmd, scheduler.MovementModeDrain)
	},
}

var placementPlanRebalanceCmd = &cobra.Command{
	Use: "rebalance", Short: "Plan a conservative stateless replica rebalance", SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlacementPlan(cmd, scheduler.MovementModeRebalance)
	},
}

var placementVerifyCmd = &cobra.Command{
	Use: "verify PLAN", Short: "Verify a placement plan digest after review", Args: cobra.ExactArgs(1), SilenceUsage: true,
	RunE: runPlacementVerify,
}

func init() {
	rootCmd.AddCommand(placementCmd)
	placementCmd.AddCommand(placementPlanCmd, placementVerifyCmd)
	placementPlanCmd.AddCommand(placementPlanCordonCmd, placementPlanDrainCmd, placementPlanRebalanceCmd)
	placementPlanCordonCmd.Flags().StringVar(&placementPlanNode, "node", "", "Logical node name or immutable node UUID (required)")
	placementPlanCordonCmd.Flags().StringVar(&placementPlanFile, "file", "", "Write the reviewable plan JSON atomically")
	placementPlanDrainCmd.Flags().StringVar(&placementPlanNode, "node", "", "Logical node name or immutable node UUID (required)")
	placementPlanDrainCmd.Flags().StringVar(&placementPlanFile, "file", "", "Write the reviewable plan JSON atomically")
	placementPlanRebalanceCmd.Flags().StringVar(&placementPlanFile, "file", "", "Write the reviewable plan JSON atomically")
	placementVerifyCmd.Flags().StringVar(&placementVerifyID, "plan-id", "", "Exact plan digest recorded during review (required)")
	_ = placementPlanDrainCmd.MarkFlagRequired("node")
	_ = placementPlanCordonCmd.MarkFlagRequired("node")
	_ = placementVerifyCmd.MarkFlagRequired("plan-id")
}

func runPlacementPlan(cmd *cobra.Command, mode string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	plan, err := cliEngine().PlanPlacementMovement(cmd.Context(), engine.PlacementPlanRequest{
		Config: cfg, Environment: getEnvironmentName(cfg), Mode: mode, TargetNode: placementPlanNode,
	})
	if err != nil {
		return err
	}
	if placementPlanFile != "" {
		data, err := json.MarshalIndent(plan, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to encode placement plan: %w", err)
		}
		if err := fileutil.WriteFileAtomic(placementPlanFile, append(data, '\n'), 0600); err != nil {
			return fmt.Errorf("failed to write placement plan %s: %w", placementPlanFile, err)
		}
	}
	if !machineOutputEnabled() {
		printPlacementPlan(cmd, plan)
	}
	return emitResultDocument(plan)
}

func runPlacementVerify(cmd *cobra.Command, args []string) error {
	plan, err := readPlacementPlan(args[0])
	if err != nil {
		return &engine.InvalidRequestError{Err: err}
	}
	if err := scheduler.ValidateMovementPlan(*plan); err != nil {
		return &engine.InvalidRequestError{Err: err}
	}
	if plan.PlanID != strings.TrimSpace(placementVerifyID) {
		return &engine.InvalidRequestError{Err: fmt.Errorf("placement plan ID %s does not match reviewed ID %s", plan.PlanID, strings.TrimSpace(placementVerifyID))}
	}
	if !machineOutputEnabled() {
		fmt.Fprintf(cmd.OutOrStdout(), "Placement plan %s is intact and still requires operator review against desired revision %s.\n", plan.PlanID, plan.InputRevisionID)
	}
	return emitResultDocument(plan)
}

func readPlacementPlan(path string) (*scheduler.MovementPlan, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open placement plan: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxPlacementPlanBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read placement plan: %w", err)
	}
	if len(data) > maxPlacementPlanBytes {
		return nil, fmt.Errorf("placement plan exceeds %d bytes", maxPlacementPlanBytes)
	}
	var plan scheduler.MovementPlan
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return nil, fmt.Errorf("invalid placement plan: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("invalid placement plan: multiple JSON documents")
		}
		return nil, fmt.Errorf("invalid placement plan trailing content: %w", err)
	}
	return &plan, nil
}

func printPlacementPlan(cmd *cobra.Command, plan *scheduler.MovementPlan) {
	fmt.Fprintf(cmd.OutOrStdout(), "Placement %s plan %s\n", plan.Mode, plan.PlanID)
	fmt.Fprintf(cmd.OutOrStdout(), "Desired revision: %s\nMoves: %d  Blockers: %d  Executable: %t\n", plan.InputRevisionID, len(plan.Moves), len(plan.Blockers), plan.Executable)
	for _, impact := range plan.Impacts {
		fmt.Fprintf(cmd.OutOrStdout(), "- %s slot %d remains on %s; future assignments exclude the cordoned node\n", impact.Service, impact.Slot, impact.Node)
	}
	for _, move := range plan.Moves {
		if move.RequiresVolumeMigration {
			fmt.Fprintf(cmd.OutOrStdout(), "- %s slot %d: %s -> blocked (persistent volumes: %s)\n", move.Service, move.Slot, move.FromNode, strings.Join(move.Volumes, ", "))
			continue
		}
		fmt.Fprintf(cmd.OutOrStdout(), "- %s slot %d: %s -> %s\n", move.Service, move.Slot, move.FromNode, move.ToNode)
	}
	if len(plan.Blockers) > 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "Resolve every blocker with a separately reviewed data-migration or capacity plan before movement.")
	}
	if placementPlanFile != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "Plan written to %s; after review verify it with 'tako placement verify %s --plan-id %s'.\n", placementPlanFile, placementPlanFile, plan.PlanID)
	}
}
