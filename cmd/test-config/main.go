package main

import (
	"fmt"
	"os"

	"github.com/redentordev/tako-cli/pkg/config"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: test-config <config-file>")
		os.Exit(1)
	}

	configFile := os.Args[1]

	fmt.Printf("Testing config file: %s\n", configFile)
	fmt.Println()

	// Set environment variables for testing
	os.Setenv("SERVER_HOST", "test.example.com")
	os.Setenv("SERVER_IP", "203.0.113.10") // Using TEST-NET-3 documentation IP range

	// Load config
	cfg, err := config.LoadConfig(configFile)
	if err != nil {
		fmt.Printf("✗ Failed to load config: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✓ Config loaded successfully\n\n")

	// Display project info
	fmt.Printf("Project:\n")
	fmt.Printf("  Name:    %s\n", cfg.Project.Name)
	fmt.Printf("  Version: %s\n\n", cfg.Project.Version)

	// Display deployment config
	fmt.Printf("Deployment Configuration:\n")
	fmt.Printf("  Strategy: %s", cfg.GetDeploymentStrategy())

	if cfg.Deployment == nil {
		fmt.Printf(" (default)\n")
	} else {
		fmt.Printf(" (configured)\n")
	}

	if cfg.IsParallelDeployment() {
		fmt.Printf("  ✓ Parallel deployment ENABLED\n")
		fmt.Printf("  Max Concurrent Builds:  %d\n", cfg.GetMaxConcurrentBuilds())
		fmt.Printf("  Max Concurrent Deploys: %d\n", cfg.GetMaxConcurrentDeploys())
		fmt.Printf("  Cache Enabled:          %v\n", cfg.IsCacheEnabled())
	} else {
		fmt.Printf("  Sequential deployment\n")
	}
	fmt.Println()

	// Display services
	for envName, env := range cfg.Environments {
		fmt.Printf("Environment: %s\n", envName)
		fmt.Printf("  Services: %d\n", len(env.Services))

		for serviceName, service := range env.Services {
			fmt.Printf("    - %s", serviceName)
			if service.Build != "" {
				fmt.Printf(" (build: %s)", service.Build)
			} else if service.Image != "" {
				fmt.Printf(" (image: %s)", service.Image)
			}
			if len(service.DependsOn) > 0 {
				fmt.Printf(" [depends on: %v]", service.DependsOn)
			}
			fmt.Println()
		}
		fmt.Println()
	}

	fmt.Printf("✓ Config validation complete!\n")
}
