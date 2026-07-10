package takod

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
)

// Exec modes: attach runs the command inside a running replica via
// `docker exec`; oneoff runs it in a fresh `docker run --rm` container from
// the service's current image.
const (
	ExecModeAttach = "attach"
	ExecModeOneOff = "oneoff"
)

// ExecContainerMarker precedes the resolved-container frame emitted at the
// start of an exec stream, on its own line.
const ExecContainerMarker = "__TAKO_EXEC_CONTAINER__:"

// ExecExitMarker precedes the terminal frame of an exec stream, on its own
// line, carrying the remote command's exit code.
const ExecExitMarker = "__TAKO_EXEC_EXIT__:"

const (
	defaultExecTimeoutSeconds = 600
	maxExecTimeoutSeconds     = 24 * 60 * 60
	maxExecTerminalDim        = 4096

	// Interactive sessions get a longer absolute default than one-shot
	// commands (a shell open for an hour is normal), plus an idle timeout
	// so abandoned sessions do not hold containers forever.
	defaultExecStreamTimeoutSeconds     = 4 * 60 * 60
	defaultExecStreamIdleTimeoutSeconds = 30 * 60
)

// execRoleLabel marks one-off exec containers so orphans are identifiable
// and reaped by project cleanup.
const execRoleLabel = "tako.role=exec"

// ExecRequest asks takod to run a command in the context of a service.
type ExecRequest struct {
	Project     string   `json:"project"`
	Environment string   `json:"environment"`
	Service     string   `json:"service"`
	Mode        string   `json:"mode"`
	Command     []string `json:"command"`
	// Replica selects the 1-based running replica for attach mode
	// (containers sorted by name); 0 means the first.
	Replica int `json:"replica,omitempty"`
	// Env adds KEY=VALUE pairs on top of EnvFileContent.
	Env []string `json:"env,omitempty"`
	// EnvFileContent carries the service's env/secrets for oneoff mode;
	// it is written to a 0600 temp file and passed via --env-file.
	EnvFileContent string `json:"envFileContent,omitempty"`
	// Image overrides the oneoff image; default is the service's current
	// image from actual state.
	Image         string         `json:"image,omitempty"`
	PullImage     bool           `json:"pullImage,omitempty"`
	RegistryAuths []RegistryAuth `json:"registryAuths,omitempty"`
	// Network attaches the oneoff container; default tako_<project>_<env>.
	Network string `json:"network,omitempty"`
	// Mounts adds --mount specs to oneoff containers (volumes opt-in).
	Mounts             []string                       `json:"mounts,omitempty"`
	ExternalVolumes    []string                       `json:"externalVolumes,omitempty"`
	Entrypoint         []string                       `json:"entrypoint,omitempty"`
	Labels             map[string]string              `json:"labels,omitempty"`
	User               string                         `json:"user,omitempty"`
	WorkingDir         string                         `json:"workingDir,omitempty"`
	StopTimeoutSeconds int                            `json:"stopTimeoutSeconds,omitempty"`
	Init               bool                           `json:"init,omitempty"`
	ExtraHosts         []string                       `json:"extraHosts,omitempty"`
	Ulimits            map[string]config.UlimitConfig `json:"ulimits,omitempty"`
	ShmSize            string                         `json:"shmSize,omitempty"`
	MemoryLimit        string                         `json:"memoryLimit,omitempty"`
	CPULimit           string                         `json:"cpuLimit,omitempty"`
	TimeoutSeconds     int                            `json:"timeoutSeconds,omitempty"`
	// Interactive attaches the caller's stdin over the hijacked frame
	// stream (docker exec/run -i). Requires the Upgrade handshake.
	Interactive bool `json:"interactive,omitempty"`
	// PTY allocates a server-side pseudo-terminal around the docker
	// process (implies Interactive). Cols/Rows set the initial size;
	// resize frames adjust it live.
	PTY  bool `json:"pty,omitempty"`
	Cols int  `json:"cols,omitempty"`
	Rows int  `json:"rows,omitempty"`
	// IdleTimeoutSeconds ends an interactive session after this long
	// without input or output; 0 uses the default.
	IdleTimeoutSeconds int `json:"idleTimeoutSeconds,omitempty"`
}

