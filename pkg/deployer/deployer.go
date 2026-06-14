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
	"os/exec"
	"path"
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
	deployHooks       bool
	volumePlacements  map[string][]string
	volumeInspector   func(serverName string, volumeNames []string) (map[string]bool, error)
	meshPortCache     map[meshUpstreamPortKey]int
	meshPortCacheMu   sync.Mutex
	meshPortAllocator func(serverName string, serviceName string, slot int, containerPort int) (int, error)
	nodeInfoInspector func(serverName string) (*takod.NodeInfoResponse, error)
	generatedConfigs  map[string][]byte
	importResolver    func(alias string) ([]string, error)
}

const (
	defaultBuildContextArchiveMaxBytes     int64 = 2 << 30
	defaultBuildContextArchiveMaxFileBytes int64 = 1 << 30
	defaultBuildContextArchiveMaxEntries         = 200000
	buildContextGitArchiveTimeout                = 5 * time.Minute
)

type buildContextArchiveLimits struct {
	MaxBytes     int64
	MaxFileBytes int64
	MaxEntries   int
}

// NewDeployer creates a new deployer
func NewDeployer(client *ssh.Client, cfg *config.Config, environment string, verbose bool) *Deployer {
	return &Deployer{
		client:           client,
		config:           cfg,
		environment:      environment,
		verbose:          verbose,
		volumePlacements: make(map[string][]string),
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
		volumePlacements:  make(map[string][]string),
	}
}

func (d *Deployer) SetCLIVersion(version string) {
	d.cliVersion = strings.TrimSpace(version)
}

