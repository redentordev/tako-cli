package deployer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
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
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

// streamWriter wraps an io.Writer with a prefix for each line
type streamWriter struct {
	prefix string
	writer io.Writer
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
		if sw.writer == nil {
			sw.writer = os.Stdout
		}
		if _, err := fmt.Fprint(sw.writer, sw.prefix+line); err != nil {
			return n, err
		}
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
	targetServers     []string
	cliVersion        string
	skipBuild         bool
	meshPortCache     map[meshUpstreamPortKey]int
	meshPortCacheMu   sync.Mutex
	meshPortAllocator func(serverName string, serviceName string, revision string, slot int, containerPort int) (int, error)
	localImageClient  localImageClient
	output            io.Writer
	outputMu          sync.Mutex
	events            events.Sink
	releaseRuns       map[string]*ReleaseRun
	releaseMu         sync.Mutex
	jobImages         map[string]string
	jobMu             sync.Mutex
	baseCtx           context.Context
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
		client:      client,
		config:      cfg,
		environment: environment,
		verbose:     verbose,
		sshPool:     sshPool,
	}
}

// SetBaseContext threads the caller's context into long-running deployer
// streams (image builds, build-context archive uploads, service reconcile
// pulls) so cancelling a deploy interrupts in-flight remote work instead of
// waiting for it to finish.
func (d *Deployer) SetBaseContext(ctx context.Context) {
	d.baseCtx = ctx
}

func (d *Deployer) baseContext() context.Context {
	if d.baseCtx != nil {
		return d.baseCtx
	}
	return context.Background()
}

func (d *Deployer) SetCLIVersion(version string) {
	d.cliVersion = strings.TrimSpace(version)
}

func (d *Deployer) SetSkipBuild(skip bool) {
	d.skipBuild = skip
}

// SetOutput redirects deployer progress output. Passing nil resets output to os.Stdout.
func (d *Deployer) SetOutput(w io.Writer) {
	d.outputMu.Lock()
	defer d.outputMu.Unlock()
	if w == nil {
		d.output = os.Stdout
		return
	}
	d.output = w
}

func (d *Deployer) outputWriter() io.Writer {
	if d.output == nil {
		return os.Stdout
	}
	return d.output
}

func (d *Deployer) printf(format string, args ...any) {
	d.outputMu.Lock()
	defer d.outputMu.Unlock()
	fmt.Fprintf(d.outputWriter(), format, args...)
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
		d.printf("  Rolling back service %s to image %s...\n", serviceName, serviceState.Image)
	}

	service, err := d.config.GetService(d.environment, serviceName)
	if err != nil {
		return fmt.Errorf("failed to load service config for rollback: %w", err)
	}

	rollbackService := *service
	options := takodRollbackDeployOptionsForService(service)
	rollbackService.Image = serviceState.Image
	rollbackService.Replicas = serviceState.Replicas
	if serviceState.Port > 0 {
		rollbackService.Port = serviceState.Port
	}

	return d.deployServiceTakod(serviceName, &rollbackService, serviceState.Image, options)
}

// BuildImage builds a Docker image for a service without deploying it.
func (d *Deployer) BuildImage(serviceName string, service *config.ServiceConfig, imageRef ...string) (string, error) {
	return d.buildImageWithClient(d.client, serviceName, service, imageRef...)
}

func (d *Deployer) buildImageOnNode(serverName string, serviceName string, service *config.ServiceConfig, imageRef ...string) (string, error) {
	client, err := d.getEnvironmentClient(serverName)
	if err != nil {
		return "", err
	}
	return d.buildImageWithClient(client, serviceName, service, imageRef...)
}

type preparedBuildContext struct {
	ContextPath    string
	AbsContextPath string
	Dockerfile     string
}

func (d *Deployer) prepareBuildContext(service *config.ServiceConfig) (*preparedBuildContext, error) {
	if service == nil || service.Build == "" {
		return nil, fmt.Errorf("service build context is required")
	}

	contextPath := service.Build
	if service.Dockerfile != "" {
		dockerfilePath := filepath.Join(contextPath, filepath.Clean(service.Dockerfile))
		if _, err := os.Stat(dockerfilePath); err != nil {
			return nil, fmt.Errorf("dockerfile does not exist in build context: %s", service.Dockerfile)
		}
		if d.verbose {
			d.printf("  Found Dockerfile: %s\n", service.Dockerfile)
		}
	} else {
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
					d.printf("  Found Dockerfile: %s\n", filepath.Base(path))
				}
				break
			}
		}

		if !hasDockerfile {
			if d.verbose {
				d.printf("  No Dockerfile found - using Nixpacks auto-detection...\n")
			}

			detector := nixpacks.NewDetector(contextPath, d.verbose)
			framework, err := detector.DetectFramework()
			if err != nil {
				return nil, fmt.Errorf("failed to detect framework: %w\nHint: Either add a Dockerfile or ensure your project has recognizable framework files (package.json, go.mod, etc.)", err)
			}

			if d.verbose {
				d.printf("  Detected framework: %s\n", framework)
			}

			if err := detector.GenerateDockerfile(); err != nil {
				return nil, fmt.Errorf("failed to generate Dockerfile: %w", err)
			}
		}
	}

	absContextPath, err := filepath.Abs(contextPath)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path: %w", err)
	}

	return &preparedBuildContext{
		ContextPath:    contextPath,
		AbsContextPath: absContextPath,
		Dockerfile:     service.Dockerfile,
	}, nil
}

