package nixpacks

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Framework represents a detected framework
type Framework string

const (
	FrameworkNodeJS  Framework = "nodejs"
	FrameworkGo      Framework = "go"
	FrameworkPython  Framework = "python"
	FrameworkRuby    Framework = "ruby"
	FrameworkPHP     Framework = "php"
	FrameworkRust    Framework = "rust"
	FrameworkUnknown Framework = "unknown"
)

// Detector handles framework detection
type Detector struct {
	projectPath string
	verbose     bool
}

// NewDetector creates a new framework detector
func NewDetector(projectPath string, verbose bool) *Detector {
	return &Detector{
		projectPath: projectPath,
		verbose:     verbose,
	}
}

// DetectFramework detects the framework used in the project
func (d *Detector) DetectFramework() (Framework, error) {
	// Check for various framework indicators
	if d.fileExists("package.json") {
		return FrameworkNodeJS, nil
	}
	if d.fileExists("go.mod") || d.fileExists("go.sum") {
		return FrameworkGo, nil
	}
	if d.fileExists("requirements.txt") || d.fileExists("Pipfile") || d.fileExists("pyproject.toml") {
		return FrameworkPython, nil
	}
	if d.fileExists("Gemfile") {
		return FrameworkRuby, nil
	}
	if d.fileExists("composer.json") {
		return FrameworkPHP, nil
	}
	if d.fileExists("Cargo.toml") {
		return FrameworkRust, nil
	}

	return FrameworkUnknown, fmt.Errorf("could not detect framework - no recognized framework files found")
}

// HasDockerfile checks if a Dockerfile exists in the project
func (d *Detector) HasDockerfile() bool {
	return d.fileExists("Dockerfile") || d.fileExists("dockerfile")
}

// GenerateDockerfile generates a Dockerfile using Nixpacks
func (d *Detector) GenerateDockerfile() error {
	// Check if nixpacks is installed
	if !d.isNixpacksInstalled() {
		return fmt.Errorf("nixpacks is not installed. Install it with: curl -sSL https://nixpacks.com/install.sh | bash")
	}

	framework, err := d.DetectFramework()
	if err != nil {
		return err
	}

	if d.verbose {
		fmt.Printf("  Detected framework: %s\n", framework)
		fmt.Printf("  Generating Dockerfile with Nixpacks...\n")
	}

	// Generate Dockerfile using nixpacks
	cmd := exec.Command("nixpacks", "generate", d.projectPath, "--out", d.projectPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to generate Dockerfile with nixpacks: %w\nOutput: %s", err, string(output))
	}

	if d.verbose {
		fmt.Printf("  ✓ Dockerfile generated successfully\n")
	}

	return nil
}

// BuildWithNixpacks builds a Docker image using Nixpacks
func (d *Detector) BuildWithNixpacks(imageName string) error {
	// Check if nixpacks is installed
	if !d.isNixpacksInstalled() {
		return fmt.Errorf("nixpacks is not installed. Install it with: curl -sSL https://nixpacks.com/install.sh | bash")
	}

	framework, err := d.DetectFramework()
	if err != nil {
		return err
	}

	if d.verbose {
		fmt.Printf("  Detected framework: %s\n", framework)
		fmt.Printf("  Building with Nixpacks...\n")
	}

	// Build using nixpacks
	cmd := exec.Command("nixpacks", "build", d.projectPath, "--name", imageName)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to build with nixpacks: %w", err)
	}

	if d.verbose {
		fmt.Printf("  ✓ Image built successfully: %s\n", imageName)
	}

	return nil
}

// isNixpacksInstalled checks if nixpacks CLI is available
func (d *Detector) isNixpacksInstalled() bool {
	cmd := exec.Command("nixpacks", "--version")
	err := cmd.Run()
	return err == nil
}

// fileExists checks if a file exists in the project directory
func (d *Detector) fileExists(filename string) bool {
	path := filepath.Join(d.projectPath, filename)
	_, err := os.Stat(path)
	return err == nil
}

// GetFrameworkInfo returns information about the detected framework
func (d *Detector) GetFrameworkInfo() (map[string]interface{}, error) {
	framework, err := d.DetectFramework()
	if err != nil {
		return nil, err
	}

	info := map[string]interface{}{
		"framework":         framework,
		"hasDockerfile":     d.HasDockerfile(),
		"nixpacksAvailable": d.isNixpacksInstalled(),
	}

	return info, nil
}
