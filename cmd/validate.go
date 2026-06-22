package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
)

var validateQuiet bool

var validateCmd = &cobra.Command{
	Use:          "validate",
	Short:        "Validate tako.yaml without contacting servers",
	SilenceUsage: true,
	Long: `Validate the Tako configuration file using the same strict preflight path
that deploy uses before Git checks, SSH, builds, leases, setup, or proxy changes.`,
	Example: `  tako validate
  tako validate -e production
  tako --config ./deploy/tako.yaml validate --quiet`,
	RunE: runValidate,
}

func init() {
	rootCmd.AddCommand(validateCmd)
	validateCmd.Flags().BoolVar(&validateQuiet, "quiet", false, "Only print validation errors")
}

func runValidate(cmd *cobra.Command, args []string) error {
	configPath := resolveDeployConfigPath(cfgFile)
	cfg, err := loadDeployConfig(cfgFile)
	if err != nil {
		return err
	}
	if err := ensureDeployRuntimeSupported(cfg); err != nil {
		return formatDeployConfigError(configPath, fmt.Errorf("invalid config: %w", err))
	}

	envName := getEnvironmentName(cfg)
	servers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return formatDeployConfigError(configPath, fmt.Errorf("invalid config: %w", err))
	}
	services, err := cfg.GetServices(envName)
	if err != nil {
		return formatDeployConfigError(configPath, fmt.Errorf("invalid config: %w", err))
	}

	if !validateQuiet {
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Config valid: %s\n", filepath.Clean(configPath))
		fmt.Fprintf(out, "Environment: %s\n", envName)
		fmt.Fprintf(out, "Runtime: %s\n", cfg.GetRuntimeMode())
		fmt.Fprintf(out, "State: %s (consistency: %s)\n", cfg.GetStateBackend(), cfg.GetDeployConsistency())
		mesh := "disabled"
		if cfg.IsMeshEnabled() {
			mesh = fmt.Sprintf("enabled (%s via %s)", cfg.Mesh.NetworkCIDR, cfg.Mesh.Interface)
		}
		fmt.Fprintf(out, "Mesh: %s\n", mesh)
		fmt.Fprintf(out, "Servers: %d\n", len(servers))
		fmt.Fprintf(out, "Services: %d\n", len(services))
	}

	return nil
}
