package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
)

const (
	// KindStateLeaseResult identifies a serialized state lease read result.
	KindStateLeaseResult = "StateLeaseResult"
	// KindStateLeaseReleaseResult identifies a serialized state lease release result.
	KindStateLeaseReleaseResult = "StateLeaseReleaseResult"
)

// StateLeaseRequest describes one remote lease inspection operation.
type StateLeaseRequest struct {
	Config      *config.Config `json:"-"`
	Environment string         `json:"environment"`
	// Server is the optional requested server filter from --server.
	Server string `json:"server,omitempty"`
	// SSHPool optionally supplies the SSH pool to use. Nil creates and owns one.
	SSHPool *ssh.Pool `json:"-"`
}

// StateLeaseResult is the serializable outcome of StateLease.
type StateLeaseResult struct {
	APIVersion  string                 `json:"apiVersion"`
	Kind        string                 `json:"kind"`
	Project     string                 `json:"project"`
	Environment string                 `json:"environment"`
	Server      string                 `json:"server,omitempty"`
	Servers     []string               `json:"servers"`
	Nodes       []StateLeaseNodeResult `json:"nodes"`
}

// StateLeaseNodeResult is one node's current remote lease status.
type StateLeaseNodeResult struct {
	Name  string                 `json:"name"`
	Host  string                 `json:"host,omitempty"`
	Lease *remotestate.LeaseInfo `json:"lease,omitempty"`
	Error string                 `json:"error,omitempty"`

	Manager RemoteLeaseManager `json:"-"`
	Err     error              `json:"-"`
}

// StateLeaseReleaseRequest describes a forced/unlock-style exact-ID lease release.
type StateLeaseReleaseRequest struct {
	Config      *config.Config `json:"-"`
	Environment string         `json:"environment"`
	// Server is the optional requested server filter from --server.
	Server string `json:"server,omitempty"`
	ID     string `json:"id"`
	Force  bool   `json:"force,omitempty"`
	// Now is a test seam. Zero uses time.Now().UTC().
	Now time.Time `json:"-"`
	// SSHPool optionally supplies the SSH pool to use. Nil creates and owns one.
	SSHPool *ssh.Pool `json:"-"`
}

// StateLeaseReleaseResult is the serializable outcome of ReleaseStateLease.
type StateLeaseReleaseResult struct {
	APIVersion    string                 `json:"apiVersion"`
	Kind          string                 `json:"kind"`
	Project       string                 `json:"project"`
	Environment   string                 `json:"environment"`
	Server        string                 `json:"server,omitempty"`
	Servers       []string               `json:"servers"`
	LeaseID       string                 `json:"leaseId"`
	Force         bool                   `json:"force,omitempty"`
	Released      []string               `json:"releasedNodes"`
	ReleasedCount int                    `json:"releasedCount"`
	Nodes         []StateLeaseNodeResult `json:"nodes"`
}

// StateLease reads current remote operation leases from the selected nodes.
func (e *Engine) StateLease(ctx context.Context, req StateLeaseRequest) (*StateLeaseResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.Config == nil {
		return nil, invalidRequestf("state lease request requires a loaded config")
	}
	if strings.TrimSpace(req.Environment) == "" {
		return nil, invalidRequestf("state lease request requires an environment")
	}
	cfg := req.Config
	for _, server := range cfg.Servers {
		e.RegisterSecret(server.Password)
	}

	envName := strings.TrimSpace(req.Environment)
	serverNames, err := ResolveStateLeaseTargetServerNames(cfg, envName, req.Server)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	pool := req.SSHPool
	if pool == nil {
		pool = ssh.NewPool()
		defer pool.CloseAll()
	}
	nodes, err := CollectStateLeaseNodes(ctx, pool, cfg, envName, serverNames)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return &StateLeaseResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindStateLeaseResult,
		Project:     cfg.Project.Name,
		Environment: envName,
		Server:      strings.TrimSpace(req.Server),
		Servers:     append([]string(nil), serverNames...),
		Nodes:       nodes,
	}, nil
}

