package syscheck

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// Requirement represents a system requirement
type Requirement struct {
	Name        string
	Command     string
	Args        []string
	Required    bool
	Installed   bool
	Version     string
	InstallHint string
}

// CheckResult holds the results of system checks
type CheckResult struct {
	Requirements []Requirement
	AllRequired  bool
	AllOptional  bool
}

// SystemChecker checks system requirements
type SystemChecker struct {
	verbose bool
}

// NewSystemChecker creates a new system checker
func NewSystemChecker(verbose bool) *SystemChecker {
	return &SystemChecker{
		verbose: verbose,
	}
}

// CheckAll checks all system requirements
func (s *SystemChecker) CheckAll() *CheckResult {
	requirements := []Requirement{
		{
			Name:        "Git",
			Command:     "git",
			Args:        []string{"--version"},
			Required:    true,
			InstallHint: "Install Git: https://git-scm.com/downloads",
		},
		{
			Name:        "Docker",
			Command:     "docker",
			Args:        []string{"--version"},
			Required:    true,
			InstallHint: "Install Docker: https://docs.docker.com/get-docker/",
		},
		{
			Name:        "Docker Compose",
			Command:     "docker",
			Args:        []string{"compose", "version"},
			Required:    true,
			InstallHint: "Docker Compose is included with Docker Desktop",
		},
		{
			Name:        "SSH",
			Command:     s.getSSHCommand(),
			Args:        []string{"-V"},
			Required:    true,
			InstallHint: s.getSSHInstallHint(),
		},
		{
			Name:        "Nixpacks",
			Command:     "nixpacks",
			Args:        []string{"--version"},
			Required:    false,
			InstallHint: "Install Nixpacks: curl -sSL https://nixpacks.com/install.sh | bash (Optional - for auto-building without Dockerfile)",
		},
	}

	result := &CheckResult{
		Requirements: make([]Requirement, 0, len(requirements)),
		AllRequired:  true,
		AllOptional:  true,
	}

	for _, req := range requirements {
		req.Installed, req.Version = s.checkRequirement(req)
		result.Requirements = append(result.Requirements, req)

		if req.Required && !req.Installed {
			result.AllRequired = false
		}
		if !req.Required && !req.Installed {
			result.AllOptional = false
		}
	}

	return result
}

// checkRequirement checks if a requirement is installed
func (s *SystemChecker) checkRequirement(req Requirement) (bool, string) {
	cmd := exec.Command(req.Command, req.Args...)
	output, err := cmd.CombinedOutput()

	if err != nil {
		// Special case for docker compose - try docker-compose as fallback
		if req.Name == "Docker Compose" {
			cmd2 := exec.Command("docker-compose", "--version")
			output2, err2 := cmd2.CombinedOutput()
			if err2 == nil {
				version := s.extractVersion(string(output2))
				return true, version
			}
		}
		return false, ""
	}

	version := s.extractVersion(string(output))
	return true, version
}

// extractVersion extracts version from command output
func (s *SystemChecker) extractVersion(output string) string {
	lines := strings.Split(output, "\n")
	if len(lines) > 0 {
		// Clean up the version string
		version := strings.TrimSpace(lines[0])
		// Limit length
		if len(version) > 80 {
			version = version[:80] + "..."
		}
		return version
	}
	return "unknown"
}

// getSSHCommand returns the SSH command based on OS
func (s *SystemChecker) getSSHCommand() string {
	if runtime.GOOS == "windows" {
		// Try OpenSSH for Windows first
		return "ssh"
	}
	return "ssh"
}

// getSSHInstallHint returns installation hint for SSH based on OS
func (s *SystemChecker) getSSHInstallHint() string {
	switch runtime.GOOS {
	case "windows":
		return "Install OpenSSH: Settings > Apps > Optional Features > Add OpenSSH Client"
	case "darwin":
		return "SSH is pre-installed on macOS"
	case "linux":
		return "Install SSH: sudo apt install openssh-client (Debian/Ubuntu) or sudo yum install openssh-clients (RHEL/CentOS)"
	default:
		return "Install SSH client for your operating system"
	}
}

