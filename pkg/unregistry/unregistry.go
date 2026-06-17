// Package unregistry distributes locally built images across the takod mesh
// without running Docker mutations from the CLI.
package unregistry

import (
	"context"
	"fmt"
	"io"
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
// into every peer node's takod daemon. The CLI brokers bytes over its existing
// SSH sessions so peer nodes do not need operator SSH keys or direct SSH trust
// between each other.
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
	if sourceClient == nil {
		return fmt.Errorf("source SSH client is required")
	}
	if m.sshPool == nil {
		return fmt.Errorf("ssh pool is required")
	}

	port := peerServer.Port
	if port == 0 {
		port = 22
	}
	peerClient, err := m.sshPool.GetOrCreateWithAuth(peerServer.Host, port, peerServer.User, peerServer.SSHKey, peerServer.Password)
	if err != nil {
		return fmt.Errorf("failed to connect to peer node: %w", err)
	}

	socket := m.takodSocket()
	sourceStream, err := sourceClient.StartStream(takodImageExportCommand(socket, imageName))
	if err != nil {
		return fmt.Errorf("failed to start takod image export: %w", err)
	}
	defer sourceStream.Close()

	sourceStderrCh := make(chan string, 1)
	go func() {
		sourceStderrCh <- readLimitedStreamText(sourceStream.Stderr, 64*1024)
	}()

	importOutput, importErr := peerClient.ExecuteWithInput(context.Background(), takodImageImportCommand(socket, imageName), sourceStream.Stdout)
	if importErr != nil {
		_ = sourceStream.Close()
		sourceStderr := <-sourceStderrCh
		if sourceStderr != "" {
			return fmt.Errorf("failed to import image into peer takod: %w, output: %s, source stderr: %s", importErr, strings.TrimSpace(importOutput), sourceStderr)
		}
		return fmt.Errorf("failed to import image into peer takod: %w, output: %s", importErr, strings.TrimSpace(importOutput))
	}

	sourceErr := sourceStream.Wait()
	sourceStderr := <-sourceStderrCh
	if sourceErr != nil {
		if sourceStderr != "" {
			return fmt.Errorf("failed to export image from source takod: %w, output: %s", sourceErr, sourceStderr)
		}
		return fmt.Errorf("failed to export image from source takod: %w", sourceErr)
	}
	return nil
}

func readLimitedStreamText(reader io.Reader, limit int64) string {
	var captured strings.Builder
	if limit > 0 {
		_, _ = io.Copy(&captured, io.LimitReader(reader, limit))
	}
	_, _ = io.Copy(io.Discard, reader)
	return strings.TrimSpace(captured.String())
}

func (m *Manager) takodSocket() string {
	if m.config != nil && m.config.Runtime != nil && m.config.Runtime.Agent != nil && m.config.Runtime.Agent.Socket != "" {
		return m.config.Runtime.Agent.Socket
	}
	return takodclient.DefaultSocket
}

func takodImageExportCommand(socket string, imageName string) string {
	return takodCurlCommand(socket, "GET", "http://takod/v1/images/export?image="+url.QueryEscape(imageName), "")
}

func takodImageImportCommand(socket string, imageName string) string {
	return takodCurlCommand(socket, "POST", "http://takod/v1/images/import?image="+url.QueryEscape(imageName), "--data-binary @-")
}

func takodCurlCommand(socket string, method string, endpoint string, bodyArg string) string {
	if socket == "" {
		socket = takodclient.DefaultSocket
	}
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

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
