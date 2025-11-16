package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/devmode"
	"github.com/spf13/cobra"
)

var (
	devDetach bool
	devBuild  bool
)

var devCmd = &cobra.Command{
	Use:   "dev",
	Short: "Run application locally in development mode",
	Long: `Run your application locally using Docker Compose.

This command:
  - Generates a docker-compose.yml for local development
  - Uses the same build process as production (Nixpacks or Dockerfile)
  - Mounts your source code for hot reload
  - Mirrors production environment variables
  - Starts services locally with docker-compose

Example:
  tako dev              # Start dev environment
  tako dev --build      # Rebuild images before starting
  tako dev -d           # Run in detached mode`,
	RunE: runDev,
}

func init() {
	rootCmd.AddCommand(devCmd)
	devCmd.Flags().BoolVarP(&devDetach, "detach", "d", false, "Run in detached mode")
	devCmd.Flags().BoolVar(&devBuild, "build", false, "Build images before starting")
}

func runDev(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	fmt.Printf("🚀 Starting local development environment...\n\n")

	// Create dev mode manager
	devManager := devmode.NewDevManager(cfg, verbose)

	// Check if Docker is running
	if err := devManager.CheckDocker(); err != nil {
		return fmt.Errorf("Docker is not running: %w\nPlease start Docker Desktop and try again.", err)
	}

	// Get environment
	envName := getEnvironmentName(cfg)
	services, err := cfg.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services: %w", err)
	}

	fmt.Printf("→ Using environment: %s\n", envName)

	// Generate docker-compose.yml
	composePath := filepath.Join(".", "docker-compose.dev.yml")
	fmt.Printf("→ Generating docker-compose configuration...\n")
	if err := devManager.GenerateCompose(composePath, envName); err != nil {
		return fmt.Errorf("failed to generate docker-compose.yml: %w", err)
	}
	fmt.Printf("  ✓ Configuration saved to %s\n\n", composePath)

	// Build images if requested
	if devBuild {
		fmt.Printf("→ Building images...\n")
		if err := devManager.Build(composePath); err != nil {
			return fmt.Errorf("failed to build images: %w", err)
		}
		fmt.Printf("  ✓ Images built successfully\n\n")
	}

	// Start docker-compose
	fmt.Printf("→ Starting services...\n")
	if err := devManager.Up(composePath, devDetach); err != nil {
		return fmt.Errorf("failed to start services: %w", err)
	}

	if devDetach {
		fmt.Printf("\n✓ Development environment started in background\n\n")
		fmt.Printf("View logs with: docker-compose -f %s logs -f\n", composePath)
		fmt.Printf("Stop services with: docker-compose -f %s down\n", composePath)
	} else {
		fmt.Printf("\n✓ Development environment stopped\n")
	}

	// Show service URLs
	if len(services) > 0 {
		fmt.Printf("\nYour services are available at:\n")
		for name, service := range services {
			if service.Port > 0 {
				fmt.Printf("  %s: http://localhost:%d\n", name, service.Port)
			}
		}
	}

	return nil
}
