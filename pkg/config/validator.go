package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
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

	// Validate servers
	if len(cfg.Servers) == 0 {
		return fmt.Errorf("at least one server must be configured")
	}
	for name, server := range cfg.Servers {
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
		if err := validateService(serviceName, &service, cfg); err != nil {
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
			fmt.Printf("⚠ Security Warning: server %s has a hardcoded password.\n", name)
			fmt.Printf("  Consider using an environment variable instead:\n")
			fmt.Printf("    password: ${SSH_PASSWORD}\n")
			fmt.Printf("  Then set: export SSH_PASSWORD='your-password'\n\n")
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
	if !isValidServiceName(name) {
		return fmt.Errorf("service name '%s' is invalid: must start with a lowercase letter, contain only lowercase letters, numbers, hyphens, and underscores, and be 1-63 characters long", name)
	}

	// Must have either Build or Image, but not both
	if service.Build == "" && service.Image == "" {
		return fmt.Errorf("service %s: either 'build' or 'image' is required", name)
	}
	if service.Build != "" && service.Image != "" {
		return fmt.Errorf("service %s: cannot specify both 'build' and 'image'", name)
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
			fmt.Printf("Warning: No Dockerfile found in %s\n", buildPath)
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

	// Set default replicas
	if service.Replicas == 0 {
		service.Replicas = 1
	}
	if service.Replicas < 0 {
		return fmt.Errorf("service %s: replicas cannot be negative", name)
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

	// Validate health check if configured
	if service.HealthCheck.Path != "" {
		if service.Port == 0 {
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
	}

	// Validate deployment strategy
	if service.Deploy.Strategy == "" {
		service.Deploy.Strategy = "blue-green" // Default
	}
	if service.Deploy.Strategy != "blue-green" && service.Deploy.Strategy != "rolling" {
		return fmt.Errorf("service %s: invalid deployment strategy: %s", name, service.Deploy.Strategy)
	}

	// Validate hooks if configured
	if service.Hooks != nil {
		totalHooks := len(service.Hooks.PreBuild) + len(service.Hooks.PostBuild) +
			len(service.Hooks.PreDeploy) + len(service.Hooks.PostDeploy) + len(service.Hooks.PostStart)
		if totalHooks == 0 {
			return fmt.Errorf("service %s: hooks configured but no commands specified", name)
		}
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

	// Validate init commands
	if len(service.Init) > 0 {
		if err := validateInitCommands(name, service.Init); err != nil {
			return err
		}
	}

	return nil
}

func validateProxy(serviceName string, proxy *ProxyConfig) error {
	// Check that at least one domain is configured (either Domain or Domains)
	if proxy.Domain == "" && len(proxy.Domains) == 0 {
		return fmt.Errorf("service %s: proxy configured but no domain specified (use 'domain' or 'domains')", serviceName)
	}

	// Validate and trim primary domain
	if proxy.Domain != "" {
		trimmed := strings.TrimSpace(proxy.Domain)
		proxy.Domain = trimmed
		if !isValidDomain(trimmed) {
			return fmt.Errorf("service %s: invalid primary domain: %s", serviceName, trimmed)
		}
	}

	// Validate and trim domains in Domains array
	for i, domain := range proxy.Domains {
		trimmed := strings.TrimSpace(domain)
		proxy.Domains[i] = trimmed
		if !isValidDomain(trimmed) {
			return fmt.Errorf("service %s: invalid domain: %s", serviceName, trimmed)
		}
	}

	// Validate redirect domains
	primaryDomain := proxy.GetPrimaryDomain()
	for i, redirectDomain := range proxy.RedirectFrom {
		trimmed := strings.TrimSpace(redirectDomain)
		proxy.RedirectFrom[i] = trimmed

		if !isValidDomain(trimmed) {
			return fmt.Errorf("service %s: invalid redirect domain: %s", serviceName, trimmed)
		}

		// Ensure redirect domain is not the same as primary domain
		if strings.EqualFold(trimmed, primaryDomain) {
			return fmt.Errorf("service %s: redirect domain '%s' cannot be the same as primary domain", serviceName, trimmed)
		}

		// Ensure redirect domain is not duplicated in Domains array
		for _, d := range proxy.GetAllDomains() {
			if strings.EqualFold(trimmed, d) {
				return fmt.Errorf("service %s: redirect domain '%s' is already in the domains list - remove it from domains or redirectFrom", serviceName, trimmed)
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

	// Check for dangerous characters that could cause issues in Traefik/shell
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

	// Check for Traefik-specific regex metacharacters that could affect routing
	// These are valid in hostnames but could cause issues if passed to HostRegexp
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

// isValidServiceName validates that a service name is safe
func isValidServiceName(name string) bool {
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
				"nfs":    true,
				"tmpfs":  true,
				"cifs":   true,
				"btrfs":  true,
				"zfs":    true,
				"convoy": true,
				"rexray": true,
			}
			if !validDrivers[vol.Driver] {
				// Allow custom drivers, just warn
				fmt.Printf("Warning: volume '%s' uses non-standard driver '%s'\n", name, vol.Driver)
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

// validateInitCommands validates init commands for a service
func validateInitCommands(serviceName string, initCommands []string) error {
	if len(initCommands) == 0 {
		return nil
	}

	for i, cmd := range initCommands {
		if strings.TrimSpace(cmd) == "" {
			return fmt.Errorf("service %s: init command at index %d is empty", serviceName, i)
		}

		// Warn about potentially dangerous commands
		dangerousPatterns := []string{
			"rm -rf /",
			"mkfs",
			"dd if=",
			"> /dev/",
			"shutdown",
			"reboot",
			"init 0",
			"init 6",
		}
		lowerCmd := strings.ToLower(cmd)
		for _, pattern := range dangerousPatterns {
			if strings.Contains(lowerCmd, pattern) {
				fmt.Printf("Warning: service %s: init command contains potentially dangerous pattern '%s': %s\n",
					serviceName, pattern, cmd)
			}
		}
	}

	return nil
}
