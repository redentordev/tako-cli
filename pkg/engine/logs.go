package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

const (
	// KindLogsResult identifies a serialized logs result document.
	KindLogsResult = "LogsResult"

	logsStatusSuccess = "success"
	logsStatusPartial = "partial"
	logsStatusFailed  = "failed"

	// logsNonFollowNodeConcurrency bounds concurrent node streams when not
	// following. Follow streams every node at once: a held slot would starve
	// the remaining nodes forever.
	logsNonFollowNodeConcurrency = 8

	logsDefaultTail = 100
	logsMaxTail     = 10000

	logsStreamFeature = "filtered/attributed logs (" + takodclient.CapabilityLogsStreamV1 + ")"
)

// LogsRequest describes one logs streaming operation. Config must be loaded
// and validated; Environment must be resolved.
type LogsRequest struct {
	Config      *config.Config
	Environment string
	// Services selects services and jobs by name (comma-separated entries
	// are split). Empty selects every long-running service; jobs and
	// deploy-time runs are only shown when named.
	Services []string
	// Service is the historical single-service selector. It is merged into
	// Services and kept for API compatibility.
	Service string
	Server  string
	// Tail limits historical lines per container (default 100, maximum
	// 10000). With Grep it counts matching lines.
	Tail int
	// TailExplicit reports that the caller chose Tail. Without it, Since
	// widens the limit to the maximum so the whole window is shown.
	TailExplicit bool
	Follow       bool
	// Since and Until bound the time window: a Go duration (evaluated by
	// each node against its own clock), an RFC3339 timestamp, or Unix
	// seconds. Until cannot be combined with Follow.
	Since string
	Until string
	// Grep keeps only lines matching this RE2 pattern (at most 1024 bytes);
	// IgnoreCase makes it case-insensitive.
	Grep       string
	IgnoreCase bool
	// Timestamps prefixes human lines with the Docker timestamp. Machine
	// events carry data.time whenever the node supplied one.
	Timestamps bool
}

