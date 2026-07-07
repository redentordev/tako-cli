package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/takoapi/ptystream"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	execServer      string
	execReplica     int
	execOneOff      bool
	execTimeout     time.Duration
	execInteractive bool
	execTTY         bool
)

var execCmd = &cobra.Command{
	Use:          "exec SERVICE -- COMMAND [ARGS...]",
	Short:        "Run a command in a running service container",
	SilenceUsage: true,
	Long: `Run a command in the context of a deployed service.

By default the command runs inside a running replica (docker exec). With
--oneoff it runs in a fresh temporary container created from the service's
current image with the service's env, secrets, and network — useful for
framework tasks that should not share a serving process.

Without -i/-t the command is non-interactive: output streams back combined
(stdout+stderr) and there is no stdin. With -i stdin is forwarded over a
full-duplex stream; with -t the remote process additionally runs under a
pseudo-terminal that follows local window resizes — 'tako exec -it web -- sh'
is a usable shell. When local stdin is not a terminal, -t degrades to -i.

In the default text mode the tako process mirrors the remote command's exit
code; in machine modes the exit code is structured into the ExecResult
document instead. Interactive flags are rejected in machine modes: raw
terminal bytes are not events (control planes speak the documented ptystream
frame protocol directly).`,
	Example: `  # Inspect the environment inside the running web service
  tako exec web -- env

  # Open an interactive shell in the running container
  tako exec -it web -- sh

  # Pipe data through a command running in the container
  cat dump.sql | tako exec -i db -- psql -U app

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
	execCmd.Flags().DurationVar(&execTimeout, "timeout", engine.DefaultExecTimeout, "Kill the remote command after this duration (interactive default: 4h)")
	execCmd.Flags().BoolVarP(&execInteractive, "interactive", "i", false, "Forward stdin to the remote command")
	execCmd.Flags().BoolVarP(&execTTY, "tty", "t", false, "Run the remote command under a pseudo-terminal (implies -i)")
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

	if execInteractive || execTTY {
		return runExecInteractive(cmd, request)
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

// runExecInteractive bridges the local terminal to the remote framed exec
// stream: raw mode + resize forwarding for -t, plain stdin forwarding for -i.
func runExecInteractive(cmd *cobra.Command, request engine.ExecRequest) error {
	if machineOutputEnabled() {
		return &engine.InvalidRequestError{Err: fmt.Errorf("interactive exec (-i/-t) is not available with --output json or --events ndjson; control planes bridge the ptystream frame protocol directly")}
	}
	// The interactive default is the server-side session timeout (4h with a
	// 30m idle cutoff), not the 10m one-shot default baked into the flag.
	if !cmd.Flags().Changed("timeout") {
		request.Timeout = 0
	}

	stdinFd := int(os.Stdin.Fd())
	tty := execTTY
	if tty && !term.IsTerminal(stdinFd) {
		fmt.Fprintln(os.Stderr, "stdin is not a terminal; continuing with -i only")
		tty = false
	}

	terminal := engine.ExecTerminal{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		TTY:    tty,
	}
	if tty {
		if w, h, err := term.GetSize(stdinFd); err == nil {
			terminal.InitialSize = ptystream.Winsize{Cols: uint16(w), Rows: uint16(h)}
		}
		oldState, err := term.MakeRaw(stdinFd)
		if err != nil {
			return fmt.Errorf("failed to set raw terminal: %w", err)
		}
		defer term.Restore(stdinFd, oldState)

		resize := make(chan ptystream.Winsize, 4)
		terminal.Resize = resize
		stopResize := watchExecResize(stdinFd, resize)
		defer stopResize()
	}

	result, err := cliEngine().ExecInteractive(cmd.Context(), request, terminal)
	if err != nil {
		return err
	}
	if result != nil && result.ExitCode != 0 {
		return &engine.RemoteExitError{Code: result.ExitCode}
	}
	return nil
}
