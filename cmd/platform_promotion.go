package cmd

import (
	"fmt"

	"github.com/redentordev/tako-cli/pkg/platform"
	"github.com/spf13/cobra"
)

var (
	platformPromotionStagedRoot     string
	platformPromotionClusterID      string
	platformPromotionKeyFingerprint string
)

var platformControllerCmd = &cobra.Command{Use: "controller", Short: "Inspect single-controller recovery readiness"}
var platformPromotionCmd = &cobra.Command{Use: "promotion", Short: "Prove passive controller promotion readiness"}
var platformPromotionVerifyCmd = &cobra.Command{
	Use:          "verify",
	Short:        "Verify a cold recovery tree can restore the single controller",
	SilenceUsage: true,
	RunE: func(_ *cobra.Command, _ []string) error {
		if !platform.RunningAsRoot() {
			return fmt.Errorf("passive controller promotion proof must run as root against a protected staging tree")
		}
		proof, err := platform.VerifyPassivePromotion(platformPromotionStagedRoot, platformPromotionClusterID, platformPromotionKeyFingerprint)
		if err != nil {
			return err
		}
		fmt.Fprintf(humanOut(), "Passive controller promotion proof verified\nCluster: %s\nController node: %s\nMembership generation: %d\nMode: %s\nActive-active: %t\n", proof.ClusterID, proof.ControllerNodeID, proof.MembershipGeneration, proof.ControllerMode, proof.ActiveActive)
		fmt.Fprintln(humanOut(), "Promotion remains a cold, operator-controlled recovery action; stop the old controller before activating this copy.")
		return nil
	},
}

func init() {
	platformCmd.AddCommand(platformControllerCmd)
	platformControllerCmd.AddCommand(platformPromotionCmd)
	platformPromotionCmd.AddCommand(platformPromotionVerifyCmd)
	markHumanOnly(platformPromotionVerifyCmd)
	platformPromotionVerifyCmd.Flags().StringVar(&platformPromotionStagedRoot, "staged-root", "", "Authenticated recovery staging root")
	platformPromotionVerifyCmd.Flags().StringVar(&platformPromotionClusterID, "cluster-id", "", "Expected immutable cluster UUID")
	platformPromotionVerifyCmd.Flags().StringVar(&platformPromotionKeyFingerprint, "controller-key-sha256", "", "Externally recorded controller recovery-key fingerprint")
	_ = platformPromotionVerifyCmd.MarkFlagRequired("staged-root")
	_ = platformPromotionVerifyCmd.MarkFlagRequired("cluster-id")
	_ = platformPromotionVerifyCmd.MarkFlagRequired("controller-key-sha256")
}
