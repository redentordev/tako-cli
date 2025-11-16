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
	"github.com/redentordev/tako-cli/pkg/secrets"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/swarm"
	"github.com/redentordev/tako-cli/pkg/traefik"
	"github.com/redentordev/tako-cli/pkg/verification"
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

// Helper methods to get managers (created on-demand)

func (d *Deployer) getContainerManager() *ContainerManager {
	return NewContainerManager(d.client, d.verbose)
}

func (d *Deployer) getMaintenanceManager() *MaintenanceManager {
	return NewMaintenanceManager(d.client, d.config.Project.Name, d.verbose)
}

func (d *Deployer) getReplicaManager() *ReplicaManager {
	return NewReplicaManager(d.client, d.config.Project.Name, d.environment, d.verbose)
}

func (d *Deployer) getVolumeTransformer() *VolumeTransformer {
	return NewVolumeTransformer(d.config.Project.Name, d.environment)
}

func (d *Deployer) getEnvManager() *EnvManager {
	return NewEnvManager(d.client, d.config.Project.Name, d.environment, d.verbose)
}

func (d *Deployer) getHealthChecker() *HealthChecker {
	return NewHealthChecker(d.client, d.verbose)
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

// loadAndMergeEnv loads environment variables from envFile and merges with explicit env vars
// Priority: explicit env > envFile
func (d *Deployer) loadAndMergeEnv(service *config.ServiceConfig) map[string]string {
	return d.getEnvManager().LoadAndMerge(service)
}

// transformVolumes prefixes named volumes with project name and environment for isolation
// Leaves bind mounts (absolute paths) unchanged
func (d *Deployer) transformVolumes(volumes []string) []string {
	return d.getVolumeTransformer().Transform(volumes)
}

// DeployService deploys a service with multi-replica support using blue-green strategy
func (d *Deployer) DeployService(serviceName string, service *config.ServiceConfig, skipBuild bool) error {
	deployDir := fmt.Sprintf("/opt/%s", d.config.Project.Name)

	// Validate hooks before deployment
	if service.Hooks != nil {
		if err := hooks.ValidateHooks(service.Hooks); err != nil {
			return fmt.Errorf("hook validation failed: %w", err)
		}
	}

	// Create hook executor
	hookExecutor := hooks.NewExecutor(d.client, d.config.Project.Name, d.environment, serviceName, d.verbose)

	// Execute pre-deploy hooks
	if service.Hooks != nil && len(service.Hooks.PreDeploy) > 0 {
		if err := hookExecutor.ExecutePreDeploy(service.Hooks.PreDeploy, service.Env); err != nil {
			return fmt.Errorf("pre-deploy hooks failed: %w", err)
		}
	}

	// Get full image name - only for services with build path
	// Services using pre-built images will use service.Image directly
	fullImageName := ""
	if service.Build != "" {
		fullImageName = d.config.GetFullImageName(serviceName, d.environment)
	}

	// Remove maintenance mode if active
	// This ensures the service returns to normal operation after deployment
	if err := d.removeMaintenanceMode(serviceName); err != nil {
		// Log but don't fail deployment
		if d.verbose {
			fmt.Printf("  Warning: Failed to check/remove maintenance mode: %v\n", err)
		}
	}

	// Step 1: Build image if not skipped and service has Build path
	if !skipBuild && service.Build != "" {
		// Execute pre-build hooks
		if service.Hooks != nil && len(service.Hooks.PreBuild) > 0 {
			if err := hookExecutor.ExecutePreBuild(service.Hooks.PreBuild, deployDir); err != nil {
				return fmt.Errorf("pre-build hooks failed: %w", err)
			}
		}

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
				return fmt.Errorf("failed to detect framework: %w\nHint: Either add a Dockerfile or ensure your project has recognizable framework files (package.json, go.mod, etc.)", err)
			}

			if d.verbose {
				fmt.Printf("  Detected framework: %s\n", framework)
			}

			// Build locally with Nixpacks
			if err := detector.BuildWithNixpacks(fullImageName); err != nil {
				return fmt.Errorf("failed to build with Nixpacks: %w", err)
			}

			// Save image as tar
			if d.verbose {
				fmt.Printf("  Exporting image...\n")
			}

			// We'll need to transfer the image to the server
			// For now, we'll build on server with generated Dockerfile
			if err := detector.GenerateDockerfile(); err != nil {
				return fmt.Errorf("failed to generate Dockerfile: %w", err)
			}

			hasDockerfile = true
		}

		// Build on remote server
		if d.verbose {
			fmt.Printf("  Preparing deployment directory...\n")
		}

		// Create deployment directory on server
		if _, err := d.client.Execute(fmt.Sprintf("mkdir -p %s", deployDir)); err != nil {
			return fmt.Errorf("failed to create deployment directory: %w", err)
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
			return fmt.Errorf("failed to get absolute path: %w", err)
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
				return fmt.Errorf("failed to create zip archive: %w", err)
			}

			defer os.Remove(zipPath)

			// Copy zip to server
			remoteTarPath = fmt.Sprintf("%s/deploy.zip", deployDir)
			if err := d.client.CopyFile(zipPath, remoteTarPath); err != nil {
				return fmt.Errorf("failed to copy zip archive: %w", err)
			}

			// Extract on server using Python (universally available)
			extractCmd := fmt.Sprintf("cd %s && python3 -m zipfile -e deploy.zip . && rm deploy.zip", deployDir)
			if _, err := d.client.Execute(extractCmd); err != nil {
				return fmt.Errorf("failed to extract files on server: %w", err)
			}
		} else {
			// Unix/Linux: Use tar
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
			output, err := cmd.CombinedOutput()
			if err != nil && !strings.Contains(string(output), "file changed as we read it") {
				return fmt.Errorf("failed to create tar archive: %w, output: %s", err, string(output))
			}

			defer os.Remove(tarPath)

			// Copy tar to server
			remoteTarPath = fmt.Sprintf("%s/deploy.tar.gz", deployDir)
			if err := d.client.CopyFile(tarPath, remoteTarPath); err != nil {
				return fmt.Errorf("failed to copy tar archive: %w", err)
			}

			// Extract on server
			extractCmd := fmt.Sprintf("cd %s && tar -xzf deploy.tar.gz && rm deploy.tar.gz", deployDir)
			if _, err := d.client.Execute(extractCmd); err != nil {
				return fmt.Errorf("failed to extract files on server: %w", err)
			}
		}

		// Verify files were copied and show summary
		listCmd := fmt.Sprintf("ls -la %s 2>/dev/null || ls -la %s", deployDir, deployDir)
		fileList, _ := d.client.Execute(listCmd)
		fileCount := len(strings.Split(strings.TrimSpace(fileList), "\n")) - 3 // Subtract ., .., total line

		if d.verbose {
			fmt.Printf("  Files in deployment directory (%d files):\n", fileCount)
			for _, line := range strings.Split(fileList, "\n") {
				if line != "" {
					fmt.Printf("    %s\n", line)
				}
			}
		} else {
			fmt.Printf("  ✓ Copied %d files to build context\n", fileCount)
		}

		// Show critical files status
		criticalFiles := []string{"Dockerfile", "package.json", "package-lock.json", ".dockerignore"}
		var foundFiles []string
		var missingFiles []string

		for _, file := range criticalFiles {
			checkCmd := fmt.Sprintf("test -f %s/%s && echo 'EXISTS' || echo 'MISSING'", deployDir, file)
			result, _ := d.client.Execute(checkCmd)
			if strings.Contains(result, "EXISTS") {
				foundFiles = append(foundFiles, file)
			} else {
				missingFiles = append(missingFiles, file)
			}
		}

		if d.verbose && len(foundFiles) > 0 {
			fmt.Printf("  ✓ Found: %s\n", strings.Join(foundFiles, ", "))
		}
		if d.verbose && len(missingFiles) > 0 {
			fmt.Printf("  ℹ Missing (optional): %s\n", strings.Join(missingFiles, ", "))
		}

		// Build Docker image on server
		fmt.Printf("  Building Docker image on server...\n")

		buildCmd := fmt.Sprintf("cd %s && docker build -t %s . 2>&1 | tee build.log", deployDir, fullImageName)

		// Use streaming output for better visibility
		var buildOutput strings.Builder
		buildWriter := io.MultiWriter(&buildOutput, &streamWriter{prefix: "    "})

		err = d.client.ExecuteStream(buildCmd, buildWriter, buildWriter)

		// Save build output to log file for debugging
		buildLogPath := filepath.Join(deployDir, "build.log")
		if d.verbose {
			fmt.Printf("  Build log saved to: %s\n", buildLogPath)
		}

		if err != nil {
			// Show summary of files that were copied for debugging
			fmt.Printf("\n  Build failed! Debugging information:\n")
			fmt.Printf("  Files copied to build context:\n")
			listCmd := fmt.Sprintf("cd %s && ls -lah", deployDir)
			if fileList, _ := d.client.Execute(listCmd); fileList != "" {
				for _, line := range strings.Split(fileList, "\n") {
					if line != "" {
						fmt.Printf("    %s\n", line)
					}
				}
			}

			// Show Dockerfile content
			fmt.Printf("\n  Dockerfile content:\n")
			dockerfileCmd := fmt.Sprintf("cat %s/Dockerfile", deployDir)
			if dockerfile, _ := d.client.Execute(dockerfileCmd); dockerfile != "" {
				for _, line := range strings.Split(dockerfile, "\n") {
					if line != "" {
						fmt.Printf("    %s\n", line)
					}
				}
			}

			return fmt.Errorf("failed to build Docker image: %w", err)
		}

		// Verify image was built
		checkImageCmd := fmt.Sprintf("docker image ls --format '{{.Repository}}:{{.Tag}}' | grep '%s' || echo 'NOT_FOUND'", fullImageName)
		imgCheck, _ := d.client.Execute(checkImageCmd)

		if d.verbose {
			fmt.Printf("  Image verification: %s\n", strings.TrimSpace(imgCheck))
		}

		if strings.Contains(imgCheck, "NOT_FOUND") {
			return fmt.Errorf("docker build completed but image '%s' was not created - check build output above", fullImageName)
		}

		if d.verbose {
			fmt.Printf("  ✓ Image built and verified: %s\n", fullImageName)
		}

		// Execute post-build hooks
		if service.Hooks != nil && len(service.Hooks.PostBuild) > 0 {
			if err := hookExecutor.ExecutePostBuild(service.Hooks.PostBuild, fullImageName); err != nil {
				return fmt.Errorf("post-build hooks failed: %w", err)
			}
		}
	} else if !skipBuild && service.Image != "" {
		// Service uses pre-built image (e.g., postgres, redis)
		if d.verbose {
			fmt.Printf("  Using pre-built image: %s\n", service.Image)
		}
		fullImageName = service.Image
	}

	// Step 2: Create Docker network
	if d.verbose {
		fmt.Printf("  Setting up Docker network...\n")
	}

	networkMgr := network.NewManager(d.client, d.config.Project.Name, d.environment, d.verbose)
	if err := networkMgr.EnsureNetwork(); err != nil {
		return fmt.Errorf("failed to create network: %w", err)
	}

	// Secrets are now handled via env files during container deployment (see deployReplica)

	// Step 3: Determine number of replicas (default to 1 if not specified)
	replicas := service.Replicas
	if replicas <= 0 {
		replicas = 1
	}

	if d.verbose {
		fmt.Printf("  Deploying %d replica(s) for service '%s'...\n", replicas, serviceName)
	}

	// Step 4: Deploy each replica
	for i := 1; i <= replicas; i++ {
		if d.verbose {
			fmt.Printf("\n  === Deploying replica %d/%d ===\n", i, replicas)
		}

		if err := d.deployReplica(serviceName, service, i, networkMgr, fullImageName); err != nil {
			return fmt.Errorf("failed to deploy replica %d: %w", i, err)
		}
	}

	// Step 5: Cleanup old replicas if scaling down
	if err := d.cleanupOldReplicas(serviceName, replicas); err != nil {
		if d.verbose {
			fmt.Printf("  Warning: failed to cleanup old replicas: %v\n", err)
		}
	}

	// Step 6: Update Traefik configuration if service is public (has proxy config)
	if service.IsPublic() {
		if d.verbose {
			fmt.Printf("\n  Updating reverse proxy configuration...\n")
		}
		if err := d.updateProxyForReplicas(serviceName, service, replicas); err != nil {
			return fmt.Errorf("failed to update proxy: %w", err)
		}
	}

	if d.verbose {
		fmt.Printf("\n  ✓ Successfully deployed %d replica(s) for service '%s'\n", replicas, serviceName)
	}

	// Execute post-deploy hooks
	if service.Hooks != nil && len(service.Hooks.PostDeploy) > 0 {
		if err := hookExecutor.ExecutePostDeploy(service.Hooks.PostDeploy, service.Env); err != nil {
			return fmt.Errorf("post-deploy hooks failed: %w", err)
		}
	}

	// Execute post-start hooks (after all replicas are running)
	if service.Hooks != nil && len(service.Hooks.PostStart) > 0 {
		// Use the first replica's container name for exec hooks
		firstReplicaName := fmt.Sprintf("%s_%s_%s_1", d.config.Project.Name, d.environment, serviceName)
		if err := hookExecutor.ExecutePostStart(service.Hooks.PostStart, firstReplicaName, service.Env); err != nil {
			return fmt.Errorf("post-start hooks failed: %w", err)
		}
	}

	return nil
}

