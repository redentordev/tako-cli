package swarm

import (
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/ssh"
)

// DowngradeManager handles Swarm to single-server downgrade
type DowngradeManager struct {
	client      *ssh.Client
	projectName string
	environment string
	verbose     bool
}

// NewDowngradeManager creates a new downgrade manager
func NewDowngradeManager(client *ssh.Client, projectName, environment string, verbose bool) *DowngradeManager {
	return &DowngradeManager{
		client:      client,
		projectName: projectName,
		environment: environment,
		verbose:     verbose,
	}
}

// DowngradeToSingleServer downgrades from Swarm to single-server mode
func (d *DowngradeManager) DowngradeToSingleServer() error {
	if d.verbose {
		fmt.Println("=== Starting Swarm Downgrade ===")
	}

	// Step 1: Backup critical data
	if err := d.backupSwarmData(); err != nil {
		return fmt.Errorf("failed to backup Swarm data: %w", err)
	}

	// Step 2: Get list of services before removal
	services, err := d.getSwarmServices()
	if err != nil {
		return fmt.Errorf("failed to get Swarm services: %w", err)
	}

	if d.verbose {
		fmt.Printf("Found %d Swarm services\n", len(services))
	}

	// Step 3: Remove Swarm services
	if err := d.removeSwarmServices(); err != nil {
		return fmt.Errorf("failed to remove Swarm services: %w", err)
	}

	// Step 4: Leave Swarm cluster
	if err := d.leaveSwarm(); err != nil {
		return fmt.Errorf("failed to leave Swarm: %w", err)
	}

	// Step 5: Clean up Swarm artifacts
	if err := d.cleanupSwarmArtifacts(); err != nil {
		return fmt.Errorf("failed to cleanup Swarm artifacts: %w", err)
	}

	// Step 6: Deploy Traefik as container (not service)
	if err := d.deployTraefikContainer(); err != nil {
		return fmt.Errorf("failed to deploy Traefik container: %w", err)
	}

	if d.verbose {
		fmt.Println("\n✓ Swarm downgrade completed successfully!")
		fmt.Println("You can now deploy services using single-server mode")
	}

	return nil
}

// backupSwarmData backs up critical Swarm data
func (d *DowngradeManager) backupSwarmData() error {
	if d.verbose {
		fmt.Println("→ Backing up Swarm data...")
	}

	// Backup acme.json (SSL certificates)
	backupCmds := []string{
		"sudo mkdir -p /root/swarm-backup",
		"sudo cp -r /etc/traefik/acme /root/swarm-backup/acme 2>/dev/null || true",
		"docker service ls --format '{{.Name}}\t{{.Image}}\t{{.Replicas}}' > /root/swarm-backup/services.txt 2>/dev/null || true",
		"docker network ls --filter driver=overlay --format '{{.Name}}' > /root/swarm-backup/networks.txt 2>/dev/null || true",
	}

	for _, cmd := range backupCmds {
		if _, err := d.client.Execute(cmd); err != nil {
			if d.verbose {
				fmt.Printf("  Warning: backup command failed: %s\n", cmd)
			}
		}
	}

	if d.verbose {
		fmt.Println("  ✓ Backup completed")
	}

	return nil
}

// getSwarmServices returns list of Swarm services
func (d *DowngradeManager) getSwarmServices() ([]string, error) {
	output, err := d.client.Execute("docker service ls --format '{{.Name}}' 2>/dev/null || true")
	if err != nil {
		return nil, err
	}

	services := []string{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line != "" {
			services = append(services, line)
		}
	}

	return services, nil
}

// removeSwarmServices removes all Swarm services
func (d *DowngradeManager) removeSwarmServices() error {
	if d.verbose {
		fmt.Println("→ Removing Swarm services...")
	}

	// Get all services
	services, err := d.getSwarmServices()
	if err != nil {
		return err
	}

	// Remove each service
	for _, service := range services {
		if d.verbose {
			fmt.Printf("  Removing service: %s\n", service)
		}
		d.client.Execute(fmt.Sprintf("docker service rm %s 2>/dev/null || true", service))
	}

	if d.verbose {
		fmt.Println("  ✓ All services removed")
	}

	return nil
}

