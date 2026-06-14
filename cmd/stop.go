package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	stopService string
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop a deployed service",
	Long: `Stop a running takod service by reconciling it to 0 replicas.

Examples:
  tako stop --service web
  tako stop --service api`,
	RunE: runStop,
}

func init() {
	rootCmd.AddCommand(stopCmd)
	stopCmd.Flags().StringVar(&stopService, "service", "", "Service to stop (required)")
	stopCmd.MarkFlagRequired("service")
}

func runStop(cmd *cobra.Command, args []string) error {
	return runScaleTargets(cmd, []string{fmt.Sprintf("%s=0", stopService)})
}
