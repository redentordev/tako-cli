package engine

import (
	"fmt"
	"sort"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/reconcile"
)

func HasRunServices(services map[string]config.ServiceConfig) bool {
	for _, service := range services {
		if service.IsRun() {
			return true
		}
	}
	return false
}

func prepareRunInputHashes(d *deployer.Deployer, services map[string]config.ServiceConfig) (map[string]config.ServiceConfig, error) {
	out := CloneServiceMap(services)
	for name, service := range out {
		if !service.IsRun() {
			continue
		}
		hash, err := d.RunInputHash(&service)
		if err != nil {
			return nil, fmt.Errorf("run %s: failed to fingerprint resolved environment: %w", name, err)
		}
		service.RunInputHash = hash
		out[name] = service
	}
	return out, nil
}

func targetRunPrerequisites(services map[string]config.ServiceConfig, target string) map[string]config.ServiceConfig {
	required := make(map[string]config.ServiceConfig)
	visited := make(map[string]bool)
	var visit func(string)
	visit = func(name string) {
		if visited[name] {
			return
		}
		visited[name] = true
		service, ok := services[name]
		if !ok {
			return
		}
		for _, dependency := range service.DependsOn {
			if dependencyService, exists := services[dependency]; exists && dependencyService.IsRun() {
				required[dependency] = dependencyService
			}
			visit(dependency)
		}
	}
	visit(target)
	return required
}

func ensureRunPrerequisitesCompleted(target string, services map[string]config.ServiceConfig, history *remotestate.DeploymentHistory) error {
	if len(services) == 0 {
		return nil
	}
	completed := make(map[string]*reconcile.ActualService)
	addRunHistoryActual(completed, services, history)
	for name, desired := range services {
		actual := completed[name]
		desiredHash, ok := reconcile.SafeServiceConfigHash(desired)
		if !ok || actual == nil || actual.ConfigHash != desiredHash {
			return fmt.Errorf("targeted service %s requires deploy-time run %s to complete successfully for its current fingerprint; run a full deploy first", target, name)
		}
	}
	return nil
}

func validateRunKindTransitions(services map[string]config.ServiceConfig, actual map[string]*reconcile.ActualService) error {
	for name, desired := range services {
		if !desired.IsRun() {
			continue
		}
		current := actual[name]
		if current != nil && (current.ConfigSnapshot == nil || !current.ConfigSnapshot.IsRun()) {
			return fmt.Errorf("service %s cannot change directly from a long-running service to kind: run; remove and deploy the old service first, then add the run", name)
		}
	}
	return nil
}

// prepareRunPlanServices injects each run's resolved image into a planning
// copy so the completed fingerprint changes when an imageFrom build revision
// changes, without mutating the validated config used for execution.
func prepareRunPlanServices(services map[string]config.ServiceConfig, allServices map[string]config.ServiceConfig, imageRefs map[string]string) (map[string]config.ServiceConfig, error) {
	out := CloneServiceMap(services)
	for name, service := range out {
		if !service.IsRun() {
			continue
		}
		resolved, _, err := resolveRunImage(service, allServices, imageRefs)
		if err != nil {
			return nil, fmt.Errorf("run %s: %w", name, err)
		}
		service.Image = resolved
		out[name] = service
	}
	return out, nil
}

func resolveRunImage(service config.ServiceConfig, allServices map[string]config.ServiceConfig, imageRefs map[string]string) (string, bool, error) {
	if service.Image != "" {
		return service.Image, true, nil
	}
	source, ok := allServices[service.ImageFrom]
	if !ok {
		return "", false, fmt.Errorf("imageFrom service %q not found", service.ImageFrom)
	}
	image := imageRefs[service.ImageFrom]
	if image == "" {
		return "", false, fmt.Errorf("imageFrom service %q has no resolved image", service.ImageFrom)
	}
	return image, source.Image != "", nil
}

// addRunHistoryActual synthesizes plan actual-state entries from the newest
// recorded successful execution of each run. No container is expected to
// remain after a run completes.
func addRunHistoryActual(actual map[string]*reconcile.ActualService, planServices map[string]config.ServiceConfig, history *remotestate.DeploymentHistory) {
	if history == nil {
		return
	}
	deployments := append([]*remotestate.DeploymentState(nil), history.Deployments...)
	sort.SliceStable(deployments, func(i, j int) bool { return deployments[i].Timestamp.After(deployments[j].Timestamp) })
	for name, desired := range planServices {
		if !desired.IsRun() {
			continue
		}
		if current := actual[name]; current != nil && (current.ConfigSnapshot == nil || !current.ConfigSnapshot.IsRun()) {
			continue
		}
		for _, deployment := range deployments {
			if deployment == nil {
				continue
			}
			record, ok := deployment.Services[name]
			if !ok || record.Kind != config.ServiceKindRun {
				continue
			}
			// The newest attempt wins. A failed forced/rerun attempt must not
			// fall back to an older success and silently skip the next deploy.
			if record.Run == nil || record.Run.ExitCode != 0 || record.ConfigHash == "" {
				break
			}
			snapshot := desired
			actual[name] = &reconcile.ActualService{
				Name: name, Image: record.Image, ConfigHash: record.ConfigHash,
				ConfigSnapshot: &snapshot,
			}
			break
		}
	}
}

func runServiceFingerprint(service config.ServiceConfig, resolvedImage string) string {
	service.Image = resolvedImage
	hash, _ := reconcile.SafeServiceConfigHash(service)
	return hash
}

func runOutcome(result *deployer.DeployRunResult) *RunOutcome {
	if result == nil {
		return nil
	}
	return &RunOutcome{
		Command: append([]string(nil), result.Command...), Server: result.Server,
		Image: result.Image, ExitCode: result.ExitCode, DurationMs: result.DurationMs,
	}
}

func runHistoryServiceState(name string, service config.ServiceConfig, image string, result *deployer.DeployRunResult) remotestate.ServiceState {
	state := remotestate.ServiceState{
		Kind: config.ServiceKindRun, Name: name, Image: image,
		ConfigHash: runServiceFingerprint(service, image),
	}
	if result != nil {
		state.Run = &remotestate.RunState{
			Command: append([]string(nil), result.Command...), Server: result.Server,
			Image: result.Image, ExitCode: result.ExitCode, DurationMs: result.DurationMs,
		}
	}
	return state
}
