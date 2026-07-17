package deployer

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
)

const DefaultDeployRunTimeout = time.Hour

// DeployRunResult is the machine/history record of a kind:run execution.
type DeployRunResult struct {
	Service    string
	Server     string
	Image      string
	Command    []string
	ExitCode   int
	DurationMs int64
}

// RunInputHash returns a non-reversible digest of the exact env-file content
// a deploy-time run will receive, including resolved env files and secrets.
func (d *Deployer) RunInputHash(service *config.ServiceConfig) (string, error) {
	_, hash, err := d.buildTakodEnvFileContentAndHash(service)
	if err != nil {
		return "", err
	}
	return hash, nil
}

func runInputValuesHash(values map[string]string) string {
	canonical, _ := json.Marshal(values)
	digest := sha256.Sum256(canonical)
	return fmt.Sprintf("sha256:%x", digest)
}

// RunDeployStep executes one kind:run service on its deterministic placement
// owner. It blocks until exit and treats every non-zero/missing status as a
// deployment failure.
func (d *Deployer) RunDeployStep(serviceName string, service *config.ServiceConfig, imageRef string, pullImage bool) (*DeployRunResult, error) {
	return d.RunDeployStepOnNodes(serviceName, service, imageRef, pullImage, nil)
}

// RunDeployStepOnNodes restricts execution to nodes known to carry a local
// build-backed image. A nil restriction is used for pullable images and full
// deploys where the source build has just run on its planned node.
func (d *Deployer) RunDeployStepOnNodes(serviceName string, service *config.ServiceConfig, imageRef string, pullImage bool, availableImageNodes []string) (*DeployRunResult, error) {
	if service == nil || !service.IsRun() {
		return nil, fmt.Errorf("service %s is not kind: run", serviceName)
	}
	assignments, err := d.planTakodAssignments(serviceName, service)
	if err != nil {
		return nil, err
	}
	if err := d.validateAssignmentMutationTargets(serviceName, assignments); err != nil {
		return nil, err
	}
	if len(assignments) == 0 {
		return nil, fmt.Errorf("run %s has no assigned node", serviceName)
	}
	serverName, err := d.runStepServer(service, assignments, availableImageNodes)
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", serviceName, err)
	}
	if err := d.preflightTakodCapability([]string{serverName}, takod.CapabilityExecOneOffControlsV1, "deploy-time one-off controls"); err != nil {
		return nil, fmt.Errorf("run %s: %w", serviceName, err)
	}
	if len(service.Files) > 0 {
		if err := d.preflightTakodCapability([]string{serverName}, takod.CapabilityServiceFilesV1, "operator file distribution"); err != nil {
			return nil, fmt.Errorf("run %s uses files: %w", serviceName, err)
		}
	}
	client, err := d.getRuntimeClient(serverName)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node %s for run %s: %w", serverName, serviceName, err)
	}
	envContent, inputHash, err := d.buildTakodEnvFileContentAndHash(service)
	if err != nil {
		return nil, fmt.Errorf("failed to build run env for %s: %w", serviceName, err)
	}
	if service.RunInputHash != "" && service.RunInputHash != inputHash {
		return nil, fmt.Errorf("run %s environment or secrets changed after planning; re-run the deploy to compute a fresh fingerprint", serviceName)
	}
	mounts, externalVolumes, err := d.buildTakodMountSpecs(serviceName, service)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve run mounts for %s: %w", serviceName, err)
	}
	fileBundles, fileMounts, filesHash, err := d.PrepareServiceFiles(serviceName, service)
	if err != nil {
		return nil, err
	}
	if filesHash != "" {
		service.FilesContentHash = filesHash
	}
	mounts = append(mounts, fileMounts...)
	fileSetID := ""
	if filesHash != "" {
		fileSetID, err = serviceFileSetID(filesHash)
		if err != nil {
			return nil, err
		}
	}
	timeout := DefaultDeployRunTimeout
	if strings.TrimSpace(service.Timeout) != "" {
		timeout, err = time.ParseDuration(service.Timeout)
		if err != nil {
			return nil, fmt.Errorf("run %s: invalid timeout: %w", serviceName, err)
		}
	}
	request := takod.ExecRequest{
		Project:            d.config.Project.Name,
		Environment:        d.environment,
		Service:            serviceName,
		Mode:               takod.ExecModeOneOff,
		Command:            service.Command.Arguments(),
		Entrypoint:         service.Entrypoint.Arguments(),
		Image:              imageRef,
		PullImage:          pullImage,
		RegistryAuths:      d.registryAuths(),
		EnvFileContent:     envContent,
		Network:            runtimeid.NetworkName(d.config.Project.Name, d.environment),
		Mounts:             mounts,
		ExternalVolumes:    externalVolumes,
		Files:              fileBundles,
		FileSetID:          fileSetID,
		CleanupFiles:       len(fileBundles) > 0,
		Labels:             copyJobLabels(service.Labels),
		User:               service.User,
		WorkingDir:         service.WorkingDir,
		StopTimeoutSeconds: serviceStopTimeoutSeconds(service),
		Init:               service.Init,
		ExtraHosts:         append([]string(nil), service.ExtraHosts...),
		Ulimits:            copyServiceUlimits(service.Ulimits),
		ShmSize:            service.ShmSize,
		MemoryLimit:        serviceMemoryLimit(service),
		CPULimit:           serviceCPULimit(service),
		TimeoutSeconds:     int(timeout / time.Second),
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("failed to encode run %s: %w", serviceName, err)
	}
	d.emitEvent(events.Event{
		Type: events.TypeDeployRunStarted, Phase: events.PhaseDeploy, Level: events.LevelInfo,
		Service: serviceName, Node: serverName,
		Message: fmt.Sprintf("→ Running deploy step %s on %s: %s\n", serviceName, serverName, strings.Join(request.Command, " ")),
		Data:    map[string]any{"command": request.Command, "image": imageRef, "node": serverName},
	})
	ctx, cancel := context.WithTimeout(d.baseContext(), timeout+releaseStreamGrace)
	defer cancel()
	started := time.Now()
	exitCode, exitSeen, streamErr := d.streamDeployExec(ctx, client, serviceName, serverName, payload, events.TypeDeployRunOutput)
	result := &DeployRunResult{
		Service: serviceName, Server: serverName, Image: imageRef,
		Command: append([]string(nil), request.Command...), ExitCode: exitCode,
		DurationMs: time.Since(started).Milliseconds(),
	}
	var runErr error
	switch {
	case streamErr != nil:
		runErr = fmt.Errorf("run %s failed: %w", serviceName, streamErr)
	case !exitSeen:
		runErr = fmt.Errorf("run %s ended without an exit status", serviceName)
	case exitCode != 0:
		runErr = fmt.Errorf("run %s exited with code %d", serviceName, exitCode)
	}
	if runErr != nil {
		d.emitEvent(events.Event{Type: events.TypeDeployRunFailed, Phase: events.PhaseDeploy, Level: events.LevelError, Service: serviceName, Node: serverName, Message: fmt.Sprintf("  ✗ Run %s failed (exit %d)\n", serviceName, exitCode), Data: map[string]any{"exitCode": exitCode, "durationMs": result.DurationMs}})
		return result, runErr
	}
	d.emitEvent(events.Event{Type: events.TypeDeployRunCompleted, Phase: events.PhaseDeploy, Level: events.LevelInfo, Service: serviceName, Node: serverName, Message: fmt.Sprintf("  ✓ Run %s completed in %.1fs\n", serviceName, float64(result.DurationMs)/1000), Data: map[string]any{"exitCode": exitCode, "durationMs": result.DurationMs}})
	return result, nil
}

