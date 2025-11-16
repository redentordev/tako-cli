package verification

import (
	"fmt"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

// Verifier handles deployment verification
type Verifier struct {
	client  *ssh.Client
	verbose bool
}

// NewVerifier creates a new deployment verifier
func NewVerifier(client *ssh.Client, verbose bool) *Verifier {
	return &Verifier{
		client:  client,
		verbose: verbose,
	}
}

// VerifyDeployment verifies a container deployment based on health check config
func (v *Verifier) VerifyDeployment(containerName string, service *config.ServiceConfig) error {
	fmt.Printf("→ Verifying deployment of %s...\n", containerName)

	// Stream logs for initial startup (10 seconds)
	fmt.Println("  Streaming startup logs...")
	if err := v.StreamLogs(containerName, 10*time.Second); err != nil {
		if v.verbose {
			fmt.Printf("  Warning: Could not stream logs: %v\n", err)
		}
	}

	// Check if service has health endpoint
	if service.HealthCheck.Path != "" && service.Port > 0 {
		// Service has health check - verify with health endpoint
		fmt.Printf("  Checking health endpoint: %s\n", service.HealthCheck.Path)
		return v.VerifyWithHealthCheck(containerName, service)
	}

	// No health check - just verify container is running
	fmt.Println("  No health endpoint - verifying container is running...")
	return v.VerifyContainerRunning(containerName, 5*time.Second)
}

// StreamLogs streams container logs for a duration
func (v *Verifier) StreamLogs(containerName string, duration time.Duration) error {
	cmd := fmt.Sprintf("docker logs --tail 20 %s 2>&1", containerName)

	output, err := v.client.Execute(cmd)
	if err != nil {
		return fmt.Errorf("failed to get logs: %w", err)
	}

	// Print logs with indentation
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for _, line := range lines {
		if line != "" {
			fmt.Printf("    %s\n", line)
		}
	}

	return nil
}

// VerifyWithHealthCheck polls health endpoint until it passes or timeout
func (v *Verifier) VerifyWithHealthCheck(containerName string, service *config.ServiceConfig) error {
	// Get container IP
	getIPCmd := fmt.Sprintf("docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' %s", containerName)
	containerIP, err := v.client.Execute(getIPCmd)
	if err != nil {
		return fmt.Errorf("failed to get container IP: %w", err)
	}
	containerIP = strings.TrimSpace(containerIP)

	if containerIP == "" {
		return fmt.Errorf("container has no IP address")
	}

	// Parse health check settings
	maxRetries := service.HealthCheck.Retries
	if maxRetries == 0 {
		maxRetries = 12 // Default 12 retries
	}

	interval := 5 * time.Second
	if service.HealthCheck.Interval != "" {
		if d, err := time.ParseDuration(service.HealthCheck.Interval); err == nil {
			interval = d
		}
	}

	// Wait for start period if configured
	if service.HealthCheck.StartPeriod != "" {
		if d, err := time.ParseDuration(service.HealthCheck.StartPeriod); err == nil {
			fmt.Printf("  Waiting %s for service to start...\n", d)
			time.Sleep(d)
		}
	}

	// Poll health endpoint
	healthURL := fmt.Sprintf("http://%s:%d%s", containerIP, service.Port, service.HealthCheck.Path)

	for attempt := 1; attempt <= maxRetries; attempt++ {
		// Use curl inside container or from host
		checkCmd := fmt.Sprintf("curl -f -s -o /dev/null -w '%%{http_code}' --max-time 5 %s 2>/dev/null || echo 'failed'", healthURL)

		statusCode, err := v.client.Execute(checkCmd)
		if err == nil {
			statusCode = strings.TrimSpace(statusCode)
			if statusCode == "200" {
				fmt.Printf("  ✓ Health check passed (attempt %d/%d)\n", attempt, maxRetries)
				return nil
			}
			if v.verbose {
				fmt.Printf("  Health check attempt %d/%d: status=%s\n", attempt, maxRetries, statusCode)
			}
		}

		if attempt < maxRetries {
			time.Sleep(interval)
		}
	}

	// Health check failed - show recent logs
	fmt.Println("  ✗ Health check failed - showing recent logs:")
	v.StreamLogs(containerName, 5*time.Second)

	return fmt.Errorf("health check failed after %d attempts", maxRetries)
}

// VerifyContainerRunning checks if container is still running after deployment
func (v *Verifier) VerifyContainerRunning(containerName string, waitTime time.Duration) error {
	fmt.Printf("  Waiting %s to verify container stability...\n", waitTime)
	time.Sleep(waitTime)

	// Check if container is still running
	checkCmd := fmt.Sprintf("docker ps --filter name=%s --filter status=running --format '{{.Names}}'", containerName)
	output, err := v.client.Execute(checkCmd)
	if err != nil {
		return fmt.Errorf("failed to check container status: %w", err)
	}

	if strings.TrimSpace(output) == "" {
		// Container is not running - get logs
		fmt.Println("  ✗ Container stopped running - showing logs:")
		v.StreamLogs(containerName, 5*time.Second)

		// Get exit code
		exitCmd := fmt.Sprintf("docker inspect %s --format '{{.State.ExitCode}}'", containerName)
		exitCode, _ := v.client.Execute(exitCmd)

		return fmt.Errorf("container crashed (exit code: %s)", strings.TrimSpace(exitCode))
	}

	fmt.Println("  ✓ Container is running and stable")
	return nil
}

// CheckContainerHealth quickly checks if a container is healthy (for monitoring)
func (v *Verifier) CheckContainerHealth(containerName string, service *config.ServiceConfig) error {
	// Check if container is running
	checkCmd := fmt.Sprintf("docker ps --filter name=%s --filter status=running --format '{{.Names}}'", containerName)
	output, err := v.client.Execute(checkCmd)
	if err != nil {
		return fmt.Errorf("failed to check container: %w", err)
	}

	if strings.TrimSpace(output) == "" {
		return fmt.Errorf("container is not running")
	}

	// If has health endpoint, check it
	if service.HealthCheck.Path != "" && service.Port > 0 {
		// Get container IP
		getIPCmd := fmt.Sprintf("docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' %s", containerName)
		containerIP, err := v.client.Execute(getIPCmd)
		if err != nil {
			return fmt.Errorf("failed to get container IP: %w", err)
		}
		containerIP = strings.TrimSpace(containerIP)

		if containerIP == "" {
			return fmt.Errorf("container has no IP")
		}

		// Check health endpoint
		healthURL := fmt.Sprintf("http://%s:%d%s", containerIP, service.Port, service.HealthCheck.Path)
		checkCmd := fmt.Sprintf("curl -f -s -o /dev/null -w '%%{http_code}' --max-time 5 %s 2>/dev/null || echo 'failed'", healthURL)

		statusCode, err := v.client.Execute(checkCmd)
		if err != nil || strings.TrimSpace(statusCode) != "200" {
			return fmt.Errorf("health check failed: %s", strings.TrimSpace(statusCode))
		}
	}

	return nil
}
