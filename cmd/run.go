package cmd

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployplan"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/spf13/cobra"
)

type runOptions struct {
	Name        string
	Port        int
	Server      string
	ServerName  string
	Environment string
	User        string
	SSHKey      string
	Password    string
	SSHPort     int
	Domain      string
	Replicas    int
	Env         []string
	Yes         bool
	PlanOnly    bool
	PlanFile    string
}

type runDeployRunner func(cmd *cobra.Command, imageRef string, opts runOptions, cfg *config.Config, service config.ServiceConfig, envVars map[string]string) error

var runRunner runDeployRunner = runImageDeploy

var runCmd = newRunCommand()

func init() {
	rootCmd.AddCommand(runCmd)
}

func newRunCommand() *cobra.Command {
	opts := runOptions{
		Environment: "production",
		SSHPort:     22,
		SSHKey:      "~/.ssh/id_rsa",
		Replicas:    1,
	}
	cmd := &cobra.Command{
		Use:          "run IMAGE",
		Short:        "Deploy a public image to an existing takod node without tako.yaml",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			imageRef := strings.TrimSpace(args[0])
			cfg, service, envVars, err := synthesizeRunConfig(imageRef, &opts)
			if err != nil {
				return err
			}
			return runRunner(cmd, imageRef, opts, cfg, service, envVars)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&opts.Name, "name", opts.Name, "Project and service name (required)")
	flags.IntVar(&opts.Port, "port", opts.Port, "Container port to expose (required)")
	flags.StringVar(&opts.Server, "server", opts.Server, "SSH host for the remote takod node (required)")
	flags.StringVar(&opts.ServerName, "server-name", opts.ServerName, "Generated config server name (defaults to sanitized --server)")
	flags.StringVar(&opts.Environment, "environment", opts.Environment, "Environment name")
	flags.StringVar(&opts.User, "user", opts.User, "SSH user (defaults to current user)")
	flags.StringVar(&opts.SSHKey, "ssh-key", opts.SSHKey, "SSH private key path")
	flags.StringVar(&opts.Password, "password", opts.Password, "SSH password")
	flags.IntVar(&opts.SSHPort, "ssh-port", opts.SSHPort, "SSH port")
	flags.StringVar(&opts.Domain, "domain", opts.Domain, "Public domain to route to the service")
	flags.IntVar(&opts.Replicas, "replicas", opts.Replicas, "Number of replicas")
	flags.StringArrayVar(&opts.Env, "env", nil, "Environment variable KEY=VALUE (repeatable)")
	flags.BoolVarP(&opts.Yes, "yes", "y", opts.Yes, "Skip confirmation prompts (non-interactive mode)")
	flags.BoolVar(&opts.PlanOnly, "plan-only", opts.PlanOnly, "Compute and show the reconciliation plan without applying it")
	flags.StringVar(&opts.PlanFile, "plan", opts.PlanFile, "Path to a reviewed plan document; apply fails if the computed plan drifted from it")
	return cmd
}

func synthesizeRunConfig(imageRef string, opts *runOptions) (*config.Config, config.ServiceConfig, map[string]string, error) {
	if opts == nil {
		return nil, config.ServiceConfig{}, nil, fmt.Errorf("run options are required")
	}
	imageRef = strings.TrimSpace(imageRef)
	if err := validateRunImageRef(imageRef); err != nil {
		return nil, config.ServiceConfig{}, nil, err
	}
	if err := normalizeRunOptions(opts); err != nil {
		return nil, config.ServiceConfig{}, nil, err
	}
	envVars, err := parseRunEnvVars(opts.Env)
	if err != nil {
		return nil, config.ServiceConfig{}, nil, err
	}
	version, err := deployplan.ImageBuildTag("", imageRef)
	if err != nil {
		return nil, config.ServiceConfig{}, nil, err
	}
	service := config.ServiceConfig{
		Image:    imageRef,
		Port:     opts.Port,
		Replicas: opts.Replicas,
		Env:      envVars,
	}
	if strings.TrimSpace(opts.Domain) != "" {
		service.Proxy = &config.ProxyConfig{
			Domain:     strings.TrimSpace(opts.Domain),
			Visibility: config.ProxyVisibilityPublic,
		}
	}
	cfg := &config.Config{
		Project: config.ProjectConfig{
			Name:    opts.Name,
			Version: version,
		},
		Servers: map[string]config.ServerConfig{
			opts.ServerName: {
				Host:     opts.Server,
				User:     opts.User,
				Port:     opts.SSHPort,
				SSHKey:   expandHome(opts.SSHKey),
				Password: opts.Password,
			},
		},
		Environments: map[string]config.EnvironmentConfig{
			opts.Environment: {
				Servers: []string{opts.ServerName},
				Services: map[string]config.ServiceConfig{
					opts.Name: service,
				},
			},
		},
	}
	if err := config.ValidateConfig(cfg); err != nil {
		return nil, config.ServiceConfig{}, nil, err
	}
	return cfg, cfg.Environments[opts.Environment].Services[opts.Name], envVars, nil
}

