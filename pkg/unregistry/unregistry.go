// Package unregistry provides integration with the unregistry tool for efficient
// Docker image distribution to remote servers without a persistent registry.
//
// Unregistry (https://github.com/psviderski/unregistry) uses SSH tunneling to push
// images directly to remote Docker hosts, transferring only missing layers.
//
// IMPORTANT: docker-pussh runs LOCALLY and pushes images FROM the local machine
// TO a remote server. It is NOT meant to be run on a remote server.
//
// For Tako CLI deployments where images are built on the manager node (not locally),
// we provide a fallback mechanism using docker save/load over SSH.
//
// Benefits of docker-pussh (when images are built locally):
//   - Only transfers missing layers (like rsync for Docker images)
//   - Uses existing SSH connections (no additional ports/firewall rules)
//   - No persistent registry service to maintain
//
// For multi-server swarm deployments where images are built on the manager:
//   - We use docker save | ssh docker load to distribute images
//   - This transfers the full image but works reliably without local builds
package unregistry

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

// Manager handles image distribution across Swarm nodes
type Manager struct {
	config      *config.Config
	sshPool     *ssh.Pool
	environment string
	verbose     bool
}

// NewManager creates a new unregistry manager
func NewManager(cfg *config.Config, sshPool *ssh.Pool, environment string, verbose bool) *Manager {
	return &Manager{
		config:      cfg,
		sshPool:     sshPool,
		environment: environment,
		verbose:     verbose,
	}
}

// IsDockerPusshAvailable checks if docker-pussh is installed locally
// Note: docker-pussh is only useful when images are built locally, not on remote servers
func IsDockerPusshAvailable() bool {
	cmd := exec.Command("docker", "pussh", "--version")
	err := cmd.Run()
	return err == nil
}

// GetInstallInstructions returns instructions for installing docker-pussh
func GetInstallInstructions() string {
	return `docker-pussh can be installed for local-to-remote image transfers:

  # macOS/Linux via Homebrew
  brew install psviderski/tap/docker-pussh
  mkdir -p ~/.docker/cli-plugins
  ln -sf $(brew --prefix)/bin/docker-pussh ~/.docker/cli-plugins/docker-pussh

  # Or via direct download
  mkdir -p ~/.docker/cli-plugins
  curl -sSL https://raw.githubusercontent.com/psviderski/unregistry/main/docker-pussh \
    -o ~/.docker/cli-plugins/docker-pussh
  chmod +x ~/.docker/cli-plugins/docker-pussh

For more info: https://github.com/psviderski/unregistry`
}

// DistributeImage distributes a Docker image from the manager node to all worker nodes
// using docker save/load over SSH. This is the reliable method for Tako deployments
// where images are built on the manager node (not locally).
func (m *Manager) DistributeImage(managerClient *ssh.Client, imageName string) error {
	if m.verbose {
		fmt.Printf("\n-> Distributing image to worker nodes...\n")
		fmt.Printf("   Image: %s\n", imageName)
	}

	// Get all servers for this environment
	servers, err := m.config.GetEnvironmentServers(m.environment)
	if err != nil {
		return fmt.Errorf("failed to get environment servers: %w", err)
	}

	// Get manager server to skip it
	managerName, err := m.config.GetManagerServer(m.environment)
	if err != nil {
		return fmt.Errorf("failed to get manager server: %w", err)
	}

	// Count workers
	workerCount := 0
	for _, serverName := range servers {
		if serverName != managerName {
			workerCount++
		}
	}

	if workerCount == 0 {
		if m.verbose {
			fmt.Printf("   No worker nodes to distribute to (single-server deployment)\n")
		}
		return nil
	}

	if m.verbose {
		fmt.Printf("   Distributing to %d worker node(s)...\n", workerCount)
	}

	// Save image to tar on manager (streamed approach - no temp file)
	// We'll pipe docker save directly through SSH to docker load on workers
	managerServer := m.config.Servers[managerName]

	for _, serverName := range servers {
		if serverName == managerName {
			continue // Skip manager
		}

		server := m.config.Servers[serverName]
		if m.verbose {
			fmt.Printf("   -> %s (%s)...\n", serverName, server.Host)
		}

		// Transfer image using piped docker save | ssh docker load
		// This streams the image directly without creating a temp file
		if err := m.transferImageStream(managerClient, managerServer, server, imageName); err != nil {
			return fmt.Errorf("failed to distribute to %s: %w", serverName, err)
		}

		if m.verbose {
			fmt.Printf("      Done\n")
		}
	}

	if m.verbose {
		fmt.Printf("   Image distributed successfully\n")
	}

	return nil
}