// deployReplica deploys a single replica of a service
func (d *Deployer) deployReplica(serviceName string, service *config.ServiceConfig, replicaNum int, networkMgr *network.Manager, fullImageName string) error {
	// Build container name: {project}_{environment}_{service}_{replica}
	containerName := fmt.Sprintf("%s_%s_%s_%d", d.config.Project.Name, d.environment, serviceName, replicaNum)

	// Calculate port for this replica
	replicaPort := 0
	if service.Port > 0 {
		replicaPort = service.Port + (replicaNum - 1)
	}

	if d.verbose {
		fmt.Printf("  Container: %s\n", containerName)
		if replicaPort > 0 {
			fmt.Printf("  Port: %d\n", replicaPort)
		}
	}

	// Check for port conflicts only for non-public services (Traefik handles public services)
	if !service.IsPublic() && replicaPort > 0 {
		if d.verbose {
			fmt.Printf("  Checking port %d availability...\n", replicaPort)
		}

		portInfo, err := d.CheckPortAvailability(replicaPort)
		if err != nil {
			if d.verbose {
				fmt.Printf("  Warning: Could not check port availability: %v\n", err)
			}
		} else if portInfo != nil {
			// Port is in use - try to resolve
			if err := d.ResolvePortConflict(portInfo, serviceName, true); err != nil {
				return fmt.Errorf("port conflict on %d: %w", replicaPort, err)
			}
		} else if d.verbose {
			fmt.Printf("  ✓ Port %d is available\n", replicaPort)
		}
	}

	// BLUE-GREEN DEPLOYMENT: Check if old container exists
	// We'll start the new container with a temporary name first
	oldContainerExists, err := d.containerExists(containerName)
	if err != nil {
		if d.verbose {
			fmt.Printf("  Warning: Could not check if container exists: %v\n", err)
		}
		oldContainerExists = false // Assume doesn't exist
	}

	// Use temporary name for new container during blue-green deployment
	tempContainerName := containerName
	if oldContainerExists {
		tempContainerName = fmt.Sprintf("%s_new", containerName)
		if d.verbose {
			fmt.Printf("  Old container exists, using blue-green deployment...\n")
			fmt.Printf("  Starting new container: %s\n", tempContainerName)
		}
	} else {
		if d.verbose {
			fmt.Printf("  No old container found, starting fresh deployment...\n")
		}

		// Clean up legacy naming patterns on first deployment
		if replicaNum == 1 {
			// Legacy without environment or replica number: {project}_{service}
			legacyName1 := fmt.Sprintf("%s_%s", d.config.Project.Name, serviceName)
			d.client.Execute(fmt.Sprintf("docker stop %s 2>/dev/null || true", legacyName1))
			d.client.Execute(fmt.Sprintf("docker rm -f %s 2>/dev/null || true", legacyName1))

			// Legacy with replica but no environment: {project}_{service}_{replica}
			legacyName2 := fmt.Sprintf("%s_%s_%d", d.config.Project.Name, serviceName, replicaNum)
			d.client.Execute(fmt.Sprintf("docker stop %s 2>/dev/null || true", legacyName2))
			d.client.Execute(fmt.Sprintf("docker rm -f %s 2>/dev/null || true", legacyName2))
		}
	}

	// Build environment variables
	// Merge envFile vars with explicit env vars (explicit takes priority)
	mergedEnv := d.loadAndMergeEnv(service)

	envVars := ""
	for key, value := range mergedEnv {
		// Escape single quotes in value for shell safety
		escapedValue := strings.ReplaceAll(value, "'", "'\\''")
		envVars += fmt.Sprintf(" -e %s='%s'", key, escapedValue)
	}

	// Add replica number as environment variable
	envVars += fmt.Sprintf(" -e REPLICA_NUM=%d", replicaNum)

	// Build docker run command (use temp name for blue-green deployment)
	runCmd := fmt.Sprintf("docker run -d --name %s --restart=%s", tempContainerName, service.Restart)

	// Add network
	runCmd += fmt.Sprintf(" --network %s", networkMgr.GetNetworkName())

	// Add network aliases for service discovery
	// Alias 1: service name (load balanced across all replicas)
	runCmd += fmt.Sprintf(" --network-alias %s", serviceName)
	// Alias 2: service_replica (specific replica)
	runCmd += fmt.Sprintf(" --network-alias %s_%d", serviceName, replicaNum)
	// Alias 3: global alias for exported services (accessible from other projects)
	// This allows other projects to import this service using a predictable name
	if service.Export {
		globalAlias := fmt.Sprintf("%s_%s_%s", d.config.Project.Name, d.environment, serviceName)
		runCmd += fmt.Sprintf(" --network-alias %s", globalAlias)
		if d.verbose {
			fmt.Printf("  Adding global export alias: %s\n", globalAlias)
		}
	}

	// Add Traefik labels at creation time if service is public
	// This avoids the need to recreate containers later
	// Use tempContainerName for blue-green deployment
	if service.IsPublic() {
		traefikMgr := traefik.NewManager(d.client, d.config.Project.Name, d.environment, d.verbose)
		traefikLabels := traefikMgr.GetContainerLabels(tempContainerName, service)
		runCmd += fmt.Sprintf(" %s", traefikLabels)
	}

	// Port mapping is no longer needed for public services - Traefik handles routing
	// Only map ports for non-public services (internal services)
	if !service.IsPublic() && replicaPort > 0 {
		runCmd += fmt.Sprintf(" -p %d:%d", replicaPort, service.Port)
	}

	// Add environment variables
	runCmd += envVars

	// Handle new Tako secrets via env file
	var envFilePath string
	if len(service.Secrets) > 0 {
		secretsMgr, err := secrets.NewManager(d.environment)
		if err != nil {
			return fmt.Errorf("failed to create secrets manager: %w", err)
		}

		envFile, err := secretsMgr.CreateEnvFile(service)
		if err != nil {
			return fmt.Errorf("failed to create env file: %w", err)
		}

		envFilePath = envFile.GetPath(d.config.Project.Name, serviceName)

		if d.verbose {
			fmt.Printf("  Uploading secrets as env file (redacted): %s\n", envFilePath)
		}

		if err := d.client.UploadReader(envFile.ToReader(), envFilePath, 0600); err != nil {
			return fmt.Errorf("failed to upload env file: %w", err)
		}

		runCmd += fmt.Sprintf(" --env-file %s", envFilePath)
	}

	// Handle Docker Swarm secrets (backward compatibility)
	if len(service.DockerSecrets) > 0 {
		secretsDir := fmt.Sprintf("/opt/%s/secrets", d.config.Project.Name)
		for _, secret := range service.DockerSecrets {
			secretFileName := fmt.Sprintf("%s_%s_%s", d.config.Project.Name, d.environment, secret.Name)
			sourcePath := fmt.Sprintf("%s/%s", secretsDir, secretFileName)
			targetPath := secret.Target
			if targetPath == "" {
				targetPath = fmt.Sprintf("/run/secrets/%s", secret.Name)
			}
			runCmd += fmt.Sprintf(" -v %s:%s:ro", sourcePath, targetPath)
		}
	}

	// Add volumes (with project-scoping for named volumes)
	transformedVolumes := d.transformVolumes(service.Volumes)
	for _, volume := range transformedVolumes {
		runCmd += fmt.Sprintf(" -v %s", volume)
	}

	// Add image - use fullImageName which is calculated from config
	imageName := fullImageName
	if imageName == "" {
		// Fallback for services using pre-built images
		imageName = service.Image
	}
	runCmd += fmt.Sprintf(" %s", imageName)

	// Add command if specified
	if service.Command != "" {
		runCmd += fmt.Sprintf(" %s", service.Command)
	}

	if d.verbose {
		fmt.Printf("  Starting container...\n")
		fmt.Printf("  Command: %s\n", runCmd)
	}

	output, err := d.client.Execute(runCmd)
	if err != nil {
		// Try to get more specific error from Docker
		if d.verbose {
			fmt.Printf("  Docker run failed, checking logs...\n")
			// Check if image exists
			checkImgCmd := fmt.Sprintf("docker image ls --format '{{.Repository}}:{{.Tag}}' | grep '%s' || echo 'Image not found'", imageName)
			imgCheck, _ := d.client.Execute(checkImgCmd)
			fmt.Printf("  Image check: %s\n", imgCheck)

			// List all images
			allImagesCmd := "docker image ls --format 'table {{.Repository}}\\t{{.Tag}}\\t{{.ID}}' | head -10"
			allImages, _ := d.client.Execute(allImagesCmd)
			fmt.Printf("  All images (first 10):\n%s\n", allImages)

			// Try to get Docker error
			dockerErr, _ := d.client.Execute("docker info 2>&1 | head -5")
			fmt.Printf("  Docker status:\n%s\n", dockerErr)
		}
		return fmt.Errorf("failed to start container: %w\nCommand: %s\nOutput: %s", err, runCmd, output)
	}

	// Wait for container to be healthy
	if service.HealthCheck.Path != "" {
		if d.verbose {
			fmt.Printf("  Waiting for health check...\n")
		}

		retries := service.HealthCheck.Retries
		if retries <= 0 {
			retries = 5 // Default retries
		}

		if err := d.waitForHealthy(tempContainerName, retries); err != nil {
			// Health check failed - cleanup temp container
			d.client.Execute(fmt.Sprintf("docker stop %s 2>/dev/null || true", tempContainerName))
			d.client.Execute(fmt.Sprintf("docker rm -f %s 2>/dev/null || true", tempContainerName))
			return fmt.Errorf("health check failed: %w", err)
		}

		if d.verbose {
			fmt.Printf("  ✓ Container is healthy\n")
		}
	} else {
		// No health check, just wait a bit for container to start
		time.Sleep(3 * time.Second)

		// Verify container is running
		checkCmd := fmt.Sprintf("docker inspect -f '{{.State.Running}}' %s", tempContainerName)
		running, err := d.client.Execute(checkCmd)
		if err != nil || strings.TrimSpace(running) != "true" {
			// Container failed - cleanup
			d.client.Execute(fmt.Sprintf("docker stop %s 2>/dev/null || true", tempContainerName))
			d.client.Execute(fmt.Sprintf("docker rm -f %s 2>/dev/null || true", tempContainerName))
			return fmt.Errorf("container failed to start")
		}

		if d.verbose {
			fmt.Printf("  ✓ Container is running\n")
		}
	}

	// Verify deployment before marking as successful
	if d.verbose {
		fmt.Printf("  Verifying deployment...\n")
	}

	verifier := verification.NewVerifier(d.client, d.verbose)
	if err := verifier.VerifyDeployment(tempContainerName, service); err != nil {
		// Verification failed - cleanup and return error
		if d.verbose {
			fmt.Printf("  ✗ Verification failed: %v\n", err)
			fmt.Printf("  Cleaning up failed container...\n")
		}

		// Stop and remove the failed container
		d.client.Execute(fmt.Sprintf("docker stop %s 2>/dev/null || true", tempContainerName))
		d.client.Execute(fmt.Sprintf("docker rm -f %s 2>/dev/null || true", tempContainerName))

		return fmt.Errorf("deployment verification failed: %w", err)
	}

	if d.verbose {
		fmt.Printf("  ✓ Deployment verified successfully\n")
	}

	// BLUE-GREEN SWITCHOVER: If old container exists, perform graceful switchover
	if oldContainerExists {
		if d.verbose {
			fmt.Printf("\n  Performing blue-green switchover...\n")
		}

		// Wait for traffic drain period (Traefik will automatically discover new container)
		// This gives Traefik time to detect the new container and update routing
		d.waitForTrafficDrain(containerName, 5)

		// Now gracefully stop the old container
		if err := d.stopContainerGracefully(containerName, 30); err != nil {
			if d.verbose {
				fmt.Printf("  Warning: Failed to stop old container gracefully: %v\n", err)
			}
		}

		// Rename new container to final name
		if d.verbose {
			fmt.Printf("  Renaming new container to final name...\n")
		}
		renameCmd := fmt.Sprintf("docker rename %s %s", tempContainerName, containerName)
		if _, err := d.client.Execute(renameCmd); err != nil {
			// This is critical - if rename fails, we're in a bad state
			return fmt.Errorf("failed to rename container after switchover: %w", err)
		}

		if d.verbose {
			fmt.Printf("  ✓ Blue-green switchover completed\n")
		}
	}

	// Handle cross-project imports (network bridging)
	if len(service.Imports) > 0 {
		if d.verbose {
			fmt.Printf("\n  Connecting to imported services...\n")
		}

		for _, importSpec := range service.Imports {
			parts := strings.Split(importSpec, ".")
			if len(parts) != 2 {
				fmt.Printf("  ⚠ Invalid import format: %s (expected project.service)\n", importSpec)
				continue
			}

			targetProject := parts[0]
			targetService := parts[1]

			// Connect to target project's network (defaults to same environment)
			// Note: After blue-green switchover, tempContainerName == containerName (renamed)
			currentContainerName := containerName
			networkMgr := network.NewManager(d.client, d.config.Project.Name, d.environment, d.verbose)
			// Pass empty string for targetEnvironment to use current environment
			if err := networkMgr.ConnectToExternalNetwork(currentContainerName, targetProject, ""); err != nil {
				fmt.Printf("  ⚠ Failed to import %s: %v\n", importSpec, err)
			} else {
				if d.verbose {
					fmt.Printf("  ✓ Connected to %s (access via %s_%s_%s)\n",
						importSpec, targetProject, d.environment, targetService)
				}
			}
		}
	}

	// Cleanup env file after successful deployment
	if envFilePath != "" {
		if d.verbose {
			fmt.Printf("  Cleaning up temporary env file...\n")
		}
		d.client.Execute(fmt.Sprintf("rm -f %s", envFilePath))
	}

	return nil
}

