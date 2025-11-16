package devmode

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/nixpacks"
	"gopkg.in/yaml.v3"
)

// DevManager handles local development environment
type DevManager struct {
	config  *config.Config
	verbose bool
}

// NewDevManager creates a new development manager
func NewDevManager(cfg *config.Config, verbose bool) *DevManager {
	return &DevManager{
		config:  cfg,
		verbose: verbose,
	}
}

// DockerCompose represents a docker-compose.yml structure
type DockerCompose struct {
	Version  string                    `yaml:"version"`
	Services map[string]ComposeService `yaml:"services"`
	Networks map[string]interface{}    `yaml:"networks,omitempty"`
	Volumes  map[string]interface{}    `yaml:"volumes,omitempty"`
}

// ComposeService represents a service in docker-compose
type ComposeService struct {
	Build       *ComposeBuild     `yaml:"build,omitempty"`
	Image       string            `yaml:"image,omitempty"`
	Container   string            `yaml:"container_name,omitempty"`
	Ports       []string          `yaml:"ports,omitempty"`
	Environment map[string]string `yaml:"environment,omitempty"`
	Volumes     []string          `yaml:"volumes,omitempty"`
	Command     string            `yaml:"command,omitempty"`
	Restart     string            `yaml:"restart,omitempty"`
	DependsOn   []string          `yaml:"depends_on,omitempty"`
}

// ComposeBuild represents build configuration
type ComposeBuild struct {
	Context    string   `yaml:"context"`
	Dockerfile string   `yaml:"dockerfile,omitempty"`
	Args       []string `yaml:"args,omitempty"`
}

// CheckDocker checks if Docker is installed and running
func (d *DevManager) CheckDocker() error {
	cmd := exec.Command("docker", "info")
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Try docker ps as a fallback - sometimes docker info fails on Windows
		// but docker ps works fine
		cmd2 := exec.Command("docker", "ps")
		if err2 := cmd2.Run(); err2 != nil {
			if d.verbose {
				fmt.Printf("Docker check failed: %v\nOutput: %s\n", err, string(output))
			}
			return fmt.Errorf("docker is not running or not installed")
		}
	}
	return nil
}

// getDockerComposeCmd returns the correct docker-compose command
// Tries "docker compose" (v2) first, falls back to "docker-compose" (v1)
func (d *DevManager) getDockerComposeCmd() (string, []string) {
	// Try docker compose (v2)
	cmd := exec.Command("docker", "compose", "version")
	if err := cmd.Run(); err == nil {
		return "docker", []string{"compose"}
	}

	// Fall back to docker-compose (v1)
	return "docker-compose", []string{}
}