// PrintResults prints the check results in a formatted way
func (s *SystemChecker) PrintResults(result *CheckResult) {
	fmt.Println("\n🔍 System Requirements Check")
	fmt.Println(strings.Repeat("─", 80))
	fmt.Printf("%-20s %-12s %-50s\n", "REQUIREMENT", "STATUS", "VERSION")
	fmt.Println(strings.Repeat("─", 80))

	for _, req := range result.Requirements {
		status := "✗ Missing"
		if req.Installed {
			status = "✓ Installed"
		}

		required := ""
		if !req.Required {
			required = " (optional)"
		}

		fmt.Printf("%-20s %-12s %-50s%s\n",
			req.Name,
			status,
			req.Version,
			required,
		)
	}

	fmt.Println(strings.Repeat("─", 80))

	// Print summary
	fmt.Println()
	if result.AllRequired {
		fmt.Println("✓ All required dependencies are installed!")
	} else {
		fmt.Println("✗ Some required dependencies are missing.")
		fmt.Println("\nRequired installations:")
		for _, req := range result.Requirements {
			if req.Required && !req.Installed {
				fmt.Printf("  • %s: %s\n", req.Name, req.InstallHint)
			}
		}
	}

	// Show optional dependencies
	if !result.AllOptional {
		fmt.Println("\nOptional (recommended for best experience):")
		for _, req := range result.Requirements {
			if !req.Required && !req.Installed {
				fmt.Printf("  • %s: %s\n", req.Name, req.InstallHint)
			}
		}
	}

	fmt.Println()
}

// CheckDocker specifically checks if Docker daemon is running
func (s *SystemChecker) CheckDocker() (bool, string) {
	// Try docker info first
	cmd := exec.Command("docker", "info")
	if err := cmd.Run(); err == nil {
		return true, "Docker daemon is running"
	}

	// Fallback to docker ps
	cmd = exec.Command("docker", "ps")
	if err := cmd.Run(); err == nil {
		return true, "Docker daemon is running"
	}

	return false, "Docker daemon is not running. Please start Docker Desktop."
}

// InstallNixpacks installs Nixpacks based on the operating system
func (s *SystemChecker) InstallNixpacks() error {
	fmt.Println("\n→ Installing Nixpacks...")

	switch runtime.GOOS {
	case "windows":
		return s.installNixpacksWindows()
	case "darwin":
		return s.installNixpacksMacOS()
	case "linux":
		return s.installNixpacksLinux()
	default:
		return fmt.Errorf("unsupported operating system: %s", runtime.GOOS)
	}
}

// installNixpacksWindows installs Nixpacks on Windows
func (s *SystemChecker) installNixpacksWindows() error {
	// Check if Scoop is installed
	cmd := exec.Command("scoop", "--version")
	if err := cmd.Run(); err != nil {
		fmt.Println("\n⚠️  Scoop package manager is not installed.")
		fmt.Println("Nixpacks on Windows requires Scoop.")
		fmt.Println("\nTo install Scoop, run this in PowerShell:")
		fmt.Println("  Set-ExecutionPolicy RemoteSigned -Scope CurrentUser")
		fmt.Println("  irm get.scoop.sh | iex")
		fmt.Println("\nThen run 'tako init' again to install Nixpacks.")
		return fmt.Errorf("scoop not installed")
	}

	fmt.Println("  Installing via Scoop...")
	cmd = exec.Command("scoop", "install", "nixpacks")
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to install nixpacks via scoop: %w", err)
	}

	fmt.Println("  ✓ Nixpacks installed successfully")
	return nil
}

// installNixpacksMacOS installs Nixpacks on macOS
func (s *SystemChecker) installNixpacksMacOS() error {
	// Try Homebrew first
	cmd := exec.Command("brew", "--version")
	if err := cmd.Run(); err == nil {
		fmt.Println("  Installing via Homebrew...")
		cmd = exec.Command("brew", "install", "nixpacks")
		cmd.Stdout = nil
		cmd.Stderr = nil

		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to install nixpacks via brew: %w", err)
		}

		fmt.Println("  ✓ Nixpacks installed successfully")
		return nil
	}

	// Fallback to curl install script
	return s.installNixpacksCurl()
}

// installNixpacksLinux installs Nixpacks on Linux
func (s *SystemChecker) installNixpacksLinux() error {
	return s.installNixpacksCurl()
}

// installNixpacksCurl installs Nixpacks using the curl install script
func (s *SystemChecker) installNixpacksCurl() error {
	fmt.Println("  Downloading and running install script...")

	// Download and execute the install script
	cmd := exec.Command("sh", "-c", "curl -sSL https://nixpacks.com/install.sh | bash")
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to install nixpacks: %w", err)
	}

	fmt.Println("  ✓ Nixpacks installed successfully")
	fmt.Println("\n  Note: You may need to restart your terminal for Nixpacks to be available in PATH")
	return nil
}

// PromptNixpacksInstall asks the user if they want to install Nixpacks
func (s *SystemChecker) PromptNixpacksInstall() bool {
	fmt.Print("\nWould you like to install Nixpacks now? (Y/n): ")

	var response string
	fmt.Scanln(&response)

	// Default to yes if empty or starts with Y/y
	if response == "" || strings.ToLower(response)[0] == 'y' {
		return true
	}

	return false
}