func (d *Deployer) SetDeployHooksEnabled(enabled bool) {
	d.deployHooks = enabled
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

func createCrossPlatformTarGzWithForcedIncludes(sourceDir, archivePath string, forcedIncludes []string) error {
	return createCrossPlatformTarGzWithOptions(sourceDir, archivePath, buildContextArchiveLimits{
		MaxBytes:     defaultBuildContextArchiveMaxBytes,
		MaxFileBytes: defaultBuildContextArchiveMaxFileBytes,
		MaxEntries:   defaultBuildContextArchiveMaxEntries,
	}, forcedIncludes)
}

func createCrossPlatformTarGzWithLimits(sourceDir, archivePath string, limits buildContextArchiveLimits) error {
	return createCrossPlatformTarGzWithOptions(sourceDir, archivePath, limits, nil)
}

func createCrossPlatformTarGzWithOptions(sourceDir, archivePath string, limits buildContextArchiveLimits, forcedIncludes []string) error {
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
	forced := map[string]bool{}
	for _, include := range forcedIncludes {
		include = strings.TrimSpace(include)
		if include == "" {
			continue
		}
		forced[filepath.ToSlash(filepath.Clean(include))] = true
	}

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

		relArchivePath := filepath.ToSlash(relPath)
		if ignoreParser.ShouldIgnore(relPath) && !forced[relArchivePath] {
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
		header.Name = relArchivePath

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

		header.Mode = normalizedBuildContextFileMode(info.Mode())
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

func createGitSourceTarGzWithForcedIncludes(sourceDir, archivePath string, forcedIncludes []string) error {
	contextDir, cleanup, err := createGitSourceContext(sourceDir, buildContextArchiveLimits{
		MaxBytes:     defaultBuildContextArchiveMaxBytes,
		MaxFileBytes: defaultBuildContextArchiveMaxFileBytes,
		MaxEntries:   defaultBuildContextArchiveMaxEntries,
	})
	if err != nil {
		return err
	}
	defer cleanup()
	return createCrossPlatformTarGzWithForcedIncludes(contextDir, archivePath, forcedIncludes)
}

func createGitSourceContext(sourceDir string, limits buildContextArchiveLimits) (string, func(), error) {
	sourceAbs, err := filepath.Abs(sourceDir)
	if err != nil {
		return "", nil, fmt.Errorf("failed to resolve build context path: %w", err)
	}
	if realSource, err := filepath.EvalSymlinks(sourceAbs); err == nil {
		sourceAbs = realSource
	}
	repoRoot, err := gitBuildOutput(sourceAbs, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", nil, fmt.Errorf("failed to locate git repository for build context: %w", err)
	}
	repoRootAbs, err := filepath.Abs(strings.TrimSpace(repoRoot))
	if err != nil {
		return "", nil, fmt.Errorf("failed to resolve git root: %w", err)
	}
	if realRoot, err := filepath.EvalSymlinks(repoRootAbs); err == nil {
		repoRootAbs = realRoot
	}
	relContext, err := filepath.Rel(repoRootAbs, sourceAbs)
	if err != nil {
		return "", nil, fmt.Errorf("failed to resolve build context relative to git root: %w", err)
	}
	if relContext == ".." || strings.HasPrefix(relContext, ".."+string(filepath.Separator)) || filepath.IsAbs(relContext) {
		return "", nil, fmt.Errorf("build context must be inside the git repository")
	}
	gitContext := filepath.ToSlash(filepath.Clean(relContext))
	if gitContext == "." {
		gitContext = ""
	}

	tempDir, err := os.MkdirTemp("", "tako-git-source-*")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create git build context: %w", err)
	}
	cleanup := func() {
		_ = os.RemoveAll(tempDir)
	}
	rawArchive := filepath.Join(tempDir, "source.tar")
	args := []string{"-C", repoRootAbs, "archive", "--format=tar", "--output", rawArchive, "HEAD"}
	if gitContext != "" {
		args = append(args, "--", gitContext)
	}
	if err := gitBuildRun(args...); err != nil {
		cleanup()
		if gitContext == "" {
			return "", nil, fmt.Errorf("failed to archive committed git source: %w", err)
		}
		return "", nil, fmt.Errorf("failed to archive committed git source for %s: %w", gitContext, err)
	}

	contextDir := filepath.Join(tempDir, "context")
	if err := os.MkdirAll(contextDir, 0755); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("failed to create extracted git context: %w", err)
	}
	if err := extractGitSourceContext(rawArchive, contextDir, gitContext, limits); err != nil {
		cleanup()
		return "", nil, err
	}
	return contextDir, cleanup, nil
}

func extractGitSourceContext(archivePath, destDir, stripPrefix string, limits buildContextArchiveLimits) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("failed to open committed git archive: %w", err)
	}
	defer file.Close()

	prefix := strings.Trim(strings.TrimSpace(stripPrefix), "/")
	tarReader := tar.NewReader(file)
	var totalBytes int64
	entries := 0
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("failed to read committed git archive: %w", err)
		}

		relPath, err := stripGitArchivePrefix(header.Name, prefix)
		if err != nil {
			return err
		}
		if relPath == "" {
			continue
		}
		entries++
		if limits.MaxEntries > 0 && entries > limits.MaxEntries {
			return fmt.Errorf("committed build context exceeds maximum entry count %d", limits.MaxEntries)
		}

		target, err := safeExtractTarget(destDir, relPath)
		if err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return fmt.Errorf("failed to create committed build context directory: %w", err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 {
				return fmt.Errorf("committed build context file %s has invalid size", relPath)
			}
			if limits.MaxFileBytes > 0 && header.Size > limits.MaxFileBytes {
				return fmt.Errorf("committed build context file %s exceeds maximum size %d bytes", relPath, limits.MaxFileBytes)
			}
			if limits.MaxBytes > 0 && totalBytes+header.Size > limits.MaxBytes {
				return fmt.Errorf("committed build context exceeds maximum total size %d bytes", limits.MaxBytes)
			}
			totalBytes += header.Size
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("failed to create committed build context parent: %w", err)
			}
			mode := os.FileMode(normalizedBuildContextFileMode(header.FileInfo().Mode()))
			if mode == 0 {
				mode = 0600
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return fmt.Errorf("failed to write committed build context file: %w", err)
			}
			_, copyErr := io.CopyN(out, tarReader, header.Size)
			closeErr := out.Close()
			if copyErr != nil {
				return fmt.Errorf("failed to extract committed build context file %s: %w", relPath, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("failed to close committed build context file %s: %w", relPath, closeErr)
			}
			if err := os.Chmod(target, mode); err != nil {
				return fmt.Errorf("failed to set committed build context file mode for %s: %w", relPath, err)
			}
		default:
			// Git archives can contain symlinks. Build contexts created by Tako only
			// preserve regular files and directories, so ignore other entry types.
			continue
		}
	}
}

