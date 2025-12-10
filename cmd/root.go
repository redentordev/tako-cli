package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/joho/godotenv"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/updater"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	cfgFile        string
	verbose        bool
	envFlag        string
	hostKeyModeFlag string
	// Version, GitCommit, and BuildTime are set via ldflags during build
	Version   = "dev"
	GitCommit = "unknown"
	BuildTime = "unknown"
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "tako",
	Short: "Deploy applications to any VPS with zero configuration and zero downtime",
	Long: `Tako CLI brings Platform-as-a-Service (PaaS) simplicity to your own infrastructure.
Deploy Docker containers to your VPS servers with automatic HTTPS, health checks,
zero-downtime deployments, and complete control over your infrastructure.

It uses SSH for remote server management without requiring any agents on the servers.`,
	Version: Version,
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

	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
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
		os.WriteFile(lastCheckFile, []byte("checked"), 0644)
		return true
	}

	// Check if 24 hours have passed
	if time.Since(info.ModTime()) > 24*time.Hour {
		os.WriteFile(lastCheckFile, []byte("checked"), 0644)
		return true
	}

	return false
}

func init() {
	cobra.OnInitialize(initConfig)

	// Set custom version template
	rootCmd.SetVersionTemplate(fmt.Sprintf(`Tako CLI {{.Version}}
Commit:  %s
Built:   %s
`, GitCommit, BuildTime))

	// Global flags
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is ./tako.yaml)")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "verbose output")
	rootCmd.PersistentFlags().StringVarP(&envFlag, "env", "e", "", "environment to deploy (default: production or only environment)")
	rootCmd.PersistentFlags().StringVar(&hostKeyModeFlag, "host-key-mode", "", "SSH host key verification mode: tofu, strict, ask, insecure (default: tofu)")

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
		parsedMode := ssh.ParseHostKeyMode(mode)
		ssh.SetGlobalHostKeyMode(parsedMode)

		// Warn if using insecure mode
		if parsedMode == ssh.HostKeyModeInsecure {
			fmt.Fprintln(os.Stderr, "Warning: SSH host key verification is disabled. This is insecure!")
		}
	}
}