func validateExecRequest(req *ExecRequest) error {
	if !isSafeProjectName(req.Project) {
		return fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(req.Environment) {
		return fmt.Errorf("invalid environment name")
	}
	if !isSafeServiceName(req.Service) {
		return fmt.Errorf("invalid service name")
	}
	if req.Mode != ExecModeAttach && req.Mode != ExecModeOneOff {
		return fmt.Errorf("mode must be %q or %q", ExecModeAttach, ExecModeOneOff)
	}
	if len(req.Command) == 0 || strings.TrimSpace(req.Command[0]) == "" {
		return fmt.Errorf("command is required")
	}
	if err := validateContainerArgv("command", req.Command); err != nil {
		return err
	}
	for _, entry := range req.Env {
		if !strings.Contains(entry, "=") || hasControlChars(entry) {
			return fmt.Errorf("env entries must be KEY=VALUE")
		}
	}
	if req.Image != "" {
		if err := validateImageName(req.Image); err != nil {
			return err
		}
	}
	if err := validateRegistryAuths(req.RegistryAuths); err != nil {
		return err
	}
	if err := validateContainerArgv("entrypoint", req.Entrypoint); err != nil {
		return err
	}
	if err := validateDockerLabels(req.Labels); err != nil {
		return fmt.Errorf("invalid label: %w", err)
	}
	for key := range req.Labels {
		if strings.HasPrefix(key, "tako.") {
			return fmt.Errorf("invalid label: %q uses reserved tako. prefix", key)
		}
	}
	if err := validateContainerRuntimeControls(req.User, req.WorkingDir, req.StopTimeoutSeconds, req.ExtraHosts, req.Ulimits, req.ShmSize); err != nil {
		return err
	}
	if req.MemoryLimit != "" && !isSafeDockerMemoryLimit(req.MemoryLimit) {
		return fmt.Errorf("invalid memory limit")
	}
	if req.CPULimit != "" && !isSafeDockerCPULimit(req.CPULimit) {
		return fmt.Errorf("invalid CPU limit")
	}
	if req.Network != "" && !isSafeRuntimeName(req.Network) {
		return fmt.Errorf("invalid network name")
	}
	for _, mount := range req.Mounts {
		if strings.TrimSpace(mount) == "" || hasControlChars(mount) {
			return fmt.Errorf("invalid mount value")
		}
	}
	for _, volume := range req.ExternalVolumes {
		if !isSafeDockerVolumeName(volume) {
			return fmt.Errorf("invalid external volume")
		}
	}
	if req.Replica < 0 {
		return fmt.Errorf("replica must be positive")
	}
	if req.TimeoutSeconds < 0 || req.TimeoutSeconds > maxExecTimeoutSeconds {
		return fmt.Errorf("timeoutSeconds must be between 0 and %d", maxExecTimeoutSeconds)
	}
	if req.PTY {
		req.Interactive = true
	}
	if req.Cols < 0 || req.Cols > maxExecTerminalDim || req.Rows < 0 || req.Rows > maxExecTerminalDim {
		return fmt.Errorf("cols and rows must be between 0 and %d", maxExecTerminalDim)
	}
	if (req.Cols != 0 || req.Rows != 0) && !req.PTY {
		return fmt.Errorf("cols and rows require pty")
	}
	if req.IdleTimeoutSeconds < 0 || req.IdleTimeoutSeconds > maxExecTimeoutSeconds {
		return fmt.Errorf("idleTimeoutSeconds must be between 0 and %d", maxExecTimeoutSeconds)
	}
	if req.IdleTimeoutSeconds != 0 && !req.Interactive {
		return fmt.Errorf("idleTimeoutSeconds requires an interactive session")
	}
	if req.Mode != ExecModeOneOff && (req.PullImage || len(req.RegistryAuths) > 0 || len(req.Entrypoint) > 0 || len(req.Labels) > 0 || req.User != "" || req.WorkingDir != "" || req.StopTimeoutSeconds != 0 || req.Init || len(req.ExtraHosts) > 0 || len(req.Ulimits) > 0 || req.ShmSize != "" || req.MemoryLimit != "" || req.CPULimit != "" || len(req.ExternalVolumes) > 0) {
		return fmt.Errorf("container run controls require oneoff mode")
	}
	return nil
}

// execRun is a resolved exec ready to start: the docker argv, the target
// container name, and any temp-file cleanup.
type execRun struct {
	args      []string
	container string
	cleanup   func()
}

// prepareExecRun resolves the attach container or oneoff image/env-file and
// builds the docker argv. Callers own calling cleanup.
func prepareExecRun(ctx context.Context, req ExecRequest) (*execRun, error) {
	switch req.Mode {
	case ExecModeAttach:
		container, err := resolveExecContainer(ctx, req)
		if err != nil {
			return nil, err
		}
		return &execRun{args: buildExecAttachArgs(req, container), container: container}, nil
	default: // oneoff
		network := req.Network
		if network == "" {
			network = fmt.Sprintf("tako_%s_%s", req.Project, req.Environment)
		}
		if err := ensureDockerNetwork(ctx, network); err != nil {
			return nil, err
		}
		if err := ensureServiceVolumes(ctx, ReconcileServiceRequest{
			Project: req.Project, Environment: req.Environment, Service: req.Service,
			Mounts: req.Mounts, ExternalVolumes: req.ExternalVolumes,
		}); err != nil {
			return nil, err
		}
		image := req.Image
		if image == "" {
			resolved, err := resolveExecImage(ctx, req)
			if err != nil {
				return nil, err
			}
			image = resolved
		}
		if req.PullImage {
			if output, err := runDockerWithAuth(ctx, req.RegistryAuths, "pull", image); err != nil {
				return nil, fmt.Errorf("failed to pull image %s: %w: %s", image, err, annotateRegistryAuthFailure(strings.TrimSpace(output)))
			}
		}
		envFile, envCleanup, err := writeTempEnvFile(req.EnvFileContent, req.Project, req.Environment, req.Service)
		if err != nil {
			return nil, err
		}
		container := fmt.Sprintf("tako_%s_%s_%s_exec_%d", req.Project, req.Environment, req.Service, time.Now().UnixNano())
		return &execRun{
			args:      buildExecOneOffArgs(req, container, image, envFile),
			container: container,
			cleanup:   envCleanup,
		}, nil
	}
}

// StreamExec runs the requested command and streams its combined output to
// writer, followed by a terminal ExecExitMarker line with the exit code. It
// returns an error only for failures before any output is streamed
// (validation, container/image resolution); run-phase failures surface in
// the stream and the exit marker instead.
func StreamExec(ctx context.Context, req ExecRequest, writer io.Writer) error {
	if err := validateExecRequest(&req); err != nil {
		return err
	}
	timeoutSeconds := req.TimeoutSeconds
	if timeoutSeconds == 0 {
		timeoutSeconds = defaultExecTimeoutSeconds
	}
	timeout := time.Duration(timeoutSeconds) * time.Second

	run, err := prepareExecRun(ctx, req)
	if err != nil {
		return err
	}
	if run.cleanup != nil {
		defer run.cleanup()
	}
	args := run.args
	container := run.container

	out := &lineStartWriter{writer: writer}
	fmt.Fprintf(out, "%s%s\n", ExecContainerMarker, container)

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	exitCode, runErr := runExecDocker(runCtx, out, args)
	if req.Mode == ExecModeOneOff && runCtx.Err() != nil {
		removeExecContainer(container)
	}
	if runErr != nil {
		out.ensureLineStart()
		fmt.Fprintf(out, "exec failed: %v\n", runErr)
	} else if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		out.ensureLineStart()
		fmt.Fprintf(out, "exec timed out after %s\n", timeout)
	}
	out.ensureLineStart()
	fmt.Fprintf(out, "%s%d\n", ExecExitMarker, exitCode)
	return nil
}

