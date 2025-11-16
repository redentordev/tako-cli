package traefik

import (
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/ssh"
)

// EnableJSONLogging configures Traefik to use JSON format for access logs
func EnableJSONLogging(client *ssh.Client, verbose bool) error {
	if verbose {
		fmt.Println("Configuring Traefik for JSON access logging...")
	}

	// Check if Traefik is running as service (Swarm mode)
	checkServiceCmd := "docker service ls --filter name=traefik --format '{{.Name}}' 2>/dev/null || true"
	serviceOutput, _ := client.Execute(checkServiceCmd)

	isSwarmMode := strings.TrimSpace(serviceOutput) == "traefik"

	if isSwarmMode {
		// Update Swarm service
		return enableJSONLoggingSwarm(client, verbose)
	}

	// Update standalone container
	return enableJSONLoggingContainer(client, verbose)
}

// enableJSONLoggingSwarm enables JSON logging for Traefik Swarm service
func enableJSONLoggingSwarm(client *ssh.Client, verbose bool) error {
	// Update the service with JSON logging arguments
	updateCmd := `docker service update traefik \
		--args-add "--accessLog.format=json" \
		--args-add "--accessLog.fields.defaultMode=keep" \
		--args-add "--accessLog.fields.headers.defaultMode=keep" \
		--args-add "--accessLog.filePath=/var/log/traefik/access.log" \
		--detach 2>&1`

	if verbose {
		fmt.Println("Updating Traefik service with JSON logging...")
	}

	_, err := client.Execute(updateCmd)
	if err != nil {
		return fmt.Errorf("failed to update Traefik service: %w", err)
	}

	if verbose {
		fmt.Println("✓ Traefik configured for JSON access logging")
	}

	return nil
}

// enableJSONLoggingContainer enables JSON logging for Traefik container
func enableJSONLoggingContainer(client *ssh.Client, verbose bool) error {
	// For standalone container, we need to recreate it with JSON logging
	// Get current container configuration
	inspectCmd := "docker inspect traefik --format '{{range .HostConfig.PortBindings}}{{.}}{{end}}' 2>/dev/null || true"
	_, err := client.Execute(inspectCmd)
	if err != nil {
		return fmt.Errorf("Traefik container not found")
	}

	if verbose {
		fmt.Println("Note: To enable JSON logging for standalone Traefik, it needs to be recreated.")
		fmt.Println("This will be done automatically on the next deployment.")
	}

	// We'll handle this during next deployment by updating EnsureTraefikContainer

	return nil
}

// GetAccessLogFormat returns the configured access log format
func GetAccessLogFormat(client *ssh.Client) (string, error) {
	// Check Traefik args for access log format
	checkCmd := `docker service inspect traefik --format '{{range .Spec.TaskTemplate.ContainerSpec.Args}}{{.}} {{end}}' 2>/dev/null || \
		docker inspect traefik --format '{{range .Args}}{{.}} {{end}}' 2>/dev/null || echo ""`

	output, err := client.Execute(checkCmd)
	if err != nil {
		return "unknown", err
	}

	if strings.Contains(output, "--accessLog.format=json") {
		return "json", nil
	}

	return "common", nil
}
