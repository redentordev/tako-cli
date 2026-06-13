package takod

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

var (
	dockerCommandContext = exec.CommandContext
	shellCommandContext  = exec.CommandContext
)

type ReconcileServiceRequest struct {
	Project        string            `json:"project"`
	Environment    string            `json:"environment"`
	Service        string            `json:"service"`
	Image          string            `json:"image"`
	PullImage      bool              `json:"pullImage,omitempty"`
	Restart        string            `json:"restart,omitempty"`
	Network        string            `json:"network"`
	NetworkAlias   string            `json:"networkAlias,omitempty"`
	EnvFile        string            `json:"envFile,omitempty"`
	EnvFileContent string            `json:"envFileContent,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	Mounts         []string          `json:"mounts,omitempty"`
	Containers     []ContainerSpec   `json:"containers"`
	Health         *HealthSpec       `json:"health,omitempty"`
	Command        string            `json:"command,omitempty"`
	Init           []string          `json:"init,omitempty"`
}

type ContainerSpec struct {
	Name      string   `json:"name"`
	Publishes []string `json:"publishes,omitempty"`
}

type HealthSpec struct {
	Command      string `json:"command,omitempty"`
	Interval     string `json:"interval,omitempty"`
	Timeout      string `json:"timeout,omitempty"`
	Retries      int    `json:"retries,omitempty"`
	StartPeriod  string `json:"startPeriod,omitempty"`
	WaitAttempts int    `json:"waitAttempts,omitempty"`
}

type ReconcileServiceResponse struct {
	Project     string   `json:"project"`
	Environment string   `json:"environment"`
	Service     string   `json:"service"`
	Containers  []string `json:"containers"`
}

func ReconcileService(ctx context.Context, req ReconcileServiceRequest) (*ReconcileServiceResponse, error) {
	if err := validateReconcileServiceRequest(req); err != nil {
		return nil, err
	}
	if req.Restart == "" {
		req.Restart = "unless-stopped"
	}
	if req.NetworkAlias == "" {
		req.NetworkAlias = req.Service
	}

	if err := runInitCommands(ctx, req.Init); err != nil {
		return nil, err
	}
	if err := removeServiceContainers(ctx, req.Project, req.Environment, req.Service); err != nil {
		return nil, err
	}
	if len(req.Containers) == 0 {
		return &ReconcileServiceResponse{
			Project:     req.Project,
			Environment: req.Environment,
			Service:     req.Service,
		}, nil
	}
	if err := ensureDockerNetwork(ctx, req.Network); err != nil {
		return nil, err
	}
	cleanupEnvFile, err := prepareServiceEnvFile(&req)
	if err != nil {
		return nil, err
	}
	if cleanupEnvFile != nil {
		defer cleanupEnvFile()
	}
	if req.PullImage {
		if _, err := runDocker(ctx, "pull", req.Image); err != nil {
			return nil, fmt.Errorf("failed to pull image %s: %w", req.Image, err)
		}
	}

	started := make([]string, 0, len(req.Containers))
	for _, container := range req.Containers {
		if err := runServiceContainer(ctx, req, container); err != nil {
			return nil, err
		}
		started = append(started, container.Name)
		if err := waitForContainerHealthy(ctx, container.Name, req.Health); err != nil {
			return nil, err
		}
	}

	return &ReconcileServiceResponse{
		Project:     req.Project,
		Environment: req.Environment,
		Service:     req.Service,
		Containers:  started,
	}, nil
}

func validateReconcileServiceRequest(req ReconcileServiceRequest) error {
	for label, value := range map[string]string{
		"project":     req.Project,
		"environment": req.Environment,
		"service":     req.Service,
		"image":       req.Image,
		"network":     req.Network,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", label)
		}
	}
	if req.EnvFile != "" && !strings.HasPrefix(req.EnvFile, "/") {
		return fmt.Errorf("envFile must be an absolute path")
	}
	if req.EnvFile != "" && req.EnvFileContent != "" {
		return fmt.Errorf("envFile and envFileContent cannot both be set")
	}
	if len(req.EnvFileContent) > 1<<20 {
		return fmt.Errorf("envFileContent exceeds 1 MiB")
	}
	for _, container := range req.Containers {
		if strings.TrimSpace(container.Name) == "" {
			return fmt.Errorf("container name is required")
		}
	}
	for _, command := range req.Init {
		if strings.TrimSpace(command) == "" {
			return fmt.Errorf("init command cannot be empty")
		}
	}
	return nil
}

func runInitCommands(ctx context.Context, commands []string) error {
	for _, command := range commands {
		output, err := runShell(ctx, command)
		if err != nil {
			return fmt.Errorf("init command failed: %w, output: %s", err, output)
		}
	}
	return nil
}

func prepareServiceEnvFile(req *ReconcileServiceRequest) (func(), error) {
	if req.EnvFileContent == "" {
		return nil, nil
	}
	file, err := os.CreateTemp("", envFilePattern(req.Project, req.Environment, req.Service))
	if err != nil {
		return nil, fmt.Errorf("failed to create env file: %w", err)
	}
	cleanup := func() {
		_ = os.Remove(file.Name())
	}
	if err := os.Chmod(file.Name(), 0600); err != nil {
		file.Close()
		cleanup()
		return nil, fmt.Errorf("failed to secure env file: %w", err)
	}
	if _, err := file.WriteString(req.EnvFileContent); err != nil {
		file.Close()
		cleanup()
		return nil, fmt.Errorf("failed to write env file: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to close env file: %w", err)
	}
	req.EnvFile = file.Name()
	req.EnvFileContent = ""
	return cleanup, nil
}

func envFilePattern(parts ...string) string {
	safe := make([]string, 0, len(parts))
	for _, part := range parts {
		value := strings.Trim(sanitizeFilePatternPart(part), "-")
		if value == "" {
			value = "value"
		}
		safe = append(safe, value)
	}
	return "tako-" + strings.Join(safe, "-") + "-*.env"
}

func sanitizeFilePatternPart(value string) string {
	var out strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			out.WriteRune(r)
		} else {
			out.WriteRune('-')
		}
	}
	return out.String()
}

func ensureDockerNetwork(ctx context.Context, network string) error {
	if _, err := runDocker(ctx, "network", "inspect", network); err == nil {
		return nil
	}
	if _, err := runDocker(ctx, "network", "create", network); err != nil {
		return fmt.Errorf("failed to ensure docker network %s: %w", network, err)
	}
	return nil
}

func removeServiceContainers(ctx context.Context, project string, environment string, service string) error {
	output, err := runDocker(
		ctx,
		"ps",
		"-aq",
		"--filter", "label=tako.project="+project,
		"--filter", "label=tako.environment="+environment,
		"--filter", "label=tako.service="+service,
	)
	if err != nil {
		return fmt.Errorf("failed to list old service containers: %w", err)
	}
	ids := strings.Fields(strings.TrimSpace(output))
	if len(ids) == 0 {
		return nil
	}
	args := append([]string{"rm", "-f"}, ids...)
	if _, err := runDocker(ctx, args...); err != nil {
		return fmt.Errorf("failed to remove old service containers: %w", err)
	}
	return nil
}

func runServiceContainer(ctx context.Context, req ReconcileServiceRequest, container ContainerSpec) error {
	args := buildServiceContainerArgs(req, container)
	if output, err := runDocker(ctx, args...); err != nil {
		return fmt.Errorf("failed to start container %s: %w, output: %s", container.Name, err, output)
	}
	return nil
}

func buildServiceContainerArgs(req ReconcileServiceRequest, container ContainerSpec) []string {
	args := []string{
		"run", "-d",
		"--name", container.Name,
		"--restart", req.Restart,
		"--network", req.Network,
		"--network-alias", req.NetworkAlias,
	}

	labels := map[string]string{
		"tako.project":     req.Project,
		"tako.environment": req.Environment,
		"tako.service":     req.Service,
		"tako.runtime":     "takod",
	}
	for key, value := range req.Labels {
		labels[key] = value
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := labels[key]
		args = append(args, "--label", key+"="+value)
	}

	if req.EnvFile != "" {
		args = append(args, "--env-file", req.EnvFile)
	}
	for _, mount := range req.Mounts {
		args = append(args, "--mount", mount)
	}
	for _, publish := range container.Publishes {
		args = append(args, "--publish", publish)
	}
	if req.Health != nil && req.Health.Command != "" {
		args = append(args, "--health-cmd", req.Health.Command)
		if req.Health.Interval != "" {
			args = append(args, "--health-interval", req.Health.Interval)
		}
		if req.Health.Timeout != "" {
			args = append(args, "--health-timeout", req.Health.Timeout)
		}
		if req.Health.Retries > 0 {
			args = append(args, "--health-retries", fmt.Sprintf("%d", req.Health.Retries))
		}
		if req.Health.StartPeriod != "" {
			args = append(args, "--health-start-period", req.Health.StartPeriod)
		}
	}

	args = append(args, req.Image)
	if req.Command != "" {
		args = append(args, "sh", "-c", req.Command)
	}
	return args
}

func waitForContainerHealthy(ctx context.Context, containerName string, health *HealthSpec) error {
	attempts := 30
	if health != nil {
		if health.WaitAttempts > 0 {
			attempts = health.WaitAttempts
		} else if health.Retries > 0 {
			attempts = health.Retries
		}
	}

	for i := 0; i < attempts; i++ {
		status, err := runDocker(ctx, "inspect", containerName, "--format", "{{.State.Health.Status}}")
		status = strings.TrimSpace(status)
		if err == nil && status == "healthy" {
			return nil
		}
		if err == nil && status == "unhealthy" {
			logs, _ := runDocker(ctx, "logs", containerName, "--tail", "50")
			return fmt.Errorf("container %s health check failed, last logs:\n%s", containerName, logs)
		}

		running, runErr := runDocker(ctx, "inspect", containerName, "--format", "{{.State.Running}}")
		if runErr != nil {
			return fmt.Errorf("failed to inspect container %s: %w", containerName, runErr)
		}
		if strings.TrimSpace(running) == "true" && (health == nil || health.Command == "") {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("health check timeout for %s after %d attempts", containerName, attempts)
}

func runDocker(ctx context.Context, args ...string) (string, error) {
	cmd := dockerCommandContext(ctx, "docker", args...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	return output.String(), err
}

func runShell(ctx context.Context, command string) (string, error) {
	cmd := shellCommandContext(ctx, "sh", "-c", command)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	return output.String(), err
}
