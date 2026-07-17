// Package unregistry builds and inspects local images before the deployer
// transfers their immutable archives through Tako's structured runtime API.
package unregistry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
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

// Client wraps local Docker buildx, inspection, and immutable export.
type Client struct {
	Runner Runner
	Stdout io.Writer
	Stderr io.Writer
}

type ImageDescriptor struct {
	ImageID      string
	OS           string
	Architecture string
	Variant      string
	DaemonID     string
}

var sha256ImageIDPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func (c Client) runner() Runner {
	if c.Runner != nil {
		return c.Runner
	}
	return ExecRunner{Stdout: c.Stdout, Stderr: c.Stderr}
}

// CheckAvailable verifies local Docker and buildx are usable.
func (c Client) CheckAvailable(ctx context.Context) error {
	checks := []CommandSpec{
		{Name: "docker", Args: []string{"version"}},
		{Name: "docker", Args: []string{"buildx", "version"}},
	}
	for _, check := range checks {
		if _, err := c.runner().Run(ctx, check); err != nil {
			return fmt.Errorf("local image build prerequisites are not ready: %w", err)
		}
	}
	return nil
}

func (c Client) Inspect(ctx context.Context, image string) (ImageDescriptor, error) {
	output, err := c.runner().Run(ctx, CommandSpec{Name: "docker", Args: []string{"image", "inspect", "--format", `{{json .}}`, image}})
	if err != nil {
		return ImageDescriptor{}, fmt.Errorf("inspect local image: %w", err)
	}
	var inspected struct {
		ID           string `json:"Id"`
		OS           string `json:"Os"`
		Architecture string `json:"Architecture"`
		Variant      string `json:"Variant"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &inspected); err != nil {
		return ImageDescriptor{}, fmt.Errorf("decode local image descriptor: %w", err)
	}
	daemonOutput, err := c.runner().Run(ctx, CommandSpec{Name: "docker", Args: []string{"info", "--format", `{{json .ID}}`}})
	if err != nil {
		return ImageDescriptor{}, fmt.Errorf("read local Docker daemon identity: %w", err)
	}
	var daemonID string
	if err := json.Unmarshal([]byte(strings.TrimSpace(daemonOutput)), &daemonID); err != nil {
		return ImageDescriptor{}, fmt.Errorf("decode local Docker daemon identity: %w", err)
	}
	descriptor := ImageDescriptor{
		ImageID: strings.ToLower(strings.TrimSpace(inspected.ID)), OS: strings.ToLower(strings.TrimSpace(inspected.OS)),
		Architecture: strings.ToLower(strings.TrimSpace(inspected.Architecture)), Variant: strings.ToLower(strings.TrimSpace(inspected.Variant)), DaemonID: strings.TrimSpace(daemonID),
	}
	if !sha256ImageIDPattern.MatchString(descriptor.ImageID) || descriptor.OS == "" || descriptor.Architecture == "" || descriptor.DaemonID == "" {
		return ImageDescriptor{}, fmt.Errorf("local image descriptor omitted immutable digest, platform, or daemon identity")
	}
	return descriptor, nil
}

// Export writes an immutable image ID to a Docker archive. Saving by ID keeps
// a concurrent tag change from selecting different bytes.
func (c Client) Export(ctx context.Context, imageID string, output io.Writer) error {
	if !sha256ImageIDPattern.MatchString(strings.ToLower(strings.TrimSpace(imageID))) {
		return fmt.Errorf("immutable image ID is required for export")
	}
	cmd := exec.CommandContext(ctx, "docker", "save", imageID)
	cmd.Stdout = output
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("export local image %s: %w: %s", imageID, err, strings.TrimSpace(stderr.String()))
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
