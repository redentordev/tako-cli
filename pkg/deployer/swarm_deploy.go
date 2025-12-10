package deployer

import (
	"fmt"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/provisioner"
	"github.com/redentordev/tako-cli/pkg/setup"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/swarm"
	"github.com/redentordev/tako-cli/pkg/traefik"
	"github.com/redentordev/tako-cli/pkg/unregistry"
)

// SetupSwarmCluster initializes the Docker Swarm cluster
func (d *Deployer) SetupSwarmCluster() error {
	if d.swarmManager == nil {
		return fmt.Errorf("swarm manager not initialized (use NewDeployerWithPool)")
	}

	if d.verbose {
		fmt.Printf("\n=== Setting up Docker Swarm ===\n\n")
	}

	// Get servers for this environment
	servers, err := d.config.GetEnvironmentServers(d.environment)
	if err != nil {
		return fmt.Errorf("failed to get environment servers: %w", err)
	}

	// Get manager server (first server is manager)
	managerName, err := d.config.GetManagerServer(d.environment)
	if err != nil {
		return fmt.Errorf("failed to get manager server: %w", err)
	}

	managerServer := d.config.Servers[managerName]

	if d.verbose {
		if len(servers) == 1 {
			fmt.Printf("→ Single-server deployment (manager only): %s (%s)\n", managerName, managerServer.Host)
		} else {
			fmt.Printf("→ Manager node: %s (%s)\n", managerName, managerServer.Host)
			fmt.Printf("→ Worker nodes: %d\n", len(servers)-1)
		}
	}

	// Get or create SSH client for manager (supports both key and password auth)
	managerClient, err := d.sshPool.GetOrCreateWithAuth(managerServer.Host, managerServer.Port, managerServer.User, managerServer.SSHKey, managerServer.Password)
	if err != nil {
		return fmt.Errorf("failed to connect to manager: %w", err)
	}

	// Check if Swarm is already initialized
	initialized, err := d.swarmManager.IsSwarmInitialized(managerClient)
	if err != nil {
		return fmt.Errorf("failed to check swarm status: %w", err)
	}

	// Initialize Swarm if needed
	if !initialized {
		if err := d.swarmManager.InitializeSwarm(managerClient, managerServer.Host); err != nil {
			return fmt.Errorf("failed to initialize swarm: %w", err)
		}

		// Wait a moment for Swarm to stabilize
		time.Sleep(2 * time.Second)
	}

	// Only get worker token and process workers if we have multiple servers
	var workerToken string
	if len(servers) > 1 {
		var err error
		workerToken, err = d.swarmManager.GetJoinToken(managerClient, "worker")
		if err != nil {
			return fmt.Errorf("failed to get worker join token: %w", err)
		}

		if d.verbose {
			fmt.Printf("\n→ Joining worker nodes to cluster...\n")
		}
	}

	// Join worker nodes (only for multi-server deployments)
	for _, serverName := range servers {
		if serverName == managerName {
			continue // Skip manager
		}

		server := d.config.Servers[serverName]
		if d.verbose {
			fmt.Printf("  Joining %s (%s)...\n", serverName, server.Host)
		}

		workerClient, err := d.sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return fmt.Errorf("failed to connect to %s: %w", serverName, err)
		}

		// Check if Docker is installed, if not, provision the server
		dockerCheck, err := workerClient.Execute("docker --version 2>&1")
		if err != nil || !strings.Contains(dockerCheck, "Docker version") {
			if d.verbose {
				fmt.Printf("  Docker not found on %s, provisioning server...\n", serverName)
			}

			if err := d.provisionServer(workerClient, serverName); err != nil {
				return fmt.Errorf("failed to provision %s: %w", serverName, err)
			}

			if d.verbose {
				fmt.Printf("  ✓ Server %s provisioned successfully\n", serverName)
			}
		}

		// Check if already part of swarm
		inSwarm, err := d.swarmManager.IsSwarmInitialized(workerClient)
		if err != nil {
			return fmt.Errorf("failed to check swarm status on %s: %w", serverName, err)
		}

		if !inSwarm {
			if err := d.swarmManager.JoinSwarm(workerClient, managerServer.Host, workerToken); err != nil {
				return fmt.Errorf("failed to join %s to swarm: %w", serverName, err)
			}
		} else if d.verbose {
			fmt.Printf("  %s already in swarm\n", serverName)
		}

		// Verify we can get node ID - if not, the node might be in a broken state
		nodeID, err := d.swarmManager.GetNodeID(workerClient)
		if err != nil {
			if d.verbose {
				fmt.Printf("  Warning: Cannot get node ID for %s (might be in broken swarm state)\n", serverName)
				fmt.Printf("  Forcing node to leave swarm and rejoin...\n")
			}

			if leaveErr := d.swarmManager.LeaveSwarm(workerClient, true); leaveErr != nil {
				if d.verbose {
					fmt.Printf("  Warning: Failed to leave swarm: %v\n", leaveErr)
				}
			}

			time.Sleep(2 * time.Second)

			if joinErr := d.swarmManager.JoinSwarm(workerClient, managerServer.Host, workerToken); joinErr != nil {
				return fmt.Errorf("failed to rejoin %s to swarm after leave: %w", serverName, joinErr)
			}

			nodeID, err = d.swarmManager.GetNodeID(workerClient)
			if err != nil {
				return fmt.Errorf("failed to get node ID for %s even after rejoin: %w", serverName, err)
			}
		}

		labels := map[string]string{
			"environment": d.environment,
			"server":      serverName,
		}

		if err := d.swarmManager.SetNodeLabels(managerClient, nodeID, labels); err != nil {
			return fmt.Errorf("failed to set labels on %s: %w", serverName, err)
		}
	}

	// Set labels on manager node too
	managerNodeID, err := d.swarmManager.GetNodeID(managerClient)
	if err != nil {
		return fmt.Errorf("failed to get manager node ID: %w", err)
	}

	managerLabels := map[string]string{
		"environment": d.environment,
		"server":      managerName,
		"role":        "manager",
	}

	if err := d.swarmManager.SetNodeLabels(managerClient, managerNodeID, managerLabels); err != nil {
		return fmt.Errorf("failed to set manager labels: %w", err)
	}

	// For multi-server deployments, we'll distribute images using docker save/load over SSH
	// This happens during service deployment (see DeployServiceSwarm)
	if len(servers) > 1 {
		if d.verbose {
			fmt.Printf("\n→ Multi-server deployment: images will be distributed via SSH\n")
		}
	} else if d.verbose {
		fmt.Printf("\n→ Single-server deployment (no image distribution needed)\n")
	}

	// Save swarm state
	swarmState := &swarm.SwarmState{
		Initialized: true,
		ManagerHost: managerServer.Host,
		WorkerToken: workerToken,
		Nodes:       make(map[string]string),
		LastUpdated: time.Now().Format(time.RFC3339),
	}

	// Note: We no longer use a registry - unregistry handles image distribution directly

	if err := d.swarmManager.SaveSwarmState(swarmState); err != nil {
		if d.verbose {
			fmt.Printf("Warning: failed to save swarm state: %v\n", err)
		}
	}

	if d.verbose {
		fmt.Printf("\n✓ Swarm cluster ready\n")
		// List nodes
		if nodeList, err := d.swarmManager.ListNodes(managerClient); err == nil {
			fmt.Printf("\nCluster nodes:\n%s\n", nodeList)
		}
	}

	return nil
}

