package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/scheduler"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takodstate"
)

type PlacementPlanRequest struct {
	Config      *config.Config
	Environment string
	Mode        string
	TargetNode  string
}

// PlanPlacementMovement reads the current authoritative desired revision and
// returns a digest-bound proposal. It is read-only: applying movement remains
// a separately authorized operation and persistent moves always require a
// dedicated data-migration strategy.
func (e *Engine) PlanPlacementMovement(ctx context.Context, req PlacementPlanRequest) (*scheduler.MovementPlan, error) {
	if req.Config == nil {
		return nil, invalidRequestf("config is required")
	}
	if err := EnsureDeployRuntimeSupported(req.Config); err != nil {
		return nil, err
	}
	environment := strings.TrimSpace(req.Environment)
	if environment == "" {
		return nil, invalidRequestf("environment is required")
	}
	environmentServers, err := req.Config.GetEnvironmentServers(environment)
	if err != nil {
		return nil, err
	}
	targetNode, err := resolvePlacementPlanTarget(req.Config, environmentServers, req.Mode, req.TargetNode)
	if err != nil {
		return nil, err
	}
	stateSources, err := config.ResolveSchedulableEnvironmentTargets(req.Config.Servers, environmentServers, environment)
	if err != nil {
		return nil, fmt.Errorf("placement planning requires a schedulable authoritative state source: %w", err)
	}

	pool := ssh.NewPool()
	defer pool.CloseAll()
	factory, err := nodeclient.NewFactory(req.Config, pool, TakodSocketFromConfig(req.Config))
	if err != nil {
		return nil, err
	}
	defer factory.CloseIdleConnections()
	source, err := preferredPlacementStateSource(req.Config, stateSources)
	if err != nil {
		return nil, err
	}
	client, _, err := factory.Client(ctx, source)
	if err != nil {
		return nil, &ConnectivityError{Server: source, Err: fmt.Errorf("failed to read placement state from node %s: %w", source, err)}
	}
	desired, err := takodstate.NewManager(client, req.Config, environment).ReadDesired()
	if errors.Is(err, takodstate.ErrNotFound) {
		return nil, invalidRequestf("no desired state exists for %s/%s; deploy before planning movement", req.Config.Project.Name, environment)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read desired state for placement planning: %w", err)
	}

	serviceNames := make([]string, 0, len(desired.Services))
	for name := range desired.Services {
		serviceNames = append(serviceNames, name)
	}
	sort.Strings(serviceNames)
	workloads := make([]scheduler.MovementWorkload, 0, len(serviceNames))
	for _, name := range serviceNames {
		service := desired.Services[name]
		if len(service.Assignments) == 0 {
			continue
		}
		if err := validateAssignmentMembership(req.Config, name, service.Assignments); err != nil {
			return nil, fmt.Errorf("refusing placement plan from stale desired state: %w", err)
		}
		permitted, err := config.ResolvePlacementTargets(service.Placement, req.Config.Servers, environmentServers, environment)
		if err != nil {
			return nil, fmt.Errorf("service %s placement cannot be planned: %w", name, err)
		}
		eligible := make([]string, 0, len(permitted))
		for _, node := range permitted {
			if req.Config.Servers[node].Schedulable() {
				eligible = append(eligible, node)
			}
		}
		workloads = append(workloads, scheduler.MovementWorkload{
			Service: name, Global: service.Placement != nil && strings.TrimSpace(service.Placement.Strategy) == "global", Persistent: service.Persistent, Volumes: append([]string(nil), service.Volumes...),
			Current: append([]scheduler.Assignment(nil), service.Assignments...), Eligible: eligible,
		})
	}
	nodeIDs := make(map[string]string, len(environmentServers))
	for _, name := range environmentServers {
		nodeIDs[name] = req.Config.Servers[name].NodeID
	}
	plan, err := scheduler.PlanMovement(req.Mode, targetNode, desired.RevisionID, nodeIDs, workloads)
	if err != nil {
		return nil, invalidRequestf("%v", err)
	}
	return &plan, nil
}

func preferredPlacementStateSource(cfg *config.Config, candidates []string) (string, error) {
	for _, name := range candidates {
		server := cfg.Servers[name]
		for _, role := range server.Roles {
			if role == nodeidentity.RoleControlPlane {
				return name, nil
			}
		}
	}
	return PreferredRuntimeServer(cfg, candidates)
}

func resolvePlacementPlanTarget(cfg *config.Config, environmentServers []string, mode, requested string) (string, error) {
	if strings.TrimSpace(mode) == scheduler.MovementModeRebalance {
		if strings.TrimSpace(requested) != "" {
			return "", invalidRequestf("rebalance plans do not accept a target node")
		}
		return "", nil
	}
	if strings.TrimSpace(mode) != scheduler.MovementModeDrain && strings.TrimSpace(mode) != scheduler.MovementModeCordon {
		return "", invalidRequestf("placement movement mode must be cordon, drain, or rebalance")
	}
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", invalidRequestf("%s plans require --node", mode)
	}
	for _, name := range environmentServers {
		server := cfg.Servers[name]
		if name == requested || (server.NodeID != "" && server.NodeID == requested) {
			return name, nil
		}
	}
	return "", invalidRequestf("node %s is not a member of the selected environment", requested)
}
