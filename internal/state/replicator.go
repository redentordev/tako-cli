package state

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

// StateReplicator replicates deployment state across the takod mesh as read-only
// backups. Replication is fire-and-forget: failures are logged as warnings and
// never block or fail a deployment.
type StateReplicator struct {
	sshPool     *ssh.Pool
	config      *config.Config
	environment string
	projectName string
	verbose     bool
}

// NewStateReplicator creates a new state replicator.
func NewStateReplicator(pool *ssh.Pool, cfg *config.Config, environment, projectName string, verbose bool) *StateReplicator {
	return &StateReplicator{
		sshPool:     pool,
		config:      cfg,
		environment: environment,
		projectName: projectName,
		verbose:     verbose,
	}
}

// ReplicateDeployment replicates a deployment and its history to mesh peers. It
// returns immediately; the actual replication runs in a background goroutine
// with a 30-second hard timeout. Errors are logged as warnings.
func (r *StateReplicator) ReplicateDeployment(deployment *DeploymentState, history *DeploymentHistory) {
	peers := r.getReplicaServers()
	if len(peers) == 0 {
		return
	}

	// Serialize once
	depData, err := json.MarshalIndent(deployment, "", "  ")
	if err != nil {
		if r.verbose {
			fmt.Fprintf(os.Stderr, "Warning: failed to serialize deployment for replication: %v\n", err)
		}
		return
	}

	var histData []byte
	if history != nil {
		histData, err = json.MarshalIndent(history, "", "  ")
		if err != nil {
			if r.verbose {
				fmt.Fprintf(os.Stderr, "Warning: failed to serialize history for replication: %v\n", err)
			}
			return
		}
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		var wg sync.WaitGroup
		for name, srv := range peers {
			wg.Add(1)
			go func(serverName string, server config.ServerConfig) {
				defer wg.Done()
				r.replicateToNode(ctx, serverName, server, deployment.ID, depData, histData)
			}(name, srv)
		}

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-ctx.Done():
			if r.verbose {
				fmt.Fprintf(os.Stderr, "Warning: state replication timed out after 30s\n")
			}
		}
	}()
}

// replicateToNode writes deployment JSON and history.json to a single peer.
func (r *StateReplicator) replicateToNode(ctx context.Context, serverName string, server config.ServerConfig, deployID string, depData, histData []byte) {
	client, err := r.sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
	if err != nil {
		if r.verbose {
			fmt.Fprintf(os.Stderr, "Warning: replication to %s failed (connect): %v\n", serverName, err)
		}
		return
	}

	statePath := fmt.Sprintf("%s/%s", StateDir, r.projectName)

	// mkdir + write deployment + write history in one SSH call when possible
	mkdirCmd := fmt.Sprintf("sudo mkdir -p %s && sudo chmod -R 755 %s", statePath, StateDir)
	if _, err := client.ExecuteWithContext(ctx, mkdirCmd); err != nil {
		if r.verbose {
			fmt.Fprintf(os.Stderr, "Warning: replication to %s failed (mkdir): %v\n", serverName, err)
		}
		return
	}

	// Write deployment JSON via base64 + atomic move
	if err := r.writeFileViaSSH(ctx, client, statePath, deployID+".json", depData); err != nil {
		if r.verbose {
			fmt.Fprintf(os.Stderr, "Warning: replication to %s failed (deployment): %v\n", serverName, err)
		}
		return
	}

	// Write history.json
	if histData != nil {
		if err := r.writeFileViaSSH(ctx, client, statePath, "history.json", histData); err != nil {
			if r.verbose {
				fmt.Fprintf(os.Stderr, "Warning: replication to %s failed (history): %v\n", serverName, err)
			}
			return
		}
	}

	if r.verbose {
		fmt.Fprintf(os.Stderr, "  ✓ State replicated to node %s\n", serverName)
	}
}

// writeFileViaSSH writes data to a remote file using base64 encoding and atomic move.
func (r *StateReplicator) writeFileViaSSH(ctx context.Context, client *ssh.Client, dir, filename string, data []byte) error {
	tmpFile := fmt.Sprintf("/tmp/tako-replica-%d-%d.json", time.Now().UnixNano(), os.Getpid())
	encoded := base64.StdEncoding.EncodeToString(data)
	cmd := fmt.Sprintf("echo '%s' | base64 -d > %s && sudo mv %s %s/%s", encoded, tmpFile, tmpFile, dir, filename)
	_, err := client.ExecuteWithContext(ctx, cmd)
	if err != nil {
		// Clean up temp file on failure
		client.ExecuteWithContext(ctx, fmt.Sprintf("rm -f %s", tmpFile))
	}
	return err
}

// RecoverStateFromPeers attempts to recover deployment history from mesh
// peers. It reads history.json from all peers in parallel and returns the
// one with the most recent LastUpdated timestamp.
// Returns (nil, "", nil) if no peer has state.
func (r *StateReplicator) RecoverStateFromPeers() (*DeploymentHistory, string, error) {
	peers := r.getReplicaServers()
	if len(peers) == 0 {
		return nil, "", nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	type result struct {
		history *DeploymentHistory
		source  string
	}

	results := make(chan result, len(peers))
	var wg sync.WaitGroup

	for name, srv := range peers {
		wg.Add(1)
		go func(serverName string, server config.ServerConfig) {
			defer wg.Done()

			client, err := r.sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
			if err != nil {
				return
			}

			historyPath := fmt.Sprintf("%s/%s/history.json", StateDir, r.projectName)
			cmd := fmt.Sprintf("sudo cat %s 2>/dev/null", historyPath)
			output, err := client.ExecuteWithContext(ctx, cmd)
			if err != nil || strings.TrimSpace(output) == "" {
				return
			}

			var history DeploymentHistory
			if err := json.Unmarshal([]byte(output), &history); err != nil {
				return
			}

			results <- result{history: &history, source: serverName}
		}(name, srv)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	var best *DeploymentHistory
	var bestSource string

	for res := range results {
		if best == nil || res.history.LastUpdated.After(best.LastUpdated) {
			best = res.history
			bestSource = res.source
		}
	}

	return best, bestSource, nil
}

// getReplicaServers returns all environment nodes except the primary node.
// Returns nil if there are 0 or 1 servers (single-server deployment).
func (r *StateReplicator) getReplicaServers() map[string]config.ServerConfig {
	servers, err := r.config.GetEnvironmentServers(r.environment)
	if err != nil || len(servers) <= 1 {
		return nil
	}

	primaryName, err := r.config.GetPrimaryServer(r.environment)
	if err != nil {
		return nil
	}

	replicas := make(map[string]config.ServerConfig)
	for _, name := range servers {
		if name == primaryName {
			continue
		}
		if srv, exists := r.config.Servers[name]; exists {
			replicas[name] = srv
		}
	}

	if len(replicas) == 0 {
		return nil
	}

	return replicas
}
