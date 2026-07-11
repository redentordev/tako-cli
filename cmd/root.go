package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/fileutil"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/updater"
	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
	"github.com/spf13/viper"
)

var (
	cfgFile         string
	verbose         bool
	envFlag         string
	hostKeyModeFlag string
	// Version, GitCommit, and BuildTime are set via ldflags during build
	Version   = "dev"
	GitCommit = "unknown"
	BuildTime = "unknown"
)

// Machine output modes; see docs/MACHINE-INTERFACE.md.
const (
	outputFormatText = "text"
	outputFormatJSON = "json"

	eventsFormatNDJSON = "ndjson"
)

var (
	outputFormatFlag = outputFormatText
	eventsFormatFlag string
)

func validateMachineOutputFlags() error {
	switch outputFormatFlag {
	case outputFormatText, outputFormatJSON:
	default:
		return &engine.InvalidRequestError{Err: fmt.Errorf("invalid --output %q: supported values are text, json", outputFormatFlag)}
	}
	switch eventsFormatFlag {
	case "", eventsFormatNDJSON:
	default:
		return &engine.InvalidRequestError{Err: fmt.Errorf("invalid --events %q: supported value is ndjson", eventsFormatFlag)}
	}
	return nil
}

// humanOnlyAnnotation marks commands whose output is human terminal text by
// design (docs/MACHINE-INTERFACE.md command coverage). Machine output modes
// are rejected for them so stdout never mixes prose with parseable output.
const humanOnlyAnnotation = "tako.humanOnly"

func markHumanOnly(cmds ...*cobra.Command) {
	for _, command := range cmds {
		if command.Annotations == nil {
			command.Annotations = map[string]string{}
		}
		command.Annotations[humanOnlyAnnotation] = "true"
	}
}

// rejectMachineOutputForHumanOnly fails fast with a typed invalid-request
// error when --output json or --events ndjson targets a human-only command.
func rejectMachineOutputForHumanOnly(cmd *cobra.Command) error {
	if cmd == nil || !machineOutputEnabled() {
		return nil
	}
	if cmd.Annotations[humanOnlyAnnotation] != "true" {
		return nil
	}
	return &engine.InvalidRequestError{Err: fmt.Errorf("%s is human-only and does not support --output json or --events ndjson; see the command coverage table in docs/MACHINE-INTERFACE.md", cmd.CommandPath())}
}

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "tako",
	Short: "Deploy applications to any VPS with a small takod mesh",
	Long: `Tako reconciles Git-backed app config onto one or more owned servers.
One server is a one-node mesh; adding nodes keeps the same config and commands.

The CLI uses SSH for bootstrap and a node-local takod agent for Docker
reconciliation, proxy config, remote leases, and replicated deployment state.`,
	Version: Version,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if err := validateMachineOutputFlags(); err != nil {
			return err
		}
		return rejectMachineOutputForHumanOnly(cmd)
	},
}

// GetVersionInfo returns formatted version information
func GetVersionInfo() string {
	info := fmt.Sprintf("Tako CLI %s", Version)
	if GitCommit != "unknown" && GitCommit != "" {
		info += fmt.Sprintf(" (commit: %s)", GitCommit)
	}
	if BuildTime != "unknown" && BuildTime != "" {
		info += fmt.Sprintf("\nBuilt: %s", BuildTime)
	}
	return info
}

// Execute adds all child commands to the root command and sets flags appropriately.
func Execute() {
	// Check for updates on startup (once per day, non-blocking)
	checkForUpdatesOnStartup()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	err := rootCmd.ExecuteContext(ctx)
	if err != nil {
		os.Exit(exitCodeForError(err))
	}
}

// exitCodeForError maps engine error classes to the documented process exit
// codes: 0 success, 1 operation failed, 2 invalid request or confirmation
// required, 3 lock/lease held, 4 connectivity, 5 cancelled, 6 partial
// success needing attention.
func exitCodeForError(err error) int {
	var remote *engine.RemoteExitError
	if errors.As(err, &remote) {
		return remote.Code
	}
	switch engine.Classify(err) {
	case engine.ClassNone:
		return 0
	case engine.ClassInvalid:
		return 2
	case engine.ClassLocked:
		return 3
	case engine.ClassConnectivity:
		return 4
	case engine.ClassCancelled:
		return 5
	case engine.ClassAttention:
		return 6
	default:
		return 1
	}
}

// GenerateManPages writes Unix manual pages for the current command tree.
func GenerateManPages(dir string) error {
	if dir == "" {
		return fmt.Errorf("manual page output directory is required")
	}
	disableManPageAutoGenTag(rootCmd)
	manualDate := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
	header := &doc.GenManHeader{
		Title:   "TAKO",
		Section: "1",
		Date:    &manualDate,
		Source:  "Tako CLI",
		Manual:  "Tako CLI Manual",
	}
	return doc.GenManTree(rootCmd, header, dir)
}

func disableManPageAutoGenTag(command *cobra.Command) {
	command.DisableAutoGenTag = true
	for _, child := range command.Commands() {
		disableManPageAutoGenTag(child)
	}
}

