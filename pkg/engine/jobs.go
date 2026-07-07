package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

// Kinds of the serialized job result documents.
const (
	KindJobsResult       = "JobsResult"
	KindJobRunsResult    = "JobRunsResult"
	KindJobTriggerResult = "JobTriggerResult"
)

// jobTriggerStreamGrace keeps the client-side deadline behind takod's
// server-side run timeout so the terminal exit frame arrives first.
const jobTriggerStreamGrace = 30 * time.Second

// JobsRequest lists scheduled jobs for an environment.
type JobsRequest struct {
	Config      *config.Config
	Environment string
	Server      string
}

// JobRunsRequest reads run history for one job or every job.
type JobRunsRequest struct {
	Config      *config.Config
	Environment string
	Job         string
	Server      string
}

// JobTriggerRequest runs a scheduled job immediately on its owning node.
type JobTriggerRequest struct {
	Config      *config.Config
	Environment string
	Job         string
	Server      string
}

// JobInfo is one scheduled job as reported by its owning node.
type JobInfo struct {
	Name           string      `json:"name"`
	Server         string      `json:"server"`
	Schedule       string      `json:"schedule"`
	Timezone       string      `json:"timezone,omitempty"`
	Image          string      `json:"image,omitempty"`
	Command        []string    `json:"command,omitempty"`
	TimeoutSeconds int         `json:"timeoutSeconds,omitempty"`
	NextRun        *time.Time  `json:"nextRun,omitempty"`
	LastRun        *JobRunInfo `json:"lastRun,omitempty"`
}

