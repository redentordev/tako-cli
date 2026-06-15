package cmd

import (
	"fmt"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/spf13/cobra"
)

var (
	runServer       string
	runOneOff       bool
	runOneOffRemove bool
	runTTY          bool
	runStdin        bool
)

var runCmd = &cobra.Command{
	Use:          "run SERVICE [COMMAND...] --one-off",
	Short:        "Run a one-off command from a service image",
	SilenceUsage: true,
	Long: `Run a one-off command through takod using a service's image, env, volumes,
and app/stage network.

The command does not store output in Tako state. Use --rm to remove the
one-off container after it exits.

Examples:
  tako run web --one-off -- npm run migrate
  tako run web --one-off --rm -- sh -c 'node scripts/seed.js'
  tako run --server prod-a worker --one-off -- node worker.js --once`,
	Args: cobra.MinimumNArgs(1),
	RunE: runOneOffCommand,
}

func init() {
	rootCmd.AddCommand(runCmd)
	runCmd.Flags().StringVarP(&runServer, "server", "s", "", "Node to run on")
	runCmd.Flags().BoolVar(&runOneOff, "one-off", false, "Run as a one-off container")
	runCmd.Flags().BoolVar(&runOneOffRemove, "rm", false, "Remove the one-off container after it exits")
	runCmd.Flags().BoolVarP(&runStdin, "stdin", "i", false, "Forward stdin to the command")
	runCmd.Flags().BoolVarP(&runTTY, "tty", "t", false, "Force TTY allocation")
}

func runOneOffCommand(cmd *cobra.Command, args []string) error {
	if !runOneOff {
		return fmt.Errorf("--one-off is required")
	}

	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}

	envName := getEnvironmentName(cfg)
	services, err := cfg.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services: %w", err)
	}
	serviceName := args[0]
	service, ok := services[serviceName]
	if !ok {
		return fmt.Errorf("service %s not found in environment %s", serviceName, envName)
	}

	imageRef := service.Image
	if imageRef == "" {
		imageRef = cfg.GetFullImageName(serviceName, envName)
	}
	command := args[1:]
	if len(command) == 0 && service.Command != "" {
		command = []string{"sh", "-c", service.Command}
	}
	tty := commandWantsTTY(command, runTTY)
	stdin := commandWantsStdin(command, tty, runStdin)

	envFileContent, err := buildOperatorEnvFileContent(envName, &service)
	if err != nil {
		return err
	}
	mounts, err := buildOperatorMountSpecs(cfg, envName, serviceName, &service)
	if err != nil {
		return err
	}

	pool := ssh.NewPool()
	defer pool.CloseAll()

	target, serverNames, err := selectRunTarget(pool, cfg, envName, runServer)
	if err != nil {
		return err
	}

	leaseSet, err := acquireRemoteOperationLeases(pool, cfg, envName, []string{target.serverName}, "run")
	if err != nil {
		return err
	}
	defer leaseSet.Release(verbose)
	if verbose {
		fmt.Printf("Using node: %s (%s)\n", target.serverName, target.server.Host)
		fmt.Printf("Target environment nodes: %v\n", serverNames)
	}

	request := takod.RunOneOffRequest{
		Project:        cfg.Project.Name,
		Environment:    envName,
		Service:        serviceName,
		Image:          imageRef,
		PullImage:      service.Image != "",
		RegistryAuth:   commandRegistryAuth(cfg, imageRef),
		Network:        runOneOffNetworkName(cfg.Project.Name, envName),
		EnvFileContent: envFileContent,
		Mounts:         mounts,
		Command:        command,
		Stdin:          stdin,
		TTY:            tty,
		Remove:         runOneOffRemove,
	}
	return streamTakodCommand(target, cfg, "/v1/run", request, tty, stdin)
}

func commandRegistryAuth(cfg *config.Config, image string) *takod.RegistryAuth {
	auth := cfg.RegistryAuthForImage(image)
	if auth == nil {
		return nil
	}
	return &takod.RegistryAuth{
		Server:        auth.Server,
		Username:      auth.Username,
		Password:      auth.Password,
		IdentityToken: auth.IdentityToken,
	}
}
