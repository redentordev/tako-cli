package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/utils"
)

const (
	maxServiceHealthRetries  = 100
	maxServiceHealthDuration = 24 * time.Hour
	maxServiceHookDuration   = 24 * time.Hour
	maxServiceHookCommand    = 4096
	maxServiceHookField      = 1024
	maxConfigFileBytes       = 1 << 20
)

// ValidateConfig validates the configuration
func ValidateConfig(cfg *Config) error {
	// Validate project
	if cfg.Project.Name == "" {
		return fmt.Errorf("project name is required")
	}
	// Validate project name format (alphanumeric + hyphen, must start with letter)
	if !isValidProjectName(cfg.Project.Name) {
		return fmt.Errorf("project name '%s' is invalid: must start with a lowercase letter, contain only lowercase letters, numbers, and hyphens, and be 1-63 characters long", cfg.Project.Name)
	}
	if cfg.Project.Version == "" {
		return fmt.Errorf("project version is required")
	}

	if err := validateRuntimeConfig(cfg); err != nil {
		return err
	}
	if err := validateDeploymentConfig(cfg.Deployment); err != nil {
		return err
	}
	if err := validateRegistryConfig(cfg.Registry); err != nil {
		return err
	}

	// Validate servers
	if len(cfg.Servers) == 0 {
		return fmt.Errorf("at least one server must be configured")
	}
	for name, server := range cfg.Servers {
		if !isValidRuntimeIdentifier(name) {
			return fmt.Errorf("server name '%s' is invalid: must start with a lowercase letter, contain only lowercase letters, numbers, hyphens, and underscores, and be 1-63 characters long", name)
		}
		if err := validateServer(name, &server); err != nil {
			return err
		}
		// Update the server in the map with defaults applied
		cfg.Servers[name] = server
	}

	if len(cfg.Imports) > 0 {
		if err := validateImports(cfg); err != nil {
			return err
		}
	}
	if len(cfg.Configs) > 0 {
		if err := validateConfigFiles(cfg); err != nil {
			return err
		}
	}

	// Validate environments
	if len(cfg.Environments) == 0 {
		return fmt.Errorf("at least one environment must be configured")
	}
	for envName, env := range cfg.Environments {
		if !isValidRuntimeIdentifier(envName) {
			return fmt.Errorf("environment name '%s' is invalid: must start with a lowercase letter, contain only lowercase letters, numbers, hyphens, and underscores, and be 1-63 characters long", envName)
		}
		if err := validateEnvironment(envName, &env, cfg); err != nil {
			return err
		}
		// Update the environment in the map with defaults applied
		cfg.Environments[envName] = env
	}

	// Validate top-level volumes section
	if len(cfg.Volumes) > 0 {
		if err := validateVolumes(cfg.Volumes); err != nil {
			return err
		}
	}

	return nil
}

func validateDeploymentConfig(deployment *DeploymentConfig) error {
	if deployment == nil {
		return nil
	}
	deployment.Source = strings.TrimSpace(deployment.Source)
	if deployment.Source == "" {
		deployment.Source = DeploymentSourceLocal
	}
	switch deployment.Source {
	case DeploymentSourceLocal, DeploymentSourceGit:
	default:
		return fmt.Errorf("deployment.source must be local or git")
	}

	if deployment.Cache == nil {
		return nil
	}
	cache := deployment.Cache
	cache.Type = strings.TrimSpace(cache.Type)
	cache.Ref = strings.TrimSpace(cache.Ref)
	cache.Builder = strings.TrimSpace(cache.Builder)
	if cache.Type == "" {
		cache.Type = "local"
	}
	switch cache.Type {
	case "local":
	case "registry":
		if cache.Enabled && cache.Ref == "" {
			return fmt.Errorf("deployment.cache.ref is required when deployment.cache.type=registry")
		}
	default:
		return fmt.Errorf("deployment.cache.type must be local or registry")
	}
	if cache.Ref != "" {
		if err := validateCacheField("ref", cache.Ref, 512); err != nil {
			return err
		}
	}
	if cache.Builder != "" {
		if err := validateCacheField("builder", cache.Builder, 128); err != nil {
			return err
		}
		if strings.HasPrefix(cache.Builder, "-") {
			return fmt.Errorf("deployment.cache.builder must not start with '-'")
		}
	}
	deployment.Cache = cache
	return nil
}

func validateCacheField(field string, value string, maxLen int) error {
	if len(value) > maxLen {
		return fmt.Errorf("deployment.cache.%s is too long", field)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f || r == ' ' || r == '\t' {
			return fmt.Errorf("deployment.cache.%s must not contain whitespace or control characters", field)
		}
	}
	return nil
}

func validateRegistryConfig(registry *RegistryConfig) error {
	if registry == nil {
		return nil
	}
	registry.URL = strings.TrimSpace(registry.URL)
	registry.Username = strings.TrimSpace(registry.Username)
	if registry.URL == "" {
		return fmt.Errorf("registry.url is required when registry is configured")
	}
	if NormalizeRegistryServer(registry.URL) == "" {
		return fmt.Errorf("registry.url is invalid")
	}
	if registry.IdentityToken == "" {
		if registry.Username == "" {
			return fmt.Errorf("registry.username is required when registry.identityToken is not set")
		}
		if registry.Password == "" {
			return fmt.Errorf("registry.password is required when registry.identityToken is not set")
		}
	}
	for field, value := range map[string]string{
		"url":           registry.URL,
		"username":      registry.Username,
		"password":      registry.Password,
		"identityToken": registry.IdentityToken,
	} {
		if err := validateRegistryField(field, value); err != nil {
			return err
		}
	}
	return nil
}

func validateRegistryField(field string, value string) error {
	if value == "" {
		return nil
	}
	if len(value) > 8192 {
		return fmt.Errorf("registry.%s is too long", field)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("registry.%s must not contain control characters", field)
		}
	}
	return nil
}