// cleanupOldReplicas removes replica containers that exceed the desired count
func (d *Deployer) cleanupOldReplicas(serviceName string, desiredCount int) error {
	return d.getReplicaManager().CleanupOld(serviceName, desiredCount)
}

// updateProxyForReplicas updates Traefik reverse proxy configuration for load balancing across replicas
// NOTE: Traefik labels are now added during container creation (see deployReplica)
// This function only ensures Traefik is running and connected to the project network
func (d *Deployer) updateProxyForReplicas(serviceName string, service *config.ServiceConfig, replicas int) error {
	// Use Traefik manager for proxy configuration
	traefikMgr := traefik.NewManager(d.client, d.config.Project.Name, d.environment, d.verbose)

	// Get network manager to determine network name
	networkMgr := network.NewManager(d.client, d.config.Project.Name, d.environment, d.verbose)

	// Get email for SSL certificates
	email := service.Proxy.Email
	if email == "" {
		email = "tako@redentor.dev"
	}

	// Ensure Traefik container is running and connected to the project network
	// This will automatically connect Traefik to the network if it's not already
	if err := traefikMgr.EnsureTraefikContainer(networkMgr.GetNetworkName(), email); err != nil {
		return fmt.Errorf("failed to ensure Traefik container: %w", err)
	}

	// IMPORTANT: Ensure Traefik is connected to ALL project networks
	// This prevents network isolation issues when multiple projects are deployed
	if err := networkMgr.EnsureContainerConnectedToAllNetworks("traefik"); err != nil {
		if d.verbose {
			fmt.Printf("  Warning: Failed to connect Traefik to all networks: %v\n", err)
		}
		// Don't fail deployment, just log warning
	}

	// Verify all replica containers are running and properly labeled
	for i := 1; i <= replicas; i++ {
		containerName := fmt.Sprintf("%s_%s_%s_%d", d.config.Project.Name, d.environment, serviceName, i)

		// Verify container exists
		checkCmd := fmt.Sprintf("docker inspect %s --format '{{.State.Running}}' 2>/dev/null", containerName)
		running, err := d.client.Execute(checkCmd)
		if err != nil || strings.TrimSpace(running) != "true" {
			if d.verbose {
				fmt.Printf("  Warning: container %s is not running\n", containerName)
			}
			continue
		}

		// Verify Traefik labels are present
		labelsCmd := fmt.Sprintf("docker inspect %s --format '{{index .Config.Labels \"traefik.enable\"}}' 2>/dev/null", containerName)
		hasLabels, _ := d.client.Execute(labelsCmd)
		if strings.TrimSpace(hasLabels) != "true" {
			if d.verbose {
				fmt.Printf("  Warning: container %s is missing Traefik labels (may have been created before this update)\n", containerName)
			}
		}
	}

	if d.verbose {
		fmt.Printf("  ✓ Traefik proxy configured for %d replica(s)\n", replicas)
		fmt.Printf("  ✓ Traefik is connected to all project networks\n")
	}

	return nil
}

