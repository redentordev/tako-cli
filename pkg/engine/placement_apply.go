package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/scheduler"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takodstate"
)

type PlacementApplyRequest struct {
	Config      *config.Config
	Environment string
	Plan        scheduler.MovementPlan
	PlanID      string
	Verbose     bool
}

type PlacementApplyResult struct {
	APIVersion  string    `json:"apiVersion"`
	Kind        string    `json:"kind"`
	Project     string    `json:"project"`
	Environment string    `json:"environment"`
	PlanID      string    `json:"planId"`
	Moves       int       `json:"moves"`
	Status      string    `json:"status"`
	CompletedAt time.Time `json:"completedAt"`
}

func (e *Engine) ApplyPlacementMovement(ctx context.Context, req PlacementApplyRequest) (*PlacementApplyResult, error) {
	if req.Config == nil || strings.TrimSpace(req.Environment) == "" {
		return nil, invalidRequestf("placement apply requires config and environment")
	}
	if err := scheduler.ValidateMovementPlan(req.Plan); err != nil {
		return nil, invalidRequestf("invalid placement plan: %v", err)
	}
	if req.Plan.PlanID != strings.TrimSpace(req.PlanID) {
		return nil, invalidRequestf("placement plan ID does not match reviewed ID")
	}
	if !req.Plan.Executable || len(req.Plan.Blockers) != 0 {
		return nil, invalidRequestf("placement plan has unresolved blockers")
	}
	if len(req.Plan.Moves) == 0 {
		return nil, invalidRequestf("placement plan contains no executable moves")
	}
	cfg, environment := req.Config, strings.TrimSpace(req.Environment)
	services, err := cfg.GetServices(environment)
	if err != nil {
		return nil, err
	}
	environmentServers, err := cfg.GetEnvironmentServers(environment)
	if err != nil {
		return nil, err
	}
	sources, err := config.ResolveSchedulableEnvironmentTargets(cfg.Servers, environmentServers, environment)
	if err != nil || len(sources) == 0 {
		// A drain may intentionally leave its source non-schedulable; read the
		// authoritative controller directly instead of requiring every worker.
		sources = StateAuthorityTargets(cfg, environmentServers)
	}
	source, err := preferredPlacementStateSource(cfg, sources)
	if err != nil {
		return nil, err
	}
	pool := ssh.NewPool()
	defer pool.CloseAll()
	factory, err := nodeclient.NewFactory(cfg, pool, TakodSocketFromConfig(cfg))
	if err != nil {
		return nil, err
	}
	defer factory.CloseIdleConnections()
	controllerClient, _, err := factory.Client(ctx, source)
	if err != nil {
		return nil, &ConnectivityError{Server: source, Err: err}
	}
	manager := takodstate.NewManager(controllerClient, cfg, environment)
	desired, err := manager.ReadDesired()
	if err != nil {
		if errors.Is(err, takodstate.ErrNotFound) {
			return nil, invalidRequestf("placement desired state is missing")
		}
		return nil, err
	}
	assignments, alreadyIntent, err := validatePlacementApplyState(req.Plan, desired)
	if err != nil {
		return nil, err
	}
	targets := placementApplyTargets(cfg, services, req.Plan)
	leaseSet, err := AcquireRemoteOperationLeasesContext(ctx, pool, cfg, environment, targets, "placement-apply")
	if err != nil {
		return nil, err
	}
	defer leaseSet.Release()
	factory.SetOperationFenceSource(leaseSet)
	leaseCtx, cancel := leaseSet.BindContext(ctx)
	defer cancel()
	ctx = leaseCtx
	desired, err = manager.ReadDesired()
	if err != nil {
		return nil, err
	}
	assignments, alreadyIntent, err = validatePlacementApplyState(req.Plan, desired)
	if err != nil {
		return nil, err
	}
	imageRefs := make(map[string]string, len(desired.Services))
	for name, service := range desired.Services {
		imageRefs[name] = service.Image
	}
	if !alreadyIntent {
		preserved := preservedDesiredServices(desired, services, nil)
		next, err := takodstate.BuildDesiredRevisionWithPlacementSnapshot(cfg, environment, "placement:"+req.Plan.PlanID, services, imageRefs, desired.TargetNodes, assignments, nil, preserved, desired.Git)
		if err != nil {
			return nil, err
		}
		if err := manager.WriteDesired(next); err != nil {
			return nil, fmt.Errorf("persist reviewed placement intent: %w", err)
		}
		desired = next
	}
	deploy := deployer.NewDeployerWithPool(controllerClient, cfg, environment, pool, req.Verbose)
	deploy.SetRuntimeFactory(factory)
	deploy.SetBaseContext(ctx)
	deploy.SetPriorAssignments(assignments)
	deploy.SetSkipBuild(true)
	if err := deploy.SetPlacementMovementTargets(targets); err != nil {
		return nil, err
	}
	movesByService := make(map[string][]scheduler.Movement)
	for _, move := range req.Plan.Moves {
		movesByService[move.Service] = append(movesByService[move.Service], move)
	}
	names := make([]string, 0, len(movesByService))
	for name := range movesByService {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		service, ok := services[name]
		if !ok {
			return nil, invalidRequestf("placement service %s is absent from current config", name)
		}
		image := imageRefs[name]
		if image == "" {
			return nil, invalidRequestf("placement service %s has no exact deployed image", name)
		}
		var destinations, oldSources []string
		for _, move := range movesByService[name] {
			destinations = append(destinations, move.ToNode)
			oldSources = append(oldSources, move.FromNode)
		}
		if err := deploy.MovePreparedServiceTakod(name, &service, image, destinations, oldSources); err != nil {
			return nil, err
		}
	}
	if err := deploy.ReconcileTakodProxy(services); err != nil {
		return nil, fmt.Errorf("movement succeeded but proxy reconciliation failed: %w", err)
	}
	if err := manager.AppendEvent(takodstate.NewEvent(cfg.Project.Name, environment, "placement.applied", desired.RevisionID, fmt.Sprintf("applied reviewed placement plan %s", req.Plan.PlanID), map[string]string{"planId": req.Plan.PlanID})); err != nil {
		return nil, err
	}
	return &PlacementApplyResult{APIVersion: takoapi.APIVersionCurrent, Kind: "PlacementApplyResult", Project: cfg.Project.Name, Environment: environment, PlanID: req.Plan.PlanID, Moves: len(req.Plan.Moves), Status: "succeeded", CompletedAt: time.Now().UTC()}, nil
}

