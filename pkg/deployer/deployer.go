package deployer

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/hooks"
	"github.com/redentordev/tako-cli/pkg/network"
	"github.com/redentordev/tako-cli/pkg/nixpacks"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/swarm"
)

// streamWriter wraps an io.Writer with a prefix for each line
type streamWriter struct {
	prefix string
	buffer bytes.Buffer
}

func (sw *streamWriter) Write(p []byte) (n int, err error) {
	n = len(p)
	sw.buffer.Write(p)

	// Process complete lines
	for {
		line, err := sw.buffer.ReadString('\n')
		if err != nil {
			// No complete line yet, write back partial line
			if len(line) > 0 {
				sw.buffer.WriteString(line)
			}
			break
		}
		// Print line with prefix
		fmt.Print(sw.prefix + line)
	}

	return n, nil
}

// Deployer handles deployment operations
type Deployer struct {
	client       *ssh.Client
	config       *config.Config
	environment  string
	verbose      bool
	sshPool      *ssh.Pool
	swarmManager *swarm.Manager
}

// NewDeployer creates a new deployer
func NewDeployer(client *ssh.Client, cfg *config.Config, environment string, verbose bool) *Deployer {
	return &Deployer{
		client:      client,
		config:      cfg,
		environment: environment,
		verbose:     verbose,
	}
}

// NewDeployerWithPool creates a deployer with SSH pool for multi-server support
func NewDeployerWithPool(client *ssh.Client, cfg *config.Config, environment string, sshPool *ssh.Pool, verbose bool) *Deployer {
	swarmMgr := swarm.NewManager(cfg, sshPool, environment, verbose)
	return &Deployer{
		client:       client,
		config:       cfg,
		environment:  environment,
		verbose:      verbose,
		sshPool:      sshPool,
		swarmManager: swarmMgr,
	}
}