func normalizeRunOptions(opts *runOptions) error {
	opts.Name = strings.TrimSpace(opts.Name)
	opts.Environment = strings.TrimSpace(opts.Environment)
	opts.Server = strings.TrimSpace(opts.Server)
	opts.ServerName = strings.TrimSpace(opts.ServerName)
	opts.User = strings.TrimSpace(opts.User)
	opts.SSHKey = strings.TrimSpace(opts.SSHKey)
	opts.Domain = strings.TrimSpace(opts.Domain)
	if opts.Name == "" {
		return fmt.Errorf("--name is required")
	}
	if opts.Port <= 0 {
		return fmt.Errorf("--port must be greater than 0")
	}
	if opts.Server == "" {
		return fmt.Errorf("--server is required")
	}
	if opts.Environment == "" {
		opts.Environment = "production"
	}
	if opts.SSHKey == "" {
		opts.SSHKey = "~/.ssh/id_rsa"
	}
	if opts.SSHPort == 0 {
		opts.SSHPort = 22
	}
	if opts.Replicas == 0 {
		opts.Replicas = 1
	}
	if opts.ServerName == "" {
		opts.ServerName = sanitizeConfigExportServerName(opts.Server)
	} else {
		opts.ServerName = sanitizeConfigExportServerName(opts.ServerName)
	}
	if opts.ServerName == "" {
		return fmt.Errorf("--server-name could not be derived from --server")
	}
	if opts.User == "" {
		opts.User = currentUsername()
	}
	if opts.User == "" {
		return fmt.Errorf("--user is required when the current user cannot be determined")
	}
	if opts.SSHPort <= 0 {
		return fmt.Errorf("--ssh-port must be greater than 0")
	}
	if opts.Replicas <= 0 {
		return fmt.Errorf("--replicas must be greater than 0")
	}
	return nil
}

func parseRunEnvVars(values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	env := make(map[string]string, len(values))
	for _, raw := range values {
		idx := strings.Index(raw, "=")
		if idx <= 0 {
			return nil, fmt.Errorf("--env must be KEY=VALUE")
		}
		key := strings.TrimSpace(raw[:idx])
		if key == "" || strings.ContainsFunc(key, unicode.IsSpace) {
			return nil, fmt.Errorf("--env has invalid key %q", raw[:idx])
		}
		env[key] = raw[idx+1:]
	}
	return env, nil
}

func validateRunImageRef(imageRef string) error {
	if imageRef == "" {
		return fmt.Errorf("IMAGE is required")
	}
	if strings.HasPrefix(imageRef, "-") {
		return fmt.Errorf("IMAGE must not start with '-'")
	}
	for _, r := range imageRef {
		if unicode.IsSpace(r) || r < 0x20 || r == 0x7f {
			return fmt.Errorf("IMAGE contains unsupported characters")
		}
	}
	return nil
}

func runDeploymentPlan(cfg *config.Config, envName string, serviceName string, service config.ServiceConfig, actualState map[string]*reconcile.ActualService) (map[string]config.ServiceConfig, *reconcile.ReconciliationPlan) {
	return engine.RunDeploymentPlan(cfg, envName, serviceName, service, actualState)
}

func runImageDeploy(cmd *cobra.Command, imageRef string, opts runOptions, cfg *config.Config, service config.ServiceConfig, envVars map[string]string) error {
	request := engine.RunRequest{
		Config:        cfg,
		Environment:   opts.Environment,
		ServiceName:   opts.Name,
		Service:       service,
		ImageRef:      imageRef,
		ServerName:    opts.ServerName,
		ServerDisplay: opts.Server,
		EnvVars:       envVars,
		Verbose:       verbose,
	}

	session, err := cliEngine().PlanRun(cmd.Context(), request)
	if err != nil {
		return err
	}
	defer session.Close()

	if opts.PlanFile != "" {
		if err := verifyPlanFileMatches(opts.PlanFile, session.Plan()); err != nil {
			return err
		}
	}
	if opts.PlanOnly {
		return emitResultDocument(session.Plan())
	}

	if session.NeedsConfirmation() && !opts.Yes {
		reason := "deployment plan includes updates to an existing service"
		if machineOutputEnabled() {
			if err := emitResultDocument(newConfirmationRequiredDocument(reason, session.Plan())); err != nil {
				return err
			}
			return &engine.ConfirmationRequiredError{Reason: reason}
		}
		confirmed, err := confirmDeployAction("\nProceed with deployment? (y/N): ", reason)
		if err != nil {
			return err
		}
		if !confirmed {
			fmt.Println("Deployment cancelled")
			return nil
		}
	}

	result, err := session.Apply(cmd.Context())
	if result != nil {
		if emitErr := emitResultDocument(result); emitErr != nil && err == nil {
			err = emitErr
		}
	}
	return err
}
