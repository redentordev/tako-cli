package takod

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	dockerCommandContext = exec.CommandContext
)

const (
	maxHealthRetries      = 100
	maxHealthWaitAttempts = 600
	maxHealthFieldBytes   = 4096
	maxHealthDuration     = 24 * time.Hour
	maxDockerVolumeName   = 255
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
}

type RemoveServiceRequest struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Service     string `json:"service"`
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
	Project           string   `json:"project"`
	Environment       string   `json:"environment"`
	Service           string   `json:"service"`
	Containers        []string `json:"containers"`
	RemovedContainers int      `json:"removedContainers,omitempty"`
}

type RemoveServiceResponse struct {
	Project           string `json:"project"`
	Environment       string `json:"environment"`
	Service           string `json:"service"`
	RemovedContainers int    `json:"removedContainers"`
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

	removedContainers, err := removeServiceContainers(ctx, req.Project, req.Environment, req.Service)
	if err != nil {
		return nil, err
	}
	if len(req.Containers) == 0 {
		return &ReconcileServiceResponse{
			Project:           req.Project,
			Environment:       req.Environment,
			Service:           req.Service,
			RemovedContainers: removedContainers,
		}, nil
	}
	if err := ensureDockerNetwork(ctx, req.Network); err != nil {
		return nil, err
	}
	if err := ensureServiceVolumes(ctx, req); err != nil {
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
		Project:           req.Project,
		Environment:       req.Environment,
		Service:           req.Service,
		Containers:        started,
		RemovedContainers: removedContainers,
	}, nil
}

