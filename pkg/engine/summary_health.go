package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/takod"
)

// serviceHealth is one service's post-operation state as `tako ps` reads it.
type serviceHealth struct {
	Desired int
	Running int
	Health  string
	Nodes   []string
}

// readServiceHealth reads actual state from the selected nodes in parallel,
// bounded by timeout, and aggregates it the way `tako ps` does (worst-wins
// health, running vs desired replicas). Job and run services report n/a.
// A reader that outlives the timeout is abandoned; the read returns an
// error and the caller reports unknown health.
func readServiceHealth(
	ctx context.Context,
	servers map[string]config.ServerConfig,
	services map[string]config.ServiceConfig,
	envServers []string,
	selected []string,
	timeout time.Duration,
	read StatusActualStateReadFunc,
) (map[string]serviceHealth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = SummaryHealthTimeout
	}
	readCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type gathered struct {
		nodes []StatusNodeActualState
		err   error
	}
	done := make(chan gathered, 1)
	go func() {
		nodes, err := GatherStatusActualStateByNodeWith(readCtx, servers, selected, read)
		done <- gathered{nodes: nodes, err: err}
	}()
	var nodeStates []StatusNodeActualState
	select {
	case <-readCtx.Done():
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("timed out after %s", timeout)
	case result := <-done:
		if result.err != nil {
			return nil, result.err
		}
		nodeStates = result.nodes
	}

	infos, err := BuildStatusServiceInfo(servers, services, MergeStatusActualStates(nodeStates), nil, envServers, selected, "")
	if err != nil {
		return nil, err
	}
	AttachStatusServiceNodes(infos, nodeStates)
	health := make(map[string]serviceHealth, len(infos))
	for _, info := range infos {
		if info.Kind != "" {
			health[info.Name] = serviceHealth{Health: SummaryHealthNA}
			continue
		}
		entry := serviceHealth{Desired: info.Desired, Running: info.Running, Health: summaryHealthFor(info.Desired, info.Running, info.Health)}
		for _, node := range info.Nodes {
			entry.Nodes = append(entry.Nodes, node.Name)
		}
		health[info.Name] = entry
	}
	return health, nil
}

// summaryHealthFor maps replica counts and the docker health aggregate to a
// summary health value.
func summaryHealthFor(desired int, running int, dockerHealth string) string {
	switch {
	case desired == 0:
		return SummaryHealthStopped
	case dockerHealth == takod.HealthStateUnhealthy:
		return SummaryHealthUnhealthy
	case running < desired:
		return SummaryHealthDegraded
	case dockerHealth == takod.HealthStateStarting:
		return SummaryHealthStarting
	default:
		return SummaryHealthHealthy
	}
}

// summaryHealth runs the bounded post-operation health read through the
// operation's runtime factory.
func (e *Engine) summaryHealth(ctx context.Context, factory *nodeclient.Factory, cfg *config.Config, envName string, selected []string, services map[string]config.ServiceConfig) (map[string]serviceHealth, error) {
	if e != nil && e.summaryHealthRead != nil {
		return e.summaryHealthRead(ctx, cfg, envName, selected, services)
	}
	if factory == nil || cfg == nil {
		return nil, fmt.Errorf("no runtime connection")
	}
	envServers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return nil, err
	}
	read := func(serverName string, _ config.ServerConfig) (map[string]*takod.ActualService, error) {
		client, _, err := factory.Client(ctx, serverName)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to node %s: %w", serverName, err)
		}
		response, err := ActualStateViaTakod(client, cfg, envName)
		if err != nil {
			return nil, fmt.Errorf("failed to query takod on node %s: %w", serverName, err)
		}
		return response.Services, nil
	}
	return readServiceHealth(ctx, cfg.Servers, services, envServers, selected, SummaryHealthTimeout, read)
}