// resolveExecContainer picks the attach-mode target: the request's 1-based
// replica among the service's running containers, sorted by name.
func resolveExecContainer(ctx context.Context, req ExecRequest) (string, error) {
	output, err := runDocker(
		ctx,
		"ps",
		"--filter", "label=tako.project="+req.Project,
		"--filter", "label=tako.environment="+req.Environment,
		"--filter", "label=tako.service="+req.Service,
		"--filter", "status=running",
		"--format", "{{.Names}}",
	)
	if err != nil {
		return "", fmt.Errorf("failed to list running service containers: %w", err)
	}
	containers := strings.Fields(strings.TrimSpace(output))
	if len(containers) == 0 {
		return "", fmt.Errorf("service %s has no running containers", req.Service)
	}
	sort.Strings(containers)
	replica := req.Replica
	if replica == 0 {
		replica = 1
	}
	if replica > len(containers) {
		return "", fmt.Errorf("service %s has %d running replica(s); replica %d not found", req.Service, len(containers), replica)
	}
	return containers[replica-1], nil
}

// resolveExecImage reads the service's current image from actual state.
func resolveExecImage(ctx context.Context, req ExecRequest) (string, error) {
	actual, err := GatherActualState(ctx, req.Project, req.Environment)
	if err != nil {
		return "", fmt.Errorf("failed to resolve service image: %w", err)
	}
	if actual != nil {
		if service, ok := actual.Services[req.Service]; ok && service != nil && strings.TrimSpace(service.Image) != "" {
			return service.Image, nil
		}
	}
	return "", fmt.Errorf("service %s has no deployed image on this node; pass image explicitly", req.Service)
}

