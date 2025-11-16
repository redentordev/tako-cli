package deployer

import (
	"fmt"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

// HealthChecker handles container health verification
type HealthChecker struct {
	client  *ssh.Client
	verbose bool
}

// NewHealthChecker creates a new health checker
func NewHealthChecker(client *ssh.Client, verbose bool) *HealthChecker {
	return &HealthChecker{
		client:  client,
		verbose: verbose,
	}
}

// WaitForHealthy waits for a container to become healthy
func (hc *HealthChecker) WaitForHealthy(containerName string, retries int) error {
	if retries <= 0 {
		retries = 30 // Default 30 retries (30 seconds with 1s sleep)
	}

	if hc.verbose {
		fmt.Printf("  Waiting for health check...\n")
	}

	for i := 0; i < retries; i++ {
		// Check container health status
		checkCmd := fmt.Sprintf("docker inspect %s --format '{{.State.Health.Status}}' 2>/dev/null || echo 'unknown'", containerName)
		status, err := hc.client.Execute(checkCmd)
		if err != nil {
			return fmt.Errorf("failed to check health: %w", err)
		}

		status = strings.TrimSpace(status)

		// Container is healthy
		if status == "healthy" {
			if hc.verbose {
				fmt.Printf("  ✓ Container is healthy\n")
			}
			return nil
		}

		// Container health check failed
		if status == "unhealthy" {
			// Get container logs for debugging
			logsCmd := fmt.Sprintf("docker logs %s --tail 50 2>&1", containerName)
			logs, _ := hc.client.Execute(logsCmd)
			return fmt.Errorf("container health check failed, last logs:\n%s", logs)
		}

		// Still starting or no health check defined
		// If no health check is defined, just verify container is running
		if status == "unknown" || status == "" {
			runningCmd := fmt.Sprintf("docker inspect %s --format '{{.State.Running}}' 2>/dev/null", containerName)
			running, err := hc.client.Execute(runningCmd)
			if err != nil {
				return fmt.Errorf("failed to check if running: %w", err)
			}

			if strings.TrimSpace(running) == "true" {
				if hc.verbose {
					fmt.Printf("  ✓ Container is running (no health check defined)\n")
				}
				return nil
			}
		}

		// Wait before retry
		time.Sleep(1 * time.Second)
	}

	return fmt.Errorf("health check timeout after %d retries", retries)
}

// VerifyDatabaseConnectivity verifies that a service can reach its database
func (hc *HealthChecker) VerifyDatabaseConnectivity(containerName string, service *config.ServiceConfig) error {
	if hc.verbose {
		fmt.Printf("  Verifying database connectivity for %s...\n", containerName)
	}

	// Check if service has database environment variables
	dbHost := ""
	dbPort := ""
	dbType := ""

	// Common database environment variable patterns
	for key, value := range service.Env {
		keyUpper := strings.ToUpper(key)
		if strings.Contains(keyUpper, "DATABASE_HOST") || strings.Contains(keyUpper, "DB_HOST") {
			dbHost = value
		}
		if strings.Contains(keyUpper, "DATABASE_PORT") || strings.Contains(keyUpper, "DB_PORT") {
			dbPort = value
		}
		if strings.Contains(keyUpper, "DATABASE_URL") || strings.Contains(keyUpper, "DB_URL") {
			// Parse connection string
			if strings.Contains(value, "postgres://") {
				dbType = "postgres"
			} else if strings.Contains(value, "mysql://") {
				dbType = "mysql"
			}
		}
	}

	// If no database configuration found, skip check
	if dbHost == "" {
		if hc.verbose {
			fmt.Printf("  No database configuration found, skipping check\n")
		}
		return nil
	}

	// Default ports if not specified
	if dbPort == "" {
		if dbType == "postgres" {
			dbPort = "5432"
		} else if dbType == "mysql" {
			dbPort = "3306"
		} else {
			dbPort = "5432" // Default to postgres
		}
	}

	// Check if database host is reachable from the container
	// Use nc (netcat) to check connectivity
	checkCmd := fmt.Sprintf("docker exec %s sh -c 'timeout 5 nc -zv %s %s 2>&1' || echo 'FAILED'",
		containerName, dbHost, dbPort)

	output, err := hc.client.Execute(checkCmd)
	if err != nil || strings.Contains(output, "FAILED") {
		return fmt.Errorf("cannot reach database at %s:%s - ensure database is on the same network", dbHost, dbPort)
	}

	if hc.verbose {
		fmt.Printf("  ✓ Database connectivity verified\n")
	}

	return nil
}