func validateRuntimeConfig(cfg *Config) error {
	if cfg.Runtime == nil {
		cfg.Runtime = &RuntimeConfig{}
	}
	if cfg.Runtime.Mode == "" {
		cfg.Runtime.Mode = RuntimeModeTakod
	}
	if cfg.Runtime.Proxy == "" {
		cfg.Runtime.Proxy = RuntimeProxyTako
	}

	validRuntimeModes := map[string]bool{
		RuntimeModeTakod: true,
	}
	if !validRuntimeModes[cfg.Runtime.Mode] {
		return fmt.Errorf("runtime.mode must be takod")
	}

	validRuntimeProxies := map[string]bool{
		RuntimeProxyTako: true,
	}
	if !validRuntimeProxies[cfg.Runtime.Proxy] {
		return fmt.Errorf("runtime.proxy must be %s", RuntimeProxyTako)
	}

	if cfg.Runtime.Agent == nil {
		cfg.Runtime.Agent = &AgentConfig{}
	}
	if cfg.Runtime.Agent.Enabled != nil && !*cfg.Runtime.Agent.Enabled {
		return fmt.Errorf("runtime.agent.enabled=false is not supported; takod agent is required")
	}
	if cfg.Runtime.Agent.Enabled == nil {
		cfg.Runtime.Agent.Enabled = boolPointer(true)
	}
	if cfg.Runtime.Agent.Socket == "" {
		cfg.Runtime.Agent.Socket = "/run/tako/takod.sock"
	}
	if cfg.Runtime.Agent.DataDir == "" {
		cfg.Runtime.Agent.DataDir = "/var/lib/tako"
	}

	if cfg.Mesh == nil {
		cfg.Mesh = &MeshConfig{}
	}
	if cfg.Mesh.Enabled != nil && !*cfg.Mesh.Enabled {
		return fmt.Errorf("mesh.enabled=false is not supported; single-node deploys use a one-node mesh")
	}
	if cfg.Mesh.Enabled == nil {
		cfg.Mesh.Enabled = boolPointer(true)
	}
	if cfg.Mesh.NetworkCIDR == "" {
		cfg.Mesh.NetworkCIDR = "10.210.0.0/16"
	}
	if _, _, err := net.ParseCIDR(cfg.Mesh.NetworkCIDR); err != nil {
		return fmt.Errorf("mesh.networkCIDR is invalid: %w", err)
	}
	if cfg.Mesh.Interface == "" {
		cfg.Mesh.Interface = "tako"
	}
	if !isValidRuntimeName(cfg.Mesh.Interface) {
		return fmt.Errorf("mesh.interface '%s' is invalid: must contain only letters, numbers, hyphens, and underscores", cfg.Mesh.Interface)
	}
	if cfg.Mesh.ListenPort == 0 {
		cfg.Mesh.ListenPort = 51820
	}
	if cfg.Mesh.ListenPort < 1 || cfg.Mesh.ListenPort > 65535 {
		return fmt.Errorf("mesh.listenPort must be between 1 and 65535")
	}
	if cfg.Mesh.SubnetBits == 0 {
		cfg.Mesh.SubnetBits = 24
	}
	if cfg.Mesh.SubnetBits < 8 || cfg.Mesh.SubnetBits > 30 {
		return fmt.Errorf("mesh.subnetBits must be between 8 and 30")
	}
	cfg.Mesh.NATTraversal = true

	if cfg.State == nil {
		cfg.State = &StateConfig{}
	}
	if cfg.State.Backend == "" {
		cfg.State.Backend = StateBackendReplicated
	}
	if cfg.State.DeployConsistency == "" {
		cfg.State.DeployConsistency = StateDeployConsistencyLease
	}
	if cfg.State.OnUnreachableNode == "" {
		cfg.State.OnUnreachableNode = StateUnreachableBlock
	}
	cfg.State.RemoteCacheEnabled = true

	validStateBackends := map[string]bool{
		StateBackendReplicated: true,
	}
	if !validStateBackends[cfg.State.Backend] {
		return fmt.Errorf("state.backend must be replicated")
	}

	if cfg.State.DeployConsistency != StateDeployConsistencyLease {
		return fmt.Errorf("state.deployConsistency must be lease")
	}

	if cfg.State.OnUnreachableNode != StateUnreachableBlock {
		return fmt.Errorf("state.onUnreachableNode must be block")
	}

	return nil
}

func validateEnvironment(envName string, env *EnvironmentConfig, cfg *Config) error {
	// Validate servers or server selector
	if len(env.Servers) == 0 && env.ServerSelector == nil {
		return fmt.Errorf("environment %s: must specify either 'servers' or 'serverSelector'", envName)
	}

	// Validate server names exist in config
	if len(env.Servers) > 0 {
		for _, serverName := range env.Servers {
			if _, exists := cfg.Servers[serverName]; !exists {
				return fmt.Errorf("environment %s: server '%s' not found in servers section", envName, serverName)
			}
		}
	}

	// Validate server selector
	if env.ServerSelector != nil {
		if len(env.ServerSelector.Labels) == 0 && !env.ServerSelector.Any {
			return fmt.Errorf("environment %s: serverSelector must have either 'labels' or 'any=true'", envName)
		}
		if len(env.ServerSelector.Labels) > 0 && env.ServerSelector.Any {
			return fmt.Errorf("environment %s: serverSelector cannot have both 'labels' and 'any=true'", envName)
		}
	}

	// Validate services
	if len(env.Services) == 0 {
		return fmt.Errorf("environment %s: at least one service must be configured", envName)
	}

	for serviceName, service := range env.Services {
		if err := validateService(envName, serviceName, &service, env.Services, cfg); err != nil {
			return fmt.Errorf("environment %s: %w", envName, err)
		}
		// Update the service in the map with defaults applied
		env.Services[serviceName] = service
	}

	// Check for duplicate domains across services
	if err := validateDomainUniqueness(envName, env); err != nil {
		return err
	}

	return nil
}

func validateServer(name string, server *ServerConfig) error {
	if server.Host == "" {
		return fmt.Errorf("server %s: host is required", name)
	}

	// Validate host is a valid IP or hostname
	if net.ParseIP(server.Host) == nil {
		// If not an IP, check if it looks like a valid hostname
		if !isValidHostname(server.Host) {
			return fmt.Errorf("server %s: invalid host %s", name, server.Host)
		}
	}

	if server.User == "" {
		return fmt.Errorf("server %s: user is required", name)
	}

	// Validate username format to prevent command injection
	if !utils.IsValidUnixUsername(server.User) {
		return fmt.Errorf("server %s: invalid username '%s' (must be a valid POSIX username: letters, digits, underscore, hyphen; starts with letter or underscore; max 32 chars)", name, server.User)
	}

	// Set defaults
	if server.Port == 0 {
		server.Port = 22 // Default SSH port
	}

	// Validate authentication method: must have either sshKey or password
	hasPassword := server.Password != ""
	hasSSHKey := server.SSHKey != ""

	if hasPassword && hasSSHKey {
		// Both specified - this is allowed, key takes precedence
		// Just warn in verbose mode (handled elsewhere)
	}

	if hasPassword {
		// Security check: warn if password appears to be hardcoded (not an env var)
		if !strings.HasPrefix(server.Password, "${") && !strings.HasPrefix(server.Password, "$") {
			fmt.Fprintf(os.Stderr, "⚠ Security Warning: server %s has a hardcoded password.\n", name)
			fmt.Fprintf(os.Stderr, "  Consider using an environment variable instead:\n")
			fmt.Fprintf(os.Stderr, "    password: ${SSH_PASSWORD}\n")
			fmt.Fprintf(os.Stderr, "  Then set: export SSH_PASSWORD='your-password'\n\n")
		}
		// Password authentication - no need to check SSH key
		return nil
	}

	// SSH key authentication (default)
	if server.SSHKey == "" {
		// Default to standard SSH key location
		homeDir, _ := os.UserHomeDir()
		server.SSHKey = filepath.Join(homeDir, ".ssh", "id_rsa")
	}

	// Expand ~ in SSH key path
	if strings.HasPrefix(server.SSHKey, "~") {
		homeDir, _ := os.UserHomeDir()
		server.SSHKey = filepath.Join(homeDir, server.SSHKey[1:])
	}

	// Check if SSH key exists
	if _, err := os.Stat(server.SSHKey); os.IsNotExist(err) {
		return fmt.Errorf("server %s: SSH key not found: %s (you can also use 'password' for password authentication)", name, server.SSHKey)
	}

	return nil
}

