package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/hooks"
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

	// With -e only the selected environment's variables are required;
	// without -e every environment is checked.
	cfg, err := loadSelectedEnvironmentConfig(cfgFile)
	if err != nil {
		return fail(err)
	}
	result.Project = cfg.Project.Name
	if deferred := cfg.DeferredEnvironmentVariables(); len(deferred) > 0 {
		result.Findings = append(result.Findings, engine.ValidateFinding{
			Severity: engine.ValidateSeverityWarn,
			Path:     configPath,
			Message:  fmt.Sprintf("skipped other environments and servers that use unset environment variable(s): %s; run 'tako validate' without -e to check every environment", strings.Join(deferred, ", ")),
		})
	}
	for _, warning := range config.ValidationWarnings(cfg) {
		result.Findings = append(result.Findings, engine.ValidateFinding{
			Severity: engine.ValidateSeverityWarn,
			Path:     configPath,
			Field:    warning.Field,
			Message:  warning.Message,
		})
	}
	// Alias targets and built-in shadowing need the command tree, so they
	// are checked here rather than in pkg/config.
	var blockingErrors []string
	for _, finding := range checkAliasTargets(rootCmd, cfg.Aliases) {
		finding.Path = configPath
		result.Findings = append(result.Findings, finding)
		if finding.Severity == engine.ValidateSeverityError {
			blockingErrors = append(blockingErrors, finding.Message)
		}
	}
	// Hooks are resolved exactly as the lifecycle commands resolve them,
	// without executing anything.
	for _, finding := range checkHooks(cfg, configPath) {
		finding.Path = configPath
		result.Findings = append(result.Findings, finding)
		if finding.Severity == engine.ValidateSeverityError {
			blockingErrors = append(blockingErrors, finding.Message)
		}
	}
	if len(blockingErrors) > 0 {
		result.Valid = false
		if err := emitResultDocument(result); err != nil {
			return err
		}
		return &engine.InvalidRequestError{Err: fmt.Errorf("invalid config %s: %s", configPath, strings.Join(blockingErrors, "; "))}
	}
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
		for _, finding := range result.Findings {
			if finding.Severity == engine.ValidateSeverityWarn {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %s\n", finding.Message)
			}
		}
	}

	return emitResultDocument(result)
}

// checkHooks reports hook resolution problems as validate findings: errors
// for hooks the lifecycle commands would refuse to run, warnings for
// hook-like files that would never run. It starts no process and emits no
// hook events.
func checkHooks(cfg *config.Config, configPath string) []engine.ValidateFinding {
	projectDir, err := filepath.Abs(filepath.Dir(configPath))
	if err != nil {
		projectDir = filepath.Dir(configPath)
	}
	resolution := hooks.Resolve(projectDir, cfg.Hooks, runtime.GOOS)
	var findings []engine.ValidateFinding
	for _, resolveErr := range resolution.Errors {
		field := "hooks"
		var hookErr hooks.HookError
		if errors.As(resolveErr, &hookErr) {
			field = "hooks." + string(hookErr.HookName())
		}
		findings = append(findings, engine.ValidateFinding{Severity: engine.ValidateSeverityError, Field: field, Message: resolveErr.Error()})
	}
	for _, unknown := range resolution.Unknown {
		findings = append(findings, engine.ValidateFinding{Severity: engine.ValidateSeverityWarn, Field: "hooks", Message: unknown.Message})
	}
	return findings
}
