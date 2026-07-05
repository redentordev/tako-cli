package deployplan

import (
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/reconcile"
)

// ProxyActiveRevisions returns active proxy revisions for rolling and blue-green services.
func ProxyActiveRevisions(
	cfg *config.Config,
	envName string,
	services map[string]config.ServiceConfig,
	servicesToDeploy map[string]config.ServiceConfig,
	imageRefs map[string]string,
	actualState map[string]*reconcile.ActualService,
) map[string]string {
	if cfg == nil || len(services) == 0 {
		return nil
	}
	revisions := make(map[string]string)
	for serviceName, service := range services {
		switch service.Deploy.Strategy {
		case config.DeployStrategyRolling, config.DeployStrategyBlueGreen:
		default:
			continue
		}

		if _, deploying := servicesToDeploy[serviceName]; deploying {
			if IsManualBlueGreenService(service) {
				if actual := actualState[serviceName]; actual != nil && actual.CurrentRevision != "" {
					revisions[serviceName] = actual.CurrentRevision
					continue
				}
			}
			imageRef := imageRefs[serviceName]
			if imageRef == "" {
				imageRef = ImageRef(cfg, envName, serviceName, service, "")
			}
			revisions[serviceName] = deployer.ServiceRevisionID(cfg.Project.Name, envName, serviceName, imageRef, service)
			continue
		}

		if actual := actualState[serviceName]; actual != nil && actual.CurrentRevision != "" {
			revisions[serviceName] = actual.CurrentRevision
		}
	}
	if len(revisions) == 0 {
		return nil
	}
	return revisions
}

// DeployedProxyActiveRevisions returns deployed active proxy revisions eligible for pruning.
func DeployedProxyActiveRevisions(servicesToDeploy map[string]config.ServiceConfig, activeRevisions map[string]string) map[string]string {
	if len(servicesToDeploy) == 0 || len(activeRevisions) == 0 {
		return nil
	}
	deployed := make(map[string]string)
	for serviceName, service := range servicesToDeploy {
		if IsManualBlueGreenService(service) {
			continue
		}
		if revision := activeRevisions[serviceName]; revision != "" {
			deployed[serviceName] = revision
		}
	}
	if len(deployed) == 0 {
		return nil
	}
	return deployed
}
