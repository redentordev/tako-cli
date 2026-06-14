package cmd

import (
	"fmt"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/spf13/cobra"
)

var internalE2EServer string

var internalCmd = &cobra.Command{
	Use:    "internal",
	Short:  "Internal Tako helpers",
	Hidden: true,
}

var internalE2EServerSSHCmd = &cobra.Command{
	Use:    "e2e-server-ssh",
	Short:  "Print resolved server SSH fields for the E2E harness",
	Hidden: true,
	RunE:   runInternalE2EServerSSH,
}

type internalServerSSH struct {
	Host   string
	User   string
	Port   int
	SSHKey string
}

func init() {
	rootCmd.AddCommand(internalCmd)
	internalCmd.AddCommand(internalE2EServerSSHCmd)
	internalE2EServerSSHCmd.Flags().StringVar(&internalE2EServer, "server", "", "Server name to resolve")
}

func runInternalE2EServerSSH(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	resolved, err := resolveInternalServerSSH(cfg, getEnvironmentName(cfg), internalE2EServer)
	if err != nil {
		return err
	}

	printInternalServerSSH(cmd, resolved)
	return nil
}

func printInternalServerSSH(cmd *cobra.Command, resolved internalServerSSH) {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, resolved.Host)
	fmt.Fprintln(out, resolved.User)
	fmt.Fprintln(out, resolved.Port)
	fmt.Fprintln(out, resolved.SSHKey)
}

func resolveInternalServerSSH(cfg *config.Config, envName string, serverName string) (internalServerSSH, error) {
	if serverName == "" {
		return internalServerSSH{}, fmt.Errorf("--server is required")
	}

	serverNames, err := statePullServerNames(cfg, envName, serverName)
	if err != nil {
		return internalServerSSH{}, err
	}
	if len(serverNames) != 1 {
		return internalServerSSH{}, fmt.Errorf("expected one server, got %d", len(serverNames))
	}

	server, ok := cfg.Servers[serverNames[0]]
	if !ok {
		return internalServerSSH{}, fmt.Errorf("server %s not found in configuration", serverNames[0])
	}

	return internalServerSSH{
		Host:   server.Host,
		User:   server.User,
		Port:   server.Port,
		SSHKey: server.SSHKey,
	}, nil
}
