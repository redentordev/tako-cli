package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
	"github.com/redentordev/tako-cli/pkg/secrets"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

// KindExecResult identifies a serialized exec result document.
const KindExecResult = "ExecResult"

// DefaultExecTimeout bounds a remote exec when the caller sets none.
const DefaultExecTimeout = 10 * time.Minute

// execStreamGrace keeps the client-side deadline behind takod's server-side
// timeout so the terminal exit frame arrives before the SSH stream is cut.
const execStreamGrace = 30 * time.Second

// RemoteExitError carries a remote command's non-zero exit code so the CLI
// process can mirror it in human mode.
type RemoteExitError struct {
	Code int
}

func (e *RemoteExitError) Error() string {
	return fmt.Sprintf("remote command exited with code %d", e.Code)
}

// ExecRequest runs a command in a service's context on one node.
type ExecRequest struct {
	Config      *config.Config
	Environment string
	Service     string
	// Server pins the target node; empty resolves to the first node where
	// the service is deployed.
	Server string
	// Replica selects the 1-based running replica in attach mode.
	Replica int
	// OneOff runs the command in a fresh container from the service's
	// current image with its env/secrets and network instead of attaching
	// to a running replica.
	OneOff  bool
	Timeout time.Duration
	Command []string
}

// ExecResult is the serializable outcome of `tako exec`. ExitCode is the
// remote command's code; the tako process itself reports success when the
// exec ran to completion (human mode mirrors ExitCode for scripting).
type ExecResult struct {
	APIVersion  string   `json:"apiVersion"`
	Kind        string   `json:"kind"`
	Project     string   `json:"project"`
	Environment string   `json:"environment"`
	Service     string   `json:"service"`
	Server      string   `json:"server"`
	Host        string   `json:"host,omitempty"`
	Container   string   `json:"container,omitempty"`
	Mode        string   `json:"mode"`
	Command     []string `json:"command"`
	ExitCode    int      `json:"exitCode"`
	DurationMs  int64    `json:"durationMs"`
	Error       string   `json:"error,omitempty"`
}

// Exec resolves placement, opens the SSH stream to takod's /v1/exec, and
// forwards output as exec.* events until the terminal exit frame.
func (e *Engine) Exec(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.Config == nil {
		return nil, invalidRequestf("exec request requires a loaded config")
	}
	if strings.TrimSpace(req.Environment) == "" {
		return nil, invalidRequestf("exec request requires an environment")
	}
	if strings.TrimSpace(req.Service) == "" {
		return nil, invalidRequestf("exec request requires a service")
	}
	if len(req.Command) == 0 {
		return nil, invalidRequestf("exec request requires a command (tako exec %s -- CMD [ARGS...])", req.Service)
	}
	if req.Replica < 0 {
		return nil, invalidRequestf("replica must be positive")
	}
	if req.Replica > 0 && req.OneOff {
		return nil, invalidRequestf("--replica selects a running container and cannot be combined with --oneoff")
	}

	cfg := req.Config
	envName := req.Environment
	services, err := cfg.GetServices(envName)
	if err != nil {
		return nil, fmt.Errorf("failed to get services: %w", err)
	}
	service, ok := services[req.Service]
	if !ok {
		return nil, invalidRequestf("service '%s' not found in environment %s", req.Service, envName)
	}
	for _, server := range cfg.Servers {
		e.RegisterSecret(server.Password)
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultExecTimeout
	}

	mode := takod.ExecModeAttach
	if req.OneOff {
		mode = takod.ExecModeOneOff
	}

	result := &ExecResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindExecResult,
		Project:     cfg.Project.Name,
		Environment: envName,
		Service:     req.Service,
		Mode:        mode,
		Command:     append([]string(nil), req.Command...),
		ExitCode:    -1,
	}

	serverName, serverCfg, err := e.resolveExecServer(ctx, cfg, envName, req.Service, req.Server)
	if err != nil {
		return nil, err
	}
	result.Server = serverName
	result.Host = serverCfg.Host

	takodReq := takod.ExecRequest{
		Project:        cfg.Project.Name,
		Environment:    envName,
		Service:        req.Service,
		Mode:           mode,
		Command:        append([]string(nil), req.Command...),
		Replica:        req.Replica,
		Network:        runtimeid.NetworkName(cfg.Project.Name, envName),
		TimeoutSeconds: int(timeout / time.Second),
	}
	if req.OneOff {
		envContent, err := buildExecEnvFileContent(e, envName, &service)
		if err != nil {
			return nil, err
		}
		takodReq.EnvFileContent = envContent
	}

	payload, err := json.Marshal(takodReq)
	if err != nil {
		return nil, fmt.Errorf("failed to encode exec request: %w", err)
	}

	client, err := connectTakodStreamNodeContext(ctx, serverCfg)
	if err != nil {
		return nil, &ConnectivityError{Err: fmt.Errorf("failed to connect to node %s: %w", serverName, err)}
	}
	defer client.Close()

	e.emit(events.Event{
		Type:    events.TypeExecStarted,
		Phase:   events.PhaseLogs,
		Level:   events.LevelDebug,
		Service: req.Service,
		Node:    serverName,
		Message: fmt.Sprintf("Using node: %s (%s)\n", serverName, serverCfg.Host),
		Data:    map[string]any{"mode": mode, "node": serverName, "host": serverCfg.Host, "command": takodReq.Command},
	})

	streamCtx, cancel := context.WithTimeout(ctx, timeout+execStreamGrace)
	defer cancel()

	started := time.Now()
	reader, writer := io.Pipe()
	streamDone := make(chan error, 1)
	go func() {
		err := takodclient.StreamRequestOutputWithContext(streamCtx, client, TakodSocketFromConfig(cfg), "POST", takodclient.ExecEndpoint(), strings.NewReader(string(payload)), writer, writer)
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
			Type:    events.TypeExecOutput,
			Phase:   events.PhaseLogs,
			Level:   events.LevelInfo,
			Service: req.Service,
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
		opErr = fmt.Errorf("exec stream ended without an exit status")
	}
	if opErr != nil && ctx.Err() != nil {
		opErr = ctx.Err()
	}
	if opErr != nil {
		result.Error = opErr.Error()
	}

	e.emit(events.Event{
		Type:    events.TypeExecCompleted,
		Phase:   events.PhaseLogs,
		Level:   events.LevelDebug,
		Service: req.Service,
		Node:    serverName,
		Data:    map[string]any{"exitCode": result.ExitCode, "durationMs": result.DurationMs},
	})
	return result, opErr
}

