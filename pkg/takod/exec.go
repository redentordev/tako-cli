package takod

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

const (
	maxExecCommandArgs  = 128
	maxExecCommandBytes = 4096
)

type ExecTargetRequest struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Service     string `json:"service"`
	Slot        int    `json:"slot,omitempty"`
}

type ExecTargetResponse struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Service     string `json:"service"`
	Slot        int    `json:"slot,omitempty"`
	Container   string `json:"container"`
	ContainerID string `json:"containerId"`
}

type ExecStreamRequest struct {
	Project     string   `json:"project"`
	Environment string   `json:"environment"`
	Service     string   `json:"service"`
	Slot        int      `json:"slot,omitempty"`
	Command     []string `json:"command,omitempty"`
	Stdin       bool     `json:"stdin,omitempty"`
	TTY         bool     `json:"tty,omitempty"`
}

type RunOneOffRequest struct {
	Project        string        `json:"project"`
	Environment    string        `json:"environment"`
	Service        string        `json:"service"`
	Image          string        `json:"image"`
	PullImage      bool          `json:"pullImage,omitempty"`
	RegistryAuth   *RegistryAuth `json:"registryAuth,omitempty"`
	Network        string        `json:"network"`
	EnvFile        string        `json:"-"`
	EnvFileContent string        `json:"envFileContent,omitempty"`
	Mounts         []string      `json:"mounts,omitempty"`
	Command        []string      `json:"command,omitempty"`
	Stdin          bool          `json:"stdin,omitempty"`
	TTY            bool          `json:"tty,omitempty"`
	Remove         bool          `json:"remove,omitempty"`
}