// checkForUpdatesOnStartup checks for updates in the background
func checkForUpdatesOnStartup() {
	// Skip update check if user disabled it
	if os.Getenv("TAKO_SKIP_UPDATE_CHECK") == "1" {
		return
	}

	// Only check once per day
	shouldCheck := shouldCheckForUpdate()
	if !shouldCheck {
		return
	}

	// Run check in background to not slow down command execution
	go func() {
		defer func() {
			// Recover from any panics in update check
			if r := recover(); r != nil {
				// Silently ignore errors in background update check
			}
		}()

		checkForUpdate()
	}()
}

// checkForUpdate checks if an update is available
func checkForUpdate() {
	updater.CheckForUpdate(Version, true) // silent mode
}

// shouldCheckForUpdate checks if it's time to check for updates
func shouldCheckForUpdate() bool {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return false
	}

	lastCheckFile := filepath.Join(homeDir, ".tako_last_update_check")

	info, err := os.Stat(lastCheckFile)
	if err != nil {
		// File doesn't exist, create it
		_ = fileutil.WriteFileAtomic(lastCheckFile, []byte("checked"), 0644)
		return true
	}

	// Check if 24 hours have passed
	if time.Since(info.ModTime()) > 24*time.Hour {
		_ = fileutil.WriteFileAtomic(lastCheckFile, []byte("checked"), 0644)
		return true
	}

	return false
}

func init() {
	cobra.OnInitialize(initConfig)

	// Human-only command registry (docs/MACHINE-INTERFACE.md command
	// coverage): these print terminal text or prompt by design, so machine
	// output modes are rejected up front. `upgrade servers` keeps its full
	// machine contract; only the CLI self-update is human-only.
	markHumanOnly(
		initCmd,
		configExplainCmd,
		monitorCmd,
		envPushCmd,
		envPullCmd,
		secretsInitCmd,
		secretsSetCmd,
		secretsDeleteCmd,
		secretsFetchCmd,
		secretsImportCmd,
		upgradeCmd,
	)

	// Set custom version template
	rootCmd.SetVersionTemplate(fmt.Sprintf(`Tako CLI {{.Version}}
Commit:  %s
Built:   %s
`, GitCommit, BuildTime))

	// Global flags
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is ./tako.yaml or ./tako.json)")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "verbose output")
	rootCmd.PersistentFlags().StringVarP(&envFlag, "env", "e", "", "environment to deploy (default: production or only environment)")
	rootCmd.PersistentFlags().StringVar(&hostKeyModeFlag, "host-key-mode", "", "SSH host key verification mode: tofu, strict, ask (default: tofu)")
	rootCmd.PersistentFlags().StringVar(&outputFormatFlag, "output", outputFormatText, "output format: text or json (json reserves stdout for the final result document)")
	rootCmd.PersistentFlags().StringVar(&eventsFormatFlag, "events", "", "stream progress events to stdout: ndjson (human output moves to stderr)")

	// Bind flags to viper
	viper.BindPFlag("verbose", rootCmd.PersistentFlags().Lookup("verbose"))
	viper.BindPFlag("env", rootCmd.PersistentFlags().Lookup("env"))
	viper.BindPFlag("host_key_mode", rootCmd.PersistentFlags().Lookup("host-key-mode"))
}

// findEnvFile searches for .env file in current directory and parent directories
func findEnvFile() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}

	// Search up to 10 levels up
	for i := 0; i < 10; i++ {
		envPath := filepath.Join(dir, ".env")
		if _, err := os.Stat(envPath); err == nil {
			return envPath
		}

		// Move to parent directory
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached filesystem root
			break
		}
		dir = parent
	}

	return ""
}

// initConfig reads in config file and ENV variables if set
func initConfig() {
	// Load .env file from current or parent directories
	envFile := findEnvFile()
	if envFile != "" {
		_ = godotenv.Load(envFile)
	}

	// Configure SSH host key verification mode
	initHostKeyMode()

	if cfgFile != "" {
		// Use config file from the flag
		viper.SetConfigFile(cfgFile)
	} else {
		// Search for tako.yaml in current directory
		viper.AddConfigPath(".")
		viper.SetConfigType("yaml")
		viper.SetConfigName("tako")
	}

	viper.AutomaticEnv() // read in environment variables that match
	viper.SetEnvPrefix("TAKO")

	// If a config file is found, read it in
	if err := viper.ReadInConfig(); err == nil {
		if verbose {
			fmt.Fprintln(os.Stderr, "Using config file:", viper.ConfigFileUsed())
		}
	}
}

// getEnvironmentName returns the environment name from flag or uses default
// This is a helper for all commands that need environment
func getEnvironmentName(cfg interface{}) string {
	if envFlag != "" {
		return envFlag
	}

	// Use default from config (GetDefaultEnvironment)
	if c, ok := cfg.(interface{ GetDefaultEnvironment() string }); ok {
		return c.GetDefaultEnvironment()
	}

	return "production"
}

// initHostKeyMode configures SSH host key verification based on flag/env
func initHostKeyMode() {
	// Priority: flag > environment variable > default (tofu)
	mode := hostKeyModeFlag
	if mode == "" {
		mode = os.Getenv("TAKO_HOST_KEY_MODE")
	}

	if mode != "" {
		parsedMode, err := ssh.ParseHostKeyMode(mode)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Invalid SSH host key mode: %v\n", err)
			os.Exit(1)
		}
		ssh.SetGlobalHostKeyMode(parsedMode)
	}
}
