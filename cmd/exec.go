package cmd

import (
	"fmt"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/spf13/cobra"
)

var (
	execServer  string
	execReplica int
	execTTY     bool
	execStdin   bool
)

var execCmd = &cobra.Command{
	Use:          "exec SERVICE [COMMAND...]",
	Short:        "Execute a command in a running service replica",
	SilenceUsage: true,
	Long: `Execute a command in a running service replica through takod.

When COMMAND is omitted, Tako opens a shell in the selected replica. By default,
Tako selects the first healthy replica deterministically across environment
nodes unless --server or --replica is provided.

Examples:
  tako exec web
  tako exec web -- env
  tako exec --replica 2 web -- sh -c 'php artisan migrate --force'`,
	Args: cobra.MinimumNArgs(1),
	RunE: runExec,
}

func init() {
	rootCmd.AddCommand(execCmd)
	execCmd.Flags().StringVarP(&execServer, "server", "s", "", "Node to exec on")
	execCmd.Flags().IntVar(&execReplica, "replica", 0, "Replica slot to exec into")
	execCmd.Flags().BoolVarP(&execStdin, "stdin", "i", false, "Forward stdin to the command")
	execCmd.Flags().BoolVarP(&execTTY, "tty", "t", false, "Force TTY allocation")
}

func runExec(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}
	if execReplica < 0 {
		return fmt.Errorf("--replica cannot be negative")
	}

	envName := getEnvironmentName(cfg)
	services, err := cfg.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services: %w", err)
	}
	serviceName := args[0]
	if _, ok := services[serviceName]; !ok {
		return fmt.Errorf("service %s not found in environment %s", serviceName, envName)
	}

	command := args[1:]
	tty := commandWantsTTY(command, execTTY)
	stdin := commandWantsStdin(command, tty, execStdin)

	pool := ssh.NewPool()
	defer pool.CloseAll()

	target, err := selectExecTarget(pool, cfg, envName, serviceName, execReplica, execServer)
	if err != nil {
		return err
	}
	if verbose {
		fmt.Printf("Using node: %s (%s)\n", target.serverName, target.server.Host)
	}

	request := takod.ExecStreamRequest{
		Project:     cfg.Project.Name,
		Environment: envName,
		Service:     serviceName,
		Slot:        execReplica,
		Command:     command,
		Stdin:       stdin,
		TTY:         tty,
	}
	return streamTakodCommand(target, cfg, "/v1/exec", request, tty, stdin)
}