func validateService(envName string, name string, service *ServiceConfig, envServices map[string]ServiceConfig, cfg *Config) error {
	// Validate service name format
	if !isValidRuntimeIdentifier(name) {
		return fmt.Errorf("service name '%s' is invalid: must start with a lowercase letter, contain only lowercase letters, numbers, hyphens, and underscores, and be 1-63 characters long", name)
	}

	// Must have either Build or Image, but not both
	if service.Build == "" && service.Image == "" {
		return fmt.Errorf("service %s: either 'build' or 'image' is required", name)
	}
	if service.Build != "" && service.Image != "" {
		return fmt.Errorf("service %s: cannot specify both 'build' and 'image'", name)
	}
	if service.Dockerfile != "" && service.Build == "" {
		return fmt.Errorf("service %s: 'dockerfile' requires 'build'", name)
	}
	if service.Platform != "" && !isValidDockerPlatform(service.Platform) {
		return fmt.Errorf("service %s: invalid platform %q; expected linux/amd64, linux/arm64, linux/arm/v6, linux/arm/v7, or linux/386", name, service.Platform)
	}

	// If Build is specified, check if path exists and detect Dockerfile
	if service.Build != "" {
		buildPath := service.Build
		if !filepath.IsAbs(buildPath) {
			// Make it absolute relative to current directory
			cwd, _ := os.Getwd()
			buildPath = filepath.Join(cwd, buildPath)
		}

		// Check if build path exists
		if _, err := os.Stat(buildPath); os.IsNotExist(err) {
			return fmt.Errorf("service %s: build path does not exist: %s", name, service.Build)
		}

		if service.Dockerfile != "" {
			normalizedDockerfile, err := validateBuildDockerfilePath(buildPath, service.Dockerfile)
			if err != nil {
				return fmt.Errorf("service %s: %w", name, err)
			}
			service.Dockerfile = normalizedDockerfile
		} else {
			// Try to find Dockerfile in build path
			dockerfileCandidates := []string{
				"Dockerfile",
				"Dockerfile.prod",
				"dockerfile",
				".dockerfile",
			}
			dockerfileFound := false
			for _, candidate := range dockerfileCandidates {
				dockerfilePath := filepath.Join(buildPath, candidate)
				if _, err := os.Stat(dockerfilePath); err == nil {
					dockerfileFound = true
					break
				}
			}
			if !dockerfileFound {
				fmt.Fprintf(os.Stderr, "Warning: No Dockerfile found in %s\n", buildPath)
			}
		}
	}

	// Validate envFile if specified
	if service.EnvFile != "" {
		envFilePath := service.EnvFile
		if !filepath.IsAbs(envFilePath) {
			// Make it absolute relative to current directory
			cwd, _ := os.Getwd()
			envFilePath = filepath.Join(cwd, envFilePath)
		}

		if err := validateEnvFilePath(envFilePath); err != nil {
			return fmt.Errorf("service %s: %w", name, err)
		}
	}

	if err := validateServiceVolumes(name, service, cfg); err != nil {
		return err
	}
	if err := validateServiceConfigFileMounts(name, service, cfg); err != nil {
		return err
	}
	if err := validateServicePorts(name, service); err != nil {
		return err
	}
	if err := normalizeServiceShare(name, service); err != nil {
		return err
	}
	if err := validateServiceExport(name, service); err != nil {
		return err
	}
	if err := validateServiceEnvLinks(envName, name, service, envServices, cfg); err != nil {
		return err
	}

	// Set default replicas
	if service.Replicas == 0 {
		service.Replicas = 1
	}
	if service.Replicas < 0 {
		return fmt.Errorf("service %s: replicas cannot be negative", name)
	}
	if err := ValidatePlacementConfig(service.Placement); err != nil {
		return fmt.Errorf("service %s: %w", name, err)
	}

	// Validate proxy if configured (per-service)
	if service.Proxy != nil {
		if err := validateProxy(name, service.Proxy); err != nil {
			return err
		}
	}

	// Validate load balancer strategy
	if service.LoadBalancer.Strategy == "" && service.Replicas > 1 {
		service.LoadBalancer.Strategy = "round_robin" // Default strategy
	}
	validStrategies := map[string]bool{
		"round_robin": true,
		"least_conn":  true,
		"ip_hash":     true,
		"random":      true,
	}
	if service.LoadBalancer.Strategy != "" && !validStrategies[service.LoadBalancer.Strategy] {
		return fmt.Errorf("service %s: invalid load balancer strategy: %s", name, service.LoadBalancer.Strategy)
	}

	// Set load balancer health check defaults
	if service.LoadBalancer.HealthCheck.Enabled && service.LoadBalancer.HealthCheck.Path == "" {
		// Use service health check path if available
		if service.HealthCheck.Path != "" {
			service.LoadBalancer.HealthCheck.Path = service.HealthCheck.Path
		} else {
			service.LoadBalancer.HealthCheck.Path = "/health"
		}
	}
	if service.LoadBalancer.HealthCheck.Enabled && service.LoadBalancer.HealthCheck.Interval == "" {
		service.LoadBalancer.HealthCheck.Interval = "10s"
	}
	if service.LoadBalancer.HealthCheck.Enabled {
		path, err := normalizeHTTPPath(service.LoadBalancer.HealthCheck.Path)
		if err != nil {
			return fmt.Errorf("service %s: invalid load balancer health check path: %w", name, err)
		}
		service.LoadBalancer.HealthCheck.Path = path
	}

	// Validate health check if configured
	if service.HealthCheck.Path != "" {
		path, err := normalizeHTTPPath(service.HealthCheck.Path)
		if err != nil {
			return fmt.Errorf("service %s: invalid health check path: %w", name, err)
		}
		service.HealthCheck.Path = path
		if service.PrimaryTargetPort() == 0 {
			return fmt.Errorf("service %s: port is required when health check is configured", name)
		}
		if service.HealthCheck.Interval == "" {
			service.HealthCheck.Interval = "10s"
		}
		if service.HealthCheck.Timeout == "" {
			service.HealthCheck.Timeout = "5s"
		}
		if service.HealthCheck.Retries == 0 {
			service.HealthCheck.Retries = 3
		}
		if err := validateServiceHealthTiming(name, service.HealthCheck); err != nil {
			return err
		}
	}

	// Validate deployment strategy
	if service.Deploy.Strategy == "" {
		service.Deploy.Strategy = "recreate"
	}
	switch service.Deploy.Strategy {
	case "recreate":
	case "rolling":
		if service.Deploy.Order == "" {
			service.Deploy.Order = "start-first"
		}
		switch service.Deploy.Order {
		case "start-first", "stop-first":
		default:
			return fmt.Errorf("service %s: invalid rolling deploy order %q", name, service.Deploy.Order)
		}
	default:
		return fmt.Errorf("service %s: invalid deployment strategy %q; expected recreate or rolling", name, service.Deploy.Strategy)
	}
	if service.Deploy.MaxUnavailable < 0 {
		return fmt.Errorf("service %s: deploy.maxUnavailable cannot be negative", name)
	}
	if service.Deploy.Monitor != "" {
		duration, err := time.ParseDuration(service.Deploy.Monitor)
		if err != nil || duration < 0 || duration > 24*time.Hour {
			return fmt.Errorf("service %s: deploy.monitor must be a duration between 0s and 24h", name)
		}
	}
	if err := validateServiceHooks(name, service.Hooks); err != nil {
		return err
	}

	// Validate backup if configured
	if service.Backup != nil {
		if service.Backup.Schedule == "" {
			return fmt.Errorf("service %s: backup schedule is required", name)
		}
		if service.Backup.Retain <= 0 {
			service.Backup.Retain = 7 // Default 7 days
		}
	}

	// Validate and set restart policy
	if service.Restart == "" {
		// Default restart policy based on service type
		if service.Persistent {
			service.Restart = "always" // Databases always restart
		} else {
			service.Restart = "unless-stopped" // Apps restart unless manually stopped
		}
	}
	validRestartPolicies := map[string]bool{
		"always":         true,
		"unless-stopped": true,
		"on-failure":     true,
		"no":             true,
	}
	if !validRestartPolicies[service.Restart] {
		return fmt.Errorf("service %s: invalid restart policy: %s (must be: always, unless-stopped, on-failure, or no)", name, service.Restart)
	}

	// Validate monitoring if configured
	if service.Monitoring != nil && service.Monitoring.Enabled {
		if service.Monitoring.Interval == "" {
			service.Monitoring.Interval = "60s" // Default 60 second check interval
		}
		if service.Monitoring.Webhook == "" {
			return fmt.Errorf("service %s: monitoring enabled but no webhook configured", name)
		}
		// Auto-detect check type if not specified
		if service.Monitoring.CheckType == "" {
			if service.IsPublic() && service.HealthCheck.Path != "" {
				service.Monitoring.CheckType = "http" // Check domain health endpoint
			} else {
				service.Monitoring.CheckType = "container" // Check if container running
			}
		}
		validCheckTypes := map[string]bool{
			"http":      true,
			"container": true,
		}
		if !validCheckTypes[service.Monitoring.CheckType] {
			return fmt.Errorf("service %s: invalid monitoring checkType: %s (must be: http or container)", name, service.Monitoring.CheckType)
		}
	}

	return nil
}

