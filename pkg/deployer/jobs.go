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
		Name:           serviceName,
		Schedule:       service.Schedule,
		Timezone:       service.Timezone,
		Image:          image,
		Command:        []string{"sh", "-c", service.Command},
		EnvFileContent: envContent,
		Network:        runtimeid.NetworkName(d.config.Project.Name, d.environment),
		Mounts:         mounts,
		MemoryLimit:    serviceMemoryLimit(service),
		CPULimit:       serviceCPULimit(service),
		TimeoutSeconds: timeoutSeconds,
		ConfigHash:     hash,
	}, nil
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

	return runTakodNodeActions(targetServers, func(serverName string) error {
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