// transferImageStream transfers an image from manager to worker using streamed docker save | ssh docker load
// This avoids creating temporary files and is more efficient for large images
func (m *Manager) transferImageStream(managerClient *ssh.Client, managerServer, workerServer config.ServerConfig, imageName string) error {
	// Build SSH options for the connection from manager to worker
	sshKeyArg := ""
	if workerServer.SSHKey != "" {
		sshKeyArg = fmt.Sprintf("-i %s", workerServer.SSHKey)
	}

	port := workerServer.Port
	if port == 0 {
		port = 22
	}

	// Stream docker save through SSH to docker load on worker
	// docker save <image> | ssh -o StrictHostKeyChecking=no worker docker load
	streamCmd := fmt.Sprintf(
		"docker save %s | ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null %s -p %d %s@%s docker load",
		imageName,
		sshKeyArg,
		port,
		workerServer.User,
		workerServer.Host,
	)

	if m.verbose {
		fmt.Printf("      Streaming image via SSH...\n")
	}

	output, err := managerClient.Execute(streamCmd)
	if err != nil {
		return fmt.Errorf("failed to stream image: %w, output: %s", err, output)
	}

	return nil
}

// DistributeImageParallel distributes an image to all workers in parallel
func (m *Manager) DistributeImageParallel(managerClient *ssh.Client, imageName string) error {
	if m.verbose {
		fmt.Printf("\n-> Distributing image to worker nodes (parallel)...\n")
		fmt.Printf("   Image: %s\n", imageName)
	}

	// Get all servers for this environment
	servers, err := m.config.GetEnvironmentServers(m.environment)
	if err != nil {
		return fmt.Errorf("failed to get environment servers: %w", err)
	}

	// Get manager server to skip it
	managerName, err := m.config.GetManagerServer(m.environment)
	if err != nil {
		return fmt.Errorf("failed to get manager server: %w", err)
	}

	// Collect workers
	var workers []string
	for _, serverName := range servers {
		if serverName != managerName {
			workers = append(workers, serverName)
		}
	}

	if len(workers) == 0 {
		if m.verbose {
			fmt.Printf("   No worker nodes to distribute to\n")
		}
		return nil
	}

	if m.verbose {
		fmt.Printf("   Distributing to %d worker node(s) in parallel...\n", len(workers))
	}

	// For parallel distribution, we need to save the image to a file first
	// so multiple SSH sessions can read from it simultaneously
	tarPath := fmt.Sprintf("/tmp/tako_image_%s.tar", strings.ReplaceAll(strings.ReplaceAll(imageName, "/", "_"), ":", "_"))

	if m.verbose {
		fmt.Printf("   Saving image to %s...\n", tarPath)
	}

	saveCmd := fmt.Sprintf("docker save %s -o %s", imageName, tarPath)
	if output, err := managerClient.Execute(saveCmd); err != nil {
		return fmt.Errorf("failed to save image: %w, output: %s", err, output)
	}

	// Cleanup tar file when done
	defer func() {
		managerClient.Execute(fmt.Sprintf("rm -f %s", tarPath))
	}()

	// Get manager server config
	managerServer := m.config.Servers[managerName]

	// Create channels for parallel distribution
	type result struct {
		serverName string
		err        error
	}
	results := make(chan result, len(workers))

	// Distribute to each worker in parallel
	for _, serverName := range workers {
		go func(srvName string) {
			server := m.config.Servers[srvName]

			err := m.transferImageFromFile(managerClient, managerServer, server, tarPath, imageName)
			results <- result{srvName, err}
		}(serverName)
	}

	// Collect results
	var errors []string
	for i := 0; i < len(workers); i++ {
		r := <-results
		if r.err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", r.serverName, r.err))
			if m.verbose {
				fmt.Printf("   X %s: failed\n", r.serverName)
			}
		} else {
			if m.verbose {
				fmt.Printf("   ✓ %s: done\n", r.serverName)
			}
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("failed to distribute to some nodes:\n  %s", strings.Join(errors, "\n  "))
	}

	if m.verbose {
		fmt.Printf("   Image distributed successfully to all nodes\n")
	}

	return nil
}