func (d *Deployer) buildImageWithClient(client *ssh.Client, serviceName string, service *config.ServiceConfig, imageRef ...string) (string, error) {
	if client == nil {
		return "", fmt.Errorf("ssh client is required")
	}

	fullImageName := d.config.GetFullImageName(serviceName, d.environment)
	if len(imageRef) > 0 && strings.TrimSpace(imageRef[0]) != "" {
		fullImageName = strings.TrimSpace(imageRef[0])
	}

	if service.Build != "" {
		prepared, err := d.prepareBuildContext(service)
		if err != nil {
			return "", err
		}

		if d.verbose {
			d.printf("  Streaming build context to takod...\n")
			d.printf("  Context path: %s\n", prepared.ContextPath)
		}

		archivePath := filepath.Join(os.TempDir(), fmt.Sprintf("tako-build-%s-%d.tar.gz", serviceName, time.Now().UnixNano()))
		archiveStart := time.Now()
		if err := createCrossPlatformTarGz(prepared.AbsContextPath, archivePath); err != nil {
			return "", fmt.Errorf("failed to create build context archive: %w", err)
		}
		archiveDuration := time.Since(archiveStart)
		defer os.Remove(archivePath)

		archiveInfo, err := os.Stat(archivePath)
		if err != nil {
			return "", fmt.Errorf("failed to stat build context archive: %w", err)
		}
		if d.verbose {
			d.printf("  Build context archive: %s created in %s\n", formatBuildBytes(archiveInfo.Size()), formatBuildDuration(archiveDuration))
		}

		archive, err := os.Open(archivePath)
		if err != nil {
			return "", fmt.Errorf("failed to open build context archive: %w", err)
		}
		defer archive.Close()

		endpoint := takodclient.ImageBuildEndpoint(fullImageName, service.Dockerfile)
		body := io.Reader(archive)
		if auths := d.registryAuths(); len(auths) > 0 {
			// Credentials ride the body as a JSON preamble line ahead of
			// the tar stream — never the URL or argv (ADR 10).
			preamble, err := json.Marshal(map[string]any{"registryAuths": auths})
			if err != nil {
				return "", fmt.Errorf("failed to encode registry auth preamble: %w", err)
			}
			endpoint = takodclient.ImageBuildEndpointWithAuth(fullImageName, service.Dockerfile)
			body = io.MultiReader(bytes.NewReader(append(preamble, '\n')), archive)
		}

		streamStart := time.Now()
		output, err := takodclient.StreamRequestWithContext(d.baseContext(), client, d.takodSocket(), "POST", endpoint, body)
		streamDuration := time.Since(streamStart)
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
			d.printf("  Build output (last 30 lines):\n")
			for _, line := range lines[start:] {
				if line != "" {
					d.printf("    %s\n", line)
				}
			}
		}

		if err != nil {
			return "", d.wrapRegistryAuthError("", fmt.Errorf("failed to build Docker image through takod: %w", err))
		}

		if d.verbose {
			d.printf("  Remote build request: %s\n", formatBuildDuration(streamDuration))
			if response.Timings != nil {
				d.printf("  Remote build timings: extract=%s docker=%s total=%s\n",
					formatBuildDuration(time.Duration(response.Timings.ExtractMS)*time.Millisecond),
					formatBuildDuration(time.Duration(response.Timings.DockerBuildMS)*time.Millisecond),
					formatBuildDuration(time.Duration(response.Timings.TotalMS)*time.Millisecond),
				)
			}
			d.printf("  ✓ Image built and verified: %s\n", fullImageName)
		}

	} else if service.Image != "" {
		// Service uses pre-built image (e.g., postgres, redis)
		if d.verbose {
			d.printf("  Using pre-built image: %s\n", service.Image)
		}
		fullImageName = service.Image
	}

	return fullImageName, nil
}

func formatBuildDuration(duration time.Duration) string {
	if duration < time.Second {
		return fmt.Sprintf("%dms", duration.Milliseconds())
	}
	return duration.Round(100 * time.Millisecond).String()
}

func formatBuildBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	value := float64(size)
	for _, suffix := range []string{"KiB", "MiB", "GiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f TiB", value/unit)
}