func validateServiceHooks(serviceName string, hooks HooksConfig) error {
	for hookName, hook := range map[string]*HookConfig{
		"preDeploy":  hooks.PreDeploy,
		"postDeploy": hooks.PostDeploy,
	} {
		if hook == nil {
			continue
		}
		command := strings.TrimSpace(hook.Command)
		if command == "" {
			return fmt.Errorf("service %s: hooks.%s.command is required", serviceName, hookName)
		}
		if len(command) > maxServiceHookCommand || strings.ContainsRune(command, '\x00') {
			return fmt.Errorf("service %s: hooks.%s.command is invalid", serviceName, hookName)
		}
		hook.Command = command
		if hook.Timeout != "" {
			duration, err := time.ParseDuration(hook.Timeout)
			if err != nil || duration <= 0 || duration > maxServiceHookDuration {
				return fmt.Errorf("service %s: hooks.%s.timeout is invalid", serviceName, hookName)
			}
		}
		if err := validateHookField(serviceName, hookName, "user", hook.User); err != nil {
			return err
		}
		if err := validateHookField(serviceName, hookName, "workingDir", hook.WorkingDir); err != nil {
			return err
		}
	}
	return nil
}

func validateHookField(serviceName string, hookName string, field string, value string) error {
	if value == "" {
		return nil
	}
	if len(value) > maxServiceHookField {
		return fmt.Errorf("service %s: hooks.%s.%s is too long", serviceName, hookName, field)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("service %s: hooks.%s.%s must not contain control characters", serviceName, hookName, field)
		}
	}
	return nil
}

func normalizeHTTPPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	if !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("must start with /")
	}
	for _, r := range path {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("must not contain control characters")
		}
	}
	return path, nil
}

func validateServiceHealthTiming(serviceName string, health HealthCheckConfig) error {
	for label, value := range map[string]string{
		"interval":     health.Interval,
		"timeout":      health.Timeout,
		"start period": health.StartPeriod,
	} {
		if value == "" {
			continue
		}
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 || duration > maxServiceHealthDuration {
			return fmt.Errorf("service %s: invalid health check %s", serviceName, label)
		}
	}
	if health.Retries < 0 || health.Retries > maxServiceHealthRetries {
		return fmt.Errorf("service %s: health check retries must be between 0 and %d", serviceName, maxServiceHealthRetries)
	}
	return nil
}

func validateServiceVolumes(name string, service *ServiceConfig, cfg *Config) error {
	for _, volume := range service.Volumes {
		if IsNFSVolume(volume) {
			return fmt.Errorf("service %s: NFS volume %q is no longer supported; use node-local volumes or an external storage service", name, volume)
		}
		if err := ValidateVolumeMountSpec(volume); err != nil {
			return fmt.Errorf("service %s: %w", name, err)
		}
	}
	return nil
}

func validateImports(cfg *Config) error {
	normalized := make(map[string]ImportConfig, len(cfg.Imports))
	for alias, importConfig := range cfg.Imports {
		if !isValidRuntimeIdentifier(alias) {
			return fmt.Errorf("import alias '%s' is invalid: must start with a lowercase letter, contain only lowercase letters, numbers, hyphens, and underscores, and be 1-63 characters long", alias)
		}
		importConfig.Project = strings.TrimSpace(importConfig.Project)
		importConfig.Environment = strings.TrimSpace(importConfig.Environment)
		importConfig.Service = strings.TrimSpace(importConfig.Service)
		importConfig.Port = strings.TrimSpace(importConfig.Port)
		if importConfig.Project == "" || importConfig.Environment == "" || importConfig.Service == "" || importConfig.Port == "" {
			return fmt.Errorf("import %s: project, environment, service, and port are required", alias)
		}
		if !isValidProjectName(importConfig.Project) {
			return fmt.Errorf("import %s: project %q is invalid", alias, importConfig.Project)
		}
		if !isValidRuntimeIdentifier(importConfig.Environment) {
			return fmt.Errorf("import %s: environment %q is invalid", alias, importConfig.Environment)
		}
		if !isValidRuntimeIdentifier(importConfig.Service) {
			return fmt.Errorf("import %s: service %q is invalid", alias, importConfig.Service)
		}
		if !isValidRuntimeIdentifier(importConfig.Port) {
			return fmt.Errorf("import %s: port %q is invalid", alias, importConfig.Port)
		}
		if importConfig.Project == cfg.Project.Name {
			return fmt.Errorf("import %s: cannot import from the same project", alias)
		}
		importConfig.Servers = normalizeImportServers(importConfig.Servers)
		for _, serverName := range importConfig.Servers {
			if _, ok := cfg.Servers[serverName]; !ok {
				return fmt.Errorf("import %s: server %q is not defined", alias, serverName)
			}
		}
		normalized[alias] = importConfig
	}
	cfg.Imports = normalized
	return nil
}

func normalizeImportServers(servers []string) []string {
	if len(servers) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(servers))
	out := make([]string, 0, len(servers))
	for _, serverName := range servers {
		serverName = strings.TrimSpace(serverName)
		if serverName == "" || seen[serverName] {
			continue
		}
		seen[serverName] = true
		out = append(out, serverName)
	}
	sort.Strings(out)
	return out
}

func normalizeServiceShare(name string, service *ServiceConfig) error {
	if service.Share == nil || !service.Share.Enabled {
		return nil
	}
	ports := service.EffectivePorts()
	if len(ports) == 0 {
		return fmt.Errorf("service %s: share requires a service port", name)
	}
	if service.Export == nil {
		service.Export = &ServiceExportConfig{}
	}
	if service.Export.Ports == nil {
		service.Export.Ports = make(map[string]int)
	}

	sharePorts := service.Share.Ports
	if len(sharePorts) == 0 {
		if len(ports) != 1 {
			return fmt.Errorf("service %s: share: true requires exactly one service port; use share: [port_name] for multi-port services", name)
		}
		return addSharedExportPort(name, service.Export.Ports, DefaultSharedPortName, ports[0].Target)
	}

	for _, portName := range sharePorts {
		port, ok := servicePortByName(service, portName)
		if !ok {
			return fmt.Errorf("service %s: share port %q is not a configured service port", name, portName)
		}
		if err := addSharedExportPort(name, service.Export.Ports, portName, port.Target); err != nil {
			return err
		}
	}
	return nil
}

func addSharedExportPort(serviceName string, exports map[string]int, portName string, target int) error {
	if !isValidRuntimeIdentifier(portName) {
		return fmt.Errorf("service %s: share port %q is invalid", serviceName, portName)
	}
	if target < 1 || target > 65535 {
		return fmt.Errorf("service %s: share port %s target must be between 1 and 65535", serviceName, portName)
	}
	if existing, ok := exports[portName]; ok && existing != target {
		return fmt.Errorf("service %s: share port %s conflicts with export target %d", serviceName, portName, existing)
	}
	exports[portName] = target
	return nil
}

