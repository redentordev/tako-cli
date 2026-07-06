package cmd

import (
	"context"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/health"
)

type domainStatusSpec = engine.DomainStatusSpec

type domainStatusOptions = engine.DomainStatusOptions

func collectConfiguredDomainSpecs(services map[string]config.ServiceConfig, serviceFilter string) []domainStatusSpec {
	return engine.CollectConfiguredDomainSpecs(services, serviceFilter)
}

func domainExpectedTargets(cfg *config.Config, envName string, overrides []string) ([]string, error) {
	return engine.DomainExpectedTargets(cfg, envName, overrides)
}

func monitorDomainStatuses(ctx context.Context, checker *health.HealthChecker, specs []domainStatusSpec, options domainStatusOptions) ([]health.DomainStatus, error) {
	return cliEngine().MonitorDomainStatuses(ctx, checker, specs, options)
}