func (d *Deployer) runStepServer(service *config.ServiceConfig, assignments []takodAssignment, availableImageNodes []string) (string, error) {
	available := make(map[string]bool, len(availableImageNodes))
	for _, node := range availableImageNodes {
		available[node] = true
	}
	isAvailable := func(node string) bool { return availableImageNodes == nil || available[node] }
	if service.ImageFrom == "" {
		for _, assignment := range assignments {
			if isAvailable(assignment.ServerName) {
				return assignment.ServerName, nil
			}
		}
		return "", fmt.Errorf("placement has no node carrying the resolved run image")
	}
	if service.SharedBuildHash != "" {
		for _, assignment := range assignments {
			if isAvailable(assignment.ServerName) {
				return assignment.ServerName, nil
			}
		}
		return "", fmt.Errorf("placement has no node carrying shared build %s", service.ImageFrom)
	}
	services, err := d.config.GetServices(d.environment)
	if err != nil {
		return "", err
	}
	source, ok := services[service.ImageFrom]
	if !ok || source.Build == "" {
		return assignments[0].ServerName, nil
	}
	sourceAssignments, err := d.planTakodAssignments(service.ImageFrom, &source)
	if err != nil {
		return "", err
	}
	sourceNodes := make(map[string]bool, len(sourceAssignments))
	for _, assignment := range sourceAssignments {
		sourceNodes[assignment.ServerName] = true
	}
	for _, assignment := range assignments {
		if sourceNodes[assignment.ServerName] && isAvailable(assignment.ServerName) {
			return assignment.ServerName, nil
		}
	}
	return "", fmt.Errorf("placement has no node carrying build-backed imageFrom service %s", service.ImageFrom)
}
