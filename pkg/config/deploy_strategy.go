package config

// Deploy strategy sources reported in plans and results.
const (
	DeployStrategySourceExplicit = "explicit"
	DeployStrategySourceDefault  = "default"
)

// DefaultDeployStrategy returns the strategy Tako selects for a service that
// omits deploy.strategy. It returns rolling only for a stateless, long-running
// service that serves a port and would pass rolling validation unchanged;
// everything else (persistent, raw host ports, volumes, portless workers,
// recreate-only rollout settings, jobs and runs) keeps recreate. The function
// never mutates the service and never makes a previously valid config
// invalid: anything rolling would reject falls back to recreate.
func DefaultDeployStrategy(service *ServiceConfig) string {
	if service == nil {
		return DeployStrategyRecreate
	}
	if service.IsRun() || service.IsJob() {
		return DeployStrategyRecreate
	}
	if service.Persistent || service.Port <= 0 || len(service.Ports) > 0 || len(service.Volumes) > 0 {
		return DeployStrategyRecreate
	}
	replicas := service.Replicas
	if replicas <= 0 {
		replicas = 1
	}
	deploy := service.Deploy
	if deploy.MaxUnavailable != 0 {
		return DeployStrategyRecreate
	}
	if deploy.MaxSurge != 0 && deploy.MaxSurge < replicas {
		return DeployStrategyRecreate
	}
	if deploy.GracePeriod != "" || deploy.Promotion != "" || deploy.SmokeTest != (DeploySmokeTestConfig{}) {
		return DeployStrategyRecreate
	}
	// Trial validation on a copy: recreate never checked readiness or release,
	// so a config with an invalid one of those must keep recreate rather than
	// start failing validation.
	trial := *service
	trial.Replicas = replicas
	trial.Deploy.Strategy = DeployStrategyRolling
	if err := validateDeployStrategy("default-strategy-trial", &trial); err != nil {
		return DeployStrategyRecreate
	}
	return DeployStrategyRolling
}

// DeployStrategySource reports whether a validated service's strategy was set
// in tako.yaml (explicit) or selected by DefaultDeployStrategy (default).
// Runs have no rollout strategy and report an empty source.
func (s *ServiceConfig) DeployStrategySource() string {
	if s == nil || s.IsRun() {
		return ""
	}
	if s.Deploy.Strategy == "" || s.Deploy.StrategyDefaulted {
		return DeployStrategySourceDefault
	}
	return DeployStrategySourceExplicit
}

// RollbackOnFailureEnabled reports the effective deploy.rollbackOnFailure
// value: unset means true.
func (d DeployConfig) RollbackOnFailureEnabled() bool {
	return d.RollbackOnFailure == nil || *d.RollbackOnFailure
}

// RollbackOnFailureExplicit reports whether tako.yaml set
// deploy.rollbackOnFailure, either true or false.
func (d DeployConfig) RollbackOnFailureExplicit() bool {
	return d.RollbackOnFailure != nil
}