func validateServiceEnvLinks(envName string, serviceName string, service *ServiceConfig, envServices map[string]ServiceConfig, cfg *Config) error {
	for key, value := range service.Env {
		key = strings.TrimSpace(key)
		if key == "" {
			return fmt.Errorf("service %s: env key cannot be empty", serviceName)
		}
		if value.IsPlain() {
			continue
		}
		if value.URL != "" {
			if containsControlCharacter(value.URL) {
				return fmt.Errorf("service %s: env %s url contains a control character", serviceName, key)
			}
			continue
		}
		if value.Link == nil {
			return fmt.Errorf("service %s: env %s must be a string, url, or link", serviceName, key)
		}
		link := value.Link
		link.App = strings.TrimSpace(link.App)
		link.Stage = strings.TrimSpace(link.Stage)
		link.Service = strings.TrimSpace(link.Service)
		link.Port = strings.TrimSpace(link.Port)
		link.Servers = normalizeImportServers(link.Servers)
		if link.Service == "" {
			return fmt.Errorf("service %s: env %s link.service is required", serviceName, key)
		}
		if !isValidRuntimeIdentifier(link.Service) {
			return fmt.Errorf("service %s: env %s link service %q is invalid", serviceName, key, link.Service)
		}
		if link.Port != "" && !isValidRuntimeIdentifier(link.Port) {
			return fmt.Errorf("service %s: env %s link port %q is invalid", serviceName, key, link.Port)
		}
		if link.App == "" && link.Stage == "" {
			if link.Service == serviceName {
				return fmt.Errorf("service %s: env %s cannot link to itself", serviceName, key)
			}
			target, ok := envServices[link.Service]
			if !ok {
				return fmt.Errorf("service %s: env %s links unknown service %q", serviceName, key, link.Service)
			}
			if _, err := resolveLinkedServicePort(link.Service, target, link.Port); err != nil {
				return fmt.Errorf("service %s: env %s: %w", serviceName, key, err)
			}
			continue
		}
		if link.App == "" || link.Stage == "" {
			return fmt.Errorf("service %s: env %s cross-project link requires app and stage", serviceName, key)
		}
		if !isValidProjectName(link.App) {
			return fmt.Errorf("service %s: env %s link app %q is invalid", serviceName, key, link.App)
		}
		if !isValidRuntimeIdentifier(link.Stage) {
			return fmt.Errorf("service %s: env %s link stage %q is invalid", serviceName, key, link.Stage)
		}
		if link.App == cfg.Project.Name && link.Stage == envName {
			return fmt.Errorf("service %s: env %s links the current app/stage; use link: %s", serviceName, key, link.Service)
		}
		for _, serverName := range link.Servers {
			if _, ok := cfg.Servers[serverName]; !ok {
				return fmt.Errorf("service %s: env %s link server %q is not defined", serviceName, key, serverName)
			}
		}
	}
	return nil
}

func resolveLinkedServicePort(serviceName string, service ServiceConfig, requested string) (PortConfig, error) {
	ports := service.EffectivePorts()
	if len(ports) == 0 {
		return PortConfig{}, fmt.Errorf("linked service %s has no service port", serviceName)
	}
	if requested == "" {
		if len(ports) != 1 {
			return PortConfig{}, fmt.Errorf("linked service %s has multiple ports; set link.port", serviceName)
		}
		return ports[0], nil
	}
	port, ok := servicePortByName(&service, requested)
	if !ok {
		return PortConfig{}, fmt.Errorf("linked service %s has no port %q", serviceName, requested)
	}
	return port, nil
}

func servicePortByName(service *ServiceConfig, name string) (PortConfig, bool) {
	name = strings.TrimSpace(name)
	for _, port := range service.EffectivePorts() {
		if port.Name == name {
			return port, true
		}
	}
	return PortConfig{}, false
}

func containsControlCharacter(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func validateServiceExport(name string, service *ServiceConfig) error {
	if service.Export == nil {
		return nil
	}
	if len(service.Export.Ports) == 0 {
		return fmt.Errorf("service %s: export.ports must declare at least one named port", name)
	}
	targets := make(map[int]bool)
	for _, port := range service.EffectivePorts() {
		if port.Target > 0 {
			targets[port.Target] = true
		}
	}
	if len(targets) == 0 {
		return fmt.Errorf("service %s: export requires a service port", name)
	}
	normalized := make(map[string]int, len(service.Export.Ports))
	for portName, target := range service.Export.Ports {
		portName = strings.TrimSpace(portName)
		if !isValidRuntimeIdentifier(portName) {
			return fmt.Errorf("service %s: export port %q is invalid", name, portName)
		}
		if target < 1 || target > 65535 {
			return fmt.Errorf("service %s: export port %s target must be between 1 and 65535", name, portName)
		}
		if !targets[target] {
			return fmt.Errorf("service %s: export port %s target %d is not a configured service port", name, portName, target)
		}
		normalized[portName] = target
	}
	service.Export.Ports = normalized
	return nil
}

func validateConfigFiles(cfg *Config) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to resolve current directory: %w", err)
	}

	normalized := make(map[string]ConfigFileConfig, len(cfg.Configs))
	for name, configFile := range cfg.Configs {
		if !isValidRuntimeIdentifier(name) {
			return fmt.Errorf("config name '%s' is invalid: must start with a lowercase letter, contain only lowercase letters, numbers, hyphens, and underscores, and be 1-63 characters long", name)
		}
		configFile.Source = strings.TrimSpace(configFile.Source)
		hasSource := configFile.Source != ""
		hasGenerate := configFile.Generate != nil
		if hasSource == hasGenerate {
			return fmt.Errorf("config %s: exactly one of source or generate is required", name)
		}
		if hasSource {
			source, err := validateConfigFileSource(cwd, configFile.Source)
			if err != nil {
				return fmt.Errorf("config %s: %w", name, err)
			}
			configFile.Source = source
		} else if err := validateGeneratedConfigFile(&configFile, cfg); err != nil {
			return fmt.Errorf("config %s: %w", name, err)
		}
		normalized[name] = configFile
	}
	cfg.Configs = normalized
	return nil
}

func validateGeneratedConfigFile(configFile *ConfigFileConfig, cfg *Config) error {
	if configFile.Generate == nil {
		return fmt.Errorf("generate is required")
	}
	generators := 0
	if configFile.Generate.Caddy != nil {
		generators++
		if err := validateGeneratedCaddyConfig(configFile.Generate.Caddy, cfg); err != nil {
			return err
		}
	}
	if generators != 1 {
		return fmt.Errorf("generate must define exactly one generator")
	}
	return nil
}