func RemoveService(ctx context.Context, req RemoveServiceRequest) (*RemoveServiceResponse, error) {
	if err := validateRemoveServiceRequest(req); err != nil {
		return nil, err
	}
	removedContainers, err := removeServiceContainers(ctx, req.Project, req.Environment, req.Service)
	if err != nil {
		return nil, err
	}
	return &RemoveServiceResponse{
		Project:           req.Project,
		Environment:       req.Environment,
		Service:           req.Service,
		RemovedContainers: removedContainers,
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
	if !isSafeProjectName(req.Project) {
		return fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(req.Environment) {
		return fmt.Errorf("invalid environment name")
	}
	if !isSafeServiceName(req.Service) {
		return fmt.Errorf("invalid service name")
	}
	if err := validateImageName(req.Image); err != nil {
		return err
	}
	if !isSafeRuntimeName(req.Network) {
		return fmt.Errorf("invalid network name")
	}
	if req.NetworkAlias != "" && !isSafeRuntimeName(req.NetworkAlias) {
		return fmt.Errorf("invalid network alias")
	}
	if !isSafeRestartPolicy(req.Restart) {
		return fmt.Errorf("invalid restart policy")
	}
	if req.EnvFile != "" {
		return fmt.Errorf("envFile path is not accepted; use envFileContent")
	}
	if len(req.EnvFileContent) > 1<<20 {
		return fmt.Errorf("envFileContent exceeds 1 MiB")
	}
	for _, container := range req.Containers {
		if !isSafeContainerName(container.Name) {
			return fmt.Errorf("invalid container name")
		}
		for _, publish := range container.Publishes {
			if hasControlChars(publish) {
				return fmt.Errorf("invalid publish value")
			}
		}
	}
	for _, mount := range req.Mounts {
		if strings.TrimSpace(mount) == "" || hasControlChars(mount) {
			return fmt.Errorf("invalid mount value")
		}
	}
	for key, value := range req.Labels {
		if strings.TrimSpace(key) == "" || hasControlChars(key) || hasControlChars(value) {
			return fmt.Errorf("invalid label")
		}
	}
	if req.Command != "" && strings.ContainsRune(req.Command, '\x00') {
		return fmt.Errorf("command contains unsupported characters")
	}
	if err := validateHealthSpec(req.Health); err != nil {
		return err
	}
	return nil
}

func validateHealthSpec(health *HealthSpec) error {
	if health == nil {
		return nil
	}
	if len(health.Command) > maxHealthFieldBytes || hasControlChars(health.Command) {
		return fmt.Errorf("invalid health command")
	}
	for label, value := range map[string]string{
		"health interval":     health.Interval,
		"health timeout":      health.Timeout,
		"health start period": health.StartPeriod,
	} {
		if value == "" {
			continue
		}
		if len(value) > 64 || hasControlChars(value) {
			return fmt.Errorf("invalid %s", label)
		}
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 || duration > maxHealthDuration {
			return fmt.Errorf("invalid %s", label)
		}
	}
	if health.Retries < 0 || health.Retries > maxHealthRetries {
		return fmt.Errorf("health retries must be between 0 and %d", maxHealthRetries)
	}
	if health.WaitAttempts < 0 || health.WaitAttempts > maxHealthWaitAttempts {
		return fmt.Errorf("health waitAttempts must be between 0 and %d", maxHealthWaitAttempts)
	}
	return nil
}

func validateRemoveServiceRequest(req RemoveServiceRequest) error {
	for label, value := range map[string]string{
		"project":     req.Project,
		"environment": req.Environment,
		"service":     req.Service,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", label)
		}
	}
	if !isSafeProjectName(req.Project) {
		return fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(req.Environment) {
		return fmt.Errorf("invalid environment name")
	}
	if !isSafeServiceName(req.Service) {
		return fmt.Errorf("invalid service name")
	}
	return nil
}

func isSafeServiceName(name string) bool {
	if len(name) == 0 || len(name) > 63 || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func isSafeContainerName(name string) bool {
	if len(name) == 0 || len(name) > 128 {
		return false
	}
	first := name[0]
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || (first >= '0' && first <= '9')) {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func isSafeRestartPolicy(value string) bool {
	if value == "" {
		return true
	}
	switch value {
	case "no", "always", "unless-stopped", "on-failure":
		return true
	}
	if !strings.HasPrefix(value, "on-failure:") {
		return false
	}
	retries, err := strconv.Atoi(strings.TrimPrefix(value, "on-failure:"))
	return err == nil && retries >= 0 && retries <= 100
}

func hasControlChars(value string) bool {
	return strings.ContainsAny(value, "\x00\r\n")
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

func ensureServiceVolumes(ctx context.Context, req ReconcileServiceRequest) error {
	for _, volume := range namedVolumeSourcesFromMounts(req.Mounts) {
		if err := ensureDockerVolume(ctx, req.Project, req.Environment, req.Service, volume); err != nil {
			return fmt.Errorf("failed to ensure docker volume %s: %w", volume, err)
		}
	}
	return nil
}

func ensureDockerVolume(ctx context.Context, project string, environment string, service string, volume string) error {
	if !isSafeDockerVolumeName(volume) {
		return fmt.Errorf("invalid volume name")
	}
	args := []string{
		"volume", "create",
		"--label", "tako.project=" + project,
		"--label", "tako.environment=" + environment,
		"--label", "tako.runtime=takod",
	}
	if service != "" {
		args = append(args, "--label", "tako.service="+service)
	}
	args = append(args, volume)
	_, err := runDocker(ctx, args...)
	return err
}

func namedVolumeSourcesFromMounts(mounts []string) []string {
	seen := make(map[string]bool)
	var names []string
	for _, mount := range mounts {
		fields := parseDockerMountFields(mount)
		if fields["type"] != "volume" {
			continue
		}
		source := fields["source"]
		if source == "" {
			source = fields["src"]
		}
		if source == "" || strings.HasPrefix(source, "/") || seen[source] {
			continue
		}
		seen[source] = true
		names = append(names, source)
	}
	sort.Strings(names)
	return names
}

func parseDockerMountFields(mount string) map[string]string {
	fields := make(map[string]string)
	for _, part := range strings.Split(mount, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return fields
}

func isSafeDockerVolumeName(name string) bool {
	if strings.TrimSpace(name) == "" || len(name) > maxDockerVolumeName {
		return false
	}
	if strings.ContainsAny(name, "/\\\x00\r\n") || strings.HasPrefix(name, "-") {
		return false
	}
	for _, r := range name {
		if r <= 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func removeServiceContainers(ctx context.Context, project string, environment string, service string) (int, error) {
	output, err := runDocker(
		ctx,
		"ps",
		"-aq",
		"--filter", "label=tako.project="+project,
		"--filter", "label=tako.environment="+environment,
		"--filter", "label=tako.service="+service,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to list old service containers: %w", err)
	}
	ids := strings.Fields(strings.TrimSpace(output))
	if len(ids) == 0 {
		return 0, nil
	}
	args := append([]string{"rm", "-f"}, ids...)
	if _, err := runDocker(ctx, args...); err != nil {
		return 0, fmt.Errorf("failed to remove old service containers: %w", err)
	}
	return len(ids), nil
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
	output := newCappedOutputBuffer(defaultCommandOutputMaxBytes)
	cmd.Stdout = output
	cmd.Stderr = output
	err := cmd.Run()
	return output.String(), err
}
