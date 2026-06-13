package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	stopServer  string
	stopService string
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop a deployed service",
	Long: `Stop a running takod service by reconciling it to 0 replicas.

Examples:
  tako stop --service web
  tako stop --service api --server prod`,
	RunE: runStop,
}

func init() {
	rootCmd.AddCommand(stopCmd)
	stopCmd.Flags().StringVarP(&stopServer, "server", "s", "", "Server to target")
	stopCmd.Flags().StringVar(&stopService, "service", "", "Service to stop (required)")
	stopCmd.MarkFlagRequired("service")
}

func runStop(cmd *cobra.Command, args []string) error {
	previousScaleServer := scaleServer
	scaleServer = stopServer
	defer func() {
		scaleServer = previousScaleServer
	}()

	return runScale(cmd, []string{fmt.Sprintf("%s=0", stopService)})
}
