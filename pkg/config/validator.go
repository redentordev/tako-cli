package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/utils"
)

const (
	maxServiceHealthRetries  = 100
	maxServiceHealthDuration = 24 * time.Hour
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
	if cfg.State.RemoteCacheEnabled == nil {
		cfg.State.RemoteCacheEnabled = boolPointer(true)
	}

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

	if !*cfg.State.RemoteCacheEnabled {
		return fmt.Errorf("state.remoteCacheEnabled must be true")
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

	if env.Proxy != nil && env.Proxy.Placement != nil {
		if err := ValidatePlacementConfig(env.Proxy.Placement); err != nil {
			return fmt.Errorf("environment %s proxy placement: %w", envName, err)
		}
		environmentServers, err := environmentServerTargets(envName, env, cfg)
		if err != nil {
			return err
		}
		if _, err := ResolveEnvironmentProxyTargets(env.Proxy, cfg.Servers, environmentServers, envName); err != nil {
			return fmt.Errorf("environment %s proxy placement: %w", envName, err)
		}
	}

	// Validate services
	if len(env.Services) == 0 {
		return fmt.Errorf("environment %s: at least one service must be configured", envName)
	}

	for serviceName, service := range env.Services {
		if err := validateService(serviceName, &service, cfg); err != nil {
			return fmt.Errorf("environment %s: %w", envName, err)
		}
		// Update the service in the map with defaults applied
		env.Services[serviceName] = service
	}

	if err := validateEnvironmentProxyACMESafety(envName, env, cfg); err != nil {
		return err
	}

	// Check for duplicate domains across services
	if err := validateDomainUniqueness(envName, env); err != nil {
		return err
	}

	return nil
}

func validateEnvironmentProxyACMESafety(envName string, env *EnvironmentConfig, cfg *Config) error {
	environmentServers, err := environmentServerTargets(envName, env, cfg)
	if err != nil {
		return err
	}
	proxyTargets, err := ResolveEnvironmentProxyTargets(env.Proxy, cfg.Servers, environmentServers, envName)
	if err != nil {
		return fmt.Errorf("environment %s proxy placement: %w", envName, err)
	}
	if len(proxyTargets) <= 1 {
		return nil
	}

	var publicServices []string
	for serviceName, service := range env.Services {
		if service.Proxy == nil {
			continue
		}
		if isAutomaticACMETLSProvider(service.Proxy.TLS.Provider) {
			publicServices = append(publicServices, serviceName)
		}
	}
	if len(publicServices) == 0 {
		return nil
	}
	sort.Strings(publicServices)

	return fmt.Errorf(
		"environment %s: automatic ACME TLS currently supports one proxy node, but proxy placement resolves to %d nodes (%s) for public service(s): %s; set environment.proxy.placement to a single edge node until shared certificate distribution is implemented",
		envName,
		len(proxyTargets),
		strings.Join(proxyTargets, ", "),
		strings.Join(publicServices, ", "),
	)
}

func isAutomaticACMETLSProvider(provider string) bool {
	switch provider {
	case "", "letsencrypt", "zerossl":
		return true
	default:
		return false
	}
}

func environmentServerTargets(envName string, env *EnvironmentConfig, cfg *Config) ([]string, error) {
	if len(env.Servers) > 0 {
		return append([]string(nil), env.Servers...), nil
	}
	if env.ServerSelector != nil {
		if env.ServerSelector.Any {
			servers := make([]string, 0, len(cfg.Servers))
			for name := range cfg.Servers {
				servers = append(servers, name)
			}
			sort.Strings(servers)
			return servers, nil
		}
		var matched []string
		for serverName, serverCfg := range cfg.Servers {
			if matchesLabels(serverCfg.Labels, env.ServerSelector.Labels) {
				matched = append(matched, serverName)
			}
		}
		if len(matched) == 0 {
			return nil, fmt.Errorf("no servers match the selector labels for environment '%s'", envName)
		}
		sort.Strings(matched)
		return matched, nil
	}
	return nil, fmt.Errorf("environment %s: must specify either 'servers' or 'serverSelector'", envName)
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

func validateService(name string, service *ServiceConfig, cfg *Config) error {
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
	if service.Dockerfile != "" {
		if service.Build == "" {
			return fmt.Errorf("service %s: dockerfile requires build", name)
		}
		if err := validateDockerfilePath(service.Dockerfile); err != nil {
			return fmt.Errorf("service %s: invalid dockerfile path: %w", name, err)
		}
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
			dockerfilePath := filepath.Join(buildPath, filepath.Clean(service.Dockerfile))
			if _, err := os.Stat(dockerfilePath); os.IsNotExist(err) {
				return fmt.Errorf("service %s: dockerfile does not exist: %s", name, service.Dockerfile)
			}
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

		// Check if env file exists
		if _, err := os.Stat(envFilePath); os.IsNotExist(err) {
			return fmt.Errorf("service %s: envFile not found: %s", name, service.EnvFile)
		}
	}

	if err := validateServiceVolumes(name, service, cfg); err != nil {
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
		"sticky":      true,
	}
	if service.LoadBalancer.Strategy != "" && !validStrategies[service.LoadBalancer.Strategy] {
		return fmt.Errorf("service %s: invalid load balancer strategy %q; supported strategies are round_robin and sticky", name, service.LoadBalancer.Strategy)
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
	hasHTTPHealthCheck := service.HealthCheck.Path != ""
	hasTCPHealthCheck := service.HealthCheck.TCPPort > 0
	if service.HealthCheck.TCPPort < 0 || service.HealthCheck.TCPPort > 65535 {
		return fmt.Errorf("service %s: health check tcpPort must be between 1 and 65535", name)
	}
	if hasHTTPHealthCheck && hasTCPHealthCheck {
		return fmt.Errorf("service %s: health check cannot set both path and tcpPort", name)
	}
	if hasHTTPHealthCheck {
		path, err := normalizeHTTPPath(service.HealthCheck.Path)
		if err != nil {
			return fmt.Errorf("service %s: invalid health check path: %w", name, err)
		}
		service.HealthCheck.Path = path
		if service.Port == 0 {
			return fmt.Errorf("service %s: port is required when health check is configured", name)
		}
	}
	if hasHTTPHealthCheck || hasTCPHealthCheck {
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
	if service.Deploy.Strategy != "recreate" {
		return fmt.Errorf("service %s: invalid deployment strategy %q; takod currently supports recreate", name, service.Deploy.Strategy)
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

	// Validate cross-project imports
	if len(service.Imports) > 0 {
		for _, importSpec := range service.Imports {
			parts := strings.Split(importSpec, ".")
			if len(parts) != 2 {
				return fmt.Errorf("service %s: invalid import format '%s' (must be 'project.service')", name, importSpec)
			}

			importProject := parts[0]
			importService := parts[1]

			// Validate not importing from self
			if importProject == cfg.Project.Name {
				return fmt.Errorf("service %s: cannot import from own project (found '%s')", name, importSpec)
			}

			// Validate project and service names are valid
			if importProject == "" || importService == "" {
				return fmt.Errorf("service %s: invalid import '%s' (project and service cannot be empty)", name, importSpec)
			}
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
	}
	return nil
}

func validateDockerfilePath(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("path is required")
	}
	if filepath.IsAbs(path) {
		return fmt.Errorf("must be relative to the build context")
	}
	if strings.ContainsAny(path, "\x00\r\n") {
		return fmt.Errorf("contains control characters")
	}
	cleaned := filepath.Clean(path)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf("must stay inside the build context")
	}
	return nil
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
		if service.Proxy == nil {
			continue
		}

		// Check primary domains and additional domains
		allDomains := service.Proxy.GetAllDomains()
		for _, domain := range allDomains {
			normalizedDomain := strings.ToLower(domain)

			if existingService, exists := domainToService[normalizedDomain]; exists {
				return fmt.Errorf(
					"environment %s: domain conflict - domain '%s' is used by both service '%s' and service '%s'\n"+
						"  Each domain can only be assigned to one service.\n"+
						"  Suggestion: Remove the duplicate domain from one of the services or use different domains.",
					envName, domain, existingService, serviceName,
				)
			}

			domainToService[normalizedDomain] = serviceName
		}

		// Check redirect domains
		for _, redirectDomain := range service.Proxy.GetRedirectDomains() {
			normalizedDomain := strings.ToLower(redirectDomain)

			if existingService, exists := domainToService[normalizedDomain]; exists {
				return fmt.Errorf(
					"environment %s: domain conflict - redirect domain '%s' (service '%s') conflicts with domain in service '%s'\n"+
						"  Each domain can only be assigned to one service.\n"+
						"  Suggestion: Remove the duplicate domain from one of the services.",
					envName, redirectDomain, serviceName, existingService,
				)
			}

			domainToService[normalizedDomain] = serviceName + " (redirect)"
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
