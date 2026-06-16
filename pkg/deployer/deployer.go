package deployer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/nixpacks"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
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
	client            *ssh.Client
	config            *config.Config
	environment       string
	verbose           bool
	sshPool           *ssh.Pool
	distributedImages map[string]bool
	targetServers     []string
	cliVersion        string
	meshPortCache     map[meshUpstreamPortKey]int
	meshPortCacheMu   sync.Mutex
	meshPortAllocator func(serverName string, serviceName string, slot int, containerPort int) (int, error)
}

const (
	defaultBuildContextArchiveMaxBytes     int64 = 2 << 30
	defaultBuildContextArchiveMaxFileBytes int64 = 1 << 30
	defaultBuildContextArchiveMaxEntries         = 200000
)

type buildContextArchiveLimits struct {
	MaxBytes     int64
	MaxFileBytes int64
	MaxEntries   int
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
	return &Deployer{
		client:            client,
		config:            cfg,
		environment:       environment,
		verbose:           verbose,
		sshPool:           sshPool,
		distributedImages: make(map[string]bool),
	}
}

func (d *Deployer) SetCLIVersion(version string) {
	d.cliVersion = strings.TrimSpace(version)
}

// SetTargetServers restricts takod reconciliation to a validated subset of the
// environment nodes. Passing an empty slice restores the full environment.
func (d *Deployer) SetTargetServers(serverNames []string) error {
	if len(serverNames) == 0 {
		d.targetServers = nil
		return nil
	}

	environmentServers, err := d.config.GetEnvironmentServers(d.environment)
	if err != nil {
		return fmt.Errorf("failed to get environment servers: %w", err)
	}

	allowed := make(map[string]bool, len(environmentServers))
	for _, serverName := range environmentServers {
		allowed[serverName] = true
	}

	targets := make([]string, 0, len(serverNames))
	seen := make(map[string]bool, len(serverNames))
	for _, serverName := range serverNames {
		if seen[serverName] {
			continue
		}
		if !allowed[serverName] {
			return fmt.Errorf("target server %s is not part of environment %s", serverName, d.environment)
		}
		if _, ok := d.config.Servers[serverName]; !ok {
			return fmt.Errorf("target server %s is not defined in servers", serverName)
		}
		targets = append(targets, serverName)
		seen[serverName] = true
	}

	if len(targets) == 0 {
		return fmt.Errorf("no target servers selected")
	}

	d.targetServers = targets
	return nil
}

// createCrossPlatformTarGz creates a Docker build context archive with
// Unix-style paths on every client platform.
func createCrossPlatformTarGz(sourceDir, archivePath string) error {
	return createCrossPlatformTarGzWithLimits(sourceDir, archivePath, buildContextArchiveLimits{
		MaxBytes:     defaultBuildContextArchiveMaxBytes,
		MaxFileBytes: defaultBuildContextArchiveMaxFileBytes,
		MaxEntries:   defaultBuildContextArchiveMaxEntries,
	})
}

