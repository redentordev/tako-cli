package takod

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

const (
	HookPreDeploy  = "preDeploy"
	HookPostDeploy = "postDeploy"

	defaultHookTimeout = 10 * time.Minute
	maxHookTimeout     = 24 * time.Hour
)

type RunHookRequest struct {
	Project        string        `json:"project"`
	Environment    string        `json:"environment"`
	Service        string        `json:"service"`
	Hook           string        `json:"hook"`
	Image          string        `json:"image"`
	PullImage      bool          `json:"pullImage,omitempty"`
	RegistryAuth   *RegistryAuth `json:"registryAuth,omitempty"`
	Network        string        `json:"network"`
	EnvFile        string        `json:"-"`
	EnvFileContent string        `json:"envFileContent,omitempty"`
	Mounts         []string      `json:"mounts,omitempty"`
	Command        []string      `json:"command,omitempty"`
	User           string        `json:"user,omitempty"`
	WorkingDir     string        `json:"workingDir,omitempty"`
	Timeout        string        `json:"timeout,omitempty"`
}

type RunHookResponse struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Service     string `json:"service"`
	Hook        string `json:"hook"`
	Container   string `json:"container"`
	ExitCode    int    `json:"exitCode"`
	Error       string `json:"error,omitempty"`
}

func RunHookCommand(ctx context.Context, req RunHookRequest) (*RunHookResponse, error) {
	if err := validateRunHookRequest(req); err != nil {
		return nil, err
	}
	timeout, err := hookTimeout(req.Timeout)
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := ensureDockerNetwork(runCtx, req.Network, dockerNetworkOwner{
		Project:     req.Project,
		Environment: req.Environment,
	}); err != nil {
		return nil, err
	}
	if req.PullImage {
		if _, err := pullImage(runCtx, req.Image, req.RegistryAuth); err != nil {
			return nil, fmt.Errorf("failed to pull image %s: %w", req.Image, err)
		}
	}
	if err := ensureOneOffVolumes(runCtx, RunOneOffRequest{
		Project:     req.Project,
		Environment: req.Environment,
		Service:     req.Service,
		Mounts:      req.Mounts,
	}); err != nil {
		return nil, err
	}
	if err := cleanupSuccessfulHookContainers(runCtx, req); err != nil {
		return nil, err
	}

	cleanupEnvFile, err := prepareHookEnvFile(&req)
	if err != nil {
		return nil, err
	}
	if cleanupEnvFile != nil {
		defer cleanupEnvFile()
	}

	containerName := hookContainerName(req.Project, req.Environment, req.Service, req.Hook)
	if _, err := runDocker(runCtx, buildHookRunArgs(req, containerName)...); err != nil {
		return nil, fmt.Errorf("failed to start %s hook container: %w", req.Hook, err)
	}

	response := &RunHookResponse{
		Project:     req.Project,
		Environment: req.Environment,
		Service:     req.Service,
		Hook:        req.Hook,
		Container:   containerName,
	}

	waitOutput, err := runDocker(runCtx, "wait", containerName)
	if err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			stopHookContainer(containerName)
			response.ExitCode = 124
			response.Error = fmt.Sprintf("%s hook timed out after %s", req.Hook, timeout)
			return response, nil
		}
		return nil, fmt.Errorf("failed waiting for %s hook container %s: %w", req.Hook, containerName, err)
	}

	exitCode, err := parseDockerWaitExitCode(waitOutput)
	if err != nil {
		return nil, fmt.Errorf("failed to parse %s hook exit code: %w", req.Hook, err)
	}
	response.ExitCode = exitCode
	if exitCode != 0 {
		response.Error = fmt.Sprintf("%s hook failed with exit code %d", req.Hook, exitCode)
		return response, nil
	}

	if _, err := runDocker(context.Background(), "rm", "-f", containerName); err != nil {
		return nil, fmt.Errorf("%s hook succeeded but failed to remove container %s: %w", req.Hook, containerName, err)
	}
	return response, nil
}

