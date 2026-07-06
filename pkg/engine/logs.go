package engine

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

const (
	// KindLogsResult identifies a serialized logs result document.
	KindLogsResult = "LogsResult"

	logsStatusSuccess = "success"
	logsStatusFailed  = "failed"
)

// LogsRequest describes one logs streaming operation. Config must be loaded
// and validated; Environment must be resolved.
type LogsRequest struct {
	Config      *config.Config
	Environment string
	Service     string
	Server      string
	Tail        int
	Follow      bool
}

// LogsNodeResult is the serializable outcome for one streamed takod node.
type LogsNodeResult struct {
	Name   string `json:"name"`
	Host   string `json:"host,omitempty"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// LogsResult is the serializable outcome of StreamLogs.
type LogsResult struct {
	APIVersion  string           `json:"apiVersion"`
	Kind        string           `json:"kind"`
	Project     string           `json:"project"`
	Environment string           `json:"environment"`
	Service     string           `json:"service"`
	Tail        int              `json:"tail"`
	Follow      bool             `json:"follow"`
	Status      string           `json:"status"`
	Nodes       []LogsNodeResult `json:"nodes"`
	StartedAt   time.Time        `json:"startedAt"`
	Duration    float64          `json:"durationSeconds"`
	Message     string           `json:"message,omitempty"`
	Error       string           `json:"error,omitempty"`
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

// StreamLogs streams service logs from the selected takod nodes. Human output
// is emitted only as events; the returned result is suitable for machine
// output documents.
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
	if strings.TrimSpace(req.Service) == "" {
		return nil, invalidRequestf("logs request requires a service")
	}
	if req.Tail < 0 {
		return nil, invalidRequestf("tail cannot be negative")
	}

	cfg := req.Config
	envName := req.Environment
	services, err := cfg.GetServices(envName)
	if err != nil {
		return nil, fmt.Errorf("failed to get services: %w", err)
	}
	if _, exists := services[req.Service]; !exists {
		return nil, invalidRequestf("service %s not found in environment %s", req.Service, envName)
	}

	servers, err := ResolveLogTargetServers(cfg, envName, req.Server)
	if err != nil {
		return nil, err
	}
	if len(servers) == 0 {
		return nil, invalidRequestf("no servers configured for environment %s", envName)
	}

	for _, server := range cfg.Servers {
		e.RegisterSecret(server.Password)
	}
	for _, service := range services {
		for _, value := range service.Env {
			e.RegisterSecret(value)
		}
	}

	startedAt := time.Now()
	result := &LogsResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindLogsResult,
		Project:     cfg.Project.Name,
		Environment: envName,
		Service:     req.Service,
		Tail:        req.Tail,
		Follow:      req.Follow,
		StartedAt:   startedAt,
	}

	banner := fmt.Sprintf("Streaming logs from %s on %d takod node(s)...\n\n", req.Service, len(servers))
	e.emit(events.Event{
		Type:    events.TypePhaseStarted,
		Phase:   events.PhaseLogs,
		Level:   events.LevelInfo,
		Service: req.Service,
		Message: banner,
		Data: map[string]any{
			"service": req.Service,
			"nodes":   len(servers),
			"tail":    req.Tail,
			"follow":  req.Follow,
		},
	})

	if err := ctx.Err(); err != nil {
		result.Status = logsStatusFailed
		result.Error = err.Error()
		result.Duration = time.Since(startedAt).Seconds()
		e.emit(events.Event{
			Type:    events.TypePhaseCompleted,
			Phase:   events.PhaseLogs,
			Level:   events.LevelError,
			Service: req.Service,
			Data:    map[string]any{"status": result.Status, "error": result.Error},
		})
		return result, err
	}

	nodeResults := StreamLogNodesWith(ctx, servers, func(serverName string, server config.ServerConfig, prefix bool) error {
		return e.streamLogsFromNode(ctx, cfg, envName, serverName, server, req.Service, req.Tail, req.Follow, prefix)
	})
	for _, nodeResult := range nodeResults {
		result.Nodes = append(result.Nodes, logsNodeResultDocument(nodeResult))
	}
	result.Duration = time.Since(startedAt).Seconds()

	if err := ctx.Err(); err != nil {
		result.Status = logsStatusFailed
		result.Error = err.Error()
		e.emit(events.Event{
			Type:    events.TypePhaseCompleted,
			Phase:   events.PhaseLogs,
			Level:   events.LevelError,
			Service: req.Service,
			Data:    map[string]any{"status": result.Status, "error": result.Error},
		})
		return result, err
	}

	if err := SummarizeLogStreamResults(nodeResults); err != nil {
		result.Status = logsStatusFailed
		result.Error = err.Error()
		e.emit(events.Event{
			Type:    events.TypePhaseCompleted,
			Phase:   events.PhaseLogs,
			Level:   events.LevelError,
			Service: req.Service,
			Data:    map[string]any{"status": result.Status, "error": result.Error},
		})
		return result, err
	}

	result.Status = logsStatusSuccess
	result.Message = fmt.Sprintf("streamed logs from %d node(s)", len(result.Nodes))
	e.emit(events.Event{
		Type:    events.TypePhaseCompleted,
		Phase:   events.PhaseLogs,
		Level:   events.LevelInfo,
		Service: req.Service,
		Data:    map[string]any{"status": result.Status, "nodes": len(result.Nodes)},
	})
	return result, nil
}

func logsNodeResultDocument(result LogNodeResult) LogsNodeResult {
	doc := LogsNodeResult{Name: result.ServerName, Host: result.Host, Status: logsStatusSuccess}
	if result.Err != nil {
		doc.Status = logsStatusFailed
		doc.Error = result.Err.Error()
	}
	return doc
}

func (e *Engine) emitLogLine(service string, serverName string, line string, prefix bool) {
	e.emit(events.Event{
		Type:    events.TypeLogLine,
		Phase:   events.PhaseLogs,
		Level:   events.LevelInfo,
		Service: service,
		Node:    serverName,
		Message: formatLogLineMessage(serverName, line, prefix),
		Data:    map[string]any{"service": service, "node": serverName, "data": line},
	})
}

func formatLogLineMessage(serverName string, line string, prefix bool) string {
	if prefix {
		return fmt.Sprintf("[%s] %s\n", serverName, line)
	}
	return line + "\n"
}

func (e *Engine) streamLogsFromNode(
	ctx context.Context,
	cfg *config.Config,
	envName string,
	serverName string,
	server config.ServerConfig,
	service string,
	tail int,
	follow bool,
	prefix bool,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	client, err := connectTakodStreamNodeContext(ctx, server)
	if err != nil {
		return fmt.Errorf("failed to connect to node %s: %w", serverName, err)
	}
	defer client.Close()

	e.emit(events.Event{
		Type:    events.TypeLogLine,
		Phase:   events.PhaseLogs,
		Level:   events.LevelDebug,
		Service: service,
		Node:    serverName,
		Message: fmt.Sprintf("Using node: %s (%s)\n", serverName, server.Host),
		Data:    map[string]any{"service": service, "node": serverName, "host": server.Host},
	})

	if err := ctx.Err(); err != nil {
		return err
	}
	endpoint := takodclient.LogsEndpoint(cfg.Project.Name, envName, service, tail, follow)
	reader, writer := io.Pipe()
	streamDone := make(chan error, 1)
	go func() {
		err := takodclient.StreamOutputWithContext(ctx, client, TakodSocketFromConfig(cfg), endpoint, writer, writer)
		if err != nil {
			_ = writer.CloseWithError(err)
		} else {
			_ = writer.Close()
		}
		streamDone <- err
	}()

	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		e.emitLogLine(service, serverName, line, prefix)
	}
	scanErr := scanner.Err()
	streamErr := <-streamDone
	if streamErr != nil {
		return streamErr
	}
	if scanErr != nil {
		return scanErr
	}
	return ctx.Err()
}

func connectTakodStreamNode(server config.ServerConfig) (*ssh.Client, error) {
	return connectTakodStreamNodeContext(context.Background(), server)
}

func connectTakodStreamNodeContext(ctx context.Context, server config.ServerConfig) (*ssh.Client, error) {
	client, err := ssh.NewClientFromConfig(ssh.ServerConfig{
		Host:     server.Host,
		Port:     server.Port,
		User:     server.User,
		SSHKey:   server.SSHKey,
		Password: server.Password,
	})
	if err != nil {
		return nil, err
	}
	if err := client.ConnectContext(ctx); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

// StreamLogNodesWith fans log streaming out to the selected nodes concurrently
// and returns results in deterministic server-name order.
func StreamLogNodesWith(ctx context.Context, servers map[string]config.ServerConfig, stream LogNodeStreamFunc) []LogNodeResult {
	if ctx == nil {
		ctx = context.Background()
	}
	names := SortedLogServerNames(servers)
	prefix := len(names) > 1
	resultCh := make(chan LogNodeResult, len(names))
	var wg sync.WaitGroup
	for index, serverName := range names {
		server := servers[serverName]
		wg.Add(1)
		go func(index int, serverName string, server config.ServerConfig) {
			defer wg.Done()
			var err error
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

// SummarizeLogStreamResults returns the historical aggregate error message for
// failed node streams.
func SummarizeLogStreamResults(results []LogNodeResult) error {
	var failures []string
	for _, result := range results {
		if result.Err == nil {
			continue
		}
		failures = append(failures, fmt.Sprintf("%s: %v", result.ServerName, result.Err))
	}
	if len(failures) == 0 {
		return nil
	}
	sort.Strings(failures)
	if len(failures) == len(results) {
		return fmt.Errorf("failed to stream logs from all target nodes: %s", strings.Join(failures, "; "))
	}
	return fmt.Errorf("log streaming completed with %d node error(s): %s", len(failures), strings.Join(failures, "; "))
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