func createCrossPlatformTarGzWithLimits(sourceDir, archivePath string, limits buildContextArchiveLimits) error {
	archiveFile, err := os.Create(archivePath)
	if err != nil {
		return fmt.Errorf("failed to create build context archive: %w", err)
	}
	defer archiveFile.Close()

	gzipWriter := gzip.NewWriter(archiveFile)
	defer gzipWriter.Close()
	tarWriter := tar.NewWriter(gzipWriter)
	defer tarWriter.Close()

	ignoreParser := NewIgnoreParser()

	dockerignorePath := filepath.Join(sourceDir, ".dockerignore")
	dockerignoreExists := false
	if _, err := os.Stat(dockerignorePath); err == nil {
		dockerignoreExists = true
		ignoreParser.LoadIgnoreFile(dockerignorePath)
	}

	if !dockerignoreExists {
		gitignorePath := filepath.Join(sourceDir, ".gitignore")
		ignoreParser.LoadIgnoreFile(gitignorePath)
	}
	ignoreParser.AddDefaultExclusions()

	var totalBytes int64
	entries := 0
	return filepath.Walk(sourceDir, func(filePath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(sourceDir, filePath)
		if err != nil {
			return err
		}
		if relPath == "." {
			return nil
		}

		if ignoreParser.ShouldIgnore(relPath) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		entries++
		if limits.MaxEntries > 0 && entries > limits.MaxEntries {
			return fmt.Errorf("build context exceeds maximum entry count %d", limits.MaxEntries)
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = strings.ReplaceAll(relPath, string(filepath.Separator), "/")

		if info.IsDir() {
			header.Name += "/"
			header.Mode = 0755
			return tarWriter.WriteHeader(header)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if limits.MaxFileBytes > 0 && info.Size() > limits.MaxFileBytes {
			return fmt.Errorf("build context file %s exceeds maximum size %d bytes", relPath, limits.MaxFileBytes)
		}
		if limits.MaxBytes > 0 && totalBytes+info.Size() > limits.MaxBytes {
			return fmt.Errorf("build context exceeds maximum total size %d bytes", limits.MaxBytes)
		}
		totalBytes += info.Size()

		header.Mode = int64(info.Mode().Perm())
		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}
		file, err := os.Open(filePath)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(tarWriter, file)
		return err
	})
}

// RollbackToState converges a service back to a saved takod deployment state.
func (d *Deployer) RollbackToState(serviceName string, serviceState *state.ServiceState) error {
	if d.sshPool == nil {
		return fmt.Errorf("ssh pool not initialized")
	}
	if strings.TrimSpace(serviceState.Image) == "" {
		return fmt.Errorf("deployment state for %s does not include an image", serviceName)
	}

	if d.verbose {
		fmt.Printf("  Rolling back service %s to image %s...\n", serviceName, serviceState.Image)
	}

	service, err := d.config.GetService(d.environment, serviceName)
	if err != nil {
		return fmt.Errorf("failed to load service config for rollback: %w", err)
	}

	rollbackService := *service
	rollbackService.Image = serviceState.Image
	rollbackService.Replicas = serviceState.Replicas
	if serviceState.Port > 0 {
		rollbackService.Port = serviceState.Port
	}

	return d.DeployServiceTakod(serviceName, &rollbackService, serviceState.Image)
}

// BuildImage builds a Docker image for a service without deploying it
func (d *Deployer) BuildImage(serviceName string, service *config.ServiceConfig) (string, error) {
	// Get full image name from config with environment
	fullImageName := d.config.GetFullImageName(serviceName, d.environment)

	if service.Build != "" {
		// Use service.Build as the build context path
		contextPath := service.Build

		if service.Dockerfile != "" {
			dockerfilePath := filepath.Join(contextPath, filepath.Clean(service.Dockerfile))
			if _, err := os.Stat(dockerfilePath); err != nil {
				return "", fmt.Errorf("dockerfile does not exist in build context: %s", service.Dockerfile)
			}
			if d.verbose {
				fmt.Printf("  Found Dockerfile: %s\n", service.Dockerfile)
			}
		} else {
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

				if err := detector.GenerateDockerfile(); err != nil {
					return "", fmt.Errorf("failed to generate Dockerfile: %w", err)
				}
			}
		}

		if d.verbose {
			fmt.Printf("  Streaming build context to takod...\n")
			fmt.Printf("  Context path: %s\n", contextPath)
		}

		absContextPath, err := filepath.Abs(contextPath)
		if err != nil {
			return "", fmt.Errorf("failed to get absolute path: %w", err)
		}

		archivePath := filepath.Join(os.TempDir(), fmt.Sprintf("tako-build-%s-%d.tar.gz", serviceName, time.Now().UnixNano()))
		if err := createCrossPlatformTarGz(absContextPath, archivePath); err != nil {
			return "", fmt.Errorf("failed to create build context archive: %w", err)
		}
		defer os.Remove(archivePath)

		archive, err := os.Open(archivePath)
		if err != nil {
			return "", fmt.Errorf("failed to open build context archive: %w", err)
		}
		defer archive.Close()

		output, err := takodclient.StreamRequest(d.client, d.takodSocket(), "POST", takodclient.ImageBuildEndpoint(fullImageName, service.Dockerfile), archive)
		var response takod.ImageBuildResponse
		if err == nil {
			if decodeErr := json.Unmarshal([]byte(output), &response); decodeErr != nil {
				err = fmt.Errorf("failed to parse takod build response: %w", decodeErr)
			}
		}
		buildOutput := response.Output
		if buildOutput == "" {
			buildOutput = output
		}
		if d.verbose || err != nil {
			lines := strings.Split(buildOutput, "\n")
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
			return "", fmt.Errorf("failed to build Docker image through takod: %w", err)
		}

		if d.verbose {
			fmt.Printf("  ✓ Image built and verified: %s\n", fullImageName)
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