func buildExecAttachArgs(req ExecRequest, container string) []string {
	args := []string{"exec"}
	if req.Interactive {
		args = append(args, "-i")
	}
	if req.PTY {
		args = append(args, "-t")
	}
	for _, entry := range req.Env {
		args = append(args, "-e", entry)
	}
	args = append(args, container)
	args = append(args, req.Command...)
	return args
}

func buildExecOneOffArgs(req ExecRequest, container string, image string, envFile string) []string {
	network := req.Network
	if network == "" {
		network = fmt.Sprintf("tako_%s_%s", req.Project, req.Environment)
	}
	args := []string{"run", "--rm"}
	if req.Interactive {
		args = append(args, "-i")
	}
	if req.PTY {
		args = append(args, "-t")
	}
	args = append(args,
		"--name", container,
		"--network", network,
		"--label", "tako.project="+req.Project,
		"--label", "tako.environment="+req.Environment,
		"--label", "tako.service="+req.Service,
		"--label", "tako.runtime=takod",
		"--label", execRoleLabel,
	)
	labelKeys := make([]string, 0, len(req.Labels))
	for key := range req.Labels {
		labelKeys = append(labelKeys, key)
	}
	sort.Strings(labelKeys)
	for _, key := range labelKeys {
		args = append(args, "--label", key+"="+req.Labels[key])
	}
	if envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	for _, entry := range req.Env {
		args = append(args, "-e", entry)
	}
	for _, mount := range req.Mounts {
		args = append(args, "--mount", mount)
	}
	if req.MemoryLimit != "" {
		args = append(args, "--memory", req.MemoryLimit)
	}
	if req.CPULimit != "" {
		args = append(args, "--cpus", req.CPULimit)
	}
	args = appendContainerRuntimeArgs(args, req.User, req.WorkingDir, req.StopTimeoutSeconds, req.Init, req.ExtraHosts, req.Ulimits, req.ShmSize)
	if len(req.Entrypoint) > 0 {
		args = append(args, "--entrypoint", req.Entrypoint[0])
	}
	args = append(args, image)
	if len(req.Entrypoint) > 1 {
		args = append(args, req.Entrypoint[1:]...)
	}
	args = append(args, req.Command...)
	return args
}

// runExecDocker runs the docker command streaming combined output to writer
// and maps the result to the remote command's exit code. A non-exit error
// (docker missing, context kill) reports -1.
func runExecDocker(ctx context.Context, writer io.Writer, args []string) (int, error) {
	cmd := dockerCommandContext(ctx, "docker", args...)
	cmd.Stdout = writer
	cmd.Stderr = writer
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return -1, err
}

// removeExecContainer force-removes a timed-out one-off container with a
// fresh context so cleanup survives the exec deadline.
func removeExecContainer(container string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = runDocker(ctx, "rm", "-f", container)
}

// lineStartWriter tracks whether the stream sits at a line boundary so
// marker frames always start on their own line.
type lineStartWriter struct {
	writer      io.Writer
	wroteAny    bool
	midLine     bool
	writeFailed bool
}

func (w *lineStartWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		w.wroteAny = true
		w.midLine = p[len(p)-1] != '\n'
	}
	n, err := w.writer.Write(p)
	if err != nil {
		w.writeFailed = true
	}
	return n, err
}

func (w *lineStartWriter) ensureLineStart() {
	if w.midLine && !w.writeFailed {
		_, _ = w.writer.Write([]byte("\n"))
		w.midLine = false
	}
}