func validatePlacementApplyState(plan scheduler.MovementPlan, desired *takodstate.DesiredRevision) (map[string][]scheduler.Assignment, bool, error) {
	if desired == nil {
		return nil, false, invalidRequestf("placement desired state is missing")
	}
	assignments := make(map[string][]scheduler.Assignment, len(desired.Services))
	for name, service := range desired.Services {
		assignments[name] = append([]scheduler.Assignment(nil), service.Assignments...)
	}
	already := desired.Source == "placement:"+plan.PlanID
	if desired.RevisionID != plan.InputRevisionID && !already {
		return nil, false, invalidRequestf("placement input revision %s is stale; controller is at %s", plan.InputRevisionID, desired.RevisionID)
	}
	for _, proposal := range plan.Services {
		current := scheduler.Stable(assignments[proposal.Service])
		expected := scheduler.Stable(proposal.Current)
		if already {
			expected = scheduler.Stable(proposal.Proposed)
		}
		if !reflect.DeepEqual(current, expected) {
			return nil, false, invalidRequestf("placement assignments for %s changed after review", proposal.Service)
		}
		assignments[proposal.Service] = append([]scheduler.Assignment(nil), proposal.Proposed...)
	}
	return assignments, already, nil
}

func placementApplyTargets(cfg *config.Config, services map[string]config.ServiceConfig, plan scheduler.MovementPlan) []string {
	seen := map[string]struct{}{}
	for _, move := range plan.Moves {
		seen[move.FromNode], seen[move.ToNode] = struct{}{}, struct{}{}
	}
	hasProxy := false
	for _, service := range services {
		hasProxy = hasProxy || service.IsProxied()
	}
	if hasProxy {
		for name, server := range cfg.Servers {
			if server.Schedulable() && server.HasPlatformRole(nodeidentity.RoleEdge) {
				seen[name] = struct{}{}
			}
		}
	}
	if controller, enrolled, err := controllerAuthorityServer(cfg, nil); err == nil && enrolled {
		seen[controller] = struct{}{}
	}
	targets := make([]string, 0, len(seen))
	for name := range seen {
		targets = append(targets, name)
	}
	sort.Strings(targets)
	return targets
}