// resolveExecServer picks the target node: the pinned --server, or the
// first environment node whose actual state carries the service.
func (e *Engine) resolveExecServer(ctx context.Context, cfg *config.Config, envName string, serviceName string, requested string) (string, config.ServerConfig, error) {
	serverNames, err := ResolveStatusTargetServerNames(cfg, envName, requested)
	if err != nil {
		return "", config.ServerConfig{}, err
	}
	if requested != "" {
		name := serverNames[0]
		return name, cfg.Servers[name], nil
	}

	var lastErr error
	for _, name := range serverNames {
		server := cfg.Servers[name]
		client, err := connectTakodStreamNodeContext(ctx, server)
		if err != nil {
			lastErr = fmt.Errorf("failed to connect to node %s: %w", name, err)
			continue
		}
		response, err := ActualStateViaTakod(client, cfg, envName)
		_ = client.Close()
		if err != nil {
			lastErr = fmt.Errorf("failed to query takod on node %s: %w", name, err)
			continue
		}
		if response != nil {
			if actual, ok := response.Services[serviceName]; ok && actual != nil {
				return name, server, nil
			}
		}
	}
	if lastErr != nil {
		return "", config.ServerConfig{}, &ConnectivityError{Err: fmt.Errorf("service %s not reachable on any node: %w", serviceName, lastErr)}
	}
	return "", config.ServerConfig{}, invalidRequestf("service %s is not deployed on any node in environment %s; deploy first or pass --server", serviceName, envName)
}

// buildExecEnvFileContent renders the service's env/secrets exactly as a
// deploy would, registering secret values with the event redactor first.
func buildExecEnvFileContent(e *Engine, envName string, service *config.ServiceConfig) (string, error) {
	if len(service.Env) == 0 && len(service.Secrets) == 0 && service.EnvFile == "" && len(service.EnvFiles) == 0 {
		return "", nil
	}
	mgr, err := secrets.NewManager(envName)
	if err != nil {
		return "", fmt.Errorf("failed to create secrets manager: %w", err)
	}
	for _, key := range service.Secrets {
		if value, err := mgr.Get(key); err == nil {
			e.RegisterSecret(value)
		}
	}
	envFile, err := mgr.CreateEnvFile(service)
	if err != nil {
		return "", fmt.Errorf("failed to create env file: %w", err)
	}
	data, err := io.ReadAll(envFile.ToReader())
	if err != nil {
		return "", fmt.Errorf("failed to render env file: %w", err)
	}
	return string(data), nil
}

// registerServiceSecretValues adds every referenced secret value to the
// event redactor so streamed output (logs, exec, release commands) never
// carries plaintext secrets.
func (e *Engine) registerServiceSecretValues(envName string, services map[string]config.ServiceConfig) {
	var mgr *secrets.Manager
	for _, service := range services {
		if len(service.Secrets) == 0 {
			continue
		}
		if mgr == nil {
			created, err := secrets.NewManager(envName)
			if err != nil {
				return
			}
			mgr = created
		}
		for _, key := range service.Secrets {
			if value, err := mgr.Get(key); err == nil {
				e.RegisterSecret(value)
			}
		}
	}
}
