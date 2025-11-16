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
		return fmt.Errorf("server %s: SSH key not found: %s", name, server.SSHKey)
	}

	return nil
}

func validateService(name string, service *ServiceConfig, cfg *Config) error {
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

	return nil
}

func validateProxy(serviceName string, proxy *ProxyConfig) error {
	if len(proxy.Domains) == 0 {
		return fmt.Errorf("service %s: proxy configured but no domains specified", serviceName)
	}

	// Trim spaces from domains and validate
	for i, domain := range proxy.Domains {
		trimmed := strings.TrimSpace(domain)
		proxy.Domains[i] = trimmed // Update with trimmed version
		if !isValidDomain(trimmed) {
			return fmt.Errorf("service %s: invalid domain: %s", serviceName, trimmed)
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
	return isValidHostname(domain)
}

// validateDomainUniqueness checks for duplicate domains across all services in an environment
func validateDomainUniqueness(envName string, env *EnvironmentConfig) error {
	domainToService := make(map[string]string)

	for serviceName, service := range env.Services {
		if service.Proxy == nil {
			continue
		}

		for _, domain := range service.Proxy.Domains {
			// Normalize domain for comparison (lowercase)
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
	}

	return nil
}
