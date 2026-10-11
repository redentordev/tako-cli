package engine

import (
	"context"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/registryauth"
)

// registryAuthPreflighter is the deployer surface used to mint provider
// registry credentials before an operation's first node mutation.
type registryAuthPreflighter interface {
	SetRegistryMinter(deployer.RegistryMinter)
	PreflightRegistryAuth(context.Context, deployer.RegistryAuthNeeds) error
}

// newRegistryMinter creates the per-operation provider credential minter.
// Every minted token is registered with the engine redactor before it is
// returned, and provider stderr placed in errors is redacted.
func (e *Engine) newRegistryMinter() *registryauth.Minter {
	return registryauth.NewMinter(registryauth.Options{
		Register: e.RegisterSecret,
		Redact:   e.RedactSensitive,
	})
}

// attachRegistryMinter installs a fresh per-operation minter on deploy.
func (e *Engine) attachRegistryMinter(deploy registryAuthPreflighter) {
	deploy.SetRegistryMinter(e.newRegistryMinter())
}

// preflightRegistryAuth mints every provider credential the operation will
// attach. Callers run it before runtime setup, placement intent or any
// other node mutation, so a missing CLI or failed login exits 1 with
// nothing changed. An operation that attaches no provider credential runs
// no provider CLI.
func preflightRegistryAuth(ctx context.Context, deploy registryAuthPreflighter, needs deployer.RegistryAuthNeeds) error {
	return deploy.PreflightRegistryAuth(ctx, needs)
}

// deployRegistryAuthNeeds covers the services a deploy may roll out plus
// the run steps a targeted deploy must complete first.
func deployRegistryAuthNeeds(deploy *deployer.Deployer, services, allServices map[string]config.ServiceConfig, target string) deployer.RegistryAuthNeeds {
	needs := deploy.DeployRegistryAuthNeeds(services, allServices)
	if target != "" {
		needs.Add(deploy.DeployRegistryAuthNeeds(targetRunPrerequisites(allServices, target), allServices))
	}
	return needs
}

// projectRollbackRegistryAuthNeeds covers each service a project rollback
// will roll back, using the image source chosen during planning.
func projectRollbackRegistryAuthNeeds(deploy *deployer.Deployer, services map[string]config.ServiceConfig, targets map[string]remotestate.ServiceState, outcomes []RollbackServiceOutcome) deployer.RegistryAuthNeeds {
	var needs deployer.RegistryAuthNeeds
	for _, outcome := range outcomes {
		if outcome.Action != RollbackActionRolledBack {
			continue
		}
		state, ok := targets[outcome.Name]
		if !ok {
			continue
		}
		needs.Add(deploy.RollbackRegistryAuthNeeds(services[outcome.Name], &state, deployer.RollbackOptions{ImageSource: outcome.ImageSource}))
	}
	return needs
}

// scaleRolloutServices returns the scaled services that roll out through a
// regular service deploy (build or pull); shared-build services only
// transfer an existing image and attach no registry credential.
func scaleRolloutServices(services map[string]config.ServiceConfig, scaleTargets map[string]int) map[string]config.ServiceConfig {
	rollout := make(map[string]config.ServiceConfig, len(scaleTargets))
	for name, replicas := range scaleTargets {
		service, ok := services[name]
		if !ok || service.SharedBuildHash != "" {
			continue
		}
		service.Replicas = replicas
		rollout[name] = service
	}
	return rollout
}
