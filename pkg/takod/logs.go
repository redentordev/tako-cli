package takod

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

type LogsRequest struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Service     string `json:"service"`
	Tail        int    `json:"tail,omitempty"`
	Follow      bool   `json:"follow,omitempty"`
}

func StreamServiceLogs(ctx context.Context, req LogsRequest, writer io.Writer) error {
	if err := validateLogsRequest(req); err != nil {
		return err
	}
	if req.Tail == 0 {
		req.Tail = 100
	}

	containers, err := listServiceLogContainers(ctx, req)
	if err != nil {
		return err
	}
	if len(containers) == 0 {
		return fmt.Errorf("no containers found for service %s", req.Service)
	}

	if !req.Follow || len(containers) == 1 {
		for _, container := range containers {
			if err := streamContainerLogs(ctx, writer, container, req.Tail, req.Follow); err != nil {
				return err
			}
		}
		return nil
	}

	streamWriter := &lockedWriter{writer: writer}
	var wg sync.WaitGroup
	errCh := make(chan error, len(containers))
	for _, container := range containers {
		container := container
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := streamContainerLogs(ctx, streamWriter, container, req.Tail, true); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

func validateLogsRequest(req LogsRequest) error {
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
	if req.Tail < 0 {
		return fmt.Errorf("tail cannot be negative")
	}
	if req.Tail > 10000 {
		return fmt.Errorf("tail cannot exceed 10000")
	}
	return nil
}

func listServiceLogContainers(ctx context.Context, req LogsRequest) ([]string, error) {
	output, err := runDocker(
		ctx,
		"ps",
		"-a",
		"--filter", "label=tako.project="+req.Project,
		"--filter", "label=tako.environment="+req.Environment,
		"--filter", "label=tako.service="+req.Service,
		"--format", "{{.Names}}",
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list service containers: %w", err)
	}

	containers := strings.Fields(strings.TrimSpace(output))
	return containers, nil
}

func streamContainerLogs(ctx context.Context, writer io.Writer, container string, tail int, follow bool) error {
	args := []string{"logs", "--tail", strconv.Itoa(tail)}
	if follow {
		args = append(args, "-f")
	}
	args = append(args, container)

	cmd := dockerCommandContext(ctx, "docker", args...)
	cmd.Stdout = writer
	cmd.Stderr = writer
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to stream logs for %s: %w", container, err)
	}
	return nil
}

type lockedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(p)
}
