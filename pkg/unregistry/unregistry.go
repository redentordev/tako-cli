// Package unregistry builds local images and transfers them to remote Docker
// hosts through psviderski/unregistry's docker-pussh plugin.
package unregistry

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// CommandSpec describes a local command invocation.
type CommandSpec struct {
	Dir                 string
	Env                 []string
	Name                string
	Args                []string
	SensitiveArgIndexes []int
}

// Runner executes local commands. Tests use this to verify exact Docker CLI
// invocations without requiring Docker.
type Runner interface {
	Run(ctx context.Context, spec CommandSpec) (string, error)
}

// ExecRunner runs commands with os/exec.
type ExecRunner struct {
	Stdout io.Writer
	Stderr io.Writer
}

// Run executes a local command and returns combined stdout/stderr output.
func (r ExecRunner) Run(ctx context.Context, spec CommandSpec) (string, error) {
	if strings.TrimSpace(spec.Name) == "" {
		return "", fmt.Errorf("command name is required")
	}
	cmd := exec.CommandContext(ctx, spec.Name, spec.Args...)
	if spec.Dir != "" {
		cmd.Dir = spec.Dir
	}
	if len(spec.Env) > 0 {
		cmd.Env = append(os.Environ(), spec.Env...)
	}

	var combined bytes.Buffer
	sensitive := len(spec.SensitiveArgIndexes) > 0
	if sensitive {
		// Buffer sensitive commands so build-arg values cannot leak through
		// verbose streaming before they are redacted.
		cmd.Stdout = &combined
		cmd.Stderr = &combined
	} else {
		if r.Stdout != nil {
			cmd.Stdout = io.MultiWriter(r.Stdout, &combined)
		} else {
			cmd.Stdout = &combined
		}
		if r.Stderr != nil {
			cmd.Stderr = io.MultiWriter(r.Stderr, &combined)
		} else {
			cmd.Stderr = &combined
		}
	}

	err := cmd.Run()
	outputText := combined.String()
	if sensitive {
		outputText = redactSensitiveArgValues(outputText, spec.Args, spec.SensitiveArgIndexes)
		if r.Stdout != nil {
			_, _ = io.WriteString(r.Stdout, outputText)
		} else if r.Stderr != nil {
			_, _ = io.WriteString(r.Stderr, outputText)
		}
	}
	if err != nil {
		output := strings.TrimSpace(outputText)
		displayArgs := redactedCommandArgs(spec.Args, spec.SensitiveArgIndexes)
		if output != "" {
			return output, fmt.Errorf("%s %s failed: %w: %s", spec.Name, strings.Join(displayArgs, " "), err, output)
		}
		return "", fmt.Errorf("%s %s failed: %w", spec.Name, strings.Join(displayArgs, " "), err)
	}
	return outputText, nil
}

func redactSensitiveArgValues(output string, args []string, sensitiveIndexes []int) string {
	for _, index := range sensitiveIndexes {
		if index < 0 || index >= len(args) {
			continue
		}
		value := args[index]
		output = strings.ReplaceAll(output, value, "[REDACTED]")
		if _, secret, ok := strings.Cut(value, "="); ok && secret != "" {
			output = strings.ReplaceAll(output, secret, "[REDACTED]")
		}
	}
	return output
}

func redactedCommandArgs(args []string, sensitiveIndexes []int) []string {
	out := append([]string(nil), args...)
	for _, index := range sensitiveIndexes {
		if index >= 0 && index < len(out) {
			out[index] = "[REDACTED]"
		}
	}
	return out
}

// Client wraps Docker buildx and docker-pussh.
type Client struct {
	Runner Runner
	Stdout io.Writer
	Stderr io.Writer
}

func (c Client) runner() Runner {
	if c.Runner != nil {
		return c.Runner
	}
	return ExecRunner{Stdout: c.Stdout, Stderr: c.Stderr}
}

// CheckAvailable verifies local Docker, buildx, and docker-pussh are usable.
func (c Client) CheckAvailable(ctx context.Context) error {
	checks := []CommandSpec{
		{Name: "docker", Args: []string{"version"}},
		{Name: "docker", Args: []string{"buildx", "version"}},
		{Name: "docker", Args: []string{"pussh", "--help"}},
	}
	for _, check := range checks {
		if _, err := c.runner().Run(ctx, check); err != nil {
			return fmt.Errorf("local unregistry build prerequisites are not ready: %w", err)
		}
	}
	return nil
}

// BuildRequest describes a platform-specific local build.
type BuildRequest struct {
	Image      string
	ContextDir string
	Dockerfile string
	Platform   string
	Args       map[string]string
	Target     string
}

// Build builds and loads a single-platform image into local Docker.
func (c Client) Build(ctx context.Context, req BuildRequest) error {
	if strings.TrimSpace(req.Image) == "" {
		return fmt.Errorf("image is required")
	}
	if strings.TrimSpace(req.ContextDir) == "" {
		return fmt.Errorf("build context directory is required")
	}
	if strings.TrimSpace(req.Platform) == "" {
		return fmt.Errorf("build platform is required")
	}

	args := []string{
		"buildx", "build",
		"--platform", req.Platform,
		"--load",
		"-t", req.Image,
	}
	if strings.TrimSpace(req.Dockerfile) != "" {
		args = append(args, "-f", req.Dockerfile)
	}
	var sensitiveArgIndexes []int
	keys := make([]string, 0, len(req.Args))
	for key := range req.Args {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		sensitiveArgIndexes = append(sensitiveArgIndexes, len(args)+1)
		args = append(args, "--build-arg", key+"="+req.Args[key])
	}
	if strings.TrimSpace(req.Target) != "" {
		args = append(args, "--target", req.Target)
	}
	args = append(args, ".")

	if _, err := c.runner().Run(ctx, CommandSpec{
		Dir:                 req.ContextDir,
		Name:                "docker",
		Args:                args,
		SensitiveArgIndexes: sensitiveArgIndexes,
	}); err != nil {
		return fmt.Errorf("failed to build local image with buildx: %w", err)
	}
	return nil
}

// PushRequest describes a docker-pussh transfer to one remote Docker host.
type PushRequest struct {
	Image      string
	Target     string
	SSHKey     string
	Platform   string
	NoHostKeys bool
}

// Push transfers an image to a remote Docker host using docker-pussh.
func (c Client) Push(ctx context.Context, req PushRequest) error {
	if strings.TrimSpace(req.Image) == "" {
		return fmt.Errorf("image is required")
	}
	if strings.TrimSpace(req.Target) == "" {
		return fmt.Errorf("target is required")
	}

	args := []string{"pussh"}
	if strings.TrimSpace(req.Platform) != "" {
		args = append(args, "--platform", req.Platform)
	}
	if req.NoHostKeys {
		args = append(args, "--no-host-key-check")
	}
	args = append(args, req.Image, req.Target)
	if strings.TrimSpace(req.SSHKey) != "" {
		args = append(args, "-i", req.SSHKey)
	}

	if _, err := c.runner().Run(ctx, CommandSpec{Name: "docker", Args: args}); err != nil {
		return fmt.Errorf("failed to push image with docker-pussh: %w", err)
	}
	return nil
}