func normalizedBuildContextFileMode(mode os.FileMode) int64 {
	if mode.Perm()&0111 != 0 {
		return 0755
	}
	return 0644
}

func stripGitArchivePrefix(name string, prefix string) (string, error) {
	clean := path.Clean(strings.TrimSpace(name))
	if clean == "." {
		return "", nil
	}
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("committed git archive path escapes root: %s", name)
	}
	if prefix != "" {
		if clean == prefix {
			return "", nil
		}
		prefixWithSlash := prefix + "/"
		if !strings.HasPrefix(clean, prefixWithSlash) {
			return "", fmt.Errorf("committed git archive path %s is outside build context %s", clean, prefix)
		}
		clean = strings.TrimPrefix(clean, prefixWithSlash)
	}
	if clean == "." {
		return "", nil
	}
	return clean, nil
}

func safeExtractTarget(destDir string, archiveName string) (string, error) {
	clean := path.Clean(archiveName)
	if clean == "." || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("committed build context path escapes root: %s", archiveName)
	}
	target := filepath.Join(destDir, filepath.FromSlash(clean))
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("failed to resolve committed build context path: %w", err)
	}
	destAbs, err := filepath.Abs(destDir)
	if err != nil {
		return "", fmt.Errorf("failed to resolve committed build context root: %w", err)
	}
	if targetAbs != destAbs && !strings.HasPrefix(targetAbs, destAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("committed build context path escapes root: %s", archiveName)
	}
	return targetAbs, nil
}

func gitBuildOutput(workDir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), buildContextGitArchiveTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = workDir
	output, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("git command timed out after %s: git %s", buildContextGitArchiveTimeout, strings.Join(args, " "))
	}
	if err != nil {
		return "", err
	}
	return string(output), nil
}