// JobRunInfo is one recorded job run.
type JobRunInfo struct {
	Job        string    `json:"job"`
	Server     string    `json:"server"`
	Trigger    string    `json:"trigger"`
	Container  string    `json:"container,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	DurationMs int64     `json:"durationMs"`
	ExitCode   int       `json:"exitCode"`
	Status     string    `json:"status"`
	Output     string    `json:"output,omitempty"`
}

// JobsResult is the serializable outcome of `tako jobs`.
type JobsResult struct {
	APIVersion  string    `json:"apiVersion"`
	Kind        string    `json:"kind"`
	Project     string    `json:"project"`
	Environment string    `json:"environment"`
	Jobs        []JobInfo `json:"jobs"`
}

// JobRunsResult is the serializable outcome of `tako jobs runs`.
type JobRunsResult struct {
	APIVersion  string       `json:"apiVersion"`
	Kind        string       `json:"kind"`
	Project     string       `json:"project"`
	Environment string       `json:"environment"`
	Job         string       `json:"job,omitempty"`
	Runs        []JobRunInfo `json:"runs"`
}

// JobTriggerResult is the serializable outcome of `tako jobs trigger`.
// ExitCode is the job command's code; the tako process itself reports
// success when the run completed (human mode mirrors ExitCode).
type JobTriggerResult struct {
	APIVersion  string `json:"apiVersion"`
	Kind        string `json:"kind"`
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Job         string `json:"job"`
	Server      string `json:"server"`
	Container   string `json:"container,omitempty"`
	ExitCode    int    `json:"exitCode"`
	DurationMs  int64  `json:"durationMs"`
	Error       string `json:"error,omitempty"`
}

// Jobs lists the environment's scheduled jobs across its nodes.
func (e *Engine) Jobs(ctx context.Context, req JobsRequest) (*JobsResult, error) {
	cfg, envName, serverNames, err := e.resolveJobTargets(ctx, req.Config, req.Environment, req.Server)
	if err != nil {
		return nil, err
	}

	var jobs []JobInfo
	for _, serverName := range serverNames {
		statuses, err := e.queryNodeJobs(ctx, cfg, envName, serverName)
		if err != nil {
			return nil, err
		}
		for _, status := range statuses {
			jobs = append(jobs, jobInfoFromStatus(status, serverName, e.redactor.Redact))
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Name < jobs[j].Name })

	return &JobsResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindJobsResult,
		Project:     cfg.Project.Name,
		Environment: envName,
		Jobs:        jobs,
	}, nil
}

// JobRuns reads run history across the environment's nodes, newest first.
func (e *Engine) JobRuns(ctx context.Context, req JobRunsRequest) (*JobRunsResult, error) {
	cfg, envName, serverNames, err := e.resolveJobTargets(ctx, req.Config, req.Environment, req.Server)
	if err != nil {
		return nil, err
	}
	job := strings.TrimSpace(req.Job)
	if job != "" {
		if err := e.requireJobService(cfg, envName, job); err != nil {
			return nil, err
		}
	}

	var runs []JobRunInfo
	for _, serverName := range serverNames {
		server := cfg.Servers[serverName]
		client, err := connectTakodStreamNodeContext(ctx, server)
		if err != nil {
			return nil, &ConnectivityError{Err: fmt.Errorf("failed to connect to node %s: %w", serverName, err)}
		}
		endpoint := fmt.Sprintf("/v1/jobs/runs?project=%s&environment=%s&job=%s", cfg.Project.Name, envName, job)
		output, err := takodclient.RequestJSON(client, TakodSocketFromConfig(cfg), "GET", endpoint, nil)
		_ = client.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to query job runs on node %s: %w", serverName, err)
		}
		var response struct {
			Runs []takod.JobRunRecord `json:"runs"`
		}
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			return nil, fmt.Errorf("failed to parse job runs from node %s: %w", serverName, err)
		}
		for _, record := range response.Runs {
			runs = append(runs, jobRunInfoFromRecord(record, serverName, e.redactor.Redact))
		}
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].StartedAt.After(runs[j].StartedAt) })

	return &JobRunsResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindJobRunsResult,
		Project:     cfg.Project.Name,
		Environment: envName,
		Job:         job,
		Runs:        runs,
	}, nil
}

// TriggerJob runs a scheduled job now on the node holding its schedule,
// streaming output as jobs.trigger.* events until the exit frame.
func (e *Engine) TriggerJob(ctx context.Context, req JobTriggerRequest) (*JobTriggerResult, error) {
	cfg, envName, serverNames, err := e.resolveJobTargets(ctx, req.Config, req.Environment, req.Server)
	if err != nil {
		return nil, err
	}
	job := strings.TrimSpace(req.Job)
	if job == "" {
		return nil, invalidRequestf("trigger requires a job name (tako jobs trigger JOB)")
	}
	if err := e.requireJobService(cfg, envName, job); err != nil {
		return nil, err
	}

	serverName, status, err := e.resolveJobOwner(ctx, cfg, envName, serverNames, job)
	if err != nil {
		return nil, err
	}
	serverCfg := cfg.Servers[serverName]

	timeoutSeconds := status.TimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = 3600
	}
	timeout := time.Duration(timeoutSeconds) * time.Second

	result := &JobTriggerResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindJobTriggerResult,
		Project:     cfg.Project.Name,
		Environment: envName,
		Job:         job,
		Server:      serverName,
		ExitCode:    -1,
	}

	payload, err := json.Marshal(takod.JobTriggerRequest{
		Project:     cfg.Project.Name,
		Environment: envName,
		Job:         job,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to encode trigger request: %w", err)
	}

	client, err := connectTakodStreamNodeContext(ctx, serverCfg)
	if err != nil {
		return nil, &ConnectivityError{Err: fmt.Errorf("failed to connect to node %s: %w", serverName, err)}
	}
	defer client.Close()

	e.emit(events.Event{
		Type:    events.TypeJobTriggerStarted,
		Phase:   events.PhaseDeploy,
		Level:   events.LevelInfo,
		Service: job,
		Node:    serverName,
		Message: fmt.Sprintf("→ Triggering job %s on %s\n", job, serverName),
		Data:    map[string]any{"job": job, "node": serverName},
	})

	streamCtx, cancel := context.WithTimeout(ctx, timeout+jobTriggerStreamGrace)
	defer cancel()

	started := time.Now()
	reader, writer := io.Pipe()
	streamDone := make(chan error, 1)
	go func() {
		err := takodclient.StreamRequestOutputWithContext(streamCtx, client, TakodSocketFromConfig(cfg), "POST", "/v1/jobs/trigger", strings.NewReader(string(payload)), writer, writer)
		if err != nil {
			_ = writer.CloseWithError(err)
		} else {
			_ = writer.Close()
		}
		streamDone <- err
	}()

	exitSeen := false
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if container, ok := strings.CutPrefix(line, takod.ExecContainerMarker); ok {
			result.Container = container
			continue
		}
		if code, ok := strings.CutPrefix(line, takod.ExecExitMarker); ok {
			if parsed, err := strconv.Atoi(strings.TrimSpace(code)); err == nil {
				result.ExitCode = parsed
				exitSeen = true
			}
			continue
		}
		e.emit(events.Event{
			Type:    events.TypeJobTriggerOutput,
			Phase:   events.PhaseDeploy,
			Level:   events.LevelInfo,
			Service: job,
			Node:    serverName,
			Message: line + "\n",
			Data:    map[string]any{"data": line},
		})
	}
	scanErr := scanner.Err()
	streamErr := <-streamDone
	result.DurationMs = time.Since(started).Milliseconds()

	var opErr error
	switch {
	case streamErr != nil:
		opErr = streamErr
	case scanErr != nil:
		opErr = scanErr
	case !exitSeen:
		opErr = fmt.Errorf("job trigger stream ended without an exit status")
	}
	if opErr != nil && ctx.Err() != nil {
		opErr = ctx.Err()
	}
	if opErr != nil {
		result.Error = opErr.Error()
	}

	e.emit(events.Event{
		Type:    events.TypeJobTriggerCompleted,
		Phase:   events.PhaseDeploy,
		Level:   events.LevelDebug,
		Service: job,
		Node:    serverName,
		Data:    map[string]any{"exitCode": result.ExitCode, "durationMs": result.DurationMs},
	})
	return result, opErr
}

// resolveJobTargets validates the request shape shared by job operations and
// registers node passwords and service secret values with the redactor:
// job run output is arbitrary command output and may echo secrets.
func (e *Engine) resolveJobTargets(ctx context.Context, cfg *config.Config, envName string, requestedServer string) (*config.Config, string, []string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, "", nil, err
	}
	if cfg == nil {
		return nil, "", nil, invalidRequestf("jobs request requires a loaded config")
	}
	if strings.TrimSpace(envName) == "" {
		return nil, "", nil, invalidRequestf("jobs request requires an environment")
	}
	serverNames, err := ResolveStatusTargetServerNames(cfg, envName, requestedServer)
	if err != nil {
		return nil, "", nil, err
	}
	for _, server := range cfg.Servers {
		e.RegisterSecret(server.Password)
	}
	if services, err := cfg.GetServices(envName); err == nil {
		e.registerServiceSecretValues(envName, services)
	}
	return cfg, envName, serverNames, nil
}

// requireJobService checks the named service exists and is a kind:job.
func (e *Engine) requireJobService(cfg *config.Config, envName string, job string) error {
	services, err := cfg.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services: %w", err)
	}
	service, ok := services[job]
	if !ok {
		return invalidRequestf("job '%s' not found in environment %s", job, envName)
	}
	if !service.IsJob() {
		return invalidRequestf("service '%s' is not a job (kind: job)", job)
	}
	return nil
}

// queryNodeJobs reads one node's scheduled jobs through takod.
func (e *Engine) queryNodeJobs(ctx context.Context, cfg *config.Config, envName string, serverName string) ([]takod.JobStatus, error) {
	server := cfg.Servers[serverName]
	client, err := connectTakodStreamNodeContext(ctx, server)
	if err != nil {
		return nil, &ConnectivityError{Err: fmt.Errorf("failed to connect to node %s: %w", serverName, err)}
	}
	defer client.Close()
	endpoint := fmt.Sprintf("/v1/jobs?project=%s&environment=%s", cfg.Project.Name, envName)
	output, err := takodclient.RequestJSON(client, TakodSocketFromConfig(cfg), "GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to query jobs on node %s: %w", serverName, err)
	}
	var response struct {
		Jobs []takod.JobStatus `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("failed to parse jobs from node %s: %w", serverName, err)
	}
	return response.Jobs, nil
}

