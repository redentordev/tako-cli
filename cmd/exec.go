package cmd

import (
	"fmt"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/spf13/cobra"
)

var (
	execServer  string
	execReplica int
	execOneOff  bool
	execTimeout time.Duration
)

var execCmd = &cobra.Command{
	Use:          "exec SERVICE -- COMMAND [ARGS...]",
	Short:        "Run a command in a running service container",
	SilenceUsage: true,
	Long: `Run a non-interactive command in the context of a deployed service.

By default the command runs inside a running replica (docker exec). With
--oneoff it runs in a fresh temporary container created from the service's
current image with the service's env, secrets, and network — useful for
framework tasks that should not share a serving process.

Output streams back combined (stdout+stderr). In the default text mode the
tako process mirrors the remote command's exit code; in machine modes the
exit code is structured into the ExecResult document instead.

There is no TTY or stdin: exec is for commands, not interactive shells.`,
	Example: `  # Inspect the environment inside the running web service
  tako exec web -- env

  # Run a framework task in a fresh container from the current image
  tako exec web --oneoff -- php artisan tinker --execute='dump(1)'

  # Target a specific node and replica
  tako exec web --server node-b --replica 2 -- date`,
	Args: cobra.MinimumNArgs(2),
	RunE: runExec,
}

func init() {
	rootCmd.AddCommand(execCmd)
	execCmd.Flags().StringVarP(&execServer, "server", "s", "", "Node to run on (default: first node where the service is deployed)")
	execCmd.Flags().IntVar(&execReplica, "replica", 0, "Running replica to attach to, 1-based (default: first)")
	execCmd.Flags().BoolVar(&execOneOff, "oneoff", false, "Run in a fresh temporary container from the service's current image")
	execCmd.Flags().DurationVar(&execTimeout, "timeout", engine.DefaultExecTimeout, "Kill the remote command after this duration")
}

func runExec(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}

	request := engine.ExecRequest{
		Config:      cfg,
		Environment: getEnvironmentName(cfg),
		Service:     args[0],
		Server:      execServer,
		Replica:     execReplica,
		OneOff:      execOneOff,
		Timeout:     execTimeout,
		Command:     args[1:],
	}

	result, err := cliEngine().Exec(cmd.Context(), request)
	if result != nil {
		if emitErr := emitResultDocument(result); emitErr != nil && err == nil {
			err = emitErr
		}
	}
	if err != nil {
		return err
	}
	// Machine modes structure the remote exit code in the ExecResult and
	// report success when the exec ran to completion; text mode mirrors it
	// so scripts can use $?.
	if result != nil && result.ExitCode != 0 && !machineOutputEnabled() {
		return &engine.RemoteExitError{Code: result.ExitCode}
	}
	return nil
}