// GenerateCompose generates a docker-compose.yml file for local development
func (d *DevManager) GenerateCompose(outputPath string, envName string) error {
	// Get services for the environment
	services, err := d.config.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services: %w", err)
	}

	compose := &DockerCompose{
		Version:  "3.8",
		Services: make(map[string]ComposeService),
		Networks: map[string]interface{}{
			"tako-dev": map[string]string{
				"driver": "bridge",
			},
		},
	}

	// Generate service configurations
	for serviceName, service := range services {
		composeService := ComposeService{
			Container:   fmt.Sprintf("%s_%s_dev", d.config.Project.Name, serviceName),
			Environment: make(map[string]string),
			Restart:     "unless-stopped",
		}

		// Determine build context (per-service)
		var contextPath string
		var hasDockerfile bool
		var dockerfilePath string

		if service.Build != "" {
			// Service has build path
			contextPath = service.Build
			if !filepath.IsAbs(contextPath) {
				cwd, _ := os.Getwd()
				contextPath = filepath.Join(cwd, contextPath)
			}

			// Try to find Dockerfile in build path
			dockerfileCandidates := []string{
				"Dockerfile",
				"Dockerfile.prod",
				"dockerfile",
				".dockerfile",
			}

			for _, candidate := range dockerfileCandidates {
				candidatePath := filepath.Join(contextPath, candidate)
				if _, err := os.Stat(candidatePath); err == nil {
					dockerfilePath = candidatePath
					hasDockerfile = true
					break
				}
			}

			// If no Dockerfile, try to generate with Nixpacks
			if !hasDockerfile {
				detector := nixpacks.NewDetector(contextPath, d.verbose)
				if _, err := detector.DetectFramework(); err == nil {
					// Generate Dockerfile with Nixpacks
					if d.verbose {
						fmt.Printf("  Generating Dockerfile with Nixpacks for %s...\n", serviceName)
					}
					if err := detector.GenerateDockerfile(); err != nil {
						return fmt.Errorf("failed to generate Dockerfile for %s: %w", serviceName, err)
					}
					dockerfilePath = filepath.Join(contextPath, ".nixpacks", "Dockerfile")
					hasDockerfile = true
				}
			}
		} else if service.Image != "" {
			// Service uses pre-built image (e.g., postgres, redis)
			composeService.Image = service.Image
		} else {
			return fmt.Errorf("service %s: neither 'build' nor 'image' specified", serviceName)
		}

		// Set up build configuration
		if hasDockerfile {
			composeService.Build = &ComposeBuild{
				Context:    contextPath,
				Dockerfile: filepath.Base(dockerfilePath),
			}
		}

		// Port mappings
		if service.Port > 0 {
			composeService.Ports = []string{
				fmt.Sprintf("%d:%d", service.Port, service.Port),
			}
		}

		// Environment variables
		for key, value := range service.Env {
			composeService.Environment[key] = value
		}

		// Add development-specific env vars (only for built services, not pre-built images)
		if service.Build != "" {
			composeService.Environment["ENVIRONMENT"] = "development"
			composeService.Environment["DEV_MODE"] = "true"

			// Volume mounts for hot reload (only for built services)
			composeService.Volumes = []string{
				fmt.Sprintf("%s:%s", contextPath, "/app"),
			}

			// Add node_modules volume for Node.js projects to prevent overwriting
			if contextPath != "" {
				detector := nixpacks.NewDetector(contextPath, false)
				if framework, _ := detector.DetectFramework(); framework == nixpacks.FrameworkNodeJS {
					composeService.Volumes = append(composeService.Volumes, "/app/node_modules")
				}
			}
		} else if len(service.Volumes) > 0 {
			// For pre-built images (like databases), use configured volumes
			composeService.Volumes = service.Volumes
		}

		// Command override if specified
		if service.Command != "" {
			composeService.Command = service.Command
		}

		compose.Services[serviceName] = composeService
	}

	// Write docker-compose.yml
	data, err := yaml.Marshal(compose)
	if err != nil {
		return fmt.Errorf("failed to marshal compose file: %w", err)
	}

	if err := os.WriteFile(outputPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write compose file: %w", err)
	}

	return nil
}

// Build builds images using docker-compose
func (d *DevManager) Build(composePath string) error {
	cmdName, baseArgs := d.getDockerComposeCmd()
	args := append(baseArgs, "-f", composePath, "build")

	cmd := exec.Command(cmdName, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker-compose build failed: %w", err)
	}

	return nil
}

// Up starts services using docker-compose
func (d *DevManager) Up(composePath string, detach bool) error {
	cmdName, baseArgs := d.getDockerComposeCmd()
	args := append(baseArgs, "-f", composePath, "up")
	if detach {
		args = append(args, "-d")
	}

	cmd := exec.Command(cmdName, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker-compose up failed: %w", err)
	}

	return nil
}

// Down stops services using docker-compose
func (d *DevManager) Down(composePath string) error {
	cmdName, baseArgs := d.getDockerComposeCmd()
	args := append(baseArgs, "-f", composePath, "down")

	cmd := exec.Command(cmdName, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker-compose down failed: %w", err)
	}

	return nil
}

// Logs streams logs from services
func (d *DevManager) Logs(composePath string, service string, follow bool) error {
	cmdName, baseArgs := d.getDockerComposeCmd()
	args := append(baseArgs, "-f", composePath, "logs")
	if follow {
		args = append(args, "-f")
	}
	if service != "" {
		args = append(args, service)
	}

	cmd := exec.Command(cmdName, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker-compose logs failed: %w", err)
	}

	return nil
}

// GetStatus gets the status of running services
func (d *DevManager) GetStatus(composePath string) (string, error) {
	cmdName, baseArgs := d.getDockerComposeCmd()
	args := append(baseArgs, "-f", composePath, "ps")

	cmd := exec.Command(cmdName, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker-compose ps failed: %w", err)
	}

	return strings.TrimSpace(string(output)), nil
}
