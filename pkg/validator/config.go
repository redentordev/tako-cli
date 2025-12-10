package validator

import (
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
)

// ConfigValidator validates Tako CLI configuration
type ConfigValidator struct {
	errors   []string
	warnings []string
}

// New creates a new ConfigValidator
func New() *ConfigValidator {
	return &ConfigValidator{
		errors:   make([]string, 0),
		warnings: make([]string, 0),
	}
}

// ValidateConfig validates a complete configuration
func (v *ConfigValidator) ValidateConfig(cfg *config.Config) error {
	v.errors = make([]string, 0)
	v.warnings = make([]string, 0)

	// Validate project
	v.validateProject(&cfg.Project)

	// Validate servers
	v.validateServers(cfg.Servers)

	// Validate environments
	v.validateEnvironments(cfg.Environments, cfg.Servers)

	// Return first error if any
	if len(v.errors) > 0 {
		return fmt.Errorf("configuration validation failed:\n  - %s", strings.Join(v.errors, "\n  - "))
	}

	return nil
}

// GetWarnings returns all validation warnings
func (v *ConfigValidator) GetWarnings() []string {
	return v.warnings
}

// validateProject validates project configuration
func (v *ConfigValidator) validateProject(project *config.ProjectConfig) {
	if project.Name == "" {
		v.errors = append(v.errors, "project.name is required")
	} else if !isValidName(project.Name) {
		v.errors = append(v.errors, fmt.Sprintf("project.name '%s' contains invalid characters (use alphanumeric, hyphens, underscores)", project.Name))
	}

	if project.Version == "" {
		v.warnings = append(v.warnings, "project.version is not set, defaulting to '1.0.0'")
	}
}

// validateServers validates server configuration
func (v *ConfigValidator) validateServers(servers map[string]config.ServerConfig) {
	if len(servers) == 0 {
		v.errors = append(v.errors, "at least one server must be configured")
		return
	}

	for name, server := range servers {
		if server.Host == "" {
			v.errors = append(v.errors, fmt.Sprintf("server '%s': host is required", name))
		} else if !isValidHostOrIP(server.Host) {
			v.errors = append(v.errors, fmt.Sprintf("server '%s': invalid host '%s'", name, server.Host))
		}

		if server.User == "" {
			v.warnings = append(v.warnings, fmt.Sprintf("server '%s': user not set, defaulting to 'root'", name))
		}

		if server.Port == 0 {
			v.warnings = append(v.warnings, fmt.Sprintf("server '%s': port not set, defaulting to 22", name))
		} else if server.Port < 1 || server.Port > 65535 {
			v.errors = append(v.errors, fmt.Sprintf("server '%s': invalid port %d (must be 1-65535)", name, server.Port))
		}
	}
}

// validateEnvironments validates environment configuration
func (v *ConfigValidator) validateEnvironments(environments map[string]config.EnvironmentConfig, servers map[string]config.ServerConfig) {
	if len(environments) == 0 {
		v.errors = append(v.errors, "at least one environment must be configured")
		return
	}

	for envName, env := range environments {
		// Validate servers exist
		if len(env.Servers) == 0 {
			v.errors = append(v.errors, fmt.Sprintf("environment '%s': no servers configured", envName))
		}

		for _, serverName := range env.Servers {
			if _, exists := servers[serverName]; !exists {
				v.errors = append(v.errors, fmt.Sprintf("environment '%s': server '%s' not found in servers configuration", envName, serverName))
			}
		}

		// Validate services
		if len(env.Services) == 0 {
			v.errors = append(v.errors, fmt.Sprintf("environment '%s': no services configured", envName))
		}

		for serviceName, service := range env.Services {
			v.validateService(envName, serviceName, service)
		}
	}
}