// ReleaseStateLease reads current leases and releases the exact matching ID on
// every selected reachable node. Active leases require Force.
func (e *Engine) ReleaseStateLease(ctx context.Context, req StateLeaseReleaseRequest) (*StateLeaseReleaseResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pool := req.SSHPool
	if pool == nil {
		pool = ssh.NewPool()
		defer pool.CloseAll()
	}
	leaseResult, err := e.StateLease(ctx, StateLeaseRequest{
		Config:      req.Config,
		Environment: req.Environment,
		Server:      req.Server,
		SSHPool:     pool,
	})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	released, releaseErr := ReleaseStateLeaseByIDContext(ctx, leaseResult.Nodes, req.ID, req.Force, now)
	result := &StateLeaseReleaseResult{
		APIVersion:    takoapi.APIVersionCurrent,
		Kind:          KindStateLeaseReleaseResult,
		Project:       leaseResult.Project,
		Environment:   leaseResult.Environment,
		Server:        leaseResult.Server,
		Servers:       append([]string(nil), leaseResult.Servers...),
		LeaseID:       strings.TrimSpace(req.ID),
		Force:         req.Force,
		Released:      append([]string(nil), released...),
		ReleasedCount: len(released),
		Nodes:         leaseResult.Nodes,
	}
	return result, releaseErr
}

// ResolveStateLeaseTargetServerNames resolves state lease --server selection
// against configured environment nodes.
func ResolveStateLeaseTargetServerNames(cfg *config.Config, envName string, requestedServer string) ([]string, error) {
	envServers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return nil, fmt.Errorf("failed to get environment servers: %w", err)
	}
	if len(envServers) == 0 {
		return nil, invalidRequestf("no servers configured for environment %s", envName)
	}
	requestedServer = strings.TrimSpace(requestedServer)
	if requestedServer == "" {
		return envServers, nil
	}
	if _, ok := cfg.Servers[requestedServer]; !ok {
		return nil, invalidRequestf("server %s not found in configuration", requestedServer)
	}
	for _, serverName := range envServers {
		if serverName == requestedServer {
			return []string{requestedServer}, nil
		}
	}
	return nil, invalidRequestf("server %s is not part of environment %s", requestedServer, envName)
}

// CollectStateLeaseNodes reads current leases from serverNames, preserving order.
func CollectStateLeaseNodes(ctx context.Context, pool *ssh.Pool, cfg *config.Config, envName string, serverNames []string) ([]StateLeaseNodeResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, invalidRequestf("state lease collection requires a loaded config")
	}
	ownPool := false
	if pool == nil {
		pool = ssh.NewPool()
		ownPool = true
	}
	if ownPool {
		defer pool.CloseAll()
	}
	factory, err := nodeclient.NewFactory(cfg, pool, TakodSocketFromConfig(cfg))
	if err != nil {
		return nil, err
	}
	defer factory.CloseIdleConnections()

	nodes := make([]StateLeaseNodeResult, len(serverNames))
	resultCh := make(chan struct {
		index int
		node  StateLeaseNodeResult
	}, len(serverNames))
	var wg sync.WaitGroup
	for index, serverName := range serverNames {
		server, ok := cfg.Servers[serverName]
		if !ok {
			return nil, invalidRequestf("server %s not found in configuration", serverName)
		}
		wg.Add(1)
		go func(index int, serverName string, server config.ServerConfig) {
			defer wg.Done()
			node := StateLeaseNodeResult{Name: serverName, Host: server.Host}
			if err := ctx.Err(); err != nil {
				node.Err = err
				node.Error = err.Error()
				resultCh <- struct {
					index int
					node  StateLeaseNodeResult
				}{index: index, node: node}
				return
			}

			client, _, err := factory.Client(ctx, serverName)
			if err != nil {
				node.Err = err
				node.Error = err.Error()
				resultCh <- struct {
					index int
					node  StateLeaseNodeResult
				}{index: index, node: node}
				return
			}
			if err := ctx.Err(); err != nil {
				node.Err = err
				node.Error = err.Error()
				resultCh <- struct {
					index int
					node  StateLeaseNodeResult
				}{index: index, node: node}
				return
			}

			manager := remotestate.NewStateManagerWithSocket(client, cfg.Project.Name, envName, server.Host, TakodSocketFromConfig(cfg))
			node.Manager = manager
			node.Lease, node.Err = manager.ReadLeaseContext(ctx)
			if node.Err == nil {
				node.Err = ctx.Err()
			}
			if node.Err != nil {
				node.Error = node.Err.Error()
			}
			resultCh <- struct {
				index int
				node  StateLeaseNodeResult
			}{index: index, node: node}
		}(index, serverName, server)
	}
	wg.Wait()
	close(resultCh)
	for result := range resultCh {
		nodes[result.index] = result.node
	}
	return nodes, nil
}

