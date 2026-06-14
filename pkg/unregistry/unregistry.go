// Package unregistry distributes locally built images across the takod mesh
// without running Docker mutations from the CLI.
package unregistry

import (
	"bytes"
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
	if m.sshPool == nil {
		return fmt.Errorf("ssh pool is required for image distribution")
	}

	peerClient, err := m.sshPool.GetOrCreateWithAuth(peerServer.Host, peerServer.Port, peerServer.User, peerServer.SSHKey, peerServer.Password)
	if err != nil {
		return fmt.Errorf("failed to connect to peer: %w", err)
	}
	return streamImageViaTakodClients(sourceClient, peerClient, m.takodSocket(), imageName)
}

func (m *Manager) takodSocket() string {
	if m.config != nil && m.config.Runtime != nil && m.config.Runtime.Agent != nil && m.config.Runtime.Agent.Socket != "" {
		return m.config.Runtime.Agent.Socket
	}
	return takodclient.DefaultSocket
}

func streamImageViaTakodClients(sourceClient *ssh.Client, peerClient *ssh.Client, socket string, imageName string) error {
	if sourceClient == nil {
		return fmt.Errorf("source client is required")
	}
	if peerClient == nil {
		return fmt.Errorf("peer client is required")
	}

	exportCommand := buildTakodImageExportCommand(socket, imageName)
	importCommand := buildTakodImageImportCommand(socket, imageName)
	stream, err := sourceClient.StartStream(exportCommand)
	if err != nil {
		return fmt.Errorf("failed to start takod image export: %w", err)
	}
	defer stream.Close()

	var exportStderr bytes.Buffer
	stderrDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(&exportStderr, stream.Stderr)
		stderrDone <- copyErr
	}()

	output, importErr := peerClient.ExecuteWithInput(context.Background(), importCommand, stream.Stdout)
	if importErr != nil {
		_ = stream.Close()
	}

	exportErr := stream.Wait()
	stderrErr := <-stderrDone
	exportStderrText := strings.TrimSpace(exportStderr.String())

	if importErr != nil {
		return fmt.Errorf("failed to import image through peer takod: %w, output: %s, export stderr: %s", importErr, strings.TrimSpace(output), exportStderrText)
	}
	if exportErr != nil {
		return fmt.Errorf("failed to export image through source takod: %w, stderr: %s", exportErr, exportStderrText)
	}
	if stderrErr != nil {
		return fmt.Errorf("failed to read source export stderr: %w", stderrErr)
	}
	return nil
}

func buildTakodImageExportCommand(socket string, imageName string) string {
	if socket == "" {
		socket = takodclient.DefaultSocket
	}
	exportURL := "http://takod/v1/images/export?image=" + url.QueryEscape(imageName)
	return takodCurlCommand(socket, "GET", exportURL, "")
}

func buildTakodImageImportCommand(socket string, imageName string) string {
	if socket == "" {
		socket = takodclient.DefaultSocket
	}
	importURL := "http://takod/v1/images/import?image=" + url.QueryEscape(imageName)
	return takodCurlCommand(socket, "POST", importURL, "--http1.1 -H "+shellQuote("Transfer-Encoding: chunked")+" --upload-file -")
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

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