func validateGeneratedCaddyConfig(caddy *GeneratedCaddyConfig, cfg *Config) error {
	caddy.Email = strings.TrimSpace(caddy.Email)
	if caddy.Email == "" {
		return fmt.Errorf("generate.caddy.email is required")
	}
	if !isSafeGeneratedCaddyScalar(caddy.Email) || !strings.Contains(caddy.Email, "@") {
		return fmt.Errorf("generate.caddy.email is invalid")
	}

	adminHost, err := NormalizeProxyDomain(caddy.AdminHost)
	if err != nil {
		return fmt.Errorf("generate.caddy.adminHost is invalid: %w", err)
	}
	siteHost, err := NormalizeProxyDomain(caddy.SiteHost)
	if err != nil {
		return fmt.Errorf("generate.caddy.siteHost is invalid: %w", err)
	}
	caddy.AdminHost = adminHost
	caddy.SiteHost = siteHost

	adminImport, err := normalizeGeneratedCaddyImportAlias(caddy.AdminImport, cfg)
	if err != nil {
		return fmt.Errorf("generate.caddy.adminImport: %w", err)
	}
	rendererImport, err := normalizeGeneratedCaddyImportAlias(caddy.RendererImport, cfg)
	if err != nil {
		return fmt.Errorf("generate.caddy.rendererImport: %w", err)
	}
	caddy.AdminImport = adminImport
	caddy.RendererImport = rendererImport

	caddy.AskImport = strings.TrimSpace(caddy.AskImport)
	if caddy.OnDemandTLS {
		if caddy.AskImport == "" {
			caddy.AskImport = caddy.AdminImport
		}
		askImport, err := normalizeGeneratedCaddyImportAlias(caddy.AskImport, cfg)
		if err != nil {
			return fmt.Errorf("generate.caddy.askImport: %w", err)
		}
		caddy.AskImport = askImport
		askPath, err := normalizeGeneratedCaddyPath(caddy.AskPath)
		if err != nil {
			return fmt.Errorf("generate.caddy.askPath: %w", err)
		}
		caddy.AskPath = askPath
		return nil
	}
	if caddy.AskImport != "" {
		askImport, err := normalizeGeneratedCaddyImportAlias(caddy.AskImport, cfg)
		if err != nil {
			return fmt.Errorf("generate.caddy.askImport: %w", err)
		}
		caddy.AskImport = askImport
	}
	if strings.TrimSpace(caddy.AskPath) != "" {
		askPath, err := normalizeGeneratedCaddyPath(caddy.AskPath)
		if err != nil {
			return fmt.Errorf("generate.caddy.askPath: %w", err)
		}
		caddy.AskPath = askPath
	}
	return nil
}

func normalizeGeneratedCaddyImportAlias(alias string, cfg *Config) (string, error) {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return "", fmt.Errorf("import alias is required")
	}
	if !isValidRuntimeIdentifier(alias) {
		return "", fmt.Errorf("import alias %q is invalid", alias)
	}
	if _, ok := cfg.Imports[alias]; !ok {
		return "", fmt.Errorf("import alias %q is not defined", alias)
	}
	return alias, nil
}

func normalizeGeneratedCaddyPath(value string) (string, error) {
	value, err := normalizeHTTPPath(value)
	if err != nil {
		return "", err
	}
	if strings.ContainsAny(value, " {}\"'\\#") || strings.Contains(value, "://") {
		return "", fmt.Errorf("path contains unsupported characters")
	}
	return value, nil
}

func isSafeGeneratedCaddyScalar(value string) bool {
	if value == "" || hasConfigControlChars(value) {
		return false
	}
	return !strings.ContainsAny(value, " {}\"'\\#")
}

func validateConfigFileSource(root string, source string) (string, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", fmt.Errorf("source is required")
	}
	if hasConfigControlChars(source) {
		return "", fmt.Errorf("source must not contain control characters")
	}
	if filepath.IsAbs(source) {
		return "", fmt.Errorf("source %q must be relative to the project checkout", source)
	}
	clean := filepath.Clean(source)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("source %q must stay inside the project checkout", source)
	}
	fullPath := filepath.Join(root, clean)
	info, err := os.Lstat(fullPath)
	if err != nil {
		return "", fmt.Errorf("source %q is not readable: %w", source, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("source %q must be a regular file, not a symlink", source)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("source %q must be a regular file", source)
	}
	if info.Size() > maxConfigFileBytes {
		return "", fmt.Errorf("source %q exceeds 1 MiB", source)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("failed to resolve project checkout: %w", err)
	}
	realPath, err := filepath.EvalSymlinks(fullPath)
	if err != nil {
		return "", fmt.Errorf("failed to resolve source %q: %w", source, err)
	}
	rel, err := filepath.Rel(realRoot, realPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("source %q must stay inside the project checkout", source)
	}
	return filepath.ToSlash(clean), nil
}

func validateServiceConfigFileMounts(name string, service *ServiceConfig, cfg *Config) error {
	if len(service.Configs) == 0 {
		return nil
	}
	seenTargets := make(map[string]bool, len(service.Configs))
	for index := range service.Configs {
		mount := &service.Configs[index]
		mount.Source = strings.TrimSpace(mount.Source)
		if mount.Source == "" {
			return fmt.Errorf("service %s: configs[%d].source is required", name, index)
		}
		if !isValidRuntimeIdentifier(mount.Source) {
			return fmt.Errorf("service %s: config source %q is invalid", name, mount.Source)
		}
		configFile, ok := cfg.Configs[mount.Source]
		if !ok {
			return fmt.Errorf("service %s: config source %q is not defined in top-level configs", name, mount.Source)
		}
		target, err := normalizeConfigFileTarget(mount.Target)
		if err != nil {
			return fmt.Errorf("service %s: config %s: %w", name, mount.Source, err)
		}
		if seenTargets[target] {
			return fmt.Errorf("service %s: duplicate config target %q", name, target)
		}
		seenTargets[target] = true
		mode, err := normalizeConfigFileMode(mount.Mode)
		if err != nil {
			return fmt.Errorf("service %s: config %s: %w", name, mount.Source, err)
		}
		mount.Target = target
		mount.Mode = mode
		if configFile.Source != "" {
			hash, err := configFileContentHash(configFile.Source)
			if err != nil {
				return fmt.Errorf("service %s: config %s: %w", name, mount.Source, err)
			}
			mount.ContentHash = hash
		} else {
			mount.ContentHash = ""
		}
	}
	return nil
}

func normalizeConfigFileTarget(target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", fmt.Errorf("target is required")
	}
	if hasConfigControlChars(target) {
		return "", fmt.Errorf("target must not contain control characters")
	}
	if strings.Contains(target, ",") {
		return "", fmt.Errorf("target must not contain commas")
	}
	if !path.IsAbs(target) {
		return "", fmt.Errorf("target %q must be an absolute container path", target)
	}
	clean := path.Clean(target)
	if clean == "/" {
		return "", fmt.Errorf("target must not be the container root")
	}
	return clean, nil
}

func normalizeConfigFileMode(mode string) (string, error) {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		mode = "0444"
	}
	if strings.HasPrefix(mode, "0o") || strings.HasPrefix(mode, "0O") {
		mode = "0" + mode[2:]
	}
	if mode == "" || len(mode) > 4 {
		return "", fmt.Errorf("mode must be an octal file mode")
	}
	parsed, err := strconv.ParseUint(mode, 8, 32)
	if err != nil || parsed == 0 || parsed > 0777 {
		return "", fmt.Errorf("mode must be an octal file mode")
	}
	if parsed&0222 != 0 {
		return "", fmt.Errorf("mode must be read-only")
	}
	return fmt.Sprintf("0%03o", parsed), nil
}