// Rollback rolls back to the previous deployment
func (d *Deployer) Rollback(serviceName string) error {
	containerName := fmt.Sprintf("%s_%s_%s", d.config.Project.Name, d.environment, serviceName)
	oldContainerName := fmt.Sprintf("%s_old", containerName)

	// Check if old container exists
	checkCmd := fmt.Sprintf("docker ps -a -q -f name=%s", oldContainerName)
	oldContainer, _ := d.client.Execute(checkCmd)

	if oldContainer == "" {
		return fmt.Errorf("no previous version found to rollback to")
	}

	// Stop current container
	d.client.Execute(fmt.Sprintf("docker stop %s", containerName))

	// Rename current to failed
	d.client.Execute(fmt.Sprintf("docker rename %s %s_failed", containerName, containerName))

	// Rename old to current
	if _, err := d.client.Execute(fmt.Sprintf("docker rename %s %s", oldContainerName, containerName)); err != nil {
		return fmt.Errorf("failed to restore old container: %w", err)
	}

	// Start old container
	if _, err := d.client.Execute(fmt.Sprintf("docker start %s", containerName)); err != nil {
		return fmt.Errorf("failed to start old container: %w", err)
	}

	// Remove failed container
	d.client.Execute(fmt.Sprintf("docker rm -f %s_failed", containerName))

	return nil
}

