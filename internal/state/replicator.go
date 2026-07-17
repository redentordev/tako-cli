package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/stateclient"
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
	runtime     *nodeclient.Factory
	runtimeOnce sync.Once
	runtimeErr  error

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
	defer func() {
		if r.runtime != nil {
			r.runtime.CloseIdleConnections()
		}
	}()

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
	factory, err := r.runtimeClientFactory()
	if err != nil {
		return err
	}
	client, _, err := factory.Client(ctx, serverName)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	deploymentDocument, err := deploymentStateDocument(deployment)
	if err != nil {
		return fmt.Errorf("clone deployment: %w", err)
	}
	historyDocument, err := deploymentHistoryDocument(history)
	if err != nil {
		return fmt.Errorf("clone history: %w", err)
	}
	r.prepareReplicaDocuments(server.Host, deploymentDocument, historyDocument)

	if err := stateclient.New(client).WithSocket(r.socket).ReplicateDeploymentContext(ctx, *deploymentDocument, historyDocument); err != nil {
		return err
	}

	if r.verbose {
		fmt.Printf("  ✓ State replicated to node %s\n", serverName)
	}
	return nil
}

func (r *StateReplicator) runtimeClientFactory() (*nodeclient.Factory, error) {
	r.runtimeOnce.Do(func() {
		r.runtime, r.runtimeErr = nodeclient.NewFactory(r.config, r.sshPool, r.socket)
	})
	if r.runtimeErr != nil {
		return nil, r.runtimeErr
	}
	return r.runtime, nil
}

func (r *StateReplicator) prepareReplicaDocuments(serverHost string, deployment *takoapi.DeploymentStateDocument, history *takoapi.DeploymentHistoryDocument) {
	if deployment != nil {
		if deployment.ID == "" {
			deployment.ID = fmt.Sprintf("%d_%d", time.Now().UnixNano(), os.Getpid())
		}
		deployment.ProjectName = r.projectName
		deployment.Environment = r.environment
		deployment.Host = serverHost
	}
	if history != nil {
		history.ProjectName = r.projectName
		history.Environment = r.environment
		history.Server = serverHost
		history.LastUpdated = time.Now().UTC()
	}
}

func deploymentStateDocument(deployment *DeploymentState) (*takoapi.DeploymentStateDocument, error) {
	if deployment == nil {
		return nil, fmt.Errorf("deployment is nil")
	}
	data, err := json.Marshal(deployment)
	if err != nil {
		return nil, err
	}
	var out takoapi.DeploymentStateDocument
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func deploymentHistoryDocument(history *DeploymentHistory) (*takoapi.DeploymentHistoryDocument, error) {
	if history == nil {
		return nil, nil
	}
	data, err := json.Marshal(history)
	if err != nil {
		return nil, err
	}
	var out takoapi.DeploymentHistoryDocument
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
		if srv, exists := r.config.Servers[name]; exists && srv.Schedulable() {
			replicas[name] = srv
		}
	}
	if len(replicas) == 0 {
		return nil
	}
	return replicas
}
