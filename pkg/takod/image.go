package takod

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

var imageTransferMu sync.Mutex

type ImageExistsResponse struct {
	Image  string `json:"image"`
	Exists bool   `json:"exists"`
}

// ImageDescriptor is the immutable evidence used for reuse and transfer.
// ImageID is Docker's config digest; a mutable tag alone is never sufficient.
// DaemonID distinguishes two Docker stores even when they expose equal tags.
type ImageDescriptor struct {
	Image        string   `json:"image"`
	Exists       bool     `json:"exists"`
	ImageID      string   `json:"imageId,omitempty"`
	RepoDigests  []string `json:"repoDigests,omitempty"`
	OS           string   `json:"os,omitempty"`
	Architecture string   `json:"architecture,omitempty"`
	Variant      string   `json:"variant,omitempty"`
	DaemonID     string   `json:"daemonId,omitempty"`
}

type PlatformResponse struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
	DaemonID     string `json:"daemonId"`
}

type ImageImportResponse struct {
	Image      string           `json:"image"`
	Output     string           `json:"output,omitempty"`
	Descriptor *ImageDescriptor `json:"descriptor,omitempty"`
}

type ImageBuildResponse struct {
	Image   string             `json:"image"`
	Output  string             `json:"output,omitempty"`
	Timings *ImageBuildTimings `json:"timings,omitempty"`
}

type ImageBuildTimings struct {
	ExtractMS     int64 `json:"extractMs,omitempty"`
	DockerBuildMS int64 `json:"dockerBuildMs,omitempty"`
	TotalMS       int64 `json:"totalMs,omitempty"`
}

type ImageBuildOptions struct {
	Dockerfile string
	BuildArgs  map[string]string
	Target     string
}

const (
	defaultBuildContextMaxBytes     int64 = 2 << 30
	defaultBuildContextMaxFileBytes int64 = 1 << 30
	defaultBuildContextMaxEntries         = 200000
	defaultImageImportMaxBytes      int64 = 8 << 30
	maxImageRefLength                     = 512
)

