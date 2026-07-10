package deployer

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

// recordJobImage remembers the image a job's deploy distributed so the
// post-loop schedule apply carries it to the owning node.
func (d *Deployer) recordJobImage(serviceName string, imageRef string) {
	d.jobMu.Lock()
	defer d.jobMu.Unlock()
	if d.jobImages == nil {
		d.jobImages = make(map[string]string)
	}
	d.jobImages[serviceName] = imageRef
}

func (d *Deployer) jobImageFor(serviceName string) string {
	d.jobMu.Lock()
	defer d.jobMu.Unlock()
	return d.jobImages[serviceName]
}

// JobOwnerServer resolves the single node that runs a job's cron schedule:
// the job's first placement target, deterministic for a stable config.
func (d *Deployer) JobOwnerServer(service *config.ServiceConfig) (string, error) {
	assignments, err := d.planTakodAssignments(service)
	if err != nil {
		return "", err
	}
	if len(assignments) == 0 {
		return "", fmt.Errorf("no target servers")
	}
	return assignments[0].ServerName, nil
}

// buildJobSpec renders the takod job spec for one kind:job service. The
// image comes from this deploy's build when the job was deployed, from the
// config for registry images, and stays empty otherwise so the owning node
// keeps the image of its existing spec.
func (d *Deployer) buildJobSpec(serviceName string, service *config.ServiceConfig) (takod.JobSpec, error) {
	envContent, err := d.buildTakodEnvFileContent(service)
	if err != nil {
		return takod.JobSpec{}, fmt.Errorf("failed to build env for job %s: %w", serviceName, err)
	}
	mounts, _, err := d.buildTakodMountSpecs(serviceName, service)
	if err != nil {
		return takod.JobSpec{}, fmt.Errorf("failed to resolve mounts for job %s: %w", serviceName, err)
	}
	timeoutSeconds := 0
	if strings.TrimSpace(service.Timeout) != "" {
		parsed, err := time.ParseDuration(service.Timeout)
		if err != nil {
			return takod.JobSpec{}, fmt.Errorf("job %s: invalid timeout: %w", serviceName, err)
		}
		timeoutSeconds = int(parsed / time.Second)
	}
	image := d.jobImageFor(serviceName)
	if image == "" && service.Image != "" {
		image = service.Image
	}
	hash, _ := reconcile.SafeServiceConfigHash(*service)
	return takod.JobSpec{
		Name:               serviceName,
		Schedule:           service.Schedule,
		Timezone:           service.Timezone,
		Image:              image,
		Command:            service.Command.ContainerCommand(),
		Entrypoint:         service.Entrypoint.Arguments(),
		Labels:             copyJobLabels(service.Labels),
		EnvFileContent:     envContent,
		Network:            runtimeid.NetworkName(d.config.Project.Name, d.environment),
		Mounts:             mounts,
		MemoryLimit:        serviceMemoryLimit(service),
		CPULimit:           serviceCPULimit(service),
		User:               service.User,
		WorkingDir:         service.WorkingDir,
		StopTimeoutSeconds: serviceStopTimeoutSeconds(service),
		Init:               service.Init,
		ExtraHosts:         append([]string(nil), service.ExtraHosts...),
		Ulimits:            copyServiceUlimits(service.Ulimits),
		ShmSize:            service.ShmSize,
		TimeoutSeconds:     timeoutSeconds,
		ConfigHash:         hash,
	}, nil
}

func copyJobLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	copy := make(map[string]string, len(labels))
	for key, value := range labels {
		copy[key] = value
	}
	return copy
}