func validateRunHookRequest(req RunHookRequest) error {
	if err := validateExecTargetRequest(ExecTargetRequest{
		Project:     req.Project,
		Environment: req.Environment,
		Service:     req.Service,
	}); err != nil {
		return err
	}
	if req.Hook != HookPreDeploy && req.Hook != HookPostDeploy {
		return fmt.Errorf("invalid hook")
	}
	if strings.TrimSpace(req.Image) == "" {
		return fmt.Errorf("image is required")
	}
	if err := validateImageName(req.Image); err != nil {
		return err
	}
	if strings.TrimSpace(req.Network) == "" {
		return fmt.Errorf("network is required")
	}
	if !isSafeRuntimeName(req.Network) {
		return fmt.Errorf("invalid network name")
	}
	if req.EnvFile != "" {
		return fmt.Errorf("envFile path is not accepted; use envFileContent")
	}
	if len(req.EnvFileContent) > 1<<20 {
		return fmt.Errorf("envFileContent exceeds 1 MiB")
	}
	for _, mount := range req.Mounts {
		if strings.TrimSpace(mount) == "" || hasControlChars(mount) {
			return fmt.Errorf("invalid mount value")
		}
	}
	if req.User != "" && hasControlChars(req.User) {
		return fmt.Errorf("invalid hook user")
	}
	if req.WorkingDir != "" && hasControlChars(req.WorkingDir) {
		return fmt.Errorf("invalid hook working directory")
	}
	if _, err := hookTimeout(req.Timeout); err != nil {
		return err
	}
	if len(req.Command) == 0 {
		return fmt.Errorf("hook command is required")
	}
	return validateCommandArgs(req.Command)
}

func hookTimeout(raw string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return defaultHookTimeout, nil
	}
	timeout, err := time.ParseDuration(raw)
	if err != nil || timeout <= 0 || timeout > maxHookTimeout {
		return 0, fmt.Errorf("invalid hook timeout")
	}
	return timeout, nil
}

func prepareHookEnvFile(req *RunHookRequest) (func(), error) {
	if req.EnvFileContent == "" {
		return nil, nil
	}
	reconcileReq := ReconcileServiceRequest{
		Project:        req.Project,
		Environment:    req.Environment,
		Service:        req.Service,
		EnvFileContent: req.EnvFileContent,
	}
	cleanup, err := prepareServiceEnvFile(&reconcileReq)
	if err != nil {
		return nil, err
	}
	req.EnvFileContent = ""
	req.EnvFile = reconcileReq.EnvFile
	return cleanup, nil
}

func buildHookRunArgs(req RunHookRequest, containerName string) []string {
	args := []string{
		"run", "-d",
		"--name", containerName,
		"--network", req.Network,
		"--label", "tako.project=" + req.Project,
		"--label", "tako.environment=" + req.Environment,
		"--label", "tako.service=" + req.Service,
		"--label", "tako.runtime=takod",
		"--label", "tako.oneOff=true",
		"--label", "tako.hook=true",
		"--label", "tako.hook.phase=" + req.Hook,
	}
	if req.User != "" {
		args = append(args, "--user", req.User)
	}
	if req.WorkingDir != "" {
		args = append(args, "--workdir", req.WorkingDir)
	}
	if req.EnvFile != "" {
		args = append(args, "--env-file", req.EnvFile)
	}
	for _, mount := range req.Mounts {
		args = append(args, "--mount", mount)
	}
	args = append(args, req.Image)
	args = append(args, req.Command...)
	return args
}

func cleanupSuccessfulHookContainers(ctx context.Context, req RunHookRequest) error {
	output, err := runDocker(ctx,
		"ps", "-aq",
		"--filter", "label=tako.project="+req.Project,
		"--filter", "label=tako.environment="+req.Environment,
		"--filter", "label=tako.service="+req.Service,
		"--filter", "label=tako.hook=true",
		"--filter", "label=tako.hook.phase="+req.Hook,
		"--filter", "exited=0",
	)
	if err != nil {
		return fmt.Errorf("failed to list successful hook containers: %w", err)
	}
	ids := strings.Fields(strings.TrimSpace(output))
	if len(ids) == 0 {
		return nil
	}
	args := append([]string{"rm", "-f"}, ids...)
	if _, err := runDocker(ctx, args...); err != nil {
		return fmt.Errorf("failed to remove successful hook containers: %w", err)
	}
	return nil
}

func stopHookContainer(containerName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = runDocker(ctx, "stop", containerName)
}

func parseDockerWaitExitCode(output string) (int, error) {
	fields := strings.Fields(strings.TrimSpace(output))
	if len(fields) == 0 {
		return 1, fmt.Errorf("empty docker wait output")
	}
	exitCode, err := strconv.Atoi(fields[len(fields)-1])
	if err != nil {
		return 1, err
	}
	if exitCode < 0 || exitCode > 255 {
		return 1, fmt.Errorf("exit code out of range: %d", exitCode)
	}
	return exitCode, nil
}

func hookContainerName(project string, environment string, service string, hook string) string {
	token := strconv.FormatInt(time.Now().UnixNano(), 36)
	base := runtimeid.ContainerName(project, environment, service+"-"+hook, 1)
	suffix := "_" + token
	if len(base)+len(suffix) > 128 {
		base = strings.TrimRight(base[:128-len(suffix)], "_")
	}
	return base + suffix
}
