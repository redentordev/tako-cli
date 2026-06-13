// Package unregistry distributes locally built images across the takod mesh
// without running Docker mutations from the CLI.
package unregistry

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takodclient"
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

// DistributeImageParallel streams an image from the source node's takod daemon
// into every peer node's takod daemon. SSH is only used as the byte transport;
// docker save/load both run inside takod on the relevant node.
func (m *Manager) DistributeImageParallel(sourceClient *ssh.Client, imageName string) error {
	if m.verbose {
		fmt.Printf("\n-> Distributing image to takod peers...\n")
		fmt.Printf("   Image: %s\n", imageName)
	}

	sourceHost := ""
	if sourceClient != nil {
		sourceHost = sourceClient.Host()
	}
	peers, err := unregistryPeerServers(m.config, m.environment, sourceHost)
	if err != nil {
		return err
	}
	if len(peers) == 0 {
		if m.verbose {
			fmt.Printf("   No peer nodes to distribute to\n")
		}
		return nil
	}

	if m.verbose {
		fmt.Printf("   Streaming to %d peer node(s) in parallel...\n", len(peers))
	}

	type result struct {
		serverName string
		err        error
	}
	results := make(chan result, len(peers))
	for _, serverName := range peers {
		go func(name string) {
			server := m.config.Servers[name]
			err := m.streamImageViaTakod(sourceClient, server, imageName)
			results <- result{serverName: name, err: err}
		}(serverName)
	}

	var failures []string
	for i := 0; i < len(peers); i++ {
		result := <-results
		if result.err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", result.serverName, result.err))
			if m.verbose {
				fmt.Printf("   X %s: failed\n", result.serverName)
			}
			continue
		}
		if m.verbose {
			fmt.Printf("   ✓ %s: done\n", result.serverName)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("failed to distribute image to some nodes:\n  %s", strings.Join(failures, "\n  "))
	}
	if m.verbose {
		fmt.Printf("   Image distributed successfully to all nodes\n")
	}
	return nil
}

func unregistryPeerServers(cfg *config.Config, environment string, sourceHost string) ([]string, error) {
	servers, err := cfg.GetEnvironmentServers(environment)
	if err != nil {
		return nil, fmt.Errorf("failed to get environment servers: %w", err)
	}

	peers := make([]string, 0, len(servers))
	for _, serverName := range servers {
		server, ok := cfg.Servers[serverName]
		if !ok {
			return nil, fmt.Errorf("server %s not found in configuration", serverName)
		}
		if sourceHost != "" && server.Host == sourceHost {
			continue
		}
		peers = append(peers, serverName)
	}
	return peers, nil
}

func (m *Manager) streamImageViaTakod(sourceClient *ssh.Client, peerServer config.ServerConfig, imageName string) error {
	command := buildTakodImageStreamCommand(m.takodSocket(), peerServer, imageName)
	output, err := sourceClient.Execute(command)
	if err != nil {
		return fmt.Errorf("failed to stream image through takod: %w, output: %s", err, output)
	}
	return nil
}

func (m *Manager) takodSocket() string {
	if m.config != nil && m.config.Runtime != nil && m.config.Runtime.Agent != nil && m.config.Runtime.Agent.Socket != "" {
		return m.config.Runtime.Agent.Socket
	}
	return takodclient.DefaultSocket
}

func buildTakodImageStreamCommand(socket string, peerServer config.ServerConfig, imageName string) string {
	if socket == "" {
		socket = takodclient.DefaultSocket
	}
	exportURL := "http://takod/v1/images/export?image=" + url.QueryEscape(imageName)
	importURL := "http://takod/v1/images/import?image=" + url.QueryEscape(imageName)
	sourceCurl := takodCurlCommand(socket, "GET", exportURL, "")
	peerCurl := takodCurlCommand(socket, "POST", importURL, "--data-binary @-")

	port := peerServer.Port
	if port == 0 {
		port = 22
	}
	return fmt.Sprintf(
		"%s | %s -p %d %s %s",
		sourceCurl,
		remoteSSHCommand(peerServer),
		port,
		shellQuote(peerServer.User+"@"+peerServer.Host),
		shellQuote(peerCurl),
	)
}

func takodCurlCommand(socket string, method string, endpoint string, bodyArg string) string {
	parts := []string{
		"if test -S " + shellQuote(socket) + " && command -v curl >/dev/null 2>&1; then",
		"curl --fail --silent --show-error",
		"--unix-socket " + shellQuote(socket),
		"-X " + shellQuote(method),
	}
	if bodyArg != "" {
		parts = append(parts, bodyArg)
	}
	parts = append(parts, shellQuote(endpoint), "; else echo 'takod socket or curl is unavailable' >&2; exit 42; fi")
	return strings.Join(parts, " ")
}

func remoteSSHCommand(peerServer config.ServerConfig) string {
	strictHostKeyChecking := "accept-new"
	if ssh.GetGlobalHostKeyMode() == ssh.HostKeyModeStrict {
		strictHostKeyChecking = "yes"
	}

	parts := []string{
		"ssh",
		"-o StrictHostKeyChecking=" + strictHostKeyChecking,
		"-o UserKnownHostsFile=~/.tako/known_hosts",
	}
	if peerServer.SSHKey != "" {
		parts = append(parts, "-i "+shellQuote(peerServer.SSHKey))
	}
	return strings.Join(parts, " ")
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
