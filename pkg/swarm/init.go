package swarm

import (
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

// RunInitCommands executes init commands for a service before it starts
// Init commands run in a temporary container with the same image and volume mounts
// This is useful for setting up permissions on volumes
func (m *Manager) RunInitCommands(client *ssh.Client, serviceName string, service *config.ServiceConfig, imageRef string) error {
	if len(service.Init) == 0 {
		return nil
	}

	if m.verbose {
		fmt.Printf("  Running %d init command(s)...\n", len(service.Init))
	}

	// Build mount arguments
	mountArgs, err := m.buildInitMountArgs(serviceName, service)
	if err != nil {
		return fmt.Errorf("failed to build mount args for init: %w", err)
	}

	// Run each init command in a temporary container
	for i, initCmd := range service.Init {
		if m.verbose {
			fmt.Printf("    [%d/%d] %s\n", i+1, len(service.Init), truncateCommand(initCmd, 60))
		}

		// Build docker run command
		// Use --rm to auto-remove container after completion
		// Use --user root to ensure we have permissions for chown/chmod
		cmd := fmt.Sprintf("docker run --rm --user root %s %s sh -c '%s'",
			mountArgs,
			imageRef,
			escapeShellArg(initCmd))

		output, err := client.Execute(cmd)
		if err != nil {
			return fmt.Errorf("init command failed: %w\nCommand: %s\nOutput: %s", err, initCmd, output)
		}

		if m.verbose && strings.TrimSpace(output) != "" {
			fmt.Printf("      Output: %s\n", strings.TrimSpace(output))
		}
	}

	if m.verbose {
		fmt.Printf("  ✓ Init commands completed\n")
	}

	return nil
}

// buildInitMountArgs builds the mount arguments for init container
func (m *Manager) buildInitMountArgs(serviceName string, service *config.ServiceConfig) (string, error) {
	var args []string

	desiredMounts, err := m.buildDesiredMounts(serviceName, service)
	if err != nil {
		return "", err
	}

	for _, mount := range desiredMounts {
		var mountArg string
		switch mount.Type {
		case "volume":
			mountArg = fmt.Sprintf("--mount type=volume,source=%s,target=%s", mount.Source, mount.Target)
		case "bind":
			mountArg = fmt.Sprintf("--mount type=bind,source=%s,target=%s", mount.Source, mount.Target)
		default:
			mountArg = fmt.Sprintf("--mount type=%s,source=%s,target=%s", mount.Type, mount.Source, mount.Target)
		}

		if mount.ReadOnly {
			// Don't mount as readonly for init commands - we need to modify
			// This is intentional: init commands are meant to set up permissions
		}

		args = append(args, mountArg)
	}

	return strings.Join(args, " "), nil
}

// escapeShellArg escapes a string for safe use inside single quotes in a shell command.
// This is the safest approach - wrap the entire string in single quotes and escape
// any single quotes within it.
func escapeShellArg(s string) string {
	// Inside single quotes, all characters are literal except single quotes.
	// To include a single quote, we end the single-quoted string, add an escaped
	// single quote, and start a new single-quoted string.
	// Example: "don't" becomes "don'\''t" which shell interprets as: don + ' + t
	return strings.ReplaceAll(s, "'", "'\\''")
}

// truncateCommand truncates a command string for display
func truncateCommand(cmd string, maxLen int) string {
	cmd = strings.TrimSpace(cmd)
	if len(cmd) <= maxLen {
		return cmd
	}
	return cmd[:maxLen-3] + "..."
}