func configFileContentHash(source string) (string, error) {
	data, err := os.ReadFile(source)
	if err != nil {
		return "", fmt.Errorf("failed to read source %q: %w", source, err)
	}
	if len(data) > maxConfigFileBytes {
		return "", fmt.Errorf("source %q exceeds 1 MiB", source)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func hasConfigControlChars(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

type VolumeMountSpec struct {
	Source    string
	Target    string
	Mode      string
	HasTarget bool
}

func ParseVolumeMountSpec(volume string) (VolumeMountSpec, error) {
	volume = strings.TrimSpace(volume)
	if volume == "" {
		return VolumeMountSpec{}, fmt.Errorf("volume spec is required")
	}
	parts := strings.Split(volume, ":")
	if len(parts) > 3 {
		return VolumeMountSpec{}, fmt.Errorf("volume %q must use source:target[:ro|rw]", volume)
	}
	spec := VolumeMountSpec{
		Source: strings.TrimSpace(parts[0]),
	}
	if len(parts) > 1 {
		spec.Target = strings.TrimSpace(parts[1])
		spec.HasTarget = true
	}
	if len(parts) == 3 {
		spec.Mode = strings.TrimSpace(parts[2])
		switch spec.Mode {
		case "ro", "rw":
		default:
			return VolumeMountSpec{}, fmt.Errorf("volume %q mode must be ro or rw", volume)
		}
	}
	if spec.Source == "" {
		return VolumeMountSpec{}, fmt.Errorf("volume %q has an empty source", volume)
	}
	if !spec.HasTarget {
		if !strings.HasPrefix(spec.Source, "/") {
			return VolumeMountSpec{}, fmt.Errorf("volume %q must be an absolute container path or use name:/container/path", volume)
		}
		return spec, nil
	}
	if spec.Target == "" || !strings.HasPrefix(spec.Target, "/") {
		return VolumeMountSpec{}, fmt.Errorf("volume %q target must be an absolute container path", volume)
	}
	if strings.HasPrefix(spec.Source, "/") {
		return spec, nil
	}
	if strings.HasPrefix(spec.Source, ".") || strings.ContainsAny(spec.Source, `/\`) {
		return VolumeMountSpec{}, fmt.Errorf("relative bind mount source %q is not supported; use an absolute host path or a named volume", spec.Source)
	}
	if !isValidDockerVolumeName(spec.Source) {
		return VolumeMountSpec{}, fmt.Errorf("volume %q source must be a valid Docker volume name or absolute host path", volume)
	}
	return spec, nil
}

func ValidateVolumeMountSpec(volume string) error {
	_, err := ParseVolumeMountSpec(volume)
	return err
}

func isValidDockerPlatform(platform string) bool {
	switch platform {
	case "linux/amd64", "linux/arm64", "linux/arm/v6", "linux/arm/v7", "linux/386":
		return true
	default:
		return false
	}
}

func validateServicePorts(serviceName string, service *ServiceConfig) error {
	if service.Port > 0 && len(service.Ports) > 0 {
		return fmt.Errorf("service %s: cannot specify both port and ports", serviceName)
	}
	if service.Proxy != nil && len(service.Ports) > 0 {
		return fmt.Errorf("service %s: top-level proxy cannot be combined with ports[]; put proxy under the port entry", serviceName)
	}
	if service.Port < 0 || service.Port > 65535 {
		return fmt.Errorf("service %s: port must be between 1 and 65535", serviceName)
	}

	seenNames := make(map[string]bool, len(service.Ports))
	for index := range service.Ports {
		port := &service.Ports[index]
		normalizePortDefaults(port)
		if strings.TrimSpace(port.Name) == "" {
			return fmt.Errorf("service %s: ports[%d].name is required", serviceName, index)
		}
		if !isValidRuntimeIdentifier(port.Name) {
			return fmt.Errorf("service %s: port name %q is invalid", serviceName, port.Name)
		}
		if seenNames[port.Name] {
			return fmt.Errorf("service %s: duplicate port name %q", serviceName, port.Name)
		}
		seenNames[port.Name] = true
		if port.Target < 1 || port.Target > 65535 {
			return fmt.Errorf("service %s: port %s target must be between 1 and 65535", serviceName, port.Name)
		}
		switch port.Protocol {
		case "http", "https", "tcp", "udp":
		default:
			return fmt.Errorf("service %s: port %s protocol must be http, https, tcp, or udp", serviceName, port.Name)
		}
		switch port.Mode {
		case "internal":
			if port.Proxy != nil {
				return fmt.Errorf("service %s: internal port %s cannot define proxy", serviceName, port.Name)
			}
			if port.Published != 0 || port.HostIP != "" {
				return fmt.Errorf("service %s: internal port %s cannot define published or hostIP", serviceName, port.Name)
			}
		case "proxy":
			if port.Proxy == nil {
				return fmt.Errorf("service %s: proxy port %s requires proxy", serviceName, port.Name)
			}
			if port.Protocol != "http" && port.Protocol != "https" {
				return fmt.Errorf("service %s: proxy port %s must use http or https protocol", serviceName, port.Name)
			}
			if err := validateProxy(serviceName+"."+port.Name, port.Proxy); err != nil {
				return err
			}
		case "host":
			if port.Proxy != nil {
				return fmt.Errorf("service %s: host port %s cannot define proxy", serviceName, port.Name)
			}
			if port.Published == 0 {
				port.Published = port.Target
			}
			if port.Published < 1 || port.Published > 65535 {
				return fmt.Errorf("service %s: host port %s published must be between 1 and 65535", serviceName, port.Name)
			}
			if port.HostIP != "" && !isValidHostBindIPOrCIDR(port.HostIP) {
				return fmt.Errorf("service %s: host port %s hostIP must be an IP address or CIDR", serviceName, port.Name)
			}
		default:
			return fmt.Errorf("service %s: port %s mode must be internal, proxy, or host", serviceName, port.Name)
		}
	}
	return nil
}

func validateBuildDockerfilePath(buildPath string, dockerfile string) (string, error) {
	trimmed := strings.TrimSpace(dockerfile)
	if trimmed == "" {
		return "", fmt.Errorf("dockerfile path is required")
	}
	clean := filepath.Clean(trimmed)
	if filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("dockerfile must be a relative path inside the build context")
	}
	buildAbs, err := filepath.Abs(buildPath)
	if err != nil {
		return "", fmt.Errorf("failed to resolve build path: %w", err)
	}
	dockerfileAbs, err := filepath.Abs(filepath.Join(buildAbs, clean))
	if err != nil {
		return "", fmt.Errorf("failed to resolve dockerfile path: %w", err)
	}
	if dockerfileAbs != buildAbs && !strings.HasPrefix(dockerfileAbs, buildAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("dockerfile must be inside the build context")
	}
	info, err := os.Lstat(dockerfileAbs)
	if err != nil {
		return "", fmt.Errorf("dockerfile does not exist: %s", dockerfile)
	}
	if info.IsDir() || !info.Mode().IsRegular() {
		return "", fmt.Errorf("dockerfile must be a regular file: %s", dockerfile)
	}
	return filepath.ToSlash(clean), nil
}

func isValidHostBindIPOrCIDR(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if net.ParseIP(value) != nil {
		return true
	}
	_, _, err := net.ParseCIDR(value)
	return err == nil
}

func validateProxy(serviceName string, proxy *ProxyConfig) error {
	if proxy.Domain == "" {
		return fmt.Errorf("service %s: proxy configured but no domain specified (use 'domain')", serviceName)
	}

	trimmed, err := NormalizeProxyDomain(proxy.Domain)
	if err != nil {
		return fmt.Errorf("service %s: invalid primary domain: %s", serviceName, strings.TrimSpace(proxy.Domain))
	}
	proxy.Domain = trimmed

	// Validate redirect domains
	primaryDomain := proxy.GetPrimaryDomain()
	for i, redirectDomain := range proxy.RedirectFrom {
		trimmed, err := NormalizeProxyDomain(redirectDomain)
		if err != nil {
			return fmt.Errorf("service %s: invalid redirect domain: %s", serviceName, strings.TrimSpace(redirectDomain))
		}
		proxy.RedirectFrom[i] = trimmed

		// Ensure redirect domain is not the same as primary domain
		if strings.EqualFold(trimmed, primaryDomain) {
			return fmt.Errorf("service %s: redirect domain '%s' cannot be the same as primary domain", serviceName, trimmed)
		}

		// Ensure redirect domain is not duplicated in serving domains
		for _, d := range proxy.GetAllDomains() {
			if strings.EqualFold(trimmed, d) {
				return fmt.Errorf("service %s: redirect domain '%s' is already the serving domain", serviceName, trimmed)
			}
		}
	}

	// Email is optional (can use default from first service with proxy)
	// But if specified, validate it
	if proxy.Email != "" && !strings.Contains(proxy.Email, "@") {
		return fmt.Errorf("service %s: invalid email address: %s", serviceName, proxy.Email)
	}

	// Set TLS provider default
	if proxy.TLS.Provider == "" {
		proxy.TLS.Provider = "letsencrypt" // Default
	}

	validProviders := map[string]bool{
		"letsencrypt": true,
		"zerossl":     true,
	}
	if !validProviders[proxy.TLS.Provider] {
		return fmt.Errorf("service %s: invalid TLS provider: %s", serviceName, proxy.TLS.Provider)
	}

	return nil
}

func isValidHostname(hostname string) bool {
	// Basic hostname validation
	if len(hostname) > 253 {
		return false
	}
	for _, label := range strings.Split(hostname, ".") {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		if !isValidLabel(label) {
			return false
		}
	}
	return true
}

func boolPointer(value bool) *bool {
	return &value
}

func isValidLabel(label string) bool {
	// Check if label contains only valid characters
	for i, ch := range label {
		if !((ch >= 'a' && ch <= 'z') ||
			(ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') ||
			(ch == '-' && i != 0 && i != len(label)-1)) {
			return false
		}
	}
	return true
}

func isValidDomain(domain string) bool {
	// Remove wildcard if present
	if strings.HasPrefix(domain, "*.") {
		domain = domain[2:]
	}

	// Check for dangerous characters that could cause issues in proxy routing or shell commands.
	// These characters could be used for injection attacks or cause routing issues
	dangerousChars := []string{
		"`", "$", "!", ";", "&", "|", ">", "<", "(", ")", "{", "}", "[", "]",
		"'", "\"", "\\", "\n", "\r", "\t", " ",
	}
	for _, ch := range dangerousChars {
		if strings.Contains(domain, ch) {
			return false
		}
	}

	// Check for regex metacharacters that could affect routing.
	// These are valid in hostnames but could cause issues if passed to HostRegexp.
	regexChars := []string{"^", "+", "?", "*", "="}
	for _, ch := range regexChars {
		if strings.Contains(domain, ch) {
			return false
		}
	}

	return isValidHostname(domain)
}

// isValidProjectName validates that a project name is safe for use in Docker, file paths, and shell commands
// Must start with lowercase letter, contain only lowercase letters, numbers, and hyphens
// Length must be 1-63 characters (Docker/DNS label limit)
func isValidProjectName(name string) bool {
	if len(name) == 0 || len(name) > 63 {
		return false
	}

	// Must start with a lowercase letter
	if name[0] < 'a' || name[0] > 'z' {
		return false
	}

	// Must end with alphanumeric (not hyphen)
	lastChar := name[len(name)-1]
	if !((lastChar >= 'a' && lastChar <= 'z') || (lastChar >= '0' && lastChar <= '9')) {
		return false
	}

	// All characters must be lowercase letter, digit, or hyphen
	for _, ch := range name {
		if !((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-') {
			return false
		}
	}

	return true
}

// isValidRuntimeIdentifier validates names used in runtime paths, Docker labels,
// volume names, and generated config fragments.
func isValidRuntimeIdentifier(name string) bool {
	if len(name) == 0 || len(name) > 63 {
		return false
	}

	// Must start with lowercase letter
	if name[0] < 'a' || name[0] > 'z' {
		return false
	}

	// All characters must be lowercase letter, digit, hyphen, or underscore
	for _, ch := range name {
		if !((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_') {
			return false
		}
	}

	return true
}

func isValidRuntimeName(name string) bool {
	if len(name) == 0 || len(name) > 32 {
		return false
	}
	for _, ch := range name {
		if !((ch >= 'a' && ch <= 'z') ||
			(ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') ||
			ch == '-' ||
			ch == '_') {
			return false
		}
	}
	return true
}

// validateDomainUniqueness checks for duplicate domains across all services in an environment
func validateDomainUniqueness(envName string, env *EnvironmentConfig) error {
	domainToService := make(map[string]string)

	for serviceName, service := range env.Services {
		for _, port := range service.EffectivePorts() {
			if port.Proxy == nil {
				continue
			}
			label := serviceName
			if port.Name != "" {
				label = serviceName + "." + port.Name
			}

			for _, domain := range port.Proxy.GetAllDomains() {
				normalizedDomain := strings.ToLower(domain)
				if existingService, exists := domainToService[normalizedDomain]; exists {
					return fmt.Errorf(
						"environment %s: domain conflict - domain '%s' is used by both service '%s' and service '%s'\n"+
							"  Each domain can only be assigned to one service.\n"+
							"  Suggestion: Remove the duplicate domain from one of the services or use different domains.",
						envName, domain, existingService, label,
					)
				}
				domainToService[normalizedDomain] = label
			}

			for _, redirectDomain := range port.Proxy.GetRedirectDomains() {
				normalizedDomain := strings.ToLower(redirectDomain)
				if existingService, exists := domainToService[normalizedDomain]; exists {
					return fmt.Errorf(
						"environment %s: domain conflict - redirect domain '%s' (service '%s') conflicts with domain in service '%s'\n"+
							"  Each domain can only be assigned to one service.\n"+
							"  Suggestion: Remove the duplicate domain from one of the services.",
						envName, redirectDomain, label, existingService,
					)
				}
				domainToService[normalizedDomain] = label + " (redirect)"
			}
		}
	}

	return nil
}

// validateVolumes validates the top-level volumes section
func validateVolumes(volumes map[string]VolumeConfig) error {
	for name, vol := range volumes {
		// Validate volume name format
		if !isValidVolumeName(name) {
			return fmt.Errorf("volume '%s': invalid name - must contain only lowercase letters, numbers, hyphens, and underscores", name)
		}

		// External volumes should not have driver or driver_opts
		if vol.External {
			if vol.Driver != "" {
				return fmt.Errorf("volume '%s': external volumes cannot specify a driver", name)
			}
			if len(vol.DriverOpts) > 0 {
				return fmt.Errorf("volume '%s': external volumes cannot specify driver_opts", name)
			}
		}

		// Validate driver if specified
		if vol.Driver != "" {
			validDrivers := map[string]bool{
				"local":  true,
				"tmpfs":  true,
				"cifs":   true,
				"btrfs":  true,
				"zfs":    true,
				"convoy": true,
				"rexray": true,
			}
			if !validDrivers[vol.Driver] {
				// Allow custom drivers, just warn
				fmt.Fprintf(os.Stderr, "Warning: volume '%s' uses non-standard driver '%s'\n", name, vol.Driver)
			}
		}

		// If custom name is specified, validate it
		if vol.Name != "" && !isValidDockerVolumeName(vol.Name) {
			return fmt.Errorf("volume '%s': custom name '%s' is invalid - must be a valid Docker volume name", name, vol.Name)
		}
	}

	return nil
}

// isValidVolumeName validates a volume key name (used in config)
func isValidVolumeName(name string) bool {
	if len(name) == 0 || len(name) > 63 {
		return false
	}

	// Must start with lowercase letter
	if name[0] < 'a' || name[0] > 'z' {
		return false
	}

	// All characters must be lowercase letter, digit, hyphen, or underscore
	for _, ch := range name {
		if !((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_') {
			return false
		}
	}

	return true
}

// isValidDockerVolumeName validates a Docker volume name
func isValidDockerVolumeName(name string) bool {
	if len(name) == 0 || len(name) > 255 {
		return false
	}

	// Docker volume names are fairly permissive but cannot contain slashes
	// or start/end with dots
	if strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return false
	}
	if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") {
		return false
	}

	return true
}