// DeployServiceSwarm deploys a service to Docker Swarm
func (d *Deployer) DeployServiceSwarm(serviceName string, service *config.ServiceConfig, fullImageName string) error {
	if d.swarmManager == nil {
		return fmt.Errorf("swarm manager not initialized")
	}

	// Get manager client
	managerName, err := d.config.GetManagerServer(d.environment)
	if err != nil {
		return fmt.Errorf("failed to get manager server: %w", err)
	}

	managerServer := d.config.Servers[managerName]
	managerClient, err := d.sshPool.GetOrCreateWithAuth(managerServer.Host, managerServer.Port, managerServer.User, managerServer.SSHKey, managerServer.Password)
	if err != nil {
		return fmt.Errorf("failed to connect to manager: %w", err)
	}

	// Get all servers to check if multi-server deployment
	servers, err := d.config.GetEnvironmentServers(d.environment)
	if err != nil {
		return fmt.Errorf("failed to get environment servers: %w", err)
	}

	// For multi-server deployments, distribute the image to worker nodes
	// using docker save/load streamed over SSH
	if len(servers) > 1 {
		if d.verbose {
			fmt.Printf("  Distributing image to worker nodes...\n")
		}

		unreg := unregistry.NewManager(d.config, d.sshPool, d.environment, d.verbose)
		if err := unreg.DistributeImage(managerClient, fullImageName); err != nil {
			return fmt.Errorf("failed to distribute image to workers: %w", err)
		}
	}

	// Use the local image name (image is now available on all nodes)
	imageRef := fullImageName

	// Ensure overlay network exists
	networkName := fmt.Sprintf("tako_%s_%s", d.config.Project.Name, d.environment)
	if err := d.swarmManager.EnsureSwarmNetwork(managerClient, networkName); err != nil {
		return fmt.Errorf("failed to ensure overlay network: %w", err)
	}

	// For single-server deployments, add placement constraint
	// to ensure service runs on the manager node where the image was built
	// For multi-server deployments, image is distributed to all nodes via unregistry
	if len(servers) == 1 {
		if service.Placement == nil {
			service.Placement = &config.PlacementConfig{}
		}

		// Only add constraint if not already specified
		if len(service.Placement.Constraints) == 0 && service.Placement.Strategy == "" {
			// Use node label to pin service to the server where image was built
			service.Placement.Constraints = append(service.Placement.Constraints,
				fmt.Sprintf("node.labels.server==%s", managerName))

			if d.verbose {
				fmt.Printf("  Adding placement constraint for single-server deployment (server=%s)\n", managerName)
			}
		}
	}

	// Prepare Traefik labels before service creation (zero-disruption approach)
	var traefikLabels []string
	if service.IsPublic() {
		// Ensure Traefik is running as a Swarm service
		traefikManager := traefik.NewManager(managerClient, d.config.Project.Name, d.environment, d.verbose)
		if err := traefikManager.EnsureTraefikService(networkName); err != nil {
			return fmt.Errorf("failed to ensure traefik service: %w", err)
		}

		// Get Traefik labels for service creation
		fullServiceName := fmt.Sprintf("%s_%s_%s", d.config.Project.Name, d.environment, serviceName)
		traefikLabels = traefikManager.GetServiceLabels(serviceName, service, fullServiceName)

		if d.verbose {
			fmt.Printf("  Preparing Traefik configuration (zero-disruption mode)...\n")
		}
	}

	// Deploy service with Traefik labels included from the start
	if err := d.swarmManager.DeployService(managerClient, serviceName, service, imageRef, networkName, traefikLabels); err != nil {
		return fmt.Errorf("failed to deploy service to swarm: %w", err)
	}

	// Wait for service to start
	if d.verbose {
		fmt.Printf("  Waiting for service to start...\n")
		time.Sleep(5 * time.Second)
	}

	// Show Traefik dashboard URL if configured
	if service.IsPublic() && d.verbose {
		fmt.Printf("  ✓ Traefik reverse proxy configured\n")

	}

	return nil
}

