package takod

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	dockerCommandContext = exec.CommandContext
)

const (
	maxHealthRetries      = 100
	maxHealthWaitAttempts = 600
	maxHealthFieldBytes   = 4096
	maxHealthDuration     = 24 * time.Hour
	maxDockerVolumeName   = 255
	maxConfigFiles        = 64
	maxConfigFileBytes    = 1 << 20
	defaultConfigDir      = "/var/lib/tako/configs"
	reconcileRecreate     = "recreate"
	reconcileRolling      = "rolling"
	rollingStartFirst     = "start-first"
	rollingStopFirst      = "stop-first"
)

type ReconcileServiceRequest struct {
	Project        string            `json:"project"`
	Environment    string            `json:"environment"`
	Service        string            `json:"service"`
	Image          string            `json:"image"`
	PullImage      bool              `json:"pullImage,omitempty"`
	RegistryAuth   *RegistryAuth     `json:"registryAuth,omitempty"`
	Restart        string            `json:"restart,omitempty"`
	Network        string            `json:"network"`
	NetworkAlias   string            `json:"networkAlias,omitempty"`
	NetworkAliases []string          `json:"networkAliases,omitempty"`
	EnvFile        string            `json:"envFile,omitempty"`
	EnvFileContent string            `json:"envFileContent,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	Mounts         []string          `json:"mounts,omitempty"`
	ConfigFiles    []ConfigFileMount `json:"configFiles,omitempty"`
	ConfigDir      string            `json:"-"`
	Containers     []ContainerSpec   `json:"containers"`
	Health         *HealthSpec       `json:"health,omitempty"`
	Command        string            `json:"command,omitempty"`
	Strategy       string            `json:"strategy,omitempty"`
	Order          string            `json:"order,omitempty"`
	MaxUnavailable int               `json:"maxUnavailable,omitempty"`
	Monitor        string            `json:"monitor,omitempty"`
}

type ConfigFileMount struct {
	Source        string `json:"source"`
	Target        string `json:"target"`
	Mode          string `json:"mode,omitempty"`
	ContentBase64 string `json:"contentBase64"`
	ContentHash   string `json:"contentHash,omitempty"`
}

type RemoveServiceRequest struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Service     string `json:"service"`
}

type ContainerSpec struct {
	Name           string   `json:"name"`
	Publishes      []string `json:"publishes,omitempty"`
	Slot           int      `json:"slot,omitempty"`
	NetworkAliases []string `json:"networkAliases,omitempty"`
}

type HealthSpec struct {
	Command      string `json:"command,omitempty"`
	Interval     string `json:"interval,omitempty"`
	Timeout      string `json:"timeout,omitempty"`
	Retries      int    `json:"retries,omitempty"`
	StartPeriod  string `json:"startPeriod,omitempty"`
	WaitAttempts int    `json:"waitAttempts,omitempty"`
}

type ReconcileServiceResponse struct {
	Project           string   `json:"project"`
	Environment       string   `json:"environment"`
	Service           string   `json:"service"`
	Containers        []string `json:"containers"`
	RemovedContainers int      `json:"removedContainers,omitempty"`
}

type RemoveServiceResponse struct {
	Project           string `json:"project"`
	Environment       string `json:"environment"`
	Service           string `json:"service"`
	RemovedContainers int    `json:"removedContainers"`
}

func ReconcileService(ctx context.Context, req ReconcileServiceRequest) (*ReconcileServiceResponse, error) {
	normalizeReconcileServiceRequest(&req)
	if err := validateReconcileServiceRequest(req); err != nil {
		return nil, err
	}
	if req.Strategy == reconcileRolling {
		return reconcileServiceRolling(ctx, req)
	}
	return reconcileServiceRecreate(ctx, req)
}

func normalizeReconcileServiceRequest(req *ReconcileServiceRequest) {
	if req.Restart == "" {
		req.Restart = "unless-stopped"
	}
	if req.NetworkAlias == "" {
		req.NetworkAlias = req.Service
	}
	if len(req.NetworkAliases) == 0 {
		req.NetworkAliases = []string{req.NetworkAlias}
	}
	if req.Strategy == "" {
		req.Strategy = reconcileRecreate
	}
	if req.Strategy == reconcileRolling && req.Order == "" {
		req.Order = rollingStartFirst
	}
}

func reconcileServiceRecreate(ctx context.Context, req ReconcileServiceRequest) (*ReconcileServiceResponse, error) {
	removedContainers, err := removeServiceContainers(ctx, req.Project, req.Environment, req.Service)
	if err != nil {
		return nil, err
	}
	if len(req.Containers) == 0 {
		return &ReconcileServiceResponse{
			Project:           req.Project,
			Environment:       req.Environment,
			Service:           req.Service,
			RemovedContainers: removedContainers,
		}, nil
	}
	if err := ensureDockerNetwork(ctx, req.Network, dockerNetworkOwner{
		Project:     req.Project,
		Environment: req.Environment,
	}); err != nil {
		return nil, err
	}
	if err := prepareServiceConfigFiles(&req); err != nil {
		return nil, err
	}
	if err := ensureServiceVolumes(ctx, req); err != nil {
		return nil, err
	}
	cleanupEnvFile, err := prepareServiceEnvFile(&req)
	if err != nil {
		return nil, err
	}
	if cleanupEnvFile != nil {
		defer cleanupEnvFile()
	}
	if req.PullImage {
		if _, err := pullImage(ctx, req.Image, req.RegistryAuth); err != nil {
			return nil, fmt.Errorf("failed to pull image %s: %w", req.Image, err)
		}
	}

	started := make([]string, 0, len(req.Containers))
	for _, container := range req.Containers {
		if err := runServiceContainer(ctx, req, container); err != nil {
			if cleanupErr := cleanupStartedContainers(started); cleanupErr != nil {
				return nil, fmt.Errorf("%w; additionally failed to clean up started containers: %v", err, cleanupErr)
			}
			return nil, err
		}
		started = append(started, container.Name)
		if err := waitForContainerHealthy(ctx, container.Name, req.Health); err != nil {
			if cleanupErr := cleanupStartedContainers(started); cleanupErr != nil {
				return nil, fmt.Errorf("%w; additionally failed to clean up started containers: %v", err, cleanupErr)
			}
			return nil, err
		}
	}

	return &ReconcileServiceResponse{
		Project:           req.Project,
		Environment:       req.Environment,
		Service:           req.Service,
		Containers:        started,
		RemovedContainers: removedContainers,
	}, nil
}

func reconcileServiceRolling(ctx context.Context, req ReconcileServiceRequest) (*ReconcileServiceResponse, error) {
	if err := ensureDockerNetwork(ctx, req.Network, dockerNetworkOwner{
		Project:     req.Project,
		Environment: req.Environment,
	}); err != nil {
		return nil, err
	}
	if err := prepareServiceConfigFiles(&req); err != nil {
		return nil, err
	}
	if err := ensureServiceVolumes(ctx, req); err != nil {
		return nil, err
	}
	cleanupEnvFile, err := prepareServiceEnvFile(&req)
	if err != nil {
		return nil, err
	}
	if cleanupEnvFile != nil {
		defer cleanupEnvFile()
	}
	if req.PullImage {
		if _, err := pullImage(ctx, req.Image, req.RegistryAuth); err != nil {
			return nil, fmt.Errorf("failed to pull image %s: %w", req.Image, err)
		}
	}

	existing, err := listServiceContainers(ctx, req.Project, req.Environment, req.Service)
	if err != nil {
		return nil, err
	}
	existingByName := make(map[string]serviceContainer, len(existing))
	for _, container := range existing {
		existingByName[container.Name] = container
	}
	desiredNames := make(map[string]bool, len(req.Containers))
	for _, container := range req.Containers {
		desiredNames[container.Name] = true
	}

	monitor, err := parseMonitorDuration(req.Monitor)
	if err != nil {
		return nil, err
	}

	started, removed, err := reconcileRollingDesiredContainers(ctx, req, existingByName, monitor)
	if err != nil {
		return nil, err
	}

	var extras []string
	for _, container := range existing {
		if !desiredNames[container.Name] {
			extras = append(extras, container.Name)
		}
	}
	if len(extras) > 0 {
		if err := removeContainersByName(ctx, extras); err != nil {
			return nil, err
		}
		removed += len(extras)
	}

	return &ReconcileServiceResponse{
		Project:           req.Project,
		Environment:       req.Environment,
		Service:           req.Service,
		Containers:        started,
		RemovedContainers: removed,
	}, nil
}

func reconcileRollingDesiredContainers(ctx context.Context, req ReconcileServiceRequest, existingByName map[string]serviceContainer, monitor time.Duration) ([]string, int, error) {
	if req.Order == rollingStopFirst && req.MaxUnavailable > 1 {
		return reconcileRollingStopFirstBatches(ctx, req, existingByName, monitor, req.MaxUnavailable)
	}

	started := make([]string, 0, len(req.Containers))
	removed := 0
	for _, container := range req.Containers {
		name, replaced, err := reconcileOneRollingContainer(ctx, req, container, existingByName, monitor)
		if err != nil {
			return nil, removed, err
		}
		removed += replaced
		started = append(started, name)
	}
	return started, removed, nil
}

type rollingContainerResult struct {
	index   int
	name    string
	removed int
	err     error
}

func reconcileRollingStopFirstBatches(ctx context.Context, req ReconcileServiceRequest, existingByName map[string]serviceContainer, monitor time.Duration, batchSize int) ([]string, int, error) {
	started := make([]string, 0, len(req.Containers))
	removed := 0
	for start := 0; start < len(req.Containers); start += batchSize {
		end := start + batchSize
		if end > len(req.Containers) {
			end = len(req.Containers)
		}
		batch := req.Containers[start:end]
		results := make(chan rollingContainerResult, len(batch))
		for index, container := range batch {
			go func(index int, container ContainerSpec) {
				name, replaced, err := reconcileOneRollingContainer(ctx, req, container, existingByName, monitor)
				results <- rollingContainerResult{index: index, name: name, removed: replaced, err: err}
			}(index, container)
		}

		batchResults := make([]rollingContainerResult, len(batch))
		for range batch {
			result := <-results
			if result.err != nil {
				return nil, removed, result.err
			}
			batchResults[result.index] = result
		}
		for _, result := range batchResults {
			removed += result.removed
			started = append(started, result.name)
		}
	}
	return started, removed, nil
}

func reconcileOneRollingContainer(ctx context.Context, req ReconcileServiceRequest, container ContainerSpec, existingByName map[string]serviceContainer, monitor time.Duration) (string, int, error) {
	oldContainer, hasOld := existingByName[container.Name]
	if !hasOld {
		if err := startServiceContainerAndWait(ctx, req, container, monitor); err != nil {
			return "", 0, err
		}
		return container.Name, 0, nil
	}

	replaced, err := replaceServiceContainerRolling(ctx, req, container, oldContainer, monitor)
	if err != nil {
		return "", 0, err
	}
	return container.Name, replaced, nil
}

func replaceServiceContainerRolling(ctx context.Context, req ReconcileServiceRequest, desired ContainerSpec, old serviceContainer, monitor time.Duration) (int, error) {
	if req.Order == rollingStopFirst {
		return replaceServiceContainerStopFirst(ctx, req, desired, old, monitor)
	}
	removed, err := replaceServiceContainerStartFirst(ctx, req, desired, old, monitor)
	if err == nil {
		return removed, nil
	}
	if !isPublishConflictError(err) {
		return 0, err
	}
	return replaceServiceContainerStopFirst(ctx, req, desired, old, monitor)
}

func replaceServiceContainerStartFirst(ctx context.Context, req ReconcileServiceRequest, desired ContainerSpec, old serviceContainer, monitor time.Duration) (int, error) {
	replacement := desired
	replacement.Name = replacementContainerName(desired.Name)
	if err := startServiceContainerAndWait(ctx, req, replacement, monitor); err != nil {
		_ = removeContainersByName(context.Background(), []string{replacement.Name})
		return 0, err
	}
	if err := removeContainersByName(ctx, []string{old.Name}); err != nil {
		_ = removeContainersByName(context.Background(), []string{replacement.Name})
		return 0, err
	}
	if _, err := runDocker(ctx, "rename", replacement.Name, desired.Name); err != nil {
		_ = removeContainersByName(context.Background(), []string{replacement.Name})
		return 1, fmt.Errorf("failed to rename replacement container %s to %s: %w", replacement.Name, desired.Name, err)
	}
	return 1, nil
}

func replaceServiceContainerStopFirst(ctx context.Context, req ReconcileServiceRequest, desired ContainerSpec, old serviceContainer, monitor time.Duration) (int, error) {
	replacement := desired
	replacement.Name = replacementContainerName(desired.Name)
	if old.Name != "" && old.State == "running" {
		if err := stopContainersByName(ctx, []string{old.Name}); err != nil {
			return 0, err
		}
	}
	if err := startServiceContainerAndWait(ctx, req, replacement, monitor); err != nil {
		_ = removeContainersByName(context.Background(), []string{replacement.Name})
		if old.Name != "" {
			_ = startContainersByName(context.Background(), []string{old.Name})
		}
		return 0, err
	}
	if old.Name != "" {
		if err := removeContainersByName(ctx, []string{old.Name}); err != nil {
			_ = removeContainersByName(context.Background(), []string{replacement.Name})
			_ = startContainersByName(context.Background(), []string{old.Name})
			return 0, err
		}
	}
	if _, err := runDocker(ctx, "rename", replacement.Name, desired.Name); err != nil {
		_ = removeContainersByName(context.Background(), []string{replacement.Name})
		if old.Name != "" {
			_ = startContainersByName(context.Background(), []string{old.Name})
		}
		return 1, fmt.Errorf("failed to rename replacement container %s to %s: %w", replacement.Name, desired.Name, err)
	}
	return 1, nil
}

func RemoveService(ctx context.Context, req RemoveServiceRequest) (*RemoveServiceResponse, error) {
	if err := validateRemoveServiceRequest(req); err != nil {
		return nil, err
	}
	removedContainers, err := removeServiceContainers(ctx, req.Project, req.Environment, req.Service)
	if err != nil {
		return nil, err
	}
	return &RemoveServiceResponse{
		Project:           req.Project,
		Environment:       req.Environment,
		Service:           req.Service,
		RemovedContainers: removedContainers,
	}, nil
}

func validateReconcileServiceRequest(req ReconcileServiceRequest) error {
	for label, value := range map[string]string{
		"project":     req.Project,
		"environment": req.Environment,
		"service":     req.Service,
		"image":       req.Image,
		"network":     req.Network,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", label)
		}
	}
	if !isSafeProjectName(req.Project) {
		return fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(req.Environment) {
		return fmt.Errorf("invalid environment name")
	}
	if !isSafeServiceName(req.Service) {
		return fmt.Errorf("invalid service name")
	}
	if err := validateImageName(req.Image); err != nil {
		return err
	}
	if !isSafeRuntimeName(req.Network) {
		return fmt.Errorf("invalid network name")
	}
	if req.NetworkAlias != "" && !isSafeRuntimeName(req.NetworkAlias) {
		return fmt.Errorf("invalid network alias")
	}
	if len(req.NetworkAliases) > 16 {
		return fmt.Errorf("too many network aliases")
	}
	seenAliases := make(map[string]bool, len(req.NetworkAliases))
	for _, alias := range req.NetworkAliases {
		if !isSafeNetworkAlias(alias) {
			return fmt.Errorf("invalid network alias")
		}
		if seenAliases[alias] {
			return fmt.Errorf("duplicate network alias")
		}
		seenAliases[alias] = true
	}
	if !isSafeRestartPolicy(req.Restart) {
		return fmt.Errorf("invalid restart policy")
	}
	if req.EnvFile != "" {
		return fmt.Errorf("envFile path is not accepted; use envFileContent")
	}
	if len(req.EnvFileContent) > 1<<20 {
		return fmt.Errorf("envFileContent exceeds 1 MiB")
	}
	for _, container := range req.Containers {
		if !isSafeContainerName(container.Name) {
			return fmt.Errorf("invalid container name")
		}
		if container.Slot < 0 {
			return fmt.Errorf("invalid container slot")
		}
		if len(container.NetworkAliases) > 8 {
			return fmt.Errorf("too many container network aliases")
		}
		seenContainerAliases := make(map[string]bool, len(container.NetworkAliases))
		for _, alias := range container.NetworkAliases {
			if !isDNSNetworkAlias(alias) {
				return fmt.Errorf("invalid container network alias")
			}
			if seenContainerAliases[alias] {
				return fmt.Errorf("duplicate container network alias")
			}
			seenContainerAliases[alias] = true
		}
		for _, publish := range container.Publishes {
			if hasControlChars(publish) {
				return fmt.Errorf("invalid publish value")
			}
		}
	}
	for _, mount := range req.Mounts {
		if strings.TrimSpace(mount) == "" || hasControlChars(mount) {
			return fmt.Errorf("invalid mount value")
		}
	}
	if err := validateConfigFileMounts(req.ConfigFiles); err != nil {
		return err
	}
	for key, value := range req.Labels {
		if strings.TrimSpace(key) == "" || hasControlChars(key) || hasControlChars(value) {
			return fmt.Errorf("invalid label")
		}
	}
	if req.Command != "" && strings.ContainsRune(req.Command, '\x00') {
		return fmt.Errorf("command contains unsupported characters")
	}
	switch req.Strategy {
	case "", reconcileRecreate, reconcileRolling:
	default:
		return fmt.Errorf("invalid reconcile strategy")
	}
	if req.Order != "" {
		switch req.Order {
		case rollingStartFirst, rollingStopFirst:
		default:
			return fmt.Errorf("invalid rolling order")
		}
	}
	if req.MaxUnavailable < 0 {
		return fmt.Errorf("maxUnavailable cannot be negative")
	}
	if req.Monitor != "" {
		duration, err := time.ParseDuration(req.Monitor)
		if err != nil || duration < 0 || duration > maxHealthDuration {
			return fmt.Errorf("invalid monitor duration")
		}
	}
	if err := validateHealthSpec(req.Health); err != nil {
		return err
	}
	return nil
}

func validateConfigFileMounts(files []ConfigFileMount) error {
	if len(files) > maxConfigFiles {
		return fmt.Errorf("too many config files")
	}
	seenTargets := make(map[string]bool, len(files))
	for _, file := range files {
		if !isSafeRuntimeName(file.Source) {
			return fmt.Errorf("invalid config source")
		}
		target, err := validateConfigFileTarget(file.Target)
		if err != nil {
			return err
		}
		if seenTargets[target] {
			return fmt.Errorf("duplicate config target")
		}
		seenTargets[target] = true
		if _, err := parseConfigFileMode(file.Mode); err != nil {
			return err
		}
		data, err := decodeConfigFileContent(file)
		if err != nil {
			return err
		}
		if len(data) > maxConfigFileBytes {
			return fmt.Errorf("config file exceeds 1 MiB")
		}
		sum := sha256.Sum256(data)
		hash := hex.EncodeToString(sum[:])
		if file.ContentHash != "" && file.ContentHash != hash {
			return fmt.Errorf("config content hash mismatch")
		}
	}
	return nil
}

func validateConfigFileTarget(target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", fmt.Errorf("config target is required")
	}
	if hasControlChars(target) || strings.Contains(target, ",") {
		return "", fmt.Errorf("invalid config target")
	}
	if !path.IsAbs(target) {
		return "", fmt.Errorf("config target must be absolute")
	}
	clean := path.Clean(target)
	if clean == "/" {
		return "", fmt.Errorf("config target must not be root")
	}
	return clean, nil
}

func parseConfigFileMode(mode string) (os.FileMode, error) {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		mode = "0444"
	}
	if strings.HasPrefix(mode, "0o") || strings.HasPrefix(mode, "0O") {
		mode = "0" + mode[2:]
	}
	parsed, err := strconv.ParseUint(mode, 8, 32)
	if err != nil || parsed == 0 || parsed > 0777 {
		return 0, fmt.Errorf("invalid config mode")
	}
	if parsed&0222 != 0 {
		return 0, fmt.Errorf("config mode must be read-only")
	}
	return os.FileMode(parsed), nil
}

func decodeConfigFileContent(file ConfigFileMount) ([]byte, error) {
	if strings.TrimSpace(file.ContentBase64) == "" {
		return nil, fmt.Errorf("config content is required")
	}
	data, err := base64.StdEncoding.DecodeString(file.ContentBase64)
	if err != nil {
		return nil, fmt.Errorf("invalid config content")
	}
	return data, nil
}

func validateHealthSpec(health *HealthSpec) error {
	if health == nil {
		return nil
	}
	if len(health.Command) > maxHealthFieldBytes || hasControlChars(health.Command) {
		return fmt.Errorf("invalid health command")
	}
	for label, value := range map[string]string{
		"health interval":     health.Interval,
		"health timeout":      health.Timeout,
		"health start period": health.StartPeriod,
	} {
		if value == "" {
			continue
		}
		if len(value) > 64 || hasControlChars(value) {
			return fmt.Errorf("invalid %s", label)
		}
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 || duration > maxHealthDuration {
			return fmt.Errorf("invalid %s", label)
		}
	}
	if health.Retries < 0 || health.Retries > maxHealthRetries {
		return fmt.Errorf("health retries must be between 0 and %d", maxHealthRetries)
	}
	if health.WaitAttempts < 0 || health.WaitAttempts > maxHealthWaitAttempts {
		return fmt.Errorf("health waitAttempts must be between 0 and %d", maxHealthWaitAttempts)
	}
	return nil
}

func validateRemoveServiceRequest(req RemoveServiceRequest) error {
	for label, value := range map[string]string{
		"project":     req.Project,
		"environment": req.Environment,
		"service":     req.Service,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", label)
		}
	}
	if !isSafeProjectName(req.Project) {
		return fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(req.Environment) {
		return fmt.Errorf("invalid environment name")
	}
	if !isSafeServiceName(req.Service) {
		return fmt.Errorf("invalid service name")
	}
	return nil
}

func isSafeServiceName(name string) bool {
	if len(name) == 0 || len(name) > 63 || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func isSafeContainerName(name string) bool {
	if len(name) == 0 || len(name) > 128 {
		return false
	}
	first := name[0]
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || (first >= '0' && first <= '9')) {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func isSafeRestartPolicy(value string) bool {
	if value == "" {
		return true
	}
	switch value {
	case "no", "always", "unless-stopped", "on-failure":
		return true
	}
	if !strings.HasPrefix(value, "on-failure:") {
		return false
	}
	retries, err := strconv.Atoi(strings.TrimPrefix(value, "on-failure:"))
	return err == nil && retries >= 0 && retries <= 100
}

func isSafeNetworkAlias(alias string) bool {
	alias = strings.TrimSpace(alias)
	if alias == "" || len(alias) > 253 || strings.HasSuffix(alias, ".") {
		return false
	}
	for _, segment := range strings.Split(alias, ".") {
		if segment == "" || len(segment) > 63 || !isSafeRuntimeName(segment) {
			return false
		}
	}
	return true
}

func isDNSNetworkAlias(alias string) bool {
	alias = strings.TrimSpace(alias)
	if alias == "" || len(alias) > 253 || strings.HasSuffix(alias, ".") {
		return false
	}
	for _, segment := range strings.Split(alias, ".") {
		if segment == "" || len(segment) > 63 {
			return false
		}
		if segment[0] == '-' || segment[len(segment)-1] == '-' {
			return false
		}
		for _, r := range segment {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				continue
			}
			return false
		}
	}
	return true
}

func hasControlChars(value string) bool {
	return strings.ContainsAny(value, "\x00\r\n")
}

func prepareServiceEnvFile(req *ReconcileServiceRequest) (func(), error) {
	if req.EnvFileContent == "" {
		return nil, nil
	}
	file, err := os.CreateTemp("", envFilePattern(req.Project, req.Environment, req.Service))
	if err != nil {
		return nil, fmt.Errorf("failed to create env file: %w", err)
	}
	cleanup := func() {
		_ = os.Remove(file.Name())
	}
	if err := os.Chmod(file.Name(), 0600); err != nil {
		file.Close()
		cleanup()
		return nil, fmt.Errorf("failed to secure env file: %w", err)
	}
	if _, err := file.WriteString(req.EnvFileContent); err != nil {
		file.Close()
		cleanup()
		return nil, fmt.Errorf("failed to write env file: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to close env file: %w", err)
	}
	req.EnvFile = file.Name()
	req.EnvFileContent = ""
	return cleanup, nil
}

func prepareServiceConfigFiles(req *ReconcileServiceRequest) error {
	if len(req.ConfigFiles) == 0 {
		return nil
	}
	root := req.ConfigDir
	if strings.TrimSpace(root) == "" {
		root = defaultConfigDir
	}
	for _, file := range req.ConfigFiles {
		target, err := validateConfigFileTarget(file.Target)
		if err != nil {
			return err
		}
		mode, err := parseConfigFileMode(file.Mode)
		if err != nil {
			return err
		}
		data, err := decodeConfigFileContent(file)
		if err != nil {
			return err
		}
		hash := file.ContentHash
		if hash == "" {
			sum := sha256.Sum256(data)
			hash = hex.EncodeToString(sum[:])
		}
		if !isSafeConfigHash(hash) {
			return fmt.Errorf("invalid config content hash")
		}

		configPath, err := writeServiceConfigFile(root, req.Project, req.Environment, req.Service, file.Source, hash, data, mode)
		if err != nil {
			return err
		}
		req.Mounts = append(req.Mounts, fmt.Sprintf("type=bind,source=%s,target=%s,readonly", configPath, target))
	}
	return nil
}

func writeServiceConfigFile(root string, project string, environment string, service string, source string, hash string, data []byte, mode os.FileMode) (string, error) {
	configDir := filepath.Join(root, project, environment, service, source, hash)
	if err := os.MkdirAll(configDir, 0750); err != nil {
		return "", fmt.Errorf("failed to create config directory: %w", err)
	}
	finalPath := filepath.Join(configDir, "content")
	tmp, err := os.CreateTemp(configDir, ".content-*")
	if err != nil {
		return "", fmt.Errorf("failed to create config file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return "", fmt.Errorf("failed to secure config file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", fmt.Errorf("failed to write config file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("failed to close config file: %w", err)
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return "", fmt.Errorf("failed to set config file mode: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return "", fmt.Errorf("failed to publish config file: %w", err)
	}
	if err := os.Chmod(finalPath, mode); err != nil {
		return "", fmt.Errorf("failed to set config file mode: %w", err)
	}
	cleanup = false
	return finalPath, nil
}

func isSafeConfigHash(hash string) bool {
	if len(hash) != sha256.Size*2 {
		return false
	}
	for _, r := range hash {
		if (r >= 'a' && r <= 'f') || (r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func envFilePattern(parts ...string) string {
	safe := make([]string, 0, len(parts))
	for _, part := range parts {
		value := strings.Trim(sanitizeFilePatternPart(part), "-")
		if value == "" {
			value = "value"
		}
		safe = append(safe, value)
	}
	return "tako-" + strings.Join(safe, "-") + "-*.env"
}

func sanitizeFilePatternPart(value string) string {
	var out strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			out.WriteRune(r)
		} else {
			out.WriteRune('-')
		}
	}
	return out.String()
}

type dockerNetworkOwner struct {
	Project     string
	Environment string
}

func ensureDockerNetwork(ctx context.Context, network string, owner dockerNetworkOwner) error {
	if _, err := runDocker(ctx, "network", "inspect", network); err == nil {
		return nil
	}
	args := []string{"network", "create"}
	if owner.Project != "" {
		args = append(args, "--label", "tako.project="+owner.Project)
	}
	if owner.Environment != "" {
		args = append(args, "--label", "tako.environment="+owner.Environment)
	}
	if owner.Project != "" || owner.Environment != "" {
		args = append(args, "--label", "tako.runtime=takod")
	}
	args = append(args, network)
	if _, err := runDocker(ctx, args...); err != nil {
		return fmt.Errorf("failed to ensure docker network %s: %w", network, err)
	}
	return nil
}

func ensureServiceVolumes(ctx context.Context, req ReconcileServiceRequest) error {
	for _, volume := range namedVolumeSourcesFromMounts(req.Mounts) {
		if err := ensureDockerVolume(ctx, req.Project, req.Environment, req.Service, volume); err != nil {
			return fmt.Errorf("failed to ensure docker volume %s: %w", volume, err)
		}
	}
	return nil
}

func ensureDockerVolume(ctx context.Context, project string, environment string, service string, volume string) error {
	if !isSafeDockerVolumeName(volume) {
		return fmt.Errorf("invalid volume name")
	}
	args := []string{
		"volume", "create",
		"--label", "tako.project=" + project,
		"--label", "tako.environment=" + environment,
		"--label", "tako.runtime=takod",
	}
	if service != "" {
		args = append(args, "--label", "tako.service="+service)
	}
	args = append(args, volume)
	_, err := runDocker(ctx, args...)
	return err
}

func namedVolumeSourcesFromMounts(mounts []string) []string {
	seen := make(map[string]bool)
	var names []string
	for _, mount := range mounts {
		fields := parseDockerMountFields(mount)
		if fields["type"] != "volume" {
			continue
		}
		source := fields["source"]
		if source == "" {
			source = fields["src"]
		}
		if source == "" || strings.HasPrefix(source, "/") || seen[source] {
			continue
		}
		seen[source] = true
		names = append(names, source)
	}
	sort.Strings(names)
	return names
}

func parseDockerMountFields(mount string) map[string]string {
	fields := make(map[string]string)
	for _, part := range strings.Split(mount, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return fields
}

func isSafeDockerVolumeName(name string) bool {
	if strings.TrimSpace(name) == "" || len(name) > maxDockerVolumeName {
		return false
	}
	if strings.ContainsAny(name, "/\\\x00\r\n") || strings.HasPrefix(name, "-") {
		return false
	}
	for _, r := range name {
		if r <= 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func removeServiceContainers(ctx context.Context, project string, environment string, service string) (int, error) {
	output, err := runDocker(
		ctx,
		"ps",
		"-aq",
		"--filter", "label=tako.project="+project,
		"--filter", "label=tako.environment="+environment,
		"--filter", "label=tako.service="+service,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to list old service containers: %w", err)
	}
	ids := strings.Fields(strings.TrimSpace(output))
	if len(ids) == 0 {
		return 0, nil
	}
	args := append([]string{"rm", "-f"}, ids...)
	if _, err := runDocker(ctx, args...); err != nil {
		return 0, fmt.Errorf("failed to remove old service containers: %w", err)
	}
	return len(ids), nil
}

type serviceContainer struct {
	ID    string
	Name  string
	State string
}

func listServiceContainers(ctx context.Context, project string, environment string, service string) ([]serviceContainer, error) {
	output, err := runDocker(
		ctx,
		"ps",
		"-a",
		"--filter", "label=tako.project="+project,
		"--filter", "label=tako.environment="+environment,
		"--filter", "label=tako.service="+service,
		"--format", "{{.ID}}|{{.Names}}|{{.State}}",
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list service containers: %w", err)
	}
	return parseServiceContainers(output), nil
}

func parseServiceContainers(output string) []serviceContainer {
	var containers []serviceContainer
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 2 {
			continue
		}
		container := serviceContainer{
			ID:    strings.TrimSpace(parts[0]),
			Name:  strings.TrimSpace(parts[1]),
			State: "unknown",
		}
		if len(parts) >= 3 && strings.TrimSpace(parts[2]) != "" {
			container.State = strings.TrimSpace(parts[2])
		}
		if container.Name == "" {
			continue
		}
		containers = append(containers, container)
	}
	sort.Slice(containers, func(i, j int) bool {
		return containers[i].Name < containers[j].Name
	})
	return containers
}

func removeContainersByName(ctx context.Context, names []string) error {
	names = uniqueContainerNames(names)
	if len(names) == 0 {
		return nil
	}
	args := append([]string{"rm", "-f"}, names...)
	if _, err := runDocker(ctx, args...); err != nil {
		return fmt.Errorf("failed to remove containers %s: %w", strings.Join(names, ", "), err)
	}
	return nil
}

func stopContainersByName(ctx context.Context, names []string) error {
	names = uniqueContainerNames(names)
	if len(names) == 0 {
		return nil
	}
	args := append([]string{"stop"}, names...)
	if _, err := runDocker(ctx, args...); err != nil {
		return fmt.Errorf("failed to stop containers %s: %w", strings.Join(names, ", "), err)
	}
	return nil
}

func startContainersByName(ctx context.Context, names []string) error {
	names = uniqueContainerNames(names)
	if len(names) == 0 {
		return nil
	}
	args := append([]string{"start"}, names...)
	if _, err := runDocker(ctx, args...); err != nil {
		return fmt.Errorf("failed to start containers %s: %w", strings.Join(names, ", "), err)
	}
	return nil
}

func uniqueContainerNames(names []string) []string {
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func runServiceContainer(ctx context.Context, req ReconcileServiceRequest, container ContainerSpec) error {
	args := buildServiceContainerArgs(req, container)
	if output, err := runDocker(ctx, args...); err != nil {
		return fmt.Errorf("failed to start container %s: %w, output: %s", container.Name, err, output)
	}
	return nil
}

func startServiceContainerAndWait(ctx context.Context, req ReconcileServiceRequest, container ContainerSpec, monitor time.Duration) error {
	if err := runServiceContainer(ctx, req, container); err != nil {
		return err
	}
	if err := waitForContainerHealthy(ctx, container.Name, req.Health); err != nil {
		_ = removeContainersByName(context.Background(), []string{container.Name})
		return err
	}
	if err := monitorContainer(ctx, container.Name, req.Health, monitor); err != nil {
		_ = removeContainersByName(context.Background(), []string{container.Name})
		return err
	}
	return nil
}

func cleanupStartedContainers(containerNames []string) error {
	if len(containerNames) == 0 {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args := append([]string{"rm", "-f"}, containerNames...)
	if _, err := runDocker(cleanupCtx, args...); err != nil {
		return fmt.Errorf("failed to remove started containers: %w", err)
	}
	return nil
}

func buildServiceContainerArgs(req ReconcileServiceRequest, container ContainerSpec) []string {
	args := []string{
		"run", "-d",
		"--name", container.Name,
		"--restart", req.Restart,
		"--network", req.Network,
	}
	for _, alias := range req.NetworkAliases {
		args = append(args, "--network-alias", alias)
	}
	for _, alias := range container.NetworkAliases {
		args = append(args, "--network-alias", alias)
	}

	labels := map[string]string{
		"tako.project":     req.Project,
		"tako.environment": req.Environment,
		"tako.service":     req.Service,
		"tako.runtime":     "takod",
	}
	for key, value := range req.Labels {
		labels[key] = value
	}
	if container.Slot > 0 {
		labels["tako.slot"] = strconv.Itoa(container.Slot)
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := labels[key]
		args = append(args, "--label", key+"="+value)
	}

	if req.EnvFile != "" {
		args = append(args, "--env-file", req.EnvFile)
	}
	for _, mount := range req.Mounts {
		args = append(args, "--mount", mount)
	}
	for _, publish := range container.Publishes {
		args = append(args, "--publish", publish)
	}
	if req.Health != nil && req.Health.Command != "" {
		args = append(args, "--health-cmd", req.Health.Command)
		if req.Health.Interval != "" {
			args = append(args, "--health-interval", req.Health.Interval)
		}
		if req.Health.Timeout != "" {
			args = append(args, "--health-timeout", req.Health.Timeout)
		}
		if req.Health.Retries > 0 {
			args = append(args, "--health-retries", fmt.Sprintf("%d", req.Health.Retries))
		}
		if req.Health.StartPeriod != "" {
			args = append(args, "--health-start-period", req.Health.StartPeriod)
		}
	}

	args = append(args, req.Image)
	if req.Command != "" {
		args = append(args, "sh", "-c", req.Command)
	}
	return args
}

func parseMonitorDuration(value string) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration < 0 || duration > maxHealthDuration {
		return 0, fmt.Errorf("invalid monitor duration")
	}
	return duration, nil
}

func monitorContainer(ctx context.Context, containerName string, health *HealthSpec, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}
	deadline := time.NewTimer(duration)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return nil
		case <-ticker.C:
			if err := assertContainerStillServing(ctx, containerName, health); err != nil {
				return err
			}
		}
	}
}

func assertContainerStillServing(ctx context.Context, containerName string, health *HealthSpec) error {
	if health != nil && health.Command != "" {
		status, err := runDocker(ctx, "inspect", containerName, "--format", "{{.State.Health.Status}}")
		if err != nil {
			return containerHealthFailureError(ctx, containerName, "failed to inspect container health", err)
		}
		status = strings.TrimSpace(status)
		if status == "unhealthy" {
			return containerHealthFailureError(ctx, containerName, "container became unhealthy during monitor window", nil)
		}
		if status != "healthy" && status != "starting" {
			return containerHealthFailureError(ctx, containerName, fmt.Sprintf("container has unexpected health status %q", status), nil)
		}
		return nil
	}

	running, err := runDocker(ctx, "inspect", containerName, "--format", "{{.State.Running}}")
	if err != nil {
		return containerHealthFailureError(ctx, containerName, "failed to inspect container", err)
	}
	if strings.TrimSpace(running) != "true" {
		return containerHealthFailureError(ctx, containerName, "container stopped during monitor window", nil)
	}
	return nil
}

func replacementContainerName(name string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", name, time.Now().UnixNano())))
	suffix := "_next_" + hex.EncodeToString(sum[:])[:10]
	if len(name)+len(suffix) <= 128 {
		return name + suffix
	}
	prefix := strings.TrimRight(name[:128-len(suffix)], "._-")
	if prefix == "" {
		prefix = "tako"
	}
	return prefix + suffix
}

func isPublishConflictError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{
		"port is already allocated",
		"address already in use",
		"bind: address",
		"port allocation",
		"driver failed programming external connectivity",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func waitForContainerHealthy(ctx context.Context, containerName string, health *HealthSpec) error {
	attempts := containerHealthWaitAttempts(health)

	for i := 0; i < attempts; i++ {
		status, err := runDocker(ctx, "inspect", containerName, "--format", "{{.State.Health.Status}}")
		status = strings.TrimSpace(status)
		if err == nil && status == "healthy" {
			return nil
		}
		if err == nil && status == "unhealthy" {
			return containerHealthFailureError(ctx, containerName, "container health check failed", nil)
		}

		running, runErr := runDocker(ctx, "inspect", containerName, "--format", "{{.State.Running}}")
		if runErr != nil {
			return containerHealthFailureError(ctx, containerName, "failed to inspect container", runErr)
		}
		if strings.TrimSpace(running) == "true" && (health == nil || health.Command == "") {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return containerHealthFailureError(ctx, containerName, fmt.Sprintf("health check timeout after %d attempts", attempts), nil)
}

func containerHealthWaitAttempts(health *HealthSpec) int {
	if health != nil && health.WaitAttempts > 0 {
		return health.WaitAttempts
	}
	return 30
}

func containerHealthFailureError(ctx context.Context, containerName string, message string, cause error) error {
	logs := recentContainerLogs(ctx, containerName)
	if cause != nil {
		return fmt.Errorf("%s for %s: %w, last logs:\n%s", message, containerName, cause, logs)
	}
	return fmt.Errorf("%s for %s, last logs:\n%s", message, containerName, logs)
}

func recentContainerLogs(ctx context.Context, containerName string) string {
	logs, err := runDocker(ctx, "logs", containerName, "--tail", "50")
	if err != nil {
		return fmt.Sprintf("(failed to read recent logs: %v)", err)
	}
	logs = strings.TrimRight(logs, "\n")
	if logs == "" {
		return "(no recent logs)"
	}
	return logs
}

func runDocker(ctx context.Context, args ...string) (string, error) {
	return runDockerWithEnv(ctx, nil, args...)
}

func runDockerWithEnv(ctx context.Context, env []string, args ...string) (string, error) {
	cmd := dockerCommandContext(ctx, "docker", args...)
	if env != nil {
		cmd.Env = env
	}
	output := newCappedOutputBuffer(defaultCommandOutputMaxBytes)
	cmd.Stdout = output
	cmd.Stderr = output
	err := cmd.Run()
	return output.String(), err
}
