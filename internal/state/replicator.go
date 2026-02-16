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

// StateReplicator replicates deployment state from the manager node to worker
// nodes as read-only backups. If the manager loses state, it can be recovered
// from any worker. Replication is fire-and-forget — failures are logged as
// warnings and never block or fail a deployment.
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

// ReplicateDeployment replicates a deployment and its history to all worker
// nodes. It returns immediately — the actual replication runs in a background
// goroutine with a 30-second hard timeout. Errors are logged as warnings.
func (r *StateReplicator) ReplicateDeployment(deployment *DeploymentState, history *DeploymentHistory) {
	workers := r.getWorkerServers()
	if len(workers) == 0 {
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
		for name, srv := range workers {
			wg.Add(1)
			go func(serverName string, server config.ServerConfig) {
				defer wg.Done()
				r.replicateToWorker(ctx, serverName, server, deployment.ID, depData, histData)
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

// replicateToWorker writes deployment JSON and history.json to a single worker.
func (r *StateReplicator) replicateToWorker(ctx context.Context, serverName string, server config.ServerConfig, deployID string, depData, histData []byte) {
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
		fmt.Fprintf(os.Stderr, "  ✓ State replicated to worker %s\n", serverName)
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

// ReplicateSwarmTokens replicates pre-encrypted swarm token data to all worker
// nodes. Returns immediately with a 15-second background timeout.
func (r *StateReplicator) ReplicateSwarmTokens(encryptedData []byte) {
	workers := r.getWorkerServers()
	if len(workers) == 0 {
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		var wg sync.WaitGroup
		for name, srv := range workers {
			wg.Add(1)
			go func(serverName string, server config.ServerConfig) {
				defer wg.Done()
				r.replicateSwarmTokenToWorker(ctx, serverName, server, encryptedData)
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
				fmt.Fprintf(os.Stderr, "Warning: swarm token replication timed out after 15s\n")
			}
		}
	}()
}

// replicateSwarmTokenToWorker writes swarm_state.enc to a single worker.
func (r *StateReplicator) replicateSwarmTokenToWorker(ctx context.Context, serverName string, server config.ServerConfig, encData []byte) {
	client, err := r.sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
	if err != nil {
		if r.verbose {
			fmt.Fprintf(os.Stderr, "Warning: swarm token replication to %s failed (connect): %v\n", serverName, err)
		}
		return
	}

	statePath := fmt.Sprintf("%s/%s", StateDir, r.projectName)
	mkdirCmd := fmt.Sprintf("sudo mkdir -p %s && sudo chmod -R 755 %s", statePath, StateDir)
	if _, err := client.ExecuteWithContext(ctx, mkdirCmd); err != nil {
		if r.verbose {
			fmt.Fprintf(os.Stderr, "Warning: swarm token replication to %s failed (mkdir): %v\n", serverName, err)
		}
		return
	}

	if err := r.writeFileViaSSH(ctx, client, statePath, "swarm_state.enc", encData); err != nil {
		if r.verbose {
			fmt.Fprintf(os.Stderr, "Warning: swarm token replication to %s failed (write): %v\n", serverName, err)
		}
		return
	}

	if r.verbose {
		fmt.Fprintf(os.Stderr, "  ✓ Swarm tokens replicated to worker %s\n", serverName)
	}
}

// RecoverStateFromWorkers attempts to recover deployment history from worker
// nodes. It reads history.json from all workers in parallel and returns the
// one with the most recent LastUpdated timestamp.
// Returns (nil, "", nil) if no worker has state.
func (r *StateReplicator) RecoverStateFromWorkers() (*DeploymentHistory, string, error) {
	workers := r.getWorkerServers()
	if len(workers) == 0 {
		return nil, "", nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	type result struct {
		history *DeploymentHistory
		source  string
	}

	results := make(chan result, len(workers))
	var wg sync.WaitGroup

	for name, srv := range workers {
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

// RecoverSwarmTokensFromWorkers attempts to recover encrypted swarm tokens
// from worker nodes. Returns the first non-empty result found.
// Returns (nil, "", nil) if no worker has tokens.
func (r *StateReplicator) RecoverSwarmTokensFromWorkers() ([]byte, string, error) {
	workers := r.getWorkerServers()
	if len(workers) == 0 {
		return nil, "", nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	type result struct {
		data   []byte
		source string
	}

	results := make(chan result, len(workers))
	var wg sync.WaitGroup

	for name, srv := range workers {
		wg.Add(1)
		go func(serverName string, server config.ServerConfig) {
			defer wg.Done()

			client, err := r.sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
			if err != nil {
				return
			}

			tokenPath := fmt.Sprintf("%s/%s/swarm_state.enc", StateDir, r.projectName)
			// Read as base64 since it's binary encrypted data
			cmd := fmt.Sprintf("sudo base64 %s 2>/dev/null", tokenPath)
			output, err := client.ExecuteWithContext(ctx, cmd)
			if err != nil || strings.TrimSpace(output) == "" {
				return
			}

			data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(output))
			if err != nil {
				return
			}

			results <- result{data: data, source: serverName}
		}(name, srv)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	// Return first non-empty result
	for res := range results {
		if len(res.data) > 0 {
			return res.data, res.source, nil
		}
	}

	return nil, "", nil
}

// getWorkerServers returns all environment servers except the manager.
// Returns nil if there are 0 or 1 servers (single-server deployment).
func (r *StateReplicator) getWorkerServers() map[string]config.ServerConfig {
	servers, err := r.config.GetEnvironmentServers(r.environment)
	if err != nil || len(servers) <= 1 {
		return nil
	}

	managerName, err := r.config.GetManagerServer(r.environment)
	if err != nil {
		return nil
	}

	workers := make(map[string]config.ServerConfig)
	for _, name := range servers {
		if name == managerName {
			continue
		}
		if srv, exists := r.config.Servers[name]; exists {
			workers[name] = srv
		}
	}

	if len(workers) == 0 {
		return nil
	}

	return workers
}