// LogsNodeResult is the serializable outcome for one streamed takod node.
type LogsNodeResult struct {
	Name   string `json:"name"`
	Host   string `json:"host,omitempty"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	// Lines counts log lines emitted from this node.
	Lines int `json:"lines"`
	// Sources counts containers (or recorded job runs) streamed.
	Sources int `json:"sources"`
}

// LogsResult is the serializable outcome of StreamLogs.
type LogsResult struct {
	APIVersion  string `json:"apiVersion"`
	Kind        string `json:"kind"`
	Project     string `json:"project"`
	Environment string `json:"environment"`
	// Service is set only when exactly one service is selected.
	Service string `json:"service"`
	// Services lists every selected service and job.
	Services   []string         `json:"services"`
	Tail       int              `json:"tail"`
	Follow     bool             `json:"follow"`
	Since      string           `json:"since,omitempty"`
	Until      string           `json:"until,omitempty"`
	Grep       string           `json:"grep,omitempty"`
	IgnoreCase bool             `json:"ignoreCase"`
	Timestamps bool             `json:"timestamps"`
	Status     string           `json:"status"`
	Nodes      []LogsNodeResult `json:"nodes"`
	StartedAt  time.Time        `json:"startedAt"`
	Duration   float64          `json:"durationSeconds"`
	Message    string           `json:"message,omitempty"`
	Error      string           `json:"error,omitempty"`
}

// LogNodeStreamFunc streams one node. prefix reports whether human log lines
// should be node-prefixed because more than one node is selected.
type LogNodeStreamFunc func(serverName string, server config.ServerConfig, prefix bool) error

// LogNodeResult captures one node's fan-out outcome for log streaming.
type LogNodeResult struct {
	Index      int
	ServerName string
	Host       string
	Err        error
}

// logsNode is one connected takod node. The default implementation talks to
// takod; tests substitute scripted nodes.
type logsNode interface {
	SupportsLogsStream(ctx context.Context) (bool, error)
	StreamFrames(ctx context.Context, request takod.LogsStreamRequest, handle func(takodclient.LogsStreamFrame) error) error
	StreamLegacy(ctx context.Context, endpoint string, handle func(line string) error) error
}

// logsHooks replaces node access in tests. Nil fields use the real
// implementations.
type logsHooks struct {
	connect  func(ctx context.Context, serverName string) (logsNode, error)
	jobOwner func(ctx context.Context, cfg *config.Config, envName string, serverNames []string, job string) (string, error)
	jobRuns  func(ctx context.Context, req JobRunsRequest) (*JobRunsResult, error)
}

// errLogsNoContainers marks a legacy agent's "no containers found" reply,
// which is zero sources rather than a node failure.
var errLogsNoContainers = errors.New("no containers found")

// logsPlan is a validated, normalized logs request.
type logsPlan struct {
	selected   []string
	services   []string
	jobs       []string
	replicas   map[string]int
	tail       int
	since      string
	until      string
	sinceTime  *time.Time
	untilTime  *time.Time
	pattern    *regexp.Regexp
	follow     bool
	timestamps bool
	grep       string
	ignoreCase bool
}

// usesStreamFeatures reports whether a node must advertise logs.stream-v1:
// anything beyond one long-running service with tail/follow.
func (p *logsPlan) usesStreamFeatures(nodeJobs []string) bool {
	return len(p.services) != 1 || len(nodeJobs) > 0 || p.since != "" || p.until != "" || p.pattern != nil || p.timestamps
}

func planLogsRequest(req LogsRequest, services map[string]config.ServiceConfig, envName string) (*logsPlan, error) {
	if req.Tail < 0 {
		return nil, invalidRequestf("tail cannot be negative")
	}
	if req.Tail > logsMaxTail {
		return nil, invalidRequestf("tail cannot exceed %d", logsMaxTail)
	}
	plan := &logsPlan{follow: req.Follow, timestamps: req.Timestamps, grep: req.Grep, ignoreCase: req.IgnoreCase, replicas: map[string]int{}}

	since, sinceTime, err := takod.NormalizeLogsTimeValue("--since", req.Since)
	if err != nil {
		return nil, &InvalidRequestError{Err: err}
	}
	until, untilTime, err := takod.NormalizeLogsTimeValue("--until", req.Until)
	if err != nil {
		return nil, &InvalidRequestError{Err: err}
	}
	if until != "" && req.Follow {
		return nil, invalidRequestf("--until cannot be combined with --follow")
	}
	if sinceTime != nil && untilTime != nil && untilTime.Before(*sinceTime) {
		return nil, invalidRequestf("--until must not be earlier than --since")
	}
	plan.since, plan.until, plan.sinceTime, plan.untilTime = since, until, sinceTime, untilTime
	pattern, err := takod.CompileLogsGrep(req.Grep, req.IgnoreCase)
	if err != nil {
		return nil, &InvalidRequestError{Err: err}
	}
	plan.pattern = pattern

	switch {
	case req.TailExplicit:
		plan.tail = req.Tail
	case since != "":
		plan.tail = logsMaxTail
	case req.Tail > 0:
		plan.tail = req.Tail
	default:
		plan.tail = logsDefaultTail
	}

	names := splitLogsServiceNames(append([]string{req.Service}, req.Services...))
	if len(names) == 0 {
		for name, service := range services {
			if !service.IsJob() && !service.IsRun() {
				names = append(names, name)
			}
		}
		if len(names) == 0 {
			return nil, invalidRequestf("environment %s has no long-running services; name a job to show its logs", envName)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		service, ok := services[name]
		if !ok {
			return nil, invalidRequestf("service %s not found in environment %s", name, envName)
		}
		switch {
		case service.IsRun():
			return nil, invalidRequestf("service %s is a deploy-time run and has no persistent container logs; inspect deployment history or deploy output", name)
		case service.IsJob():
			plan.jobs = append(plan.jobs, name)
		default:
			plan.services = append(plan.services, name)
			plan.replicas[name] = max(service.Replicas, 1)
		}
	}
	plan.selected = names
	return plan, nil
}

// splitLogsServiceNames merges flag and argument selections, splitting
// comma-separated entries and dropping blanks and duplicates.
func splitLogsServiceNames(values []string) []string {
	seen := map[string]bool{}
	var names []string
	for _, value := range values {
		for _, name := range strings.Split(value, ",") {
			name = strings.TrimSpace(name)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

// StreamLogs streams service and job logs from the selected takod nodes.
// Human output is emitted only as events; the returned result is suitable
// for machine output documents.
func (e *Engine) StreamLogs(ctx context.Context, req LogsRequest) (*LogsResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.Config == nil {
		return nil, invalidRequestf("logs request requires a loaded config")
	}
	if strings.TrimSpace(req.Environment) == "" {
		return nil, invalidRequestf("logs request requires an environment")
	}

	cfg := req.Config
	envName := req.Environment
	services, err := cfg.GetServices(envName)
	if err != nil {
		return nil, fmt.Errorf("failed to get services: %w", err)
	}
	plan, err := planLogsRequest(req, services, envName)
	if err != nil {
		return nil, err
	}
	servers, err := ResolveLogTargetServers(cfg, envName, req.Server)
	if err != nil {
		return nil, err
	}
	if len(servers) == 0 {
		return nil, invalidRequestf("no servers configured for environment %s", envName)
	}

	// Register sensitive values before emitting anything that could contain
	// them: SSH passwords, literal env values, and every secret the selected
	// services reference (local .tako/secrets* and secretSources).
	for _, server := range cfg.Servers {
		e.RegisterSecret(server.Password)
	}
	e.RegisterPublicConfigNames(cfg, services)
	e.RegisterServiceEnvValues(services)
	selectedServices := make(map[string]config.ServiceConfig, len(plan.selected))
	for _, name := range plan.selected {
		selectedServices[name] = services[name]
	}
	e.registerServiceSecretValuesOrWarn(cfg, envName, selectedServices, events.PhaseLogs)

	startedAt := time.Now()
	result := &LogsResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindLogsResult,
		Project:     cfg.Project.Name,
		Environment: envName,
		Services:    append([]string(nil), plan.selected...),
		Tail:        plan.tail,
		Follow:      plan.follow,
		Since:       plan.since,
		Until:       plan.until,
		Grep:        e.RedactSensitive(plan.grep),
		IgnoreCase:  plan.ignoreCase,
		Timestamps:  plan.timestamps,
		StartedAt:   startedAt,
	}
	if len(plan.selected) == 1 {
		result.Service = plan.selected[0]
	}
	bannerService := strings.Join(plan.selected, ", ")
	eventService := result.Service

	// Non-follow job logs are recorded run output; follow attaches the live
	// run containers through the node stream.
	var recordJobs, nodeJobs []string
	if plan.follow {
		nodeJobs = plan.jobs
	} else {
		recordJobs = plan.jobs
	}
	jobsOnly := len(plan.services) == 0

	nodeTargets := servers
	if len(plan.services) == 0 && len(nodeJobs) > 0 && req.Server == "" {
		nodeTargets, err = e.resolveLogsJobOwners(ctx, cfg, envName, servers, nodeJobs)
		if err != nil {
			return nil, err
		}
	}
	streamNodes := len(plan.services) > 0 || len(nodeJobs) > 0

	nodeCount := len(nodeTargets)
	if !streamNodes {
		nodeCount = 0
	}
	if streamNodes || !jobsOnly {
		e.emit(events.Event{
			Type:    events.TypePhaseStarted,
			Phase:   events.PhaseLogs,
			Level:   events.LevelInfo,
			Service: eventService,
			Message: fmt.Sprintf("Streaming logs from %s on %d takod node(s)...\n\n", bannerService, nodeCount),
			Data: map[string]any{
				"service":  eventService,
				"services": plan.selected,
				"nodes":    nodeCount,
				"tail":     plan.tail,
				"follow":   plan.follow,
			},
		})
	}

	if err := ctx.Err(); err != nil {
		return e.finishLogsCancelled(result, startedAt, eventService, err)
	}

	renderer := newLogsRenderer(plan, nodeTargets, streamNodes)
	totalSources := 0
	var jobFailures []string
	for _, job := range recordJobs {
		nodeResult, err := e.streamJobLogs(ctx, req, cfg, envName, job, plan, renderer)
		if err != nil {
			if jobsOnly && len(recordJobs) == 1 {
				result.Status = logsStatusFailed
				result.Error = e.RedactSensitive(err.Error())
				result.Duration = time.Since(startedAt).Seconds()
				return result, err
			}
			jobFailures = append(jobFailures, fmt.Sprintf("%s: %v", job, err))
			e.emit(events.Event{
				Type:    events.TypeWarning,
				Phase:   events.PhaseLogs,
				Level:   events.LevelWarn,
				Service: job,
				Message: fmt.Sprintf("Warning: could not read recorded runs of job %s: %v\n", job, err),
				Data:    map[string]any{"service": job, "error": err.Error(), "stream": "stderr"},
			})
			continue
		}
		if nodeResult != nil {
			totalSources += nodeResult.Sources
			if !streamNodes {
				result.Nodes = append(result.Nodes, *nodeResult)
			}
		}
	}

	if !streamNodes {
		result.Duration = time.Since(startedAt).Seconds()
		if err := ctx.Err(); err != nil {
			return e.finishLogsCancelled(result, startedAt, eventService, err)
		}
		if len(jobFailures) > 0 {
			err := &AttentionError{Err: fmt.Errorf("could not read job logs: %s", strings.Join(jobFailures, "; "))}
			return e.finishLogsFailure(result, eventService, logsStatusPartial, err)
		}
		result.Status = logsStatusSuccess
		if totalSources == 0 {
			result.Message = "job has no recorded runs yet"
		} else {
			result.Message = fmt.Sprintf("showed last run output of %d job(s)", totalSources)
		}
		return result, nil
	}

	connect := e.logsConnector(ctx, cfg)
	defer connect.close()

	var countsMu sync.Mutex
	counts := map[string]*LogsNodeResult{}
	limit := 0
	if !plan.follow {
		limit = logsNonFollowNodeConcurrency
	}
	nodeResults := StreamLogNodesWithLimit(ctx, nodeTargets, limit, func(serverName string, server config.ServerConfig, _ bool) error {
		stats := &LogsNodeResult{}
		countsMu.Lock()
		counts[serverName] = stats
		countsMu.Unlock()
		err := e.streamLogsFromNode(ctx, connect.connect, cfg, envName, serverName, server, plan, nodeJobs, renderer, stats)
		if err != nil && ctx.Err() == nil {
			e.emit(events.Event{
				Type:    events.TypeWarning,
				Phase:   events.PhaseLogs,
				Level:   events.LevelWarn,
				Service: eventService,
				Node:    serverName,
				Message: fmt.Sprintf("Warning: logs from node %s failed: %v\n", serverName, err),
				Data:    map[string]any{"node": serverName, "error": err.Error(), "stream": "stderr"},
			})
		}
		return err
	})
	for _, nodeResult := range nodeResults {
		doc := logsNodeResultDocument(nodeResult)
		doc.Error = e.RedactSensitive(doc.Error)
		if stats := counts[nodeResult.ServerName]; stats != nil {
			doc.Lines, doc.Sources = stats.Lines, stats.Sources
			totalSources += stats.Sources
		}
		result.Nodes = append(result.Nodes, doc)
	}
	result.Duration = time.Since(startedAt).Seconds()

	if err := ctx.Err(); err != nil {
		return e.finishLogsCancelled(result, startedAt, eventService, err)
	}

	if err := SummarizeLogStreamResults(nodeResults); err != nil {
		status := logsStatusFailed
		var attention *AttentionError
		if errors.As(err, &attention) {
			status = logsStatusPartial
		}
		return e.finishLogsFailure(result, eventService, status, err)
	}
	if len(jobFailures) > 0 {
		err := &AttentionError{Err: fmt.Errorf("could not read job logs: %s", strings.Join(jobFailures, "; "))}
		return e.finishLogsFailure(result, eventService, logsStatusPartial, err)
	}
	if !plan.follow && totalSources == 0 {
		err := fmt.Errorf("no containers found for %s in environment %s", bannerService, envName)
		return e.finishLogsFailure(result, eventService, logsStatusFailed, err)
	}

	result.Status = logsStatusSuccess
	result.Message = fmt.Sprintf("streamed logs from %d node(s)", len(result.Nodes))
	e.emit(events.Event{
		Type:    events.TypePhaseCompleted,
		Phase:   events.PhaseLogs,
		Level:   events.LevelInfo,
		Service: eventService,
		Data:    map[string]any{"status": result.Status, "nodes": len(result.Nodes)},
	})
	return result, nil
}

func (e *Engine) finishLogsCancelled(result *LogsResult, startedAt time.Time, service string, err error) (*LogsResult, error) {
	result.Duration = time.Since(startedAt).Seconds()
	return e.finishLogsFailure(result, service, logsStatusFailed, err)
}

func (e *Engine) finishLogsFailure(result *LogsResult, service string, status string, err error) (*LogsResult, error) {
	result.Status = status
	result.Error = e.RedactSensitive(err.Error())
	e.emit(events.Event{
		Type:    events.TypePhaseCompleted,
		Phase:   events.PhaseLogs,
		Level:   events.LevelError,
		Service: service,
		Data:    map[string]any{"status": result.Status, "error": result.Error},
	})
	return result, err
}

// resolveLogsJobOwners narrows a jobs-only follow to the nodes holding the
// jobs' schedules.
func (e *Engine) resolveLogsJobOwners(ctx context.Context, cfg *config.Config, envName string, servers map[string]config.ServerConfig, jobs []string) (map[string]config.ServerConfig, error) {
	owner := e.resolveLogsJobOwner
	if e.logsHooks != nil && e.logsHooks.jobOwner != nil {
		owner = e.logsHooks.jobOwner
	}
	names := SortedLogServerNames(servers)
	targets := map[string]config.ServerConfig{}
	for _, job := range jobs {
		serverName, err := owner(ctx, cfg, envName, names, job)
		if err != nil {
			return nil, err
		}
		targets[serverName] = servers[serverName]
	}
	return targets, nil
}

func (e *Engine) resolveLogsJobOwner(ctx context.Context, cfg *config.Config, envName string, serverNames []string, job string) (string, error) {
	serverName, _, err := e.resolveJobOwner(ctx, cfg, envName, serverNames, job)
	return serverName, err
}

// logsConnection opens nodes for one StreamLogs call.
type logsConnection struct {
	connect func(ctx context.Context, serverName string) (logsNode, error)
	close   func()
}

func (e *Engine) logsConnector(ctx context.Context, cfg *config.Config) logsConnection {
	if e.logsHooks != nil && e.logsHooks.connect != nil {
		return logsConnection{connect: e.logsHooks.connect, close: func() {}}
	}
	pool := ssh.NewPool()
	factory, err := nodeclient.NewFactory(cfg, pool, TakodSocketFromConfig(cfg))
	if err != nil {
		pool.CloseAll()
		return logsConnection{
			connect: func(context.Context, string) (logsNode, error) { return nil, err },
			close:   func() {},
		}
	}
	socket := TakodSocketFromConfig(cfg)
	return logsConnection{
		connect: func(ctx context.Context, serverName string) (logsNode, error) {
			client, _, err := factory.Client(ctx, serverName)
			if err != nil {
				return nil, err
			}
			return &takodLogsNode{client: client, socket: socket, server: serverName}, nil
		},
		close: func() {
			factory.CloseIdleConnections()
			pool.CloseAll()
		},
	}
}

// takodLogsNode streams logs from a real takod agent.
type takodLogsNode struct {
	client any
	socket string
	server string
}

func (n *takodLogsNode) SupportsLogsStream(ctx context.Context) (bool, error) {
	err := takodclient.RequireCapability(ctx, n.client, n.socket, n.server, takodclient.CapabilityLogsStreamV1, logsStreamFeature)
	if err == nil {
		return true, nil
	}
	var capability *takodclient.CapabilityRequiredError
	if errors.As(err, &capability) {
		return false, nil
	}
	return false, err
}

func (n *takodLogsNode) StreamFrames(ctx context.Context, request takod.LogsStreamRequest, handle func(takodclient.LogsStreamFrame) error) error {
	payload, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("failed to encode logs stream request: %w", err)
	}
	reader, writer := io.Pipe()
	streamDone := make(chan error, 1)
	go func() {
		err := takodclient.StreamRequestOutputWithContext(ctx, n.client, n.socket, http.MethodPost, takodclient.LogsStreamEndpoint(), bytes.NewReader(payload), writer, io.Discard)
		_ = writer.CloseWithError(err)
		streamDone <- err
	}()
	decodeErr := takodclient.DecodeLogsStream(reader, handle)
	// Unblock the transport if decoding stopped before the body ended.
	_ = reader.CloseWithError(io.ErrClosedPipe)
	streamErr := <-streamDone
	if decodeErr == nil {
		return nil
	}
	if errors.Is(decodeErr, takodclient.ErrLogsStreamTruncated) && streamErr != nil {
		return streamErr
	}
	return decodeErr
}

func (n *takodLogsNode) StreamLegacy(ctx context.Context, endpoint string, handle func(line string) error) error {
	reader, writer := io.Pipe()
	streamDone := make(chan error, 1)
	go func() {
		err := takodclient.StreamOutputWithContext(ctx, n.client, n.socket, endpoint, writer, io.Discard)
		_ = writer.CloseWithError(err)
		streamDone <- err
	}()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	var handleErr error
	for scanner.Scan() {
		if handleErr = handle(scanner.Text()); handleErr != nil {
			break
		}
	}
	scanErr := scanner.Err()
	_ = reader.CloseWithError(io.ErrClosedPipe)
	streamErr := <-streamDone
	var httpErr *takodclient.HTTPError
	if errors.As(streamErr, &httpErr) && strings.Contains(httpErr.Body, "no containers found for service") {
		return errLogsNoContainers
	}
	switch {
	case handleErr != nil:
		return handleErr
	case streamErr != nil:
		return streamErr
	case scanErr != nil && !errors.Is(scanErr, io.ErrClosedPipe):
		return scanErr
	}
	return nil
}

// streamLogsFromNode streams one node: framed when the agent advertises
// logs.stream-v1, the legacy text stream for plain single-service requests,
// otherwise a capability error naming `tako upgrade servers`.
func (e *Engine) streamLogsFromNode(
	ctx context.Context,
	connect func(ctx context.Context, serverName string) (logsNode, error),
	cfg *config.Config,
	envName string,
	serverName string,
	server config.ServerConfig,
	plan *logsPlan,
	nodeJobs []string,
	renderer *logsRenderer,
	stats *LogsNodeResult,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	node, err := connect(ctx, serverName)
	if err != nil {
		return &ConnectivityError{Server: serverName, Err: fmt.Errorf("failed to connect to node %s: %w", serverName, err)}
	}
	e.emit(events.Event{
		Type:    events.TypeLogLine,
		Phase:   events.PhaseLogs,
		Level:   events.LevelDebug,
		Node:    serverName,
		Message: fmt.Sprintf("Using node: %s (%s)\n", serverName, server.Host),
		Data:    map[string]any{"node": serverName, "host": server.Host},
	})
	supported, err := node.SupportsLogsStream(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &ConnectivityError{Server: serverName, Err: err}
	}

	if !supported {
		if plan.usesStreamFeatures(nodeJobs) {
			return &takodclient.CapabilityRequiredError{Server: serverName, Capability: takodclient.CapabilityLogsStreamV1, Feature: logsStreamFeature}
		}
		service := plan.services[0]
		endpoint := takodclient.LogsEndpoint(cfg.Project.Name, envName, service, plan.tail, plan.follow)
		sourceCounted := false
		err := node.StreamLegacy(ctx, endpoint, func(line string) error {
			if !sourceCounted {
				sourceCounted = true
				stats.Sources = 1
			}
			stats.Lines++
			e.emitLogFrame(serverName, takodclient.LogsStreamFrame{Service: service, Data: line}, renderer)
			return nil
		})
		if errors.Is(err, errLogsNoContainers) {
			stats.Sources = 0
			return nil
		}
		if err == nil {
			stats.Sources = 1
		}
		return err
	}

	tail := plan.tail
	request := takod.LogsStreamRequest{
		Project:     cfg.Project.Name,
		Environment: envName,
		Services:    plan.services,
		IncludeJobs: nodeJobs,
		Tail:        &tail,
		Since:       plan.since,
		Until:       plan.until,
		Grep:        plan.grep,
		IgnoreCase:  plan.ignoreCase,
		Follow:      plan.follow,
	}
	jobsOnly := len(plan.services) == 0 && len(nodeJobs) > 0
	return node.StreamFrames(ctx, request, func(frame takodclient.LogsStreamFrame) error {
		switch frame.Type {
		case takodclient.LogsFrameLine:
			stats.Lines++
			e.emitLogFrame(serverName, frame, renderer)
		case takodclient.LogsFrameSource:
			if frame.Event == "attached" {
				stats.Sources++
			}
			e.emit(events.Event{
				Type:    events.TypePhaseStarted,
				Phase:   events.PhaseLogs,
				Level:   events.LevelDebug,
				Service: frame.Service,
				Node:    serverName,
				Message: fmt.Sprintf("→ %s %s\n", frame.Event, logsSourcePrefix(frame.Service, frame.Slot, serverName)),
				Data:    map[string]any{"event": "source." + frame.Event, "service": frame.Service, "replica": frame.Slot, "container": frame.Container, "node": serverName},
			})
		case takodclient.LogsFrameRun:
			e.emitJobRunFrame(serverName, frame)
		case takodclient.LogsFrameReady:
			if plan.follow && jobsOnly && frame.Sources != nil && *frame.Sources == 0 {
				e.emit(events.Event{
					Type:    events.TypePhaseStarted,
					Phase:   events.PhaseLogs,
					Level:   events.LevelInfo,
					Node:    serverName,
					Message: fmt.Sprintf("Waiting for the next run of %s on %s...\n", strings.Join(nodeJobs, ", "), serverName),
					Data:    map[string]any{"event": "run.waiting", "jobs": nodeJobs, "node": serverName},
				})
			}
		}
		return nil
	})
}

func (e *Engine) emitJobRunFrame(serverName string, frame takodclient.LogsStreamFrame) {
	job := frame.Job
	if job == "" {
		job = frame.Service
	}
	data := map[string]any{"event": "run." + frame.Event, "job": job, "node": serverName, "container": frame.Container}
	var message string
	switch frame.Event {
	case "started":
		message = fmt.Sprintf("── Run of job %s started on %s ──\n", job, serverName)
	case "finished":
		code := "unknown"
		if frame.ExitCode != nil {
			code = fmt.Sprintf("%d", *frame.ExitCode)
			data["exitCode"] = *frame.ExitCode
		}
		message = fmt.Sprintf("── Run of job %s finished on %s (exit %s) ──\n", job, serverName, code)
	default:
		return
	}
	e.emit(events.Event{Type: events.TypePhaseStarted, Phase: events.PhaseLogs, Level: events.LevelInfo, Service: job, Node: serverName, Message: message, Data: data})
}

// logsSourcePrefix renders the human source attribution service.slot@node
// (service@node when the slot is unknown, as on legacy agents).
func logsSourcePrefix(service string, slot string, node string) string {
	if slot == "" {
		return service + "@" + node
	}
	return service + "." + slot + "@" + node
}

// logsRenderer renders human log lines with a padded source prefix whenever
// more than one source is expected or has been seen.
type logsRenderer struct {
	mu         sync.Mutex
	timestamps bool
	expected   int
	width      int
	seen       map[string]bool
}

func newLogsRenderer(plan *logsPlan, nodes map[string]config.ServerConfig, streamNodes bool) *logsRenderer {
	renderer := &logsRenderer{timestamps: plan.timestamps, seen: map[string]bool{}}
	nodeNames := SortedLogServerNames(nodes)
	if !streamNodes {
		nodeNames = nil
	}
	for _, service := range plan.services {
		for _, node := range nodeNames {
			replicas := plan.replicas[service]
			renderer.expected += replicas
			renderer.width = max(renderer.width, len(logsSourcePrefix(service, fmt.Sprintf("%d", replicas), node)))
		}
	}
	for _, job := range plan.jobs {
		renderer.expected++
		for _, node := range nodeNames {
			renderer.width = max(renderer.width, len(logsSourcePrefix(job, takod.LogsJobSlot, node)))
		}
	}
	return renderer
}

func (r *logsRenderer) render(prefix string, timestamp string, line string) string {
	r.mu.Lock()
	r.seen[prefix] = true
	multi := r.expected > 1 || len(r.seen) > 1
	if len(prefix) > r.width {
		r.width = len(prefix)
	}
	width := r.width
	r.mu.Unlock()

	var builder strings.Builder
	if r.timestamps && timestamp != "" {
		builder.WriteString(timestamp)
		builder.WriteByte(' ')
	}
	if multi {
		fmt.Fprintf(&builder, "%-*s | ", width, prefix)
	}
	builder.WriteString(line)
	builder.WriteByte('\n')
	return builder.String()
}

// emitLogFrame emits one log line. Line content stays in the top-level
// data.data string so the event redactor masks registered secrets.
func (e *Engine) emitLogFrame(serverName string, frame takodclient.LogsStreamFrame, renderer *logsRenderer) {
	data := map[string]any{"service": frame.Service, "node": serverName, "data": frame.Data}
	for key, value := range map[string]string{"replica": frame.Slot, "container": frame.Container, "revision": frame.Revision, "stream": frame.Stream, "time": frame.Time} {
		if value != "" {
			data[key] = value
		}
	}
	e.emit(events.Event{
		Type:    events.TypeLogLine,
		Phase:   events.PhaseLogs,
		Level:   events.LevelInfo,
		Service: frame.Service,
		Node:    serverName,
		Message: renderer.render(logsSourcePrefix(frame.Service, frame.Slot, serverName), frame.Time, frame.Data),
		Data:    data,
	})
}

// streamJobLogs serves non-follow `tako logs JOB`: jobs have no long-running
// containers, so logs are the recorded output of the most recent run (the
// newest run started inside --since/--until when given), filtered by --grep
// and limited to the last --tail lines.
func (e *Engine) streamJobLogs(ctx context.Context, req LogsRequest, cfg *config.Config, envName string, job string, plan *logsPlan, renderer *logsRenderer) (*LogsNodeResult, error) {
	jobRuns := e.JobRuns
	if e.logsHooks != nil && e.logsHooks.jobRuns != nil {
		jobRuns = e.logsHooks.jobRuns
	}
	runs, err := jobRuns(ctx, JobRunsRequest{Config: cfg, Environment: envName, Job: job, Server: req.Server})
	if err != nil {
		return nil, err
	}
	latest, ok := selectLogsJobRun(runs.Runs, plan, time.Now())
	if !ok {
		message := fmt.Sprintf("Job %s has no recorded runs yet.\n", job)
		if plan.since != "" || plan.until != "" {
			message = fmt.Sprintf("Job %s has no recorded runs in the requested time window.\n", job)
		}
		e.info(events.TypePhaseCompleted, events.PhaseLogs, message)
		return nil, nil
	}
	if plan.timestamps {
		e.emit(events.Event{
			Type:    events.TypeWarning,
			Phase:   events.PhaseLogs,
			Level:   events.LevelWarn,
			Service: job,
			Message: fmt.Sprintf("Warning: --timestamps has no effect for recorded runs of job %s (run output has no per-line times)\n", job),
			Data:    map[string]any{"service": job, "stream": "stderr"},
		})
	}
	e.emit(events.Event{
		Type:    events.TypePhaseStarted,
		Phase:   events.PhaseLogs,
		Level:   events.LevelInfo,
		Service: job,
		Node:    latest.Server,
		Message: fmt.Sprintf("Showing last run of job %s on %s (%s, started %s):\n\n", job, latest.Server, latest.Status, latest.StartedAt.Format(time.RFC3339)),
		Data:    map[string]any{"job": job, "node": latest.Server, "status": latest.Status, "startedAt": latest.StartedAt},
	})
	lines := filterLogsRecordedLines(latest.Output, plan)
	for _, line := range lines {
		e.emitLogFrame(latest.Server, takodclient.LogsStreamFrame{Service: job, Slot: takod.LogsJobSlot, Container: latest.Container, Data: line}, renderer)
	}
	return &LogsNodeResult{Name: latest.Server, Status: logsStatusSuccess, Lines: len(lines), Sources: 1}, nil
}

// selectLogsJobRun picks the newest run (runs are newest first), restricted
// to runs started inside the requested window.
func selectLogsJobRun(runs []JobRunInfo, plan *logsPlan, now time.Time) (JobRunInfo, bool) {
	since, until := plan.sinceTime, plan.untilTime
	if since == nil && plan.since != "" {
		if duration, err := time.ParseDuration(plan.since); err == nil {
			value := now.Add(-duration)
			since = &value
		}
	}
	if until == nil && plan.until != "" {
		if duration, err := time.ParseDuration(plan.until); err == nil {
			value := now.Add(-duration)
			until = &value
		}
	}
	for _, run := range runs {
		if since != nil && run.StartedAt.Before(*since) {
			continue
		}
		if until != nil && run.StartedAt.After(*until) {
			continue
		}
		return run, true
	}
	return JobRunInfo{}, false
}

// filterLogsRecordedLines applies --grep and --tail to recorded run output.
func filterLogsRecordedLines(output string, plan *logsPlan) []string {
	output = strings.TrimRight(output, "\n")
	if output == "" {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		if plan.pattern == nil || plan.pattern.MatchString(line) {
			lines = append(lines, line)
		}
	}
	if plan.tail >= 0 && len(lines) > plan.tail {
		lines = lines[len(lines)-plan.tail:]
	}
	return lines
}

func logsNodeResultDocument(result LogNodeResult) LogsNodeResult {
	doc := LogsNodeResult{Name: result.ServerName, Host: result.Host, Status: logsStatusSuccess}
	if result.Err != nil {
		doc.Status = logsStatusFailed
		doc.Error = result.Err.Error()
	}
	return doc
}

// StreamLogNodesWith fans log streaming out to every selected node
// concurrently and returns results in deterministic server-name order.
func StreamLogNodesWith(ctx context.Context, servers map[string]config.ServerConfig, stream LogNodeStreamFunc) []LogNodeResult {
	return StreamLogNodesWithLimit(ctx, servers, 0, stream)
}

// StreamLogNodesWithLimit is StreamLogNodesWith with at most limit nodes
// streaming at once (limit <= 0 means unbounded). Results keep
// deterministic server-name order.
func StreamLogNodesWithLimit(ctx context.Context, servers map[string]config.ServerConfig, limit int, stream LogNodeStreamFunc) []LogNodeResult {
	if ctx == nil {
		ctx = context.Background()
	}
	names := SortedLogServerNames(servers)
	prefix := len(names) > 1
	var slots chan struct{}
	if limit > 0 && limit < len(names) {
		slots = make(chan struct{}, limit)
	}
	resultCh := make(chan LogNodeResult, len(names))
	var wg sync.WaitGroup
	for index, serverName := range names {
		server := servers[serverName]
		wg.Add(1)
		go func(index int, serverName string, server config.ServerConfig) {
			defer wg.Done()
			var err error
			if slots != nil {
				select {
				case slots <- struct{}{}:
					defer func() { <-slots }()
				case <-ctx.Done():
				}
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				err = ctxErr
			} else {
				err = stream(serverName, server, prefix)
			}
			resultCh <- LogNodeResult{
				Index:      index,
				ServerName: serverName,
				Host:       server.Host,
				Err:        err,
			}
		}(index, serverName, server)
	}
	wg.Wait()
	close(resultCh)

	results := make([]LogNodeResult, len(names))
	for result := range resultCh {
		results[result.Index] = result
	}
	return results
}

// logsSummaryError keeps an aggregate message while exposing one typed cause
// for exit-code classification.
type logsSummaryError struct {
	message string
	cause   error
}

func (e *logsSummaryError) Error() string { return e.message }
func (e *logsSummaryError) Unwrap() error { return e.cause }

// SummarizeLogStreamResults classifies node stream outcomes: nil when every
// node succeeded; an *AttentionError (exit 6) when some failed; when all
// failed, a *ConnectivityError (exit 4) if every failure was a connection
// failure, a capability error (exit 2) if every node lacked a required
// takod capability, otherwise a plain error (exit 1).
func SummarizeLogStreamResults(results []LogNodeResult) error {
	var failures []string
	var firstCapability error
	allConnectivity, allCapability := true, true
	for _, result := range results {
		if result.Err == nil {
			continue
		}
		failures = append(failures, fmt.Sprintf("%s: %v", result.ServerName, result.Err))
		var connectivity *ConnectivityError
		if !errors.As(result.Err, &connectivity) {
			allConnectivity = false
		}
		var capability *takodclient.CapabilityRequiredError
		if errors.As(result.Err, &capability) {
			if firstCapability == nil {
				firstCapability = capability
			}
		} else {
			allCapability = false
		}
	}
	if len(failures) == 0 {
		return nil
	}
	sort.Strings(failures)
	if len(failures) < len(results) {
		return &AttentionError{Err: fmt.Errorf("log streaming completed with %d node error(s): %s", len(failures), strings.Join(failures, "; "))}
	}
	message := fmt.Sprintf("failed to stream logs from all target nodes: %s", strings.Join(failures, "; "))
	switch {
	case allConnectivity:
		return &ConnectivityError{Err: errors.New(message)}
	case allCapability:
		return &logsSummaryError{message: message, cause: firstCapability}
	}
	return errors.New(message)
}

// SortedLogServerNames returns server map keys in deterministic order.
func SortedLogServerNames(servers map[string]config.ServerConfig) []string {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ResolveLogTargetServers resolves the logs --server selection against the
// configured environment nodes.
func ResolveLogTargetServers(cfg *config.Config, envName, requestedServer string) (map[string]config.ServerConfig, error) {
	serverNames, err := resolveLogTargetServerNames(cfg, envName, requestedServer)
	if err != nil {
		return nil, err
	}
	servers := make(map[string]config.ServerConfig, len(serverNames))
	for _, serverName := range serverNames {
		server, ok := cfg.Servers[serverName]
		if !ok {
			return nil, invalidRequestf("server %s not found in config", serverName)
		}
		servers[serverName] = server
	}
	return servers, nil
}

func resolveLogTargetServerNames(cfg *config.Config, envName string, requestedServer string) ([]string, error) {
	envServers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return nil, fmt.Errorf("failed to get environment servers: %w", err)
	}
	if len(envServers) == 0 {
		return nil, invalidRequestf("no servers configured for environment %s", envName)
	}
	if requestedServer == "" {
		return envServers, nil
	}
	if _, ok := cfg.Servers[requestedServer]; !ok {
		return nil, invalidRequestf("server %s not found in configuration", requestedServer)
	}
	for _, serverName := range envServers {
		if serverName == requestedServer {
			return []string{requestedServer}, nil
		}
	}
	return nil, invalidRequestf("server %s is not part of environment %s", requestedServer, envName)
}
