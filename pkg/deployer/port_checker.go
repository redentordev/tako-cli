package deployer

import (
	"fmt"
	"regexp"
	"strings"
)

// PortInfo contains information about a port in use
type PortInfo struct {
	Port        int
	ProcessName string
	PID         string
	IsDocker    bool
	ContainerID string
}

// CheckPortAvailability checks if a port is available on the server
func (d *Deployer) CheckPortAvailability(port int) (*PortInfo, error) {
	if port == 0 {
		return nil, nil // No port to check
	}

	// Check if port is in use using lsof or ss
	checkCmd := fmt.Sprintf("lsof -i :%d -t 2>/dev/null || ss -tlnp | grep ':%d ' | awk '{print $6}' | cut -d',' -f2", port, port)
	output, err := d.client.Execute(checkCmd)
	if err != nil || strings.TrimSpace(output) == "" {
		// Port is free
		return nil, nil
	}

	// Port is in use - get details
	pid := strings.TrimSpace(strings.Split(output, "\n")[0])

	// Check if it's a Docker container
	dockerCheckCmd := fmt.Sprintf("docker ps --format '{{.ID}} {{.Names}} {{.Ports}}' | grep '0.0.0.0:%d' | head -1", port)
	dockerOutput, _ := d.client.Execute(dockerCheckCmd)

	info := &PortInfo{
		Port: port,
		PID:  pid,
	}

	if strings.TrimSpace(dockerOutput) != "" {
		// It's a Docker container
		parts := strings.Fields(dockerOutput)
		if len(parts) >= 2 {
			info.IsDocker = true
			info.ContainerID = parts[0]
			info.ProcessName = parts[1]
		}
	} else {
		// It's a regular process
		psCmd := fmt.Sprintf("ps -p %s -o comm= 2>/dev/null || echo 'unknown'", pid)
		processName, _ := d.client.Execute(psCmd)
		info.ProcessName = strings.TrimSpace(processName)
	}

	return info, nil
}

// ResolvePortConflict attempts to resolve a port conflict
func (d *Deployer) ResolvePortConflict(portInfo *PortInfo, serviceName string, autoKill bool) error {
	if portInfo == nil {
		return nil // No conflict
	}

	if d.verbose {
		if portInfo.IsDocker {
			fmt.Printf("  Port %d is in use by Docker container: %s\n", portInfo.Port, portInfo.ProcessName)
		} else {
			fmt.Printf("  Port %d is in use by process: %s (PID: %s)\n", portInfo.Port, portInfo.ProcessName, portInfo.PID)
		}
	}

	// Check if it's our own service's container
	expectedContainerName := fmt.Sprintf("%s_%s", d.config.Project.Name, serviceName)
	legacyContainerName := d.config.Project.Name

	if portInfo.IsDocker && (portInfo.ProcessName == expectedContainerName || portInfo.ProcessName == legacyContainerName) {
		if d.verbose {
			fmt.Printf("  This is our own container - will be replaced during deployment\n")
		}
		return nil // We'll handle this in deployment
	}

	// It's a different process/container
	if autoKill {
		if d.verbose {
			fmt.Printf("  Stopping conflicting process/container...\n")
		}

		if portInfo.IsDocker {
			// Stop Docker container
			if _, err := d.client.Execute(fmt.Sprintf("docker stop %s && docker rm -f %s", portInfo.ContainerID, portInfo.ContainerID)); err != nil {
				return fmt.Errorf("failed to stop conflicting container: %w", err)
			}
		} else {
			// Kill process
			if _, err := d.client.Execute(fmt.Sprintf("kill %s", portInfo.PID)); err != nil {
				return fmt.Errorf("failed to kill conflicting process: %w", err)
			}
		}

		if d.verbose {
			fmt.Printf("  ✓ Conflicting process stopped\n")
		}
		return nil
	}

	// Don't auto-kill - return error with helpful message
	if portInfo.IsDocker {
		return fmt.Errorf("port %d is in use by Docker container '%s' (ID: %s). "+
			"Stop it manually with: docker stop %s",
			portInfo.Port, portInfo.ProcessName, portInfo.ContainerID, portInfo.ContainerID)
	}

	return fmt.Errorf("port %d is in use by process '%s' (PID: %s). "+
		"Stop it manually or use a different port",
		portInfo.Port, portInfo.ProcessName, portInfo.PID)
}

// extractPIDFromSS extracts PID from ss command output
func extractPIDFromSS(line string) string {
	// ss output format: ... users:(("process",pid=1234,fd=5))
	re := regexp.MustCompile(`pid=(\d+)`)
	matches := re.FindStringSubmatch(line)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}