// Note: Legacy proxy functions have been replaced with Traefik
// Traefik provides better integration with Swarm's overlay network and DNS-based load balancing
// For both single-server and Swarm deployments, Traefik is the standard reverse proxy

// provisionServer provisions a server with Docker and required tools
func (d *Deployer) provisionServer(client *ssh.Client, serverName string) error {
	if d.verbose {
		fmt.Printf("\n→ Provisioning server %s...\n", serverName)
	}

	prov := provisioner.NewProvisioner(client, d.verbose)

	// Run provisioning steps
	steps := []struct {
		name string
		fn   func() error
	}{
		{"Checking system requirements", prov.CheckRequirements},
		{"Updating system packages", prov.UpdateSystem},
		{"Installing Docker", prov.InstallDocker},
		{"Configuring firewall", prov.ConfigureFirewall},
		{"Hardening security", prov.HardenSecurity},
		{"Verifying auto-recovery", prov.VerifyAutoRecovery},
		{"Setting up deploy user", func() error {
			// Use root by default, but could be made configurable
			return prov.SetupDeployUser("root")
		}},
		{"Installing monitoring agent", prov.InstallMonitoringAgent},
	}

	for i, step := range steps {
		if d.verbose {
			fmt.Printf("  [%d/%d] %s...\n", i+1, len(steps), step.name)
		}
		if err := step.fn(); err != nil {
			return fmt.Errorf("step '%s' failed: %w", step.name, err)
		}
	}

	// Write version file
	newVersion := &setup.ServerVersion{
		Version:        setup.CurrentVersion,
		InstalledAt:    time.Now(),
		TakoCLIVersion: setup.CurrentVersion, // Use setup version as CLI version proxy
		Components:     make(map[string]string),
		Features:       []string{"docker", "traefik-proxy", "firewall", "monitoring"},
	}

	if err := setup.WriteVersionFile(client, newVersion); err != nil {
		if d.verbose {
			fmt.Printf("  Warning: Failed to write version file: %v\n", err)
		}
	}

	return nil
}