func gitBuildRun(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), buildContextGitArchiveTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("git command timed out after %s: git %s", buildContextGitArchiveTimeout, strings.Join(args, " "))
	}
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("git %s failed: %s", strings.Join(args, " "), detail)
	}
	return nil
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
		sourceContextPath := contextPath
		cleanupSourceContext := func() {}
		if d.config.DeploymentSource() == config.DeploymentSourceGit {
			gitContextPath, cleanup, err := createGitSourceContext(contextPath, buildContextArchiveLimits{
				MaxBytes:     defaultBuildContextArchiveMaxBytes,
				MaxFileBytes: defaultBuildContextArchiveMaxFileBytes,
				MaxEntries:   defaultBuildContextArchiveMaxEntries,
			})
			if err != nil {
				return "", fmt.Errorf("failed to prepare committed git build context: %w", err)
			}
			sourceContextPath = gitContextPath
			cleanupSourceContext = cleanup
			defer cleanupSourceContext()
		}

		dockerfilePath, hasDockerfile, err := resolveServiceDockerfile(sourceContextPath, service.Dockerfile)
		if err != nil {
			return "", fmt.Errorf("failed to resolve dockerfile: %w", err)
		}
		if hasDockerfile && d.verbose {
			fmt.Printf("  Found Dockerfile: %s\n", dockerfilePath)
		}

		if !hasDockerfile {
			// No Dockerfile - try to use Nixpacks
			if d.verbose {
				fmt.Printf("  No Dockerfile found - using Nixpacks auto-detection...\n")
			}

			detector := nixpacks.NewDetector(sourceContextPath, d.verbose)

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

			hasDockerfile = true
			dockerfilePath = "Dockerfile"
		}

		if d.verbose {
			fmt.Printf("  Streaming build context to takod...\n")
			fmt.Printf("  Context path: %s\n", contextPath)
			fmt.Printf("  Build source: %s\n", d.config.DeploymentSource())
			fmt.Printf("  Dockerfile: %s\n", dockerfilePath)
		}

		absContextPath, err := filepath.Abs(sourceContextPath)
		if err != nil {
			return "", fmt.Errorf("failed to get absolute path: %w", err)
		}

		archivePath := filepath.Join(os.TempDir(), fmt.Sprintf("tako-build-%s-%d.tar.gz", serviceName, time.Now().UnixNano()))
		if err := createCrossPlatformTarGzWithForcedIncludes(absContextPath, archivePath, []string{dockerfilePath}); err != nil {
			return "", fmt.Errorf("failed to create build context archive: %w", err)
		}
		defer os.Remove(archivePath)

		archive, err := os.Open(archivePath)
		if err != nil {
			return "", fmt.Errorf("failed to open build context archive: %w", err)
		}
		defer archive.Close()

		output, err := takodclient.StreamRequest(d.client, d.takodSocket(), "POST", d.imageBuildEndpoint(fullImageName, service, dockerfilePath), archive)
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

func resolveServiceDockerfile(contextPath string, dockerfile string) (string, bool, error) {
	if strings.TrimSpace(dockerfile) != "" {
		clean := filepath.Clean(strings.TrimSpace(dockerfile))
		if filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return "", false, fmt.Errorf("dockerfile must be a relative path inside the build context")
		}
		candidate := filepath.Join(contextPath, clean)
		info, err := os.Lstat(candidate)
		if err != nil {
			return "", false, fmt.Errorf("dockerfile does not exist: %s", dockerfile)
		}
		if info.IsDir() || !info.Mode().IsRegular() {
			return "", false, fmt.Errorf("dockerfile must be a regular file: %s", dockerfile)
		}
		return filepath.ToSlash(clean), true, nil
	}

	dockerfileCandidates := []string{
		"Dockerfile",
		"Dockerfile.prod",
		"dockerfile",
		".dockerfile",
	}
	for _, candidate := range dockerfileCandidates {
		path := filepath.Join(contextPath, candidate)
		info, err := os.Lstat(path)
		if err == nil && !info.IsDir() && info.Mode().IsRegular() {
			return filepath.ToSlash(candidate), true, nil
		}
	}
	return "", false, nil
}

func (d *Deployer) imageBuildEndpoint(image string, service *config.ServiceConfig, dockerfilePath ...string) string {
	dockerfile := service.Dockerfile
	if len(dockerfilePath) > 0 && strings.TrimSpace(dockerfilePath[0]) != "" {
		dockerfile = dockerfilePath[0]
	}
	opts := takodclient.ImageBuildEndpointOptions{
		Platform:   service.Platform,
		Dockerfile: dockerfile,
	}
	cache := d.config.DeploymentCache()
	if cache != nil && cache.Enabled {
		switch cache.Type {
		case "registry":
			ref := strings.TrimSpace(cache.Ref)
			if ref != "" {
				cacheSpec := "type=registry,ref=" + ref
				opts.CacheFrom = append(opts.CacheFrom, cacheSpec)
				opts.CacheTo = append(opts.CacheTo, cacheSpec+",mode=max")
				opts.Buildx = true
			}
		default:
			opts.CacheFrom = append(opts.CacheFrom, image)
		}
		if cache.Builder != "" {
			opts.Builder = cache.Builder
			opts.Buildx = true
		}
	}
	return takodclient.ImageBuildEndpointWithOptions(image, opts)
}