// resolveJobOwner finds the node whose schedule holds the job.
func (e *Engine) resolveJobOwner(ctx context.Context, cfg *config.Config, envName string, serverNames []string, job string) (string, *takod.JobStatus, error) {
	var lastErr error
	for _, serverName := range serverNames {
		statuses, err := e.queryNodeJobs(ctx, cfg, envName, serverName)
		if err != nil {
			lastErr = err
			continue
		}
		for i := range statuses {
			if statuses[i].Name == job {
				return serverName, &statuses[i], nil
			}
		}
	}
	if lastErr != nil {
		return "", nil, &ConnectivityError{Err: fmt.Errorf("job %s not reachable on any node: %w", job, lastErr)}
	}
	return "", nil, invalidRequestf("job %s is not scheduled on any node in environment %s; deploy first", job, envName)
}

func jobInfoFromStatus(status takod.JobStatus, serverName string, redact func(string) string) JobInfo {
	info := JobInfo{
		Name:           status.Name,
		Server:         serverName,
		Schedule:       status.Schedule,
		Timezone:       status.Timezone,
		Image:          status.Image,
		Command:        append([]string(nil), status.Command...),
		TimeoutSeconds: status.TimeoutSeconds,
	}
	if status.NextRun != nil {
		nextRun := status.NextRun.UTC()
		info.NextRun = &nextRun
	}
	if status.LastRun != nil {
		lastRun := jobRunInfoFromRecord(*status.LastRun, serverName, redact)
		info.LastRun = &lastRun
	}
	return info
}

func jobRunInfoFromRecord(record takod.JobRunRecord, serverName string, redact func(string) string) JobRunInfo {
	output := record.Output
	if redact != nil {
		output = redact(output)
	}
	return JobRunInfo{
		Job:        record.Job,
		Server:     serverName,
		Trigger:    record.Trigger,
		Container:  record.Container,
		StartedAt:  record.StartedAt,
		FinishedAt: record.FinishedAt,
		DurationMs: record.DurationMs,
		ExitCode:   record.ExitCode,
		Status:     record.Status,
		Output:     output,
	}
}