// ReleaseStateLeaseByID releases a matching lease ID from already-collected nodes.
func ReleaseStateLeaseByID(nodes []StateLeaseNodeResult, leaseID string, force bool, now time.Time) ([]string, error) {
	return ReleaseStateLeaseByIDContext(context.Background(), nodes, leaseID, force, now)
}

// ReleaseStateLeaseByIDContext releases a matching lease ID from already-collected nodes bounded by ctx.
func ReleaseStateLeaseByIDContext(ctx context.Context, nodes []StateLeaseNodeResult, leaseID string, force bool, now time.Time) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	leaseID = strings.TrimSpace(leaseID)
	if leaseID == "" {
		return nil, fmt.Errorf("--id is required")
	}

	var released []string
	var releaseErrors []string
	var nodeErrors []string
	found := false
	for _, node := range nodes {
		if err := ctx.Err(); err != nil {
			return released, err
		}
		if node.Err != nil {
			nodeErrors = append(nodeErrors, fmt.Sprintf("%s: %v", node.Name, node.Err))
			continue
		}
		if node.Lease == nil || node.Lease.ID != leaseID {
			continue
		}
		found = true
		if node.Manager == nil {
			releaseErrors = append(releaseErrors, fmt.Sprintf("%s: lease manager unavailable", node.Name))
			continue
		}
		if !force && now.Before(node.Lease.ExpiresAt) {
			releaseErrors = append(releaseErrors, fmt.Sprintf("%s: lease has not expired yet; use --force to release it", node.Name))
			continue
		}
		if contextManager, ok := node.Manager.(RemoteLeaseContextManager); ok {
			if err := contextManager.ReleaseLeaseContext(ctx, node.Lease); err != nil {
				releaseErrors = append(releaseErrors, fmt.Sprintf("%s: %v", node.Name, err))
				continue
			}
		} else if err := node.Manager.ReleaseLease(node.Lease); err != nil {
			releaseErrors = append(releaseErrors, fmt.Sprintf("%s: %v", node.Name, err))
			continue
		}
		released = append(released, node.Name)
	}
	if len(releaseErrors) > 0 {
		sort.Strings(releaseErrors)
		return released, fmt.Errorf("failed to release lease %s: %s", leaseID, strings.Join(releaseErrors, "; "))
	}
	if !found {
		if len(nodeErrors) > 0 {
			sort.Strings(nodeErrors)
			return nil, fmt.Errorf("lease %s not found on reachable nodes; unreachable nodes: %s", leaseID, strings.Join(nodeErrors, "; "))
		}
		return nil, fmt.Errorf("lease %s not found on reachable nodes", leaseID)
	}
	if len(released) == 0 {
		return nil, fmt.Errorf("lease %s was found but not released", leaseID)
	}
	return released, nil
}