// createCrossPlatformZip creates a zip archive with Unix-style forward slashes
// This ensures compatibility when deploying from Windows/Mac/Linux to Linux servers
// Respects .dockerignore and .gitignore files, and excludes sensitive files
func createCrossPlatformZip(sourceDir, zipPath string, excludeDirs []string) error {
	zipFile, err := os.Create(zipPath)
	if err != nil {
		return fmt.Errorf("failed to create zip file: %w", err)
	}
	defer zipFile.Close()

	zipWriter := zip.NewWriter(zipFile)
	defer zipWriter.Close()

	// Create ignore parser
	ignoreParser := NewIgnoreParser()

	// Load .dockerignore (takes priority over .gitignore)
	dockerignorePath := filepath.Join(sourceDir, ".dockerignore")
	dockerignoreExists := false
	if _, err := os.Stat(dockerignorePath); err == nil {
		dockerignoreExists = true
		ignoreParser.LoadIgnoreFile(dockerignorePath)
	}

	// Only load .gitignore if .dockerignore doesn't exist
	if !dockerignoreExists {
		gitignorePath := filepath.Join(sourceDir, ".gitignore")
		ignoreParser.LoadIgnoreFile(gitignorePath)
	}

	// Add default exclusions (sensitive files)
	ignoreParser.AddDefaultExclusions()

	// Walk the source directory
	return filepath.Walk(sourceDir, func(filePath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Get relative path from source directory
		relPath, err := filepath.Rel(sourceDir, filePath)
		if err != nil {
			return err
		}

		// Skip the source directory itself
		if relPath == "." {
			return nil
		}

		// Check if path should be ignored
		if ignoreParser.ShouldIgnore(relPath) {
			// File should be ignored - skip it
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Convert Windows backslashes to forward slashes for cross-platform compatibility
		// This is critical for deploying from Windows to Linux
		zipPath := strings.ReplaceAll(relPath, string(filepath.Separator), "/")

		if info.IsDir() {
			// Add trailing slash for directories
			zipPath += "/"
			_, err := zipWriter.Create(zipPath)
			return err
		}

		// Create zip entry with forward slashes
		writer, err := zipWriter.Create(zipPath)
		if err != nil {
			return err
		}

		// Copy file contents
		file, err := os.Open(filePath)
		if err != nil {
			return err
		}
		defer file.Close()

		_, err = io.Copy(writer, file)
		return err
	})
}


// RollbackToState rolls back a Swarm service to a specific deployment state
func (d *Deployer) RollbackToState(serviceName string, serviceState *state.ServiceState) error {
	if d.verbose {
		fmt.Printf("  Rolling back service %s to image %s...\n", serviceName, serviceState.Image)
	}

	// Check if the target image exists
	checkImageCmd := fmt.Sprintf("docker images -q %s 2>/dev/null", serviceState.ImageID)
	imageExists, _ := d.client.Execute(checkImageCmd)

	if strings.TrimSpace(imageExists) == "" {
		checkImageByNameCmd := fmt.Sprintf("docker images -q %s 2>/dev/null", serviceState.Image)
		imageExists, _ = d.client.Execute(checkImageByNameCmd)

		if strings.TrimSpace(imageExists) == "" {
			return fmt.Errorf("image %s (ID: %s) not found on server - cannot rollback",
				serviceState.Image, serviceState.ImageID)
		}
	}

	if d.verbose {
		fmt.Printf("  Found target image: %s\n", serviceState.Image)
	}

	fullServiceName := fmt.Sprintf("%s_%s_%s", d.config.Project.Name, d.environment, serviceName)

	// Check if Swarm service exists
	checkServiceCmd := fmt.Sprintf("docker service ls --filter name=%s --format '{{.Name}}'", fullServiceName)
	existingService, _ := d.client.Execute(checkServiceCmd)

	if strings.TrimSpace(existingService) != fullServiceName {
		return fmt.Errorf("swarm service %s not found - cannot rollback", fullServiceName)
	}

	replicas := serviceState.Replicas
	if replicas <= 0 {
		replicas = 1
	}

	if d.verbose {
		fmt.Printf("  Updating Swarm service to previous image...\n")
	}

	// Build environment variable arguments
	getEnvCmd := fmt.Sprintf("docker service inspect %s --format '{{range .Spec.TaskTemplate.ContainerSpec.Env}}{{.}} {{end}}'", fullServiceName)
	currentEnvOutput, _ := d.client.Execute(getEnvCmd)

	envArgs := ""
	for _, envVar := range strings.Fields(currentEnvOutput) {
		if idx := strings.Index(envVar, "="); idx > 0 {
			key := envVar[:idx]
			envArgs += fmt.Sprintf(" --env-rm %s", key)
		}
	}

	for key, value := range serviceState.Env {
		escapedValue := strings.ReplaceAll(value, "'", "'\"'\"'")
		envArgs += fmt.Sprintf(" --env-add %s='%s'", key, escapedValue)
	}

	updateCmd := fmt.Sprintf("docker service update --detach --force --image %s --replicas %d%s %s 2>&1",
		serviceState.Image,
		replicas,
		envArgs,
		fullServiceName,
	)

	if d.verbose {
		fmt.Printf("  Command: %s\n", updateCmd)
	}

	output, err := d.client.Execute(updateCmd)
	if err != nil {
		return fmt.Errorf("failed to update swarm service: %w\nOutput: %s", err, output)
	}

	if d.verbose {
		fmt.Printf("  ✓ Swarm service updated, rolling update in progress...\n")
	}

	time.Sleep(5 * time.Second)

	verifyCmd := fmt.Sprintf("docker service ps %s --filter 'desired-state=running' --format '{{.CurrentState}}' | head -1", fullServiceName)
	status, _ := d.client.Execute(verifyCmd)

	if strings.Contains(strings.ToLower(status), "running") {
		if d.verbose {
			fmt.Printf("  ✓ Service rollback completed successfully\n")
		}
	} else {
		if d.verbose {
			fmt.Printf("  Warning: Service status: %s (rollback may still be in progress)\n", strings.TrimSpace(status))
		}
	}

	return nil
}

// BuildImage builds a Docker image for a service without deploying it
// This is used for Swarm mode where we need to build first, then deploy with docker service create
func (d *Deployer) BuildImage(serviceName string, service *config.ServiceConfig) (string, error) {
	deployDir := fmt.Sprintf("/opt/%s", d.config.Project.Name)

	// Get full image name from config with environment
	fullImageName := d.config.GetFullImageName(serviceName, d.environment)

	// Create hook executor
	hookExecutor := hooks.NewExecutor(d.client, d.config.Project.Name, d.environment, serviceName, d.verbose)

	// Execute pre-build hooks
	if service.Hooks != nil && len(service.Hooks.PreBuild) > 0 {
		if err := hookExecutor.ExecutePreBuild(service.Hooks.PreBuild, deployDir); err != nil {
			return "", fmt.Errorf("pre-build hooks failed: %w", err)
		}
	}

	if service.Build != "" {
		// Use service.Build as the build context path
		contextPath := service.Build

		// Auto-detect Dockerfile in the build context
		dockerfilePaths := []string{
			filepath.Join(contextPath, "Dockerfile"),
			filepath.Join(contextPath, "Dockerfile.prod"),
			filepath.Join(contextPath, "dockerfile"),
			filepath.Join(contextPath, ".dockerfile"),
		}

		hasDockerfile := false
		for _, path := range dockerfilePaths {
			if _, err := os.Stat(path); err == nil {
				hasDockerfile = true
				if d.verbose {
					fmt.Printf("  Found Dockerfile: %s\n", filepath.Base(path))
				}
				break
			}
		}

		if !hasDockerfile {
			// No Dockerfile - try to use Nixpacks
			if d.verbose {
				fmt.Printf("  No Dockerfile found - using Nixpacks auto-detection...\n")
			}

			detector := nixpacks.NewDetector(contextPath, d.verbose)

			// Detect framework
			framework, err := detector.DetectFramework()
			if err != nil {
				return "", fmt.Errorf("failed to detect framework: %w\nHint: Either add a Dockerfile or ensure your project has recognizable framework files (package.json, go.mod, etc.)", err)
			}

			if d.verbose {
				fmt.Printf("  Detected framework: %s\n", framework)
			}

			// Build locally with Nixpacks
			if err := detector.BuildWithNixpacks(fullImageName); err != nil {
				return "", fmt.Errorf("failed to build with Nixpacks: %w", err)
			}

			// Save image as tar
			if d.verbose {
				fmt.Printf("  Exporting image...\n")
			}

			// We'll need to transfer the image to the server
			// For now, we'll build on server with generated Dockerfile
			if err := detector.GenerateDockerfile(); err != nil {
				return "", fmt.Errorf("failed to generate Dockerfile: %w", err)
			}

			hasDockerfile = true
		}

		// Build on remote server
		if d.verbose {
			fmt.Printf("  Preparing deployment directory...\n")
		}

		// Create deployment directory on server
		if _, err := d.client.Execute(fmt.Sprintf("mkdir -p %s", deployDir)); err != nil {
			return "", fmt.Errorf("failed to create deployment directory: %w", err)
		}

		// Copy Dockerfile and context to server
		if d.verbose {
			fmt.Printf("  Copying application files...\n")
			fmt.Printf("  Context path: %s\n", contextPath)
		}

		// Use tar/zip for reliable directory copying
		// Create archive locally
		archivePath := filepath.Join(os.TempDir(), fmt.Sprintf("deploy_%s_%d", serviceName, time.Now().Unix()))

		// Get absolute context path
		absContextPath, err := filepath.Abs(contextPath)
		if err != nil {
			return "", fmt.Errorf("failed to get absolute path: %w", err)
		}

		var remoteTarPath string

		// Use cross-platform zip for Windows, native tar for Unix/Linux/Mac
		if strings.Contains(strings.ToLower(os.Getenv("OS")), "windows") || filepath.VolumeName(absContextPath) != "" {
			// Windows: Use our cross-platform Go zip implementation
			zipPath := archivePath + ".zip"
			if d.verbose {
				fmt.Printf("  Creating zip archive (cross-platform): %s\n", zipPath)
			}

			// Create zip with forward slashes for Linux compatibility
			// Note: excludeDirs parameter is no longer used, ignore parser handles all exclusions
			if err := createCrossPlatformZip(absContextPath, zipPath, nil); err != nil {
				return "", fmt.Errorf("failed to create zip archive: %w", err)
			}

			defer os.Remove(zipPath)

			// Copy zip to server
			remoteTarPath = fmt.Sprintf("%s/deploy.zip", deployDir)
			if err := d.client.CopyFile(zipPath, remoteTarPath); err != nil {
				return "", fmt.Errorf("failed to copy zip archive: %w", err)
			}

			// Ensure unzip is installed
			if _, err := d.client.Execute("which unzip || (apt-get update && apt-get install -y unzip)"); err != nil {
				// Try to install unzip if not present
				d.client.Execute("apt-get update && apt-get install -y unzip")
			}

			// Extract on server using unzip
			extractCmd := fmt.Sprintf("cd %s && unzip -o deploy.zip && rm deploy.zip", deployDir)
			if _, err := d.client.Execute(extractCmd); err != nil {
				return "", fmt.Errorf("failed to extract files on server: %w", err)
			}
		} else {
			// Unix/Linux/Mac: Use native tar
			tarPath := archivePath + ".tar.gz"
			if d.verbose {
				fmt.Printf("  Creating tar archive: %s\n", tarPath)
			}

			// Build exclude list from .dockerignore or .gitignore
			ignoreParser := NewIgnoreParser()
			dockerignorePath := filepath.Join(absContextPath, ".dockerignore")
			gitignorePath := filepath.Join(absContextPath, ".gitignore")
			dockerignoreExists := false
			if _, err := os.Stat(dockerignorePath); err == nil {
				dockerignoreExists = true
				ignoreParser.LoadIgnoreFile(dockerignorePath)
			}
			if !dockerignoreExists {
				ignoreParser.LoadIgnoreFile(gitignorePath)
			}
			ignoreParser.AddDefaultExclusions()

			// Build tar exclude arguments
			excludeArgs := []string{}
			for _, pattern := range ignoreParser.GetExcludedPatterns() {
				excludeArgs = append(excludeArgs, "--exclude="+pattern)
			}

			// Build tar command with excludes
			tarArgs := []string{"-czf", tarPath}
			tarArgs = append(tarArgs, excludeArgs...)
			tarArgs = append(tarArgs, "-C", absContextPath, ".")

			cmd := exec.Command("tar", tarArgs...)
			if output, err := cmd.CombinedOutput(); err != nil {
				return "", fmt.Errorf("failed to create tar archive: %w\nOutput: %s", err, output)
			}

			defer os.Remove(tarPath)

			// Copy tar to server
			remoteTarPath = fmt.Sprintf("%s/deploy.tar.gz", deployDir)
			if err := d.client.CopyFile(tarPath, remoteTarPath); err != nil {
				return "", fmt.Errorf("failed to copy tar archive: %w", err)
			}

			// Extract on server
			extractCmd := fmt.Sprintf("cd %s && tar -xzf deploy.tar.gz && rm deploy.tar.gz", deployDir)
			if _, err := d.client.Execute(extractCmd); err != nil {
				return "", fmt.Errorf("failed to extract files on server: %w", err)
			}
		}

		// Verify files were copied
		if d.verbose {
			listCmd := fmt.Sprintf("ls -la %s", deployDir)
			fileList, _ := d.client.Execute(listCmd)
			fmt.Printf("  Files in deployment directory:\n")
			for _, line := range strings.Split(fileList, "\n") {
				if line != "" {
					fmt.Printf("    %s\n", line)
				}
			}
		}

		// Build Docker image on server
		if d.verbose {
			fmt.Printf("  Building Docker image on server...\n")
		}

		buildCmd := fmt.Sprintf("cd %s && docker build -t %s . 2>&1 | tee build.log", deployDir, fullImageName)
		output, err := d.client.Execute(buildCmd)

		// Show build output in verbose mode or when there's an error
		if d.verbose || err != nil {
			lines := strings.Split(output, "\n")
			start := len(lines) - 30
			if start < 0 {
				start = 0
			}
			fmt.Printf("  Build output (last 30 lines):\n")
			for _, line := range lines[start:] {
				if line != "" {
					fmt.Printf("    %s\n", line)
				}
			}
		}

		if err != nil {
			return "", fmt.Errorf("failed to build Docker image: %w", err)
		}

		// Verify image was built
		checkImageCmd := fmt.Sprintf("docker image ls --format '{{.Repository}}:{{.Tag}}' | grep '%s' || echo 'NOT_FOUND'", fullImageName)
		imgCheck, _ := d.client.Execute(checkImageCmd)

		if d.verbose {
			fmt.Printf("  Image verification: %s\n", strings.TrimSpace(imgCheck))
		}

		if strings.Contains(imgCheck, "NOT_FOUND") {
			return "", fmt.Errorf("docker build completed but image '%s' was not created - check build output above", fullImageName)
		}

		if d.verbose {
			fmt.Printf("  ✓ Image built and verified: %s\n", fullImageName)
		}

		// Execute post-build hooks
		if service.Hooks != nil && len(service.Hooks.PostBuild) > 0 {
			if err := hookExecutor.ExecutePostBuild(service.Hooks.PostBuild, fullImageName); err != nil {
				return "", fmt.Errorf("post-build hooks failed: %w", err)
			}
		}
	} else if service.Image != "" {
		// Service uses pre-built image (e.g., postgres, redis)
		if d.verbose {
			fmt.Printf("  Using pre-built image: %s\n", service.Image)
		}
		fullImageName = service.Image
	}

	return fullImageName, nil
}

// VerifyNetworkSetup verifies that Traefik is connected to all project networks
func (d *Deployer) VerifyNetworkSetup() error {
	// Check if Traefik container is running
	checkCmd := "docker ps --filter name=^traefik$ --format '{{.Names}}'"
	output, _ := d.client.Execute(checkCmd)

	if strings.TrimSpace(output) != "traefik" {
		// Traefik not running yet - will be created during deployment
		return nil
	}

	// Get network manager
	networkMgr := network.NewManager(d.client, d.config.Project.Name, d.environment, d.verbose)

	// Ensure Traefik is connected to all project networks
	if err := networkMgr.EnsureContainerConnectedToAllNetworks("traefik"); err != nil {
		return fmt.Errorf("failed to verify Traefik network connections: %w", err)
	}

	return nil
}

// VerifyDatabaseConnectivity verifies that a service can reach its database
func (d *Deployer) VerifyDatabaseConnectivity(serviceName string, service *config.ServiceConfig) error {
	containerName := fmt.Sprintf("%s_%s_%s_1", d.config.Project.Name, d.environment, serviceName)
	return d.getHealthChecker().VerifyDatabaseConnectivity(containerName, service)
}
