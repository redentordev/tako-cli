// Package unregistry provides integration with the unregistry tool for efficient
// Docker image distribution to remote servers without a persistent registry.
//
// Unregistry (https://github.com/psviderski/unregistry) uses SSH tunneling to push
// images directly to remote Docker hosts, transferring only missing layers.
//
// IMPORTANT: docker-pussh runs LOCALLY and pushes images FROM the local machine
// TO a remote server. It is NOT meant to be run on a remote server.
//
// For Tako CLI deployments where images are built on the primary node (not locally),
// we provide a fallback mechanism using docker save/load over SSH.
//
// Benefits of docker-pussh (when images are built locally):
//   - Only transfers missing layers (like rsync for Docker images)
//   - Uses existing SSH connections (no additional ports/firewall rules)
//   - No persistent registry service to maintain
//
// For multi-node takod deployments where images are built on the primary node:
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

// Manager handles image distribution across takod nodes.
type Manager struct {
	config      *config.Config
	sshPool     *ssh.Pool
	environment string
	verbose     bool
}

// NewManager creates a new unregistry manager.
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

// DistributeImage distributes a Docker image from the primary node to all peers
// using docker save/load over SSH. This is the reliable method for Tako deployments
// where images are built on the primary node (not locally).
func (m *Manager) DistributeImage(sourceClient *ssh.Client, imageName string) error {
	if m.verbose {
		fmt.Printf("\n-> Distributing image to peer nodes...\n")
		fmt.Printf("   Image: %s\n", imageName)
	}

	// Get all servers for this environment
	servers, err := m.config.GetEnvironmentServers(m.environment)
	if err != nil {
		return fmt.Errorf("failed to get environment servers: %w", err)
	}

	// Get primary node to skip it.
	primaryName, err := m.config.GetPrimaryServer(m.environment)
	if err != nil {
		return fmt.Errorf("failed to get primary node: %w", err)
	}

	peerCount := 0
	for _, serverName := range servers {
		if serverName != primaryName {
			peerCount++
		}
	}

	if peerCount == 0 {
		if m.verbose {
			fmt.Printf("   No peer nodes to distribute to (single-node deployment)\n")
		}
		return nil
	}

	if m.verbose {
		fmt.Printf("   Distributing to %d peer node(s)...\n", peerCount)
	}

	// Save image to tar on the primary node (streamed approach - no temp file)
	// and pipe docker save directly through SSH to docker load on peers.
	for _, serverName := range servers {
		if serverName == primaryName {
			continue
		}

		server := m.config.Servers[serverName]
		if m.verbose {
			fmt.Printf("   -> %s (%s)...\n", serverName, server.Host)
		}

		// Transfer image using piped docker save | ssh docker load
		// This streams the image directly without creating a temp file
		if err := m.transferImageStream(sourceClient, server, imageName); err != nil {
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

// transferImageStream transfers an image from the source node to a peer using streamed docker save | ssh docker load.
// This avoids creating temporary files and is more efficient for large images
func (m *Manager) transferImageStream(sourceClient *ssh.Client, peerServer config.ServerConfig, imageName string) error {
	// Build SSH options for the connection from source node to peer.
	sshKeyArg := ""
	if peerServer.SSHKey != "" {
		sshKeyArg = fmt.Sprintf("-i %s", peerServer.SSHKey)
	}

	port := peerServer.Port
	if port == 0 {
		port = 22
	}

	// Stream docker save through SSH to docker load on the peer.
	streamCmd := fmt.Sprintf(
		"docker save %s | ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null %s -p %d %s@%s docker load",
		imageName,
		sshKeyArg,
		port,
		peerServer.User,
		peerServer.Host,
	)

	if m.verbose {
		fmt.Printf("      Streaming image via SSH...\n")
	}

	output, err := sourceClient.Execute(streamCmd)
	if err != nil {
		return fmt.Errorf("failed to stream image: %w, output: %s", err, output)
	}

	return nil
}

// DistributeImageParallel distributes an image to all peers in parallel.
func (m *Manager) DistributeImageParallel(sourceClient *ssh.Client, imageName string) error {
	if m.verbose {
		fmt.Printf("\n-> Distributing image to peer nodes (parallel)...\n")
		fmt.Printf("   Image: %s\n", imageName)
	}

	// Get all servers for this environment
	servers, err := m.config.GetEnvironmentServers(m.environment)
	if err != nil {
		return fmt.Errorf("failed to get environment servers: %w", err)
	}

	// Get primary node to skip it.
	primaryName, err := m.config.GetPrimaryServer(m.environment)
	if err != nil {
		return fmt.Errorf("failed to get primary node: %w", err)
	}

	var peers []string
	for _, serverName := range servers {
		if serverName != primaryName {
			peers = append(peers, serverName)
		}
	}

	if len(peers) == 0 {
		if m.verbose {
			fmt.Printf("   No peer nodes to distribute to\n")
		}
		return nil
	}

	if m.verbose {
		fmt.Printf("   Distributing to %d peer node(s) in parallel...\n", len(peers))
	}

	// For parallel distribution, we need to save the image to a file first
	// so multiple SSH sessions can read from it simultaneously
	tarPath := fmt.Sprintf("/tmp/tako_image_%s.tar", strings.ReplaceAll(strings.ReplaceAll(imageName, "/", "_"), ":", "_"))

	if m.verbose {
		fmt.Printf("   Saving image to %s...\n", tarPath)
	}

	saveCmd := fmt.Sprintf("docker save %s -o %s", imageName, tarPath)
	if output, err := sourceClient.Execute(saveCmd); err != nil {
		return fmt.Errorf("failed to save image: %w, output: %s", err, output)
	}

	// Cleanup tar file when done
	defer func() {
		sourceClient.Execute(fmt.Sprintf("rm -f %s", tarPath))
	}()

	// Create channels for parallel distribution
	type result struct {
		serverName string
		err        error
	}
	results := make(chan result, len(peers))

	// Distribute to each peer in parallel.
	for _, serverName := range peers {
		go func(srvName string) {
			server := m.config.Servers[srvName]

			err := m.transferImageFromFile(sourceClient, server, tarPath, imageName)
			results <- result{srvName, err}
		}(serverName)
	}

	// Collect results
	var errors []string
	for i := 0; i < len(peers); i++ {
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

// transferImageFromFile transfers an image from a tar file on the source node to a peer.
func (m *Manager) transferImageFromFile(sourceClient *ssh.Client, peerServer config.ServerConfig, tarPath, imageName string) error {
	// Build SSH options
	sshKeyArg := ""
	if peerServer.SSHKey != "" {
		sshKeyArg = fmt.Sprintf("-i %s", peerServer.SSHKey)
	}

	port := peerServer.Port
	if port == 0 {
		port = 22
	}

	// Stream the saved tar file to docker load on the peer.
	streamCmd := fmt.Sprintf(
		"cat %s | ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null %s -p %d %s@%s docker load",
		tarPath,
		sshKeyArg,
		port,
		peerServer.User,
		peerServer.Host,
	)

	output, err := sourceClient.Execute(streamCmd)
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

// EnsureImageOnAllNodes ensures an image exists on all takod nodes.
// Returns true if distribution was needed, false if image already existed everywhere
func (m *Manager) EnsureImageOnAllNodes(sourceClient *ssh.Client, imageName string) (bool, error) {
	// Get all servers for this environment
	servers, err := m.config.GetEnvironmentServers(m.environment)
	if err != nil {
		return false, fmt.Errorf("failed to get environment servers: %w", err)
	}

	// Get primary node.
	primaryName, err := m.config.GetPrimaryServer(m.environment)
	if err != nil {
		return false, fmt.Errorf("failed to get primary node: %w", err)
	}

	// Check which nodes are missing the image
	var missingNodes []string
	for _, serverName := range servers {
		if serverName == primaryName {
			continue
		}

		server := m.config.Servers[serverName]
		client, err := m.sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
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
	if err := m.DistributeImageParallel(sourceClient, imageName); err != nil {
		return true, err
	}

	return true, nil
}