// validateService validates a service configuration
func (v *ConfigValidator) validateService(envName, serviceName string, service config.ServiceConfig) {
	prefix := fmt.Sprintf("environment '%s', service '%s'", envName, serviceName)

	// Validate build or image
	if service.Build == "" && service.Image == "" {
		v.errors = append(v.errors, fmt.Sprintf("%s: either 'build' or 'image' must be specified", prefix))
	}

	// Validate port
	if service.Port != 0 && (service.Port < 1 || service.Port > 65535) {
		v.errors = append(v.errors, fmt.Sprintf("%s: invalid port %d (must be 1-65535)", prefix, service.Port))
	}

	// Validate proxy configuration
	if service.Proxy != nil {
		v.validateProxyConfig(prefix, service.Proxy)
	}

	// Validate health check
	if service.HealthCheck.Path != "" {
		if !strings.HasPrefix(service.HealthCheck.Path, "/") {
			v.warnings = append(v.warnings, fmt.Sprintf("%s: healthCheck.path should start with '/'", prefix))
		}
	}

	// Validate replicas
	if service.Replicas < 0 {
		v.errors = append(v.errors, fmt.Sprintf("%s: replicas cannot be negative", prefix))
	}
}

// validateProxyConfig validates proxy configuration including domain redirects
func (v *ConfigValidator) validateProxyConfig(prefix string, proxy *config.ProxyConfig) {
	// Check that at least one domain is configured
	primaryDomain := proxy.GetPrimaryDomain()
	if primaryDomain == "" && len(proxy.Domains) == 0 {
		v.errors = append(v.errors, fmt.Sprintf("%s: proxy.domain or proxy.domains is required when proxy is configured", prefix))
	}

	// Validate primary domain
	if proxy.Domain != "" && !isValidDomain(proxy.Domain) {
		v.errors = append(v.errors, fmt.Sprintf("%s: invalid proxy.domain '%s'", prefix, proxy.Domain))
	}

	// Validate legacy domains array
	for _, domain := range proxy.Domains {
		if !isValidDomain(domain) {
			v.errors = append(v.errors, fmt.Sprintf("%s: invalid domain '%s' in proxy.domains", prefix, domain))
		}
	}

	// Validate redirect domains
	for _, redirectDomain := range proxy.RedirectFrom {
		if !isValidDomain(redirectDomain) {
			v.errors = append(v.errors, fmt.Sprintf("%s: invalid redirect domain '%s' in proxy.redirectFrom", prefix, redirectDomain))
		}
	}

	// Check for redirect domains without primary domain
	if len(proxy.RedirectFrom) > 0 && primaryDomain == "" {
		v.errors = append(v.errors, fmt.Sprintf("%s: proxy.redirectFrom requires proxy.domain or proxy.domains to be set", prefix))
	}

	// Check for duplicate domains between primary/domains and redirectFrom
	allServingDomains := proxy.GetAllDomains()
	for _, redirectDomain := range proxy.RedirectFrom {
		for _, servingDomain := range allServingDomains {
			if redirectDomain == servingDomain {
				v.errors = append(v.errors, fmt.Sprintf("%s: domain '%s' cannot be both a serving domain and a redirect domain", prefix, redirectDomain))
			}
		}
	}

	// Validate email
	if proxy.Email != "" && !isValidEmail(proxy.Email) {
		v.errors = append(v.errors, fmt.Sprintf("%s: invalid proxy.email '%s'", prefix, proxy.Email))
	}
}

// isValidName checks if a name is valid for Docker/project names
func isValidName(name string) bool {
	// Alphanumeric, hyphens, underscores, periods
	match, _ := regexp.MatchString(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`, name)
	return match
}

// isValidHostOrIP checks if a string is a valid hostname or IP
func isValidHostOrIP(host string) bool {
	// Check if it's a valid IP
	if net.ParseIP(host) != nil {
		return true
	}

	// Check if it's a valid hostname
	match, _ := regexp.MatchString(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$`, host)
	return match
}

// isValidDomain checks if a string is a valid domain name
func isValidDomain(domain string) bool {
	// Similar to hostname but allows wildcards
	if strings.HasPrefix(domain, "*.") {
		domain = domain[2:]
	}
	return isValidHostOrIP(domain)
}

// isValidEmail checks if a string is a valid email
func isValidEmail(email string) bool {
	match, _ := regexp.MatchString(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`, email)
	return match
}
