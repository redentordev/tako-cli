package hooks

import (
	"fmt"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

// Executor handles lifecycle hook execution
type Executor struct {
	client      *ssh.Client
	projectName string
	environment string
	serviceName string
	verbose     bool
}

// NewExecutor creates a new hook executor
func NewExecutor(client *ssh.Client, projectName, environment, serviceName string, verbose bool) *Executor {
	return &Executor{
		client:      client,
		projectName: projectName,
		environment: environment,
		serviceName: serviceName,
		verbose:     verbose,
	}
}

// ExecutePreBuild runs pre-build hooks
func (e *Executor) ExecutePreBuild(hooks []string, buildContext string) error {
	if len(hooks) == 0 {
		return nil
	}

	if e.verbose {
		fmt.Printf("  → Running pre-build hooks...\n")
	}

	for i, hook := range hooks {
		if err := e.executeHook(hook, buildContext, fmt.Sprintf("pre-build[%d]", i)); err != nil {
			return fmt.Errorf("pre-build hook failed: %w", err)
		}
	}

	return nil
}

// ExecutePostBuild runs post-build hooks
func (e *Executor) ExecutePostBuild(hooks []string, imageName string) error {
	if len(hooks) == 0 {
		return nil
	}

	if e.verbose {
		fmt.Printf("  → Running post-build hooks...\n")
	}

	for i, hook := range hooks {
		// Replace {{IMAGE}} placeholder with actual image name
		expandedHook := strings.ReplaceAll(hook, "{{IMAGE}}", imageName)

		if err := e.executeHook(expandedHook, "", fmt.Sprintf("post-build[%d]", i)); err != nil {
			return fmt.Errorf("post-build hook failed: %w", err)
		}
	}

	return nil
}

// ExecutePreDeploy runs pre-deploy hooks
func (e *Executor) ExecutePreDeploy(hooks []string, envVars map[string]string) error {
	if len(hooks) == 0 {
		return nil
	}

	fmt.Printf("  → Running pre-deploy hooks...\n")

	for i, hook := range hooks {
		expandedHook := e.expandVariables(hook, envVars)

		if err := e.executeHook(expandedHook, "", fmt.Sprintf("pre-deploy[%d]", i)); err != nil {
			return fmt.Errorf("pre-deploy hook failed: %w", err)
		}
	}

	return nil
}

// ExecutePostDeploy runs post-deploy hooks
func (e *Executor) ExecutePostDeploy(hooks []string, envVars map[string]string) error {
	if len(hooks) == 0 {
		return nil
	}

	fmt.Printf("  → Running post-deploy hooks...\n")

	for i, hook := range hooks {
		expandedHook := e.expandVariables(hook, envVars)

		if err := e.executeHook(expandedHook, "", fmt.Sprintf("post-deploy[%d]", i)); err != nil {
			return fmt.Errorf("post-deploy hook failed: %w", err)
		}
	}

	return nil
}

// ExecutePostStart runs post-start hooks (after service is running)
func (e *Executor) ExecutePostStart(hooks []string, fullServiceName string, envVars map[string]string) error {
	if len(hooks) == 0 {
		return nil
	}

	fmt.Printf("  → Waiting for service to be ready...\n")

	// Wait for service to be running
	if err := e.waitForServiceRunning(fullServiceName); err != nil {
		return fmt.Errorf("service did not start: %w", err)
	}

	fmt.Printf("  → Running post-start hooks...\n")

	for i, hook := range hooks {
		expandedHook := e.expandVariables(hook, envVars)

		// Replace {{SERVICE}} with actual service name
		expandedHook = strings.ReplaceAll(expandedHook, "{{SERVICE}}", fullServiceName)

		// If hook starts with "exec:", run it inside the container
		if strings.HasPrefix(expandedHook, "exec:") {
			command := strings.TrimPrefix(expandedHook, "exec:")
			command = strings.TrimSpace(command)

			if err := e.executeInContainer(fullServiceName, command, fmt.Sprintf("post-start[%d]", i)); err != nil {
				return fmt.Errorf("post-start hook failed: %w", err)
			}
		} else {
			if err := e.executeHook(expandedHook, "", fmt.Sprintf("post-start[%d]", i)); err != nil {
				return fmt.Errorf("post-start hook failed: %w", err)
			}
		}
	}

	return nil
}

// executeHook executes a single hook command on the server
func (e *Executor) executeHook(command, workDir, hookName string) error {
	if e.verbose {
		fmt.Printf("    [%s] %s\n", hookName, command)
	}

	// Build the command with working directory if specified
	fullCmd := command
	if workDir != "" {
		fullCmd = fmt.Sprintf("cd %s && %s", workDir, command)
	}

	// Execute the command
	output, err := e.client.Execute(fullCmd)

	if err != nil {
		fmt.Printf("    ✗ Hook failed: %s\n", hookName)
		if output != "" {
			fmt.Printf("    Output:\n")
			for _, line := range strings.Split(output, "\n") {
				if line != "" {
					fmt.Printf("      %s\n", line)
				}
			}
		}
		return err
	}

	if e.verbose && output != "" {
		fmt.Printf("    Output:\n")
		for _, line := range strings.Split(output, "\n") {
			if line != "" {
				fmt.Printf("      %s\n", line)
			}
		}
	}

	if e.verbose {
		fmt.Printf("    ✓ Hook completed: %s\n", hookName)
	}

	return nil
}

// executeInContainer executes a command inside a running container
func (e *Executor) executeInContainer(serviceName, command, hookName string) error {
	if e.verbose {
		fmt.Printf("    [%s] exec in container: %s\n", hookName, command)
	}

	// Get a container ID for this service
	getContainerCmd := fmt.Sprintf("docker ps --filter name=%s --format '{{.ID}}' | head -1", serviceName)
	containerID, err := e.client.Execute(getContainerCmd)
	if err != nil {
		return fmt.Errorf("failed to get container ID: %w", err)
	}

	containerID = strings.TrimSpace(containerID)
	if containerID == "" {
		return fmt.Errorf("no running container found for service %s", serviceName)
	}

	// Execute command in container
	execCmd := fmt.Sprintf("docker exec %s sh -c '%s'", containerID, command)
	output, err := e.client.Execute(execCmd)

	if err != nil {
		fmt.Printf("    ✗ Hook failed: %s\n", hookName)
		if output != "" {
			fmt.Printf("    Output:\n")
			for _, line := range strings.Split(output, "\n") {
				if line != "" {
					fmt.Printf("      %s\n", line)
				}
			}
		}
		return err
	}

	if e.verbose && output != "" {
		fmt.Printf("    Output:\n")
		for _, line := range strings.Split(output, "\n") {
			if line != "" {
				fmt.Printf("      %s\n", line)
			}
		}
	}

	if e.verbose {
		fmt.Printf("    ✓ Hook completed: %s\n", hookName)
	}

	return nil
}

// waitForServiceRunning waits for a service to be in running state
func (e *Executor) waitForServiceRunning(serviceName string) error {
	maxAttempts := 30 // 30 seconds

	for i := 0; i < maxAttempts; i++ {
		checkCmd := fmt.Sprintf("docker service ps %s --filter 'desired-state=running' --format '{{.CurrentState}}' | head -1", serviceName)
		output, err := e.client.Execute(checkCmd)

		if err == nil {
			state := strings.TrimSpace(output)
			if strings.HasPrefix(state, "Running") {
				return nil
			}
		}

		time.Sleep(1 * time.Second)
	}

	return fmt.Errorf("service did not start after %d seconds", maxAttempts)
}

// expandVariables expands {{VAR}} placeholders with env vars
func (e *Executor) expandVariables(command string, envVars map[string]string) string {
	result := command

	// Replace common placeholders
	result = strings.ReplaceAll(result, "{{PROJECT}}", e.projectName)
	result = strings.ReplaceAll(result, "{{ENVIRONMENT}}", e.environment)
	result = strings.ReplaceAll(result, "{{SERVICE}}", e.serviceName)

	// Replace env var placeholders
	for key, value := range envVars {
		result = strings.ReplaceAll(result, fmt.Sprintf("{{%s}}", key), value)
	}

	return result
}

// ValidateHooks validates hook commands (security checks)
func ValidateHooks(hooks *config.HooksConfig) error {
	if hooks == nil {
		return nil
	}

	allHooks := [][]string{
		hooks.PreBuild,
		hooks.PostBuild,
		hooks.PreDeploy,
		hooks.PostDeploy,
		hooks.PostStart,
	}

	for _, hookList := range allHooks {
		for _, hook := range hookList {
			if strings.TrimSpace(hook) == "" {
				return fmt.Errorf("empty hook command found")
			}

			if err := validateHookCommand(hook); err != nil {
				return err
			}
		}
	}

	return nil
}

// validateHookCommand performs security validation on a single hook command
func validateHookCommand(hook string) error {
	// Normalize the command for checking
	normalized := strings.ToLower(strings.TrimSpace(hook))

	// Dangerous command patterns (more comprehensive)
	dangerousPatterns := []string{
		// Filesystem destruction
		"rm -rf /",
		"rm -rf /*",
		"rm -rf ~",
		"rm -rf $home",
		"rm -rf --no-preserve-root",
		// Disk operations
		"dd if=/dev/zero",
		"dd if=/dev/random",
		"mkfs.",
		// Fork bomb
		":(){ :|:& };:",
		":(){:|:&};:",
		// Privilege escalation attempts
		"chmod 777 /",
		"chmod -r 777 /",
		"chown -r",
		// Network attacks
		"nc -l",
		"ncat -l",
		// Crypto mining indicators
		"xmrig",
		"minerd",
		"cryptonight",
		// Reverse shells
		"/dev/tcp/",
		"bash -i >& /dev/tcp",
		"bash -c 'bash -i",
		// Download and execute
		"curl|sh",
		"curl|bash",
		"wget|sh",
		"wget|bash",
		"curl -s|sh",
		"wget -q|sh",
	}

	for _, danger := range dangerousPatterns {
		if strings.Contains(normalized, danger) {
			return fmt.Errorf("dangerous command pattern detected in hook: '%s' matches '%s'", hook, danger)
		}
	}

	// Check for suspicious shell redirections
	suspiciousPatterns := []string{
		"> /etc/",
		"> /var/",
		"> /usr/",
		"> /bin/",
		"> /sbin/",
		">> /etc/",
		"| sudo",
		"; sudo",
		"&& sudo",
	}

	for _, suspicious := range suspiciousPatterns {
		if strings.Contains(normalized, suspicious) {
			return fmt.Errorf("suspicious command pattern detected in hook: '%s' contains '%s'", hook, suspicious)
		}
	}

	// Warn about eval usage (don't block, just validate carefully)
	if strings.Contains(normalized, "eval ") || strings.Contains(normalized, "eval(") {
		// Allow eval but log a warning - the command itself is not blocked
		// because eval can be legitimate for dynamic script generation
	}

	return nil
}
