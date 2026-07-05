package cmd

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/deployplan"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takodstate"
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
	services := map[string]config.ServiceConfig{serviceName: service}
	planActualState := deployplan.FilterActualStateForServices(actualState, services)
	return services, reconcile.ComputePlan(cfg.Project.Name, envName, services, planActualState)
}

func recordRunFailureAndReturn(cmd *cobra.Command, stateManager remoteDeploymentSaver, deployment *remotestate.DeploymentState, cfg *config.Config, envName string, serverNames []string, startTime time.Time, deployErr error) error {
	if recordErr := recordFailedDeploymentState(stateManager, nil, deployment, cfg, envName, serverNames, nil, startTime, deployErr); recordErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to record failed deployment state: %v\n", recordErr)
	}
	return deployErr
}

func runImageDeploy(cmd *cobra.Command, imageRef string, opts runOptions, cfg *config.Config, service config.ServiceConfig, envVars map[string]string) error {
	envName := opts.Environment
	serverNames := []string{opts.ServerName}
	server := cfg.Servers[opts.ServerName]

	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()

	leaseSet, err := acquireRemoteOperationLeases(sshPool, cfg, envName, serverNames, "run")
	if err != nil {
		return err
	}
	defer leaseSet.Release(verbose)

	sourceClient, err := sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
	if err != nil {
		return fmt.Errorf("failed to connect to server %s: %w", opts.ServerName, err)
	}

	deploy := deployer.NewDeployerWithPool(sourceClient, cfg, envName, sshPool, verbose)
	deploy.SetCLIVersion(Version)
	deploy.SetSkipBuild(true)
	if err := deploy.SetTargetServers(serverNames); err != nil {
		return err
	}
	if err := deploy.SetupTakodRuntime(); err != nil {
		return fmt.Errorf("failed to setup takod runtime: %w", err)
	}

	actualState, err := reconcile.GatherActualStateFromServers(sshPool, cfg, envName, serverNames, nil)
	if err != nil {
		return deployActualStateError(err)
	}

	services, plan := runDeploymentPlan(cfg, envName, opts.Name, service, actualState)
	fmt.Println()
	fmt.Print(plan.FormatPlan())

	if plan.NeedsConfirmation() && !opts.Yes {
		confirmed, err := confirmDeployAction("\nProceed with deployment? (y/N): ", "deployment plan includes updates to an existing service")
		if err != nil {
			return err
		}
		if !confirmed {
			fmt.Println("Deployment cancelled")
			return nil
		}
	}

	stateManager := remotestate.NewStateManagerWithSocket(sourceClient, cfg.Project.Name, envName, server.Host, takodSocketFromConfig(cfg))
	startTime := time.Now()
	deployment := &remotestate.DeploymentState{
		Timestamp:   startTime,
		ProjectName: cfg.Project.Name,
		Version:     cfg.Project.Version,
		Status:      remotestate.StatusInProgress,
		Services: map[string]remotestate.ServiceState{
			opts.Name: {
				Name:     opts.Name,
				Image:    imageRef,
				Port:     service.Port,
				Replicas: service.Replicas,
				Env:      redactedEnvKeys(envVars),
			},
		},
		User:       remotestate.GetCurrentUser(),
		Host:       server.Host,
		Message:    "deployed image",
		CLIVersion: Version,
		CLICommit:  GitCommit,
	}
	imageRefs := map[string]string{opts.Name: imageRef}

	if plan.IsEmpty() {
		fmt.Printf("%s is up-to-date on %s; reconciling proxy and state...\n", opts.Name, opts.Server)
		activeRevisions := deployplan.ProxyActiveRevisions(cfg, envName, services, nil, nil, actualState)
		if err := reconcileDeployProxy(deploy, services, activeRevisions); err != nil {
			deployErr := fmt.Errorf("proxy reconciliation failed: %w", err)
			return recordRunFailureAndReturn(cmd, stateManager, deployment, cfg, envName, serverNames, startTime, deployErr)
		}
	} else {
		fmt.Printf("Deploying %s as %s to %s...\n", imageRef, opts.Name, opts.Server)
		if err := deploy.DeployServiceTakod(opts.Name, &service, imageRef); err != nil {
			deployErr := fmt.Errorf("takod deployment failed for %s: %w", opts.Name, err)
			return recordRunFailureAndReturn(cmd, stateManager, deployment, cfg, envName, serverNames, startTime, deployErr)
		}
		activeRevisions := deployplan.ProxyActiveRevisions(cfg, envName, services, services, imageRefs, actualState)
		if err := reconcileDeployProxy(deploy, services, activeRevisions); err != nil {
			deployErr := fmt.Errorf("proxy reconciliation failed: %w", err)
			return recordRunFailureAndReturn(cmd, stateManager, deployment, cfg, envName, serverNames, startTime, deployErr)
		}
	}

	postNodeActualState, err := reconcile.GatherActualStateByServer(sshPool, cfg, envName, serverNames)
	if err != nil {
		deployErr := fmt.Errorf("deployment succeeded but failed to gather final actual state: %w", err)
		return recordRunFailureAndReturn(cmd, stateManager, deployment, cfg, envName, serverNames, startTime, deployErr)
	}
	postActualState := reconcile.AggregateActualStateByServer(postNodeActualState)

	if err := persistTakodRuntimeState(
		sshPool,
		cfg,
		envName,
		serverNames,
		"image",
		services,
		imageRefs,
		postActualState,
		postNodeActualState,
		takodstate.GitInfo{},
		"run.succeeded",
		fmt.Sprintf("deployed image %s", imageRef),
		map[string]string{
			"image":    imageRef,
			"service":  opts.Name,
			"replicas": fmt.Sprintf("%d", service.Replicas),
		},
	); err != nil {
		deployErr := fmt.Errorf("deployment succeeded but failed to persist takod state: %w", err)
		return recordRunFailureAndReturn(cmd, stateManager, deployment, cfg, envName, serverNames, startTime, deployErr)
	}

	deployment.Status = remotestate.StatusSuccess
	deployment.Duration = time.Since(startTime)
	if plan.IsEmpty() {
		deployment.Message = "service up-to-date; proxy and state reconciled"
	} else {
		deployment.Message = "deployed image"
	}
	if err := stateManager.SaveDeployment(deployment); err != nil {
		return deployRemoteHistoryError(err)
	}

	if plan.IsEmpty() {
		fmt.Printf("✓ %s is up-to-date. Proxy routes and takod state reconciled (%s).\n", opts.Name, envName)
	} else {
		fmt.Printf("✓ Deployed %s as %s to %s (%s)\n", imageRef, opts.Name, opts.Server, envName)
	}
	if service.Proxy != nil && service.Proxy.GetPrimaryDomain() != "" {
		fmt.Printf("URL: https://%s\n", service.Proxy.GetPrimaryDomain())
	}
	return nil
}