// RollbackToState rolls back a service to a specific deployment state
func (d *Deployer) RollbackToState(serviceName string, serviceState *state.ServiceState) error {
	if d.verbose {
		fmt.Printf("  Rolling back service %s to image %s...\n", serviceName, serviceState.Image)
	}

	// Check if the target image exists
	checkImageCmd := fmt.Sprintf("docker images -q %s 2>/dev/null", serviceState.ImageID)
	imageExists, _ := d.client.Execute(checkImageCmd)

	if strings.TrimSpace(imageExists) == "" {
		// Try to find by image name:tag
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

	// Get current service configuration to check if it uses Traefik
	currentService, err := d.config.GetService(d.environment, serviceName)
	if err != nil {
		// Service might have been removed from config, use default behavior
		if d.verbose {
			fmt.Printf("  Warning: service not found in current config, using saved state\n")
		}
		currentService = nil
	}

	// Stop and remove all current replicas for this service
	if d.verbose {
		fmt.Printf("  Stopping current replicas...\n")
	}
	// Stop all containers matching the service pattern
	stopCmd := fmt.Sprintf("docker ps -a --filter 'name=%s_%s_%s_' --format '{{.Names}}' | xargs -r docker stop 2>/dev/null || true",
		d.config.Project.Name, d.environment, serviceName)
	d.client.Execute(stopCmd)

	removeCmd := fmt.Sprintf("docker ps -a --filter 'name=%s_%s_%s_' --format '{{.Names}}' | xargs -r docker rm -f 2>/dev/null || true",
		d.config.Project.Name, d.environment, serviceName)
	d.client.Execute(removeCmd)

	// Replicas count from saved state (default to 1)
	replicas := serviceState.Replicas
	if replicas <= 0 {
		replicas = 1
	}

	if d.verbose {
		fmt.Printf("  Starting %d replica(s) from saved state...\n", replicas)
	}

	// Start replicas
	for i := 1; i <= replicas; i++ {
		containerName := fmt.Sprintf("%s_%s_%s_%d", d.config.Project.Name, d.environment, serviceName, i)

		// Calculate port for this replica
		replicaPort := 0
		if serviceState.Port > 0 {
			replicaPort = serviceState.Port + (i - 1)
		}

		// Build environment variables from saved state
		envVars := ""
		for key, value := range serviceState.Env {
			envVars += fmt.Sprintf(" -e %s='%s'", key, value)
		}
		// Add REPLICA_NUM
		envVars += fmt.Sprintf(" -e REPLICA_NUM=%d", i)

		// Get network name
		networkName := fmt.Sprintf("tako_%s_%s", d.config.Project.Name, d.environment)

		// Build the docker run command
		runCmd := fmt.Sprintf("docker run -d --name %s --restart=unless-stopped --network %s --network-alias %s --network-alias %s_%d",
			containerName,
			networkName,
			serviceName,
			serviceName,
			i,
		)

		// Add Traefik labels if service is currently configured as public
		// This ensures rollback works with current Traefik-based deployments
		if currentService != nil && currentService.IsPublic() {
			traefikMgr := traefik.NewManager(d.client, d.config.Project.Name, d.environment, d.verbose)
			traefikLabels := traefikMgr.GetContainerLabels(containerName, currentService)
			runCmd += fmt.Sprintf(" %s", traefikLabels)

			if d.verbose {
				fmt.Printf("  Adding Traefik labels for public service\n")
			}
		} else {
			// For non-public services (internal APIs, etc.), use port mapping as before
			if replicaPort > 0 {
				runCmd += fmt.Sprintf(" -p %d:%d", replicaPort, serviceState.Port)
				if d.verbose {
					fmt.Printf("  Mapping port %d:%d for internal service\n", replicaPort, serviceState.Port)
				}
			}
		}

		// Add environment variables and image
		runCmd += fmt.Sprintf(" %s %s", envVars, serviceState.Image)

		if d.verbose {
			fmt.Printf("  Starting replica %d/%d: %s\n", i, replicas, containerName)
		}

		output, err := d.client.Execute(runCmd)
		if err != nil {
			return fmt.Errorf("failed to start container %s: %w\nOutput: %s", containerName, err, output)
		}

		// Wait for container to be healthy
		if serviceState.HealthCheck.Enabled {
			if d.verbose {
				fmt.Printf("    Waiting for health check...\n")
			}
			retries := 5
			if err := d.waitForHealthy(containerName, retries); err != nil {
				return fmt.Errorf("health check failed for %s after rollback: %w", containerName, err)
			}
		} else {
			// No health check, just wait a bit
			time.Sleep(2 * time.Second)
		}

		// Verify container is running
		checkCmd := fmt.Sprintf("docker inspect -f '{{.State.Running}}' %s", containerName)
		running, err := d.client.Execute(checkCmd)
		if err != nil || strings.TrimSpace(running) != "true" {
			return fmt.Errorf("container %s failed to start after rollback", containerName)
		}

		if d.verbose {
			fmt.Printf("    ✓ Replica %d/%d is running\n", i, replicas)
		}
	}

	if d.verbose {
		fmt.Printf("  ✓ All replicas started successfully\n")
	}

	return nil
}

// waitForHealthy waits for a container to become healthy
func (d *Deployer) waitForHealthy(containerName string, retries int) error {
	return d.getHealthChecker().WaitForHealthy(containerName, retries)
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

// updateProxy updates Traefik reverse proxy configuration for single-server deployments
func (d *Deployer) updateProxy(serviceName string, service *config.ServiceConfig) error {
	// Use Traefik for reverse proxy (same as updateProxyForReplicas but for single replica)
	return d.updateProxyForReplicas(serviceName, service, 1)
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

// containerExists checks if a container with the given name exists (running or stopped)
func (d *Deployer) containerExists(containerName string) (bool, error) {
	return d.getContainerManager().Exists(containerName)
}

// waitForTrafficDrain waits for in-flight requests to complete before stopping a container
func (d *Deployer) waitForTrafficDrain(containerName string, drainSeconds int) {
	d.getContainerManager().WaitForTrafficDrain(containerName, drainSeconds)
}

// stopContainerGracefully stops a container with a grace period for cleanup
func (d *Deployer) stopContainerGracefully(containerName string, gracePeriodSeconds int) error {
	return d.getContainerManager().StopGracefully(containerName, gracePeriodSeconds)
}

// removeMaintenanceMode removes the maintenance page container if it exists
// This is called automatically during deployment to restore normal traffic
func (d *Deployer) removeMaintenanceMode(serviceName string) error {
	return d.getMaintenanceManager().Remove(serviceName)
}