func validSHA256Digest(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

type buildContextLimits struct {
	MaxBytes     int64
	MaxFileBytes int64
	MaxEntries   int
}

func ImageExists(ctx context.Context, image string) (*ImageExistsResponse, error) {
	if err := validateImageName(image); err != nil {
		return nil, err
	}
	_, err := runDocker(ctx, "image", "inspect", image)
	return &ImageExistsResponse{Image: image, Exists: err == nil}, nil
}

// InspectImage returns exact image, platform, and daemon identity. A missing
// image is a successful Exists=false result; malformed daemon output fails
// closed so callers cannot fall back to tag-only reuse.
func InspectImage(ctx context.Context, image string) (*ImageDescriptor, error) {
	if err := validateImageName(image); err != nil {
		return nil, err
	}
	output, err := runDocker(ctx, "image", "inspect", image)
	if err != nil {
		if _, infoErr := dockerDaemonID(ctx); infoErr != nil {
			return nil, fmt.Errorf("inspect Docker image %s: %w", image, infoErr)
		}
		return &ImageDescriptor{Image: image, Exists: false}, nil
	}
	var inspected []struct {
		ID           string   `json:"Id"`
		RepoDigests  []string `json:"RepoDigests"`
		OS           string   `json:"Os"`
		Architecture string   `json:"Architecture"`
		Variant      string   `json:"Variant"`
	}
	if err := json.Unmarshal([]byte(output), &inspected); err != nil {
		return nil, fmt.Errorf("decode Docker image inspection for %s: %w", image, err)
	}
	if len(inspected) != 1 {
		return nil, fmt.Errorf("Docker image inspection for %s returned %d records", image, len(inspected))
	}
	entry := inspected[0]
	entry.ID = strings.ToLower(strings.TrimSpace(entry.ID))
	entry.OS = strings.ToLower(strings.TrimSpace(entry.OS))
	entry.Architecture = strings.ToLower(strings.TrimSpace(entry.Architecture))
	entry.Variant = strings.ToLower(strings.TrimSpace(entry.Variant))
	if !validSHA256Digest(entry.ID) || entry.OS == "" || entry.Architecture == "" {
		return nil, fmt.Errorf("Docker image inspection for %s omitted immutable digest or platform", image)
	}
	daemonID, err := dockerDaemonID(ctx)
	if err != nil {
		return nil, err
	}
	repoDigests := append([]string(nil), entry.RepoDigests...)
	for index := range repoDigests {
		repoDigests[index] = strings.TrimSpace(repoDigests[index])
	}
	sort.Strings(repoDigests)
	return &ImageDescriptor{
		Image: image, Exists: true, ImageID: entry.ID, RepoDigests: repoDigests,
		OS: entry.OS, Architecture: entry.Architecture, Variant: entry.Variant, DaemonID: daemonID,
	}, nil
}

func dockerDaemonID(ctx context.Context) (string, error) {
	dockerInfo, err := runDocker(ctx, "info", "--format", "{{json .ID}}")
	if err != nil {
		return "", fmt.Errorf("read Docker daemon identity: %w", err)
	}
	var daemonID string
	if err := json.Unmarshal([]byte(strings.TrimSpace(dockerInfo)), &daemonID); err != nil {
		return "", fmt.Errorf("decode Docker daemon identity: %w", err)
	}
	daemonID = strings.TrimSpace(daemonID)
	if daemonID == "" || len(daemonID) > 256 || strings.ContainsAny(daemonID, "\x00\r\n") {
		return "", fmt.Errorf("Docker daemon identity is empty or invalid")
	}
	return daemonID, nil
}

// ReadPlatform reports the Docker server platform rather than the CLI
// process architecture, which may differ when DOCKER_HOST is configured.
func ReadPlatform(ctx context.Context) (*PlatformResponse, error) {
	output, err := runDocker(ctx, "info", "--format", "{{json .}}")
	if err != nil {
		return nil, fmt.Errorf("read Docker platform: %w", err)
	}
	var info struct {
		ID           string `json:"ID"`
		OSType       string `json:"OSType"`
		Architecture string `json:"Architecture"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &info); err != nil {
		return nil, fmt.Errorf("decode Docker platform: %w", err)
	}
	info.ID = strings.TrimSpace(info.ID)
	info.OSType = strings.ToLower(strings.TrimSpace(info.OSType))
	info.Architecture = strings.ToLower(strings.TrimSpace(info.Architecture))
	if info.ID == "" || info.OSType == "" || info.Architecture == "" {
		return nil, fmt.Errorf("Docker platform response omitted daemon identity, OS, or architecture")
	}
	return &PlatformResponse{OS: info.OSType, Architecture: info.Architecture, DaemonID: info.ID}, nil
}

func ExportImage(ctx context.Context, image string, w io.Writer) error {
	if err := validateImageName(image); err != nil {
		return err
	}
	cmd := dockerCommandContext(ctx, "docker", "save", image)
	cmd.Stdout = w
	stderr := newCappedOutputBuffer(defaultCommandOutputMaxBytes)
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to export image %s: %w, output: %s", image, err, stderr.String())
	}
	return nil
}

func ImportImage(ctx context.Context, image string, r io.Reader, expectedImageIDs ...string) (*ImageImportResponse, error) {
	if err := validateImageName(image); err != nil {
		return nil, err
	}
	expectedImageID := ""
	if len(expectedImageIDs) > 0 {
		expectedImageID = strings.ToLower(strings.TrimSpace(expectedImageIDs[0]))
		if expectedImageID != "" && !validSHA256Digest(expectedImageID) {
			return nil, fmt.Errorf("expected image ID must be a sha256 digest")
		}
	}
	imageTransferMu.Lock()
	defer imageTransferMu.Unlock()
	previousImageID := ""
	previous, err := InspectImage(ctx, image)
	if err != nil {
		return nil, fmt.Errorf("inspect existing image %s before import: %w", image, err)
	}
	if previous.Exists {
		previousImageID = previous.ImageID
	}
	fail := func(primary error) (*ImageImportResponse, error) {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if rollbackErr := restoreImportedImageTag(rollbackCtx, image, previousImageID); rollbackErr != nil {
			return nil, fmt.Errorf("%v; image tag rollback failed: %w", primary, rollbackErr)
		}
		return nil, primary
	}
	r = newMaxBytesReader(r, defaultImageImportMaxBytes, "image import")
	cmd := dockerCommandContext(ctx, "docker", "load")
	cmd.Stdin = r
	output := newCappedOutputBuffer(defaultCommandOutputMaxBytes)
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Run(); err != nil {
		return fail(fmt.Errorf("failed to import image %s: %w, output: %s", image, err, output.String()))
	}
	var loaded *ImageDescriptor
	if expectedImageID != "" {
		loaded, err = InspectImage(ctx, expectedImageID)
		if err != nil || !loaded.Exists || loaded.ImageID != expectedImageID {
			if err != nil {
				return fail(fmt.Errorf("inspect expected imported image %s: %w", expectedImageID, err))
			}
			return fail(fmt.Errorf("imported archive did not contain expected image %s", expectedImageID))
		}
		if _, err := runDocker(ctx, "tag", expectedImageID, image); err != nil {
			return fail(fmt.Errorf("publish imported image %s as %s: %w", expectedImageID, image, err))
		}
	}
	if _, err := runDocker(ctx, "image", "inspect", image); err != nil {
		return fail(fmt.Errorf("imported image %s is not inspectable: %w", image, err))
	}
	descriptor, err := InspectImage(ctx, image)
	if err != nil {
		return fail(fmt.Errorf("inspect imported image %s: %w", image, err))
	}
	if err := validateImportedPublication(image, expectedImageID, loaded, descriptor); err != nil {
		return fail(err)
	}
	return &ImageImportResponse{Image: image, Output: strings.TrimSpace(output.String()), Descriptor: descriptor}, nil
}

func validateImportedPublication(image string, expectedImageID string, loaded *ImageDescriptor, published *ImageDescriptor) error {
	if published == nil || !published.Exists {
		return fmt.Errorf("imported image %s disappeared before publication completed", image)
	}
	if expectedImageID == "" {
		return nil
	}
	if loaded == nil {
		return fmt.Errorf("expected imported image %s has no verified descriptor", expectedImageID)
	}
	if published.ImageID != expectedImageID || published.OS != loaded.OS || published.Architecture != loaded.Architecture || published.Variant != loaded.Variant {
		return fmt.Errorf("published image %s changed during import: got %s %s/%s/%s, expected %s %s/%s/%s", image,
			published.ImageID, published.OS, published.Architecture, published.Variant,
			expectedImageID, loaded.OS, loaded.Architecture, loaded.Variant)
	}
	return nil
}

func restoreImportedImageTag(ctx context.Context, image string, previousImageID string) error {
	if previousImageID != "" {
		if _, err := runDocker(ctx, "tag", previousImageID, image); err != nil {
			return err
		}
		restored, err := InspectImage(ctx, image)
		if err != nil {
			return err
		}
		if !restored.Exists || restored.ImageID != previousImageID {
			return fmt.Errorf("restored tag points to %s instead of %s", restored.ImageID, previousImageID)
		}
		return nil
	}
	removeErr := error(nil)
	if _, err := runDocker(ctx, "image", "rm", image); err != nil {
		removeErr = err
	}
	restored, inspectErr := InspectImage(ctx, image)
	if inspectErr != nil {
		return inspectErr
	}
	if restored.Exists {
		if removeErr != nil {
			return fmt.Errorf("remove imported tag: %w", removeErr)
		}
		return fmt.Errorf("imported tag still exists after rollback")
	}
	return nil
}

type maxBytesReader struct {
	reader      io.Reader
	remaining   int64
	maxBytes    int64
	description string
}

func newMaxBytesReader(reader io.Reader, maxBytes int64, description string) io.Reader {
	if maxBytes <= 0 {
		return reader
	}
	return &maxBytesReader{
		reader:      reader,
		remaining:   maxBytes,
		maxBytes:    maxBytes,
		description: description,
	}
}

func (r *maxBytesReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		var one [1]byte
		n, err := r.reader.Read(one[:])
		if n > 0 {
			return 0, fmt.Errorf("%s exceeds maximum size %d bytes", r.description, r.maxBytes)
		}
		if err == nil {
			return 0, fmt.Errorf("%s reader made no progress after maximum size %d bytes", r.description, r.maxBytes)
		}
		return 0, err
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	return n, err
}

func BuildImage(ctx context.Context, image string, r io.Reader, dockerfile ...string) (*ImageBuildResponse, error) {
	return BuildImageWithAuth(ctx, image, r, nil, dockerfile...)
}

// BuildImageWithAuth builds like BuildImage; auths feed an ephemeral
// DOCKER_CONFIG so the daemon can pull private base images during FROM.
func BuildImageWithAuth(ctx context.Context, image string, r io.Reader, auths []RegistryAuth, dockerfile ...string) (*ImageBuildResponse, error) {
	options := ImageBuildOptions{}
	if len(dockerfile) > 0 {
		options.Dockerfile = dockerfile[0]
	}
	return BuildImageWithOptions(ctx, image, r, auths, options)
}

// BuildImageWithOptions builds an uploaded context with validated target and
// build args. Values arrive in the request body preamble, never the URL.
func BuildImageWithOptions(ctx context.Context, image string, r io.Reader, auths []RegistryAuth, options ImageBuildOptions) (*ImageBuildResponse, error) {
	totalStart := time.Now()
	if err := validateImageName(image); err != nil {
		return nil, err
	}
	if err := validateRegistryAuths(auths); err != nil {
		return nil, err
	}
	dockerfilePath := strings.TrimSpace(options.Dockerfile)
	if dockerfilePath != "" {
		if err := validateDockerfilePath(dockerfilePath); err != nil {
			return nil, err
		}
	}
	if err := validateImageBuildOptions(options); err != nil {
		return nil, err
	}
	r = newMaxBytesReader(r, defaultBuildContextMaxBytes, "build context upload")
	buildDir, err := os.MkdirTemp("", "tako-build-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create build directory: %w", err)
	}
	defer os.RemoveAll(buildDir)

	extractStart := time.Now()
	if err := extractTarGz(r, buildDir); err != nil {
		return nil, err
	}
	extractDuration := time.Since(extractStart)
	// Mutable image tags are published by builds and imports. Serialize the
	// Docker build through final inspection with image transfers so a transfer
	// cannot attest one ID and publish another under the same tag.
	imageTransferMu.Lock()
	defer imageTransferMu.Unlock()

	args := []string{"build", "-t", image}
	if dockerfilePath != "" {
		if _, err := safeArchiveTarget(buildDir, dockerfilePath); err != nil {
			return nil, err
		}
		if _, err := os.Stat(filepath.Join(buildDir, filepath.Clean(dockerfilePath))); os.IsNotExist(err) {
			return nil, fmt.Errorf("dockerfile does not exist in build context: %s", dockerfilePath)
		}
		args = append(args, "-f", dockerfilePath)
	}
	keys := make([]string, 0, len(options.BuildArgs))
	for key := range options.BuildArgs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--build-arg", key+"="+options.BuildArgs[key])
	}
	if options.Target != "" {
		args = append(args, "--target", options.Target)
	}
	args = append(args, ".")
	cmd := dockerCommandContext(ctx, "docker", args...)
	cmd.Dir = buildDir
	if len(auths) > 0 {
		authDir, cleanupAuth, err := writeEphemeralDockerConfig(auths)
		if err != nil {
			return nil, err
		}
		defer cleanupAuth()
		cmd.Env = dockerAuthEnv(authDir)
	}
	output := newCappedOutputBuffer(defaultCommandOutputMaxBytes)
	cmd.Stdout = output
	cmd.Stderr = output
	buildStart := time.Now()
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("failed to build image %s: %w, output: %s", image, err, annotateRegistryAuthFailure(annotateDockerBuildFailure(output.String())))
	}
	buildDuration := time.Since(buildStart)
	if _, err := runDocker(ctx, "image", "inspect", image); err != nil {
		return nil, fmt.Errorf("built image %s is not inspectable: %w", image, err)
	}
	return &ImageBuildResponse{
		Image:  image,
		Output: strings.TrimSpace(output.String()),
		Timings: &ImageBuildTimings{
			ExtractMS:     extractDuration.Milliseconds(),
			DockerBuildMS: buildDuration.Milliseconds(),
			TotalMS:       time.Since(totalStart).Milliseconds(),
		},
	}, nil
}

func validateImageBuildOptions(options ImageBuildOptions) error {
	if len(options.BuildArgs) > 128 {
		return fmt.Errorf("build args exceed maximum count 128")
	}
	total := 0
	for key, value := range options.BuildArgs {
		if key == "" || !isBuildArgName(key) {
			return fmt.Errorf("invalid build arg name %q", key)
		}
		if strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("build arg %s contains unsupported control characters", key)
		}
		total += len(key) + len(value)
	}
	if total > 64*1024 {
		return fmt.Errorf("build args exceed maximum size 65536")
	}
	if options.Target != "" {
		if len(options.Target) > 128 || strings.HasPrefix(options.Target, "-") {
			return fmt.Errorf("invalid build target")
		}
		for _, r := range options.Target {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == '-') {
				return fmt.Errorf("invalid build target")
			}
		}
	}
	return nil
}

func isBuildArgName(value string) bool {
	for index, r := range value {
		if index == 0 && !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_') {
			return false
		}
		if index > 0 && !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_') {
			return false
		}
	}
	return value != ""
}

func annotateDockerBuildFailure(output string) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return output
	}
	hint := dockerBuildFailureHint(output)
	if hint == "" || strings.Contains(output, hint) {
		return output
	}
	return output + "\n\nHint: " + hint
}

func dockerBuildFailureHint(output string) string {
	lower := strings.ToLower(output)
	buildKitMissing := strings.Contains(lower, "buildkit") || strings.Contains(lower, "buildx")
	buildKitOnlySyntax := strings.Contains(lower, "requires buildkit") ||
		strings.Contains(lower, "dockerfile frontend") ||
		strings.Contains(lower, "unknown flag: chmod")
	if buildKitMissing || buildKitOnlySyntax {
		return "the remote Docker builder cannot handle this BuildKit-dependent Dockerfile. Install/repair Docker buildx on the node, or replace BuildKit-only syntax such as COPY --chmod with portable RUN chmod steps."
	}
	return ""
}

func validateDockerfilePath(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("dockerfile path is required")
	}
	if filepath.IsAbs(path) {
		return fmt.Errorf("dockerfile path must be relative to the build context")
	}
	if strings.ContainsAny(path, "\x00\r\n") {
		return fmt.Errorf("dockerfile path contains unsupported characters")
	}
	cleaned := filepath.Clean(path)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf("dockerfile path must stay inside the build context")
	}
	return nil
}

func extractTarGz(r io.Reader, destDir string) error {
	return extractTarGzWithLimits(r, destDir, buildContextLimits{
		MaxBytes:     defaultBuildContextMaxBytes,
		MaxFileBytes: defaultBuildContextMaxFileBytes,
		MaxEntries:   defaultBuildContextMaxEntries,
	})
}

func extractTarGzWithLimits(r io.Reader, destDir string, limits buildContextLimits) error {
	gzipReader, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("failed to read build context gzip stream: %w", err)
	}
	defer gzipReader.Close()

	tarReader := tar.NewReader(gzipReader)
	var totalBytes int64
	entries := 0
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("failed to read build context tar stream: %w", err)
		}
		entries++
		if limits.MaxEntries > 0 && entries > limits.MaxEntries {
			return fmt.Errorf("build context exceeds maximum entry count %d", limits.MaxEntries)
		}

		target, err := safeArchiveTarget(destDir, header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return fmt.Errorf("failed to create build context directory %s: %w", header.Name, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 {
				return fmt.Errorf("build context file %s has invalid size", header.Name)
			}
			if limits.MaxFileBytes > 0 && header.Size > limits.MaxFileBytes {
				return fmt.Errorf("build context file %s exceeds maximum size %d bytes", header.Name, limits.MaxFileBytes)
			}
			if limits.MaxBytes > 0 && totalBytes+header.Size > limits.MaxBytes {
				return fmt.Errorf("build context exceeds maximum total size %d bytes", limits.MaxBytes)
			}
			totalBytes += header.Size

			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("failed to create build context parent for %s: %w", header.Name, err)
			}
			mode := header.FileInfo().Mode().Perm()
			if mode == 0 {
				mode = 0644
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return fmt.Errorf("failed to create build context file %s: %w", header.Name, err)
			}
			_, copyErr := io.Copy(file, tarReader)
			closeErr := file.Close()
			if copyErr != nil {
				return fmt.Errorf("failed to write build context file %s: %w", header.Name, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("failed to close build context file %s: %w", header.Name, closeErr)
			}
		default:
			return fmt.Errorf("unsupported build context entry %s", header.Name)
		}
	}
}

func safeArchiveTarget(destDir string, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("build context archive contains an empty path")
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("build context archive contains absolute path %s", name)
	}
	clean := filepath.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("build context archive path escapes root: %s", name)
	}
	destDir, err := filepath.Abs(destDir)
	if err != nil {
		return "", err
	}
	target := filepath.Join(destDir, clean)
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	if targetAbs != destDir && !strings.HasPrefix(targetAbs, destDir+string(filepath.Separator)) {
		return "", fmt.Errorf("build context archive path escapes root: %s", name)
	}
	return targetAbs, nil
}

func validateImageName(image string) error {
	if strings.TrimSpace(image) == "" {
		return fmt.Errorf("image is required")
	}
	if len(image) > maxImageRefLength {
		return fmt.Errorf("image exceeds maximum length %d", maxImageRefLength)
	}
	if strings.HasPrefix(image, "-") {
		return fmt.Errorf("image must not start with '-'")
	}
	for _, r := range image {
		if unicode.IsSpace(r) || r < 0x20 || r == 0x7f {
			return fmt.Errorf("image contains unsupported characters")
		}
	}
	return nil
}

func writeImageTarHeaders(w http.ResponseWriter, image string) {
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeImageArchiveName(image)+`.tar"`)
}

func sanitizeImageArchiveName(image string) string {
	var b strings.Builder
	for _, r := range image {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		return "image"
	}
	return name
}