// leaveSwarm leaves the Swarm cluster
func (d *DowngradeManager) leaveSwarm() error {
	if d.verbose {
		fmt.Println("→ Leaving Swarm cluster...")
	}

	// Check if this is a manager node
	checkCmd := "docker info --format '{{.Swarm.ControlAvailable}}' 2>/dev/null || echo 'false'"
	isManager, _ := d.client.Execute(checkCmd)

	if strings.TrimSpace(isManager) == "true" {
		// Manager node - force leave
		if _, err := d.client.Execute("docker swarm leave --force 2>&1"); err != nil {
			return fmt.Errorf("failed to leave Swarm: %w", err)
		}
	} else {
		// Worker node - regular leave
		if _, err := d.client.Execute("docker swarm leave 2>&1"); err != nil {
			return fmt.Errorf("failed to leave Swarm: %w", err)
		}
	}

	if d.verbose {
		fmt.Println("  ✓ Left Swarm cluster")
	}

	return nil
}

// cleanupSwarmArtifacts removes Swarm-specific artifacts
func (d *DowngradeManager) cleanupSwarmArtifacts() error {
	if d.verbose {
		fmt.Println("→ Cleaning up Swarm artifacts...")
	}

	cleanupCmds := []string{
		// Remove overlay networks (they'll be recreated as bridge networks)
		"docker network ls --filter driver=overlay --format '{{.Name}}' | xargs -r docker network rm 2>/dev/null || true",
		// Remove registry container
		"docker stop registry 2>/dev/null || true",
		"docker rm registry 2>/dev/null || true",
		// Clean up Swarm state files
		"rm -f .tako/swarm_*.json 2>/dev/null || true",
	}

	for _, cmd := range cleanupCmds {
		d.client.Execute(cmd)
	}

	if d.verbose {
		fmt.Println("  ✓ Cleanup completed")
	}

	return nil
}

// deployTraefikContainer deploys Traefik as a regular container
func (d *DowngradeManager) deployTraefikContainer() error {
	if d.verbose {
		fmt.Println("→ Deploying Traefik as container...")
	}

	// Check if Traefik service still exists
	checkCmd := "docker service ls --filter name=traefik --format '{{.Name}}' 2>/dev/null || true"
	output, _ := d.client.Execute(checkCmd)
	if strings.TrimSpace(output) == "traefik" {
		d.client.Execute("docker service rm traefik 2>/dev/null || true")
	}

	// Wait a moment for cleanup
	d.client.Execute("sleep 2")

	// Create network if it doesn't exist
	networkName := fmt.Sprintf("tako_%s_%s", d.projectName, d.environment)
	createNetworkCmd := fmt.Sprintf("docker network create --driver bridge %s 2>/dev/null || true", networkName)
	d.client.Execute(createNetworkCmd)

	// Deploy Traefik as container
	deployCmd := fmt.Sprintf(`docker run -d \
		--name traefik \
		--restart=unless-stopped \
		--network %s \
		-p 80:80 -p 443:443 -p 8080:8080 \
		-v /var/run/docker.sock:/var/run/docker.sock:ro \
		-v /etc/traefik/acme:/acme \
		-v /var/log/traefik:/var/log/traefik \
		traefik:latest \
		--api.dashboard=true \
		--api.insecure=true \
		--providers.docker=true \
		--providers.docker.exposedByDefault=false \
		--providers.docker.network=%s \
		--entryPoints.web.address=:80 \
		--entryPoints.websecure.address=:443 \
		--certificatesResolvers.letsencrypt.acme.email=tako@redentor.dev \
		--certificatesResolvers.letsencrypt.acme.storage=/acme/acme.json \
		--certificatesResolvers.letsencrypt.acme.httpChallenge.entryPoint=web \
		--log.level=INFO 2>&1`, networkName, networkName)

	if _, err := d.client.Execute(deployCmd); err != nil {
		return fmt.Errorf("failed to deploy Traefik container: %w", err)
	}

	if d.verbose {
		fmt.Println("  ✓ Traefik deployed as container")
	}

	return nil
}

// ShouldDowngrade checks if downgrade is needed
func (d *DowngradeManager) ShouldDowngrade(currentServers int) bool {
	// Check if Swarm is active
	checkCmd := "docker info --format '{{.Swarm.LocalNodeState}}' 2>/dev/null || echo 'inactive'"
	output, _ := d.client.Execute(checkCmd)
	isSwarmActive := strings.TrimSpace(output) == "active"

	// Downgrade if Swarm is active but only 1 server in config
	return isSwarmActive && currentServers == 1
}
