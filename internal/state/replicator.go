package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

// StateReplicator replicates deployment history across the takod mesh.
type StateReplicator struct {
	sshPool     *ssh.Pool
	config      *config.Config
	environment string
	projectName string
	socket      string
	verbose     bool

	replicateNode func(context.Context, string, config.ServerConfig, *DeploymentState, *DeploymentHistory) error
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

// ReplicateDeployment replicates a deployment and its history to every
// environment mesh node using the legacy 30s background timeout. Writes are
// idempotent, so including the node that initially saved the deployment is
// acceptable.
func (r *StateReplicator) ReplicateDeployment(deployment *DeploymentState, history *DeploymentHistory) error {
	return r.ReplicateDeploymentContext(context.Background(), deployment, history)
}

// ReplicateDeploymentContext replicates a deployment and its history to every
// environment mesh node, bounded by both ctx and the legacy 30s timeout.
func (r *StateReplicator) ReplicateDeploymentContext(ctx context.Context, deployment *DeploymentState, history *DeploymentHistory) error {
	peers := r.getReplicaServers()
	if len(peers) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	parentCtx := ctx
	ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer cancel()

	replicateNode := r.replicateToNode
	if r.replicateNode != nil {
		replicateNode = r.replicateNode
	}

	names := make([]string, 0, len(peers))
	for name := range peers {
		names = append(names, name)
	}
	sort.Strings(names)

	errs := make(chan error, len(names))
	var wg sync.WaitGroup
	for _, name := range names {
		server := peers[name]
		wg.Add(1)
		go func(serverName string, server config.ServerConfig) {
			defer wg.Done()
			if err := replicateNode(ctx, serverName, server, deployment, history); err != nil {
				errs <- fmt.Errorf("%s: %w", serverName, err)
			}
		}(name, server)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		if parentErr := parentCtx.Err(); parentErr != nil {
			return fmt.Errorf("state replication cancelled: %w", parentErr)
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("state replication timed out after 30s: %w", ctx.Err())
		}
		return fmt.Errorf("state replication cancelled: %w", ctx.Err())
	}

	close(errs)
	var joined []error
	for err := range errs {
		joined = append(joined, err)
	}
	if len(joined) > 0 {
		return fmt.Errorf("state replication failed: %w", errors.Join(joined...))
	}
	return nil
}

func (r *StateReplicator) replicateToNode(ctx context.Context, serverName string, server config.ServerConfig, deployment *DeploymentState, history *DeploymentHistory) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if r.sshPool == nil {
		return fmt.Errorf("ssh pool is nil")
	}

	client, err := r.sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	deploymentCopy, err := cloneDeploymentState(deployment)
	if err != nil {
		return fmt.Errorf("clone deployment: %w", err)
	}
	historyCopy, err := cloneDeploymentHistory(history)
	if err != nil {
		return fmt.Errorf("clone history: %w", err)
	}

	manager := NewStateManagerWithSocket(client, r.projectName, r.environment, server.Host, r.socket)
	if err := manager.SaveDeploymentContext(ctx, deploymentCopy); err != nil {
		return fmt.Errorf("deployment: %w", err)
	}
	if historyCopy != nil {
		if err := manager.SaveHistoryContext(ctx, historyCopy); err != nil {
			return fmt.Errorf("history: %w", err)
		}
	}

	if r.verbose {
		fmt.Printf("  ✓ State replicated to node %s\n", serverName)
	}
	return nil
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