// ApplyJobSchedules declaratively reconciles the environment's whole job set
// on every target node: each job lands on its owning node's payload and is
// absent from every other node's, so stale schedules (moved or removed
// jobs) are unscheduled in the same pass.
func (d *Deployer) ApplyJobSchedules(services map[string]config.ServiceConfig) error {
	if d.sshPool == nil {
		return fmt.Errorf("ssh pool not initialized")
	}
	targetServers, err := d.getTakodTargetServers()
	if err != nil {
		return fmt.Errorf("failed to get takod target servers: %w", err)
	}
	if len(targetServers) == 0 {
		return nil
	}

	var names []string
	for name, service := range services {
		if service.IsJob() {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	jobsByNode := make(map[string][]takod.JobSpec)
	for _, name := range names {
		service := services[name]
		owner, err := d.JobOwnerServer(&service)
		if err != nil {
			return fmt.Errorf("job %s: %w", name, err)
		}
		spec, err := d.buildJobSpec(name, &service)
		if err != nil {
			return err
		}
		jobsByNode[owner] = append(jobsByNode[owner], spec)
	}

	var argvServers []string
	var runtimeControlServers []string
	for _, serverName := range targetServers {
		needsArgv := false
		needsRuntimeControls := false
		for _, job := range jobsByNode[serverName] {
			if len(job.Entrypoint) > 0 {
				needsArgv = true
			}
			if jobSpecNeedsRuntimeControls(job) {
				needsRuntimeControls = true
			}
		}
		if needsArgv {
			argvServers = append(argvServers, serverName)
		}
		if needsRuntimeControls {
			runtimeControlServers = append(runtimeControlServers, serverName)
		}
	}
	return runTakodJobApplyPhases(targetServers, func() error {
		if err := d.preflightTakodCapability(argvServers, takod.CapabilityContainerArgvV1, "container argv payloads"); err != nil {
			return fmt.Errorf("job entrypoint requires container argv support: %w", err)
		}
		if err := d.preflightTakodCapability(runtimeControlServers, takod.CapabilityContainerRuntimeControlsV1, "container runtime controls"); err != nil {
			return fmt.Errorf("job uses container runtime controls: %w", err)
		}
		return nil
	}, func(serverName string) error {
		client, err := d.getEnvironmentClient(serverName)
		if err != nil {
			return err
		}
		request := takod.JobsApplyRequest{
			Project:     d.config.Project.Name,
			Environment: d.environment,
			Jobs:        jobsByNode[serverName],
		}
		output, err := takodclient.RequestJSON(client, d.takodSocket(), "POST", takodclient.JobsApplyEndpoint(), request)
		if err != nil {
			return fmt.Errorf("failed to apply job schedules on %s: %w", serverName, err)
		}
		var response takod.JobsApplyResponse
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			return fmt.Errorf("failed to parse jobs apply response from %s: %w", serverName, err)
		}
		for _, warning := range response.Warnings {
			d.emitEvent(events.Event{
				Type:    events.TypeDeployJobsApplied,
				Phase:   events.PhaseDeploy,
				Level:   events.LevelWarn,
				Node:    serverName,
				Message: fmt.Sprintf("  ⚠ %s\n", warning),
				Data:    map[string]any{"node": serverName, "warning": warning},
			})
		}
		if len(response.Applied) > 0 || len(response.Removed) > 0 {
			d.emitEvent(events.Event{
				Type:    events.TypeDeployJobsApplied,
				Phase:   events.PhaseDeploy,
				Level:   events.LevelInfo,
				Node:    serverName,
				Message: fmt.Sprintf("  ✓ Job schedules on %s: %d scheduled, %d removed\n", serverName, len(response.Applied), len(response.Removed)),
				Data:    map[string]any{"node": serverName, "applied": response.Applied, "removed": response.Removed},
			})
		}
		return nil
	})
}

func runTakodJobApplyPhases(targetServers []string, preflight func() error, apply func(string) error) error {
	if err := preflight(); err != nil {
		return err
	}
	return runTakodNodeActions(targetServers, apply)
}

func jobSpecNeedsRuntimeControls(spec takod.JobSpec) bool {
	return spec.User != "" || spec.WorkingDir != "" || spec.StopTimeoutSeconds > 0 || spec.Init || len(spec.ExtraHosts) > 0 || len(spec.Ulimits) > 0 || spec.ShmSize != ""
}
