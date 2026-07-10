package deployplan

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/reconcile"
)

// FilterActualStateForServices returns actual state scoped to the provided services.
func FilterActualStateForServices(actualState map[string]*reconcile.ActualService, services map[string]config.ServiceConfig) map[string]*reconcile.ActualService {
	if len(actualState) == 0 || len(services) == 0 {
		return map[string]*reconcile.ActualService{}
	}
	filtered := make(map[string]*reconcile.ActualService, len(services))
	for serviceName := range services {
		if actual, ok := actualState[serviceName]; ok {
			filtered[serviceName] = actual
		}
	}
	return filtered
}

// HasBuildServices reports whether any service is build-backed.
func HasBuildServices(services map[string]config.ServiceConfig) bool {
	for _, service := range services {
		if service.Build != "" || (service.SharedBuildHash != "" && !service.IsRun()) {
			return true
		}
	}
	return false
}

// ServicesToDeployForPlan returns the services selected for deploy by a reconciliation plan.
func ServicesToDeployForPlan(plan *reconcile.ReconciliationPlan, services map[string]config.ServiceConfig, force bool, explicitServiceTarget bool) map[string]config.ServiceConfig {
	if len(services) == 0 {
		return map[string]config.ServiceConfig{}
	}
	if plan == nil && !force {
		return cloneServiceMap(services)
	}

	selected := make(map[string]config.ServiceConfig)
	if plan != nil {
		for _, change := range plan.Changes {
			if change.Type != reconcile.ChangeAdd && change.Type != reconcile.ChangeUpdate {
				continue
			}
			service, ok := services[change.ServiceName]
			if ok {
				selected[change.ServiceName] = service
			}
		}
	}

	if force {
		for serviceName, service := range services {
			if explicitServiceTarget || !service.Persistent {
				selected[serviceName] = service
			}
		}
		return selected
	}

	for serviceName, service := range services {
		if service.Build != "" || (service.SharedBuildHash != "" && !service.IsRun()) {
			selected[serviceName] = service
		}
	}
	return selected
}

// PersistentServicesSkippedByForce returns persistent services omitted by a broad forced deploy.
func PersistentServicesSkippedByForce(services map[string]config.ServiceConfig, selected map[string]config.ServiceConfig, force bool, explicitServiceTarget bool) []string {
	if !force || explicitServiceTarget || len(services) == 0 {
		return nil
	}
	var skipped []string
	for serviceName, service := range services {
		if !service.Persistent {
			continue
		}
		if _, ok := selected[serviceName]; ok {
			continue
		}
		skipped = append(skipped, serviceName)
	}
	sort.Strings(skipped)
	return skipped
}

// BlueGreenPruneGracePeriod returns the maximum blue-green prune grace and affected service names.
func BlueGreenPruneGracePeriod(services map[string]config.ServiceConfig, keepRevisions map[string]string) (time.Duration, []string, error) {
	if len(services) == 0 || len(keepRevisions) == 0 {
		return 0, nil, nil
	}
	var maxGrace time.Duration
	var names []string
	for serviceName := range keepRevisions {
		service, ok := services[serviceName]
		if !ok || service.Deploy.Strategy != config.DeployStrategyBlueGreen || strings.TrimSpace(service.Deploy.GracePeriod) == "" {
			continue
		}
		grace, err := time.ParseDuration(strings.TrimSpace(service.Deploy.GracePeriod))
		if err != nil {
			return 0, nil, fmt.Errorf("service %s: invalid deploy.gracePeriod %q: %w", serviceName, service.Deploy.GracePeriod, err)
		}
		if grace < 0 {
			return 0, nil, fmt.Errorf("service %s: deploy.gracePeriod cannot be negative", serviceName)
		}
		if grace == 0 {
			continue
		}
		names = append(names, serviceName)
		if grace > maxGrace {
			maxGrace = grace
		}
	}
	if maxGrace == 0 {
		return 0, nil, nil
	}
	sort.Strings(names)
	return maxGrace, names, nil
}

// IsManualBlueGreenService reports whether a service uses manual blue-green promotion.
func IsManualBlueGreenService(service config.ServiceConfig) bool {
	return service.Deploy.Strategy == config.DeployStrategyBlueGreen &&
		service.Deploy.Promotion == config.DeployPromotionManual
}

// ManualPromotionPendingServices returns warm manual-promotion services awaiting promotion.
func ManualPromotionPendingServices(servicesToDeploy map[string]config.ServiceConfig, actualState map[string]*reconcile.ActualService) []string {
	if len(servicesToDeploy) == 0 {
		return nil
	}
	var pending []string
	for serviceName, service := range servicesToDeploy {
		if !IsManualBlueGreenService(service) {
			continue
		}
		actual := actualState[serviceName]
		if actual == nil || actual.CurrentRevision == "" {
			continue
		}
		pending = append(pending, serviceName)
	}
	sort.Strings(pending)
	return pending
}

// ShouldWarmManualPromotionService reports whether deploy should warm without promotion.
func ShouldWarmManualPromotionService(serviceName string, service config.ServiceConfig, actualState map[string]*reconcile.ActualService) bool {
	if !IsManualBlueGreenService(service) {
		return false
	}
	actual := actualState[serviceName]
	return actual != nil && actual.CurrentRevision != ""
}

func cloneServiceMap(services map[string]config.ServiceConfig) map[string]config.ServiceConfig {
	out := make(map[string]config.ServiceConfig, len(services))
	for name, service := range services {
		out[name] = service
	}
	return out
}