func ResolveExecTarget(ctx context.Context, req ExecTargetRequest) (*ExecTargetResponse, error) {
	if err := validateExecTargetRequest(req); err != nil {
		return nil, err
	}

	containers, err := execTargetContainerNames(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(containers) == 0 {
		return nil, fmt.Errorf("no running containers found for service %s", req.Service)
	}

	inspected, err := inspectProxyTargetContainers(ctx, containers)
	if err != nil {
		return nil, err
	}

	for _, container := range inspected {
		if !execContainerAvailable(container) {
			continue
		}
		containerName := strings.TrimPrefix(container.Name, "/")
		return &ExecTargetResponse{
			Project:     req.Project,
			Environment: req.Environment,
			Service:     req.Service,
			Slot:        req.Slot,
			Container:   containerName,
			ContainerID: container.ID,
		}, nil
	}

	return nil, fmt.Errorf("no healthy running containers found for service %s", req.Service)
}

func ExecuteServiceCommand(ctx context.Context, req ExecStreamRequest, stdin io.Reader, writer io.Writer) (int, error) {
	if err := validateExecStreamRequest(req); err != nil {
		return 1, err
	}

	target, err := ResolveExecTarget(ctx, ExecTargetRequest{
		Project:     req.Project,
		Environment: req.Environment,
		Service:     req.Service,
		Slot:        req.Slot,
	})
	if err != nil {
		return 1, err
	}

	command := req.Command
	if len(command) == 0 {
		command = []string{"sh"}
	}

	args := []string{"exec"}
	if req.Stdin || req.TTY {
		args = append(args, "-i")
	}
	if req.TTY {
		args = append(args, "-t")
	}
	args = append(args, target.Container)
	args = append(args, command...)

	cmd := dockerCommandContext(ctx, "docker", args...)
	if req.Stdin || req.TTY {
		if stdin == nil {
			stdin = strings.NewReader("")
		}
		cmd.Stdin = stdin
	}
	cmd.Stdout = writer
	cmd.Stderr = writer

	return runStreamingCommand(cmd)
}

func RunOneOffCommand(ctx context.Context, req RunOneOffRequest, stdin io.Reader, writer io.Writer) (int, error) {
	if err := validateRunOneOffRequest(req); err != nil {
		return 1, err
	}
	if err := ensureDockerNetwork(ctx, req.Network, dockerNetworkOwner{
		Project:     req.Project,
		Environment: req.Environment,
	}); err != nil {
		return 1, err
	}
	if req.PullImage {
		if _, err := pullImage(ctx, req.Image, req.RegistryAuth); err != nil {
			return 1, fmt.Errorf("failed to pull image %s: %w", req.Image, err)
		}
	}
	if err := ensureOneOffVolumes(ctx, req); err != nil {
		return 1, err
	}

	cleanupEnvFile, err := prepareOneOffEnvFile(&req)
	if err != nil {
		return 1, err
	}
	if cleanupEnvFile != nil {
		defer cleanupEnvFile()
	}

	args := buildOneOffRunArgs(req)
	cmd := dockerCommandContext(ctx, "docker", args...)
	if req.Stdin || req.TTY {
		if stdin == nil {
			stdin = strings.NewReader("")
		}
		cmd.Stdin = stdin
	}
	cmd.Stdout = writer
	cmd.Stderr = writer

	return runStreamingCommand(cmd)
}

func validateExecTargetRequest(req ExecTargetRequest) error {
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
	if req.Slot < 0 || req.Slot > 10000 {
		return fmt.Errorf("invalid replica slot")
	}
	return nil
}

func validateExecStreamRequest(req ExecStreamRequest) error {
	if err := validateExecTargetRequest(ExecTargetRequest{
		Project:     req.Project,
		Environment: req.Environment,
		Service:     req.Service,
		Slot:        req.Slot,
	}); err != nil {
		return err
	}
	return validateCommandArgs(req.Command)
}

func validateRunOneOffRequest(req RunOneOffRequest) error {
	if err := validateExecTargetRequest(ExecTargetRequest{
		Project:     req.Project,
		Environment: req.Environment,
		Service:     req.Service,
	}); err != nil {
		return err
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
	if len(req.EnvFileContent) > 1<<20 {
		return fmt.Errorf("envFileContent exceeds 1 MiB")
	}
	for _, mount := range req.Mounts {
		if strings.TrimSpace(mount) == "" || hasControlChars(mount) {
			return fmt.Errorf("invalid mount value")
		}
	}
	return validateCommandArgs(req.Command)
}

func validateCommandArgs(command []string) error {
	if len(command) > maxExecCommandArgs {
		return fmt.Errorf("command has too many arguments")
	}
	for _, arg := range command {
		if len(arg) > maxExecCommandBytes || strings.ContainsRune(arg, '\x00') {
			return fmt.Errorf("command contains unsupported characters")
		}
	}
	return nil
}

func execTargetContainerNames(ctx context.Context, req ExecTargetRequest) ([]string, error) {
	if req.Slot > 0 {
		return []string{runtimeid.ContainerName(req.Project, req.Environment, req.Service, req.Slot)}, nil
	}

	output, err := runDocker(
		ctx,
		"ps",
		"--filter", "label=tako.project="+req.Project,
		"--filter", "label=tako.environment="+req.Environment,
		"--filter", "label=tako.service="+req.Service,
		"--format", "{{.Names}}",
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list service containers: %w", err)
	}
	containers := strings.Fields(strings.TrimSpace(output))
	sort.Strings(containers)
	return containers, nil
}

func execContainerAvailable(container dockerProxyInspectContainer) bool {
	if !container.State.Running {
		return false
	}
	return container.State.Health == nil || container.State.Health.Status == "healthy"
}

func ensureOneOffVolumes(ctx context.Context, req RunOneOffRequest) error {
	for _, volume := range namedVolumeSourcesFromMounts(req.Mounts) {
		if err := ensureDockerVolume(ctx, req.Project, req.Environment, req.Service, volume); err != nil {
			return fmt.Errorf("failed to ensure docker volume %s: %w", volume, err)
		}
	}
	return nil
}

func prepareOneOffEnvFile(req *RunOneOffRequest) (func(), error) {
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

func buildOneOffRunArgs(req RunOneOffRequest) []string {
	name := oneOffContainerName(req.Project, req.Environment, req.Service)
	args := []string{"run"}
	if req.Remove {
		args = append(args, "--rm")
	}
	if req.Stdin || req.TTY {
		args = append(args, "-i")
	}
	if req.TTY {
		args = append(args, "-t")
	}
	args = append(args,
		"--name", name,
		"--network", req.Network,
		"--label", "tako.project="+req.Project,
		"--label", "tako.environment="+req.Environment,
		"--label", "tako.service="+req.Service,
		"--label", "tako.runtime=takod",
		"--label", "tako.oneOff=true",
	)
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

func oneOffContainerName(project string, environment string, service string) string {
	token := strconv.FormatInt(time.Now().UnixNano(), 36)
	base := runtimeid.ContainerName(project, environment, service+"-run", 1)
	suffix := "_" + token
	if len(base)+len(suffix) > 128 {
		base = strings.TrimRight(base[:128-len(suffix)], "_")
	}
	return base + suffix
}

func runStreamingCommand(cmd *exec.Cmd) (int, error) {
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 1, err
}
