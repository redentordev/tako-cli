package state

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

// StateReplicator replicates deployment history across the takod mesh as
// read-only backups. Replication is fire-and-forget: failures are logged as
// warnings and never block or fail a deployment.
type StateReplicator struct {
	sshPool     *ssh.Pool
	config      *config.Config
	environment string
	projectName string
	socket      string
	verbose     bool
}

// NewStateReplicator creates a new state replicator.
func NewStateReplicator(pool *ssh.Pool, cfg *config.Config, environment, projectName string, verbose bool) *StateReplicator {
	socket := takodclient.DefaultSocket
	if cfg.Runtime != nil && cfg.Runtime.Agent != nil && cfg.Runtime.Agent.Socket != "" {
		socket = cfg.Runtime.Agent.Socket
	}
	return &StateReplicator{
		sshPool:     pool,
		config:      cfg,
		environment: environment,
		projectName: projectName,
		socket:      socket,
		verbose:     verbose,
	}
}

// ReplicateDeployment replicates a deployment and its history to environment
// mesh nodes. Writes are idempotent, so including the node that initially saved
// the deployment is acceptable.
func (r *StateReplicator) ReplicateDeployment(deployment *DeploymentState, history *DeploymentHistory) {
	peers := r.getReplicaServers()
	if len(peers) == 0 {
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		var wg sync.WaitGroup
		for name, srv := range peers {
			wg.Add(1)
			go func(serverName string, server config.ServerConfig) {
				defer wg.Done()
				r.replicateToNode(ctx, serverName, server, deployment, history)
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

func (r *StateReplicator) replicateToNode(ctx context.Context, serverName string, server config.ServerConfig, deployment *DeploymentState, history *DeploymentHistory) {
	select {
	case <-ctx.Done():
		return
	default:
	}

	client, err := r.sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
	if err != nil {
		if r.verbose {
			fmt.Fprintf(os.Stderr, "Warning: replication to %s failed (connect): %v\n", serverName, err)
		}
		return
	}

	deploymentCopy, err := cloneDeploymentState(deployment)
	if err != nil {
		if r.verbose {
			fmt.Fprintf(os.Stderr, "Warning: replication to %s failed (clone deployment): %v\n", serverName, err)
		}
		return
	}
	historyCopy, err := cloneDeploymentHistory(history)
	if err != nil {
		if r.verbose {
			fmt.Fprintf(os.Stderr, "Warning: replication to %s failed (clone history): %v\n", serverName, err)
		}
		return
	}

	manager := NewStateManagerWithSocket(client, r.projectName, r.environment, server.Host, r.socket)
	if err := manager.SaveDeployment(deploymentCopy); err != nil {
		if r.verbose {
			fmt.Fprintf(os.Stderr, "Warning: replication to %s failed (deployment): %v\n", serverName, err)
		}
		return
	}
	if historyCopy != nil {
		if err := manager.SaveHistory(historyCopy); err != nil {
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

func cloneDeploymentState(deployment *DeploymentState) (*DeploymentState, error) {
	if deployment == nil {
		return nil, fmt.Errorf("deployment is nil")
	}
	data, err := json.Marshal(deployment)
	if err != nil {
		return nil, err
	}
	var out DeploymentState
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func cloneDeploymentHistory(history *DeploymentHistory) (*DeploymentHistory, error) {
	if history == nil {
		return nil, nil
	}
	data, err := json.Marshal(history)
	if err != nil {
		return nil, err
	}
	var out DeploymentHistory
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (r *StateReplicator) getReplicaServers() map[string]config.ServerConfig {
	servers, err := r.config.GetEnvironmentServers(r.environment)
	if err != nil || len(servers) <= 1 {
		return nil
	}

	replicas := make(map[string]config.ServerConfig)
	for _, name := range servers {
		if srv, exists := r.config.Servers[name]; exists {
			replicas[name] = srv
		}
	}
	if len(replicas) == 0 {
		return nil
	}
	return replicas
}
