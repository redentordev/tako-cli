package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/takoapi"
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
	configPath := filepath.Clean(resolveDeployConfigPath(cfgFile))
	result := engine.ValidateResult{
		APIVersion: takoapi.APIVersionCurrent,
		Kind:       engine.KindValidateResult,
		ConfigPath: configPath,
	}

	// fail records the validation finding, delivers the result document to
	// machine consumers, and surfaces the typed error (exit code 2).
	fail := func(err error) error {
		result.Valid = false
		result.Findings = append(result.Findings, engine.ValidateFinding{
			Severity: engine.ValidateSeverityError,
			Path:     configPath,
			Message:  strings.TrimSpace(err.Error()),
		})
		if emitErr := emitResultDocument(result); emitErr != nil {
			return emitErr
		}
		if engine.Classify(err) == engine.ClassInvalid {
			return err
		}
		return &engine.InvalidRequestError{Err: err}
	}

	cfg, err := loadDeployConfig(cfgFile)
	if err != nil {
		return fail(err)
	}
	result.Project = cfg.Project.Name
	if err := ensureDeployRuntimeSupported(cfg); err != nil {
		return fail(formatDeployConfigError(configPath, fmt.Errorf("invalid config: %w", err)))
	}

	envName := getEnvironmentName(cfg)
	result.Environment = envName
	servers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return fail(formatDeployConfigError(configPath, fmt.Errorf("invalid config: %w", err)))
	}
	services, err := cfg.GetServices(envName)
	if err != nil {
		return fail(formatDeployConfigError(configPath, fmt.Errorf("invalid config: %w", err)))
	}

	result.Valid = true
	result.Runtime = cfg.GetRuntimeMode()
	result.StateBackend = cfg.GetStateBackend()
	result.Consistency = cfg.GetDeployConsistency()
	result.MeshEnabled = cfg.IsMeshEnabled()
	if cfg.IsMeshEnabled() {
		result.MeshNetworkCIDR = cfg.Mesh.NetworkCIDR
		result.MeshInterface = cfg.Mesh.Interface
	}
	result.Servers = len(servers)
	result.Services = len(services)

	if !validateQuiet {
		var out io.Writer = cmd.OutOrStdout()
		if machineOutputEnabled() {
			// Machine modes reserve stdout for parseable output.
			out = os.Stderr
		}
		fmt.Fprintf(out, "Config valid: %s\n", configPath)
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

	return emitResultDocument(result)
}