// transferImageFromFile transfers an image from a tar file on manager to a worker
func (m *Manager) transferImageFromFile(managerClient *ssh.Client, managerServer, workerServer config.ServerConfig, tarPath, imageName string) error {
	// Build SSH options
	sshKeyArg := ""
	if workerServer.SSHKey != "" {
		sshKeyArg = fmt.Sprintf("-i %s", workerServer.SSHKey)
	}

	port := workerServer.Port
	if port == 0 {
		port = 22
	}

	// Stream the saved tar file to docker load on worker
	streamCmd := fmt.Sprintf(
		"cat %s | ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null %s -p %d %s@%s docker load",
		tarPath,
		sshKeyArg,
		port,
		workerServer.User,
		workerServer.Host,
	)

	output, err := managerClient.Execute(streamCmd)
	if err != nil {
		return fmt.Errorf("failed to transfer image: %w, output: %s", err, output)
	}

	return nil
}

// CheckImageExists checks if an image exists on a node
func (m *Manager) CheckImageExists(client *ssh.Client, imageName string) (bool, error) {
	checkCmd := fmt.Sprintf("docker image inspect %s > /dev/null 2>&1 && echo 'exists' || echo 'missing'", imageName)
	output, err := client.Execute(checkCmd)
	if err != nil {
		return false, fmt.Errorf("failed to check image: %w", err)
	}

	return strings.TrimSpace(output) == "exists", nil
}

// EnsureImageOnAllNodes ensures an image exists on all Swarm nodes
// Returns true if distribution was needed, false if image already existed everywhere
func (m *Manager) EnsureImageOnAllNodes(managerClient *ssh.Client, imageName string) (bool, error) {
	// Get all servers for this environment
	servers, err := m.config.GetEnvironmentServers(m.environment)
	if err != nil {
		return false, fmt.Errorf("failed to get environment servers: %w", err)
	}

	// Get manager server
	managerName, err := m.config.GetManagerServer(m.environment)
	if err != nil {
		return false, fmt.Errorf("failed to get manager server: %w", err)
	}

	// Check which nodes are missing the image
	var missingNodes []string
	for _, serverName := range servers {
		if serverName == managerName {
			continue // Manager should already have the image
		}

		server := m.config.Servers[serverName]
		client, err := m.sshPool.GetOrCreate(server.Host, server.Port, server.User, server.SSHKey)
		if err != nil {
			return false, fmt.Errorf("failed to connect to %s: %w", serverName, err)
		}

		exists, err := m.CheckImageExists(client, imageName)
		if err != nil {
			return false, fmt.Errorf("failed to check image on %s: %w", serverName, err)
		}

		if !exists {
			missingNodes = append(missingNodes, serverName)
		}
	}

	if len(missingNodes) == 0 {
		if m.verbose {
			fmt.Printf("   Image already exists on all nodes\n")
		}
		return false, nil
	}

	if m.verbose {
		fmt.Printf("   Image missing on %d node(s): %v\n", len(missingNodes), missingNodes)
	}

	// Distribute using parallel method for efficiency
	if err := m.DistributeImageParallel(managerClient, imageName); err != nil {
		return true, err
	}

	return true, nil
}
