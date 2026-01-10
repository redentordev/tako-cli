package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config represents the main configuration structure
type Config struct {
	Schema         string                       `yaml:"$schema,omitempty" json:"$schema,omitempty"` // JSON Schema reference
	Project        ProjectConfig                `yaml:"project" json:"project"`
	Infrastructure *InfrastructureConfig        `yaml:"infrastructure,omitempty" json:"infrastructure,omitempty"` // Cloud infrastructure provisioning
	Deployment     *DeploymentConfig            `yaml:"deployment,omitempty" json:"deployment,omitempty"`
	Notifications  *NotificationsConfig         `yaml:"notifications,omitempty" json:"notifications,omitempty"`
	Storage        *StorageConfig               `yaml:"storage,omitempty" json:"storage,omitempty"`
	Volumes        map[string]VolumeConfig      `yaml:"volumes,omitempty" json:"volumes,omitempty"` // Top-level volume definitions
	Servers        map[string]ServerConfig      `yaml:"servers" json:"servers"`
	Environments   map[string]EnvironmentConfig `yaml:"environments" json:"environments"`
}

// InfrastructureConfig defines cloud infrastructure provisioning settings
type InfrastructureConfig struct {
	Provider    string                        `yaml:"provider" json:"provider"`                           // digitalocean, hetzner, aws, linode
	Region      string                        `yaml:"region" json:"region"`                               // Provider-specific region (or friendly name like "nyc", "frankfurt")
	Credentials InfraCredentialsConfig        `yaml:"credentials,omitempty" json:"credentials,omitempty"` // Provider credentials (can use env vars)
	SSHKey      string                        `yaml:"ssh_key,omitempty" json:"ssh_key,omitempty"`         // Local SSH key path for connecting to provisioned servers
	SSHUser     string                        `yaml:"ssh_user,omitempty" json:"ssh_user,omitempty"`       // SSH user (default: root)
	Defaults    *InfraDefaultsConfig          `yaml:"defaults,omitempty" json:"defaults,omitempty"`       // Default values for servers
	Servers     map[string]InfraServerSpec    `yaml:"servers" json:"servers"`                             // Server definitions
	Networking  *InfraNetworkingConfig        `yaml:"networking,omitempty" json:"networking,omitempty"`
	Storage     *InfraStorageConfig           `yaml:"storage,omitempty" json:"storage,omitempty"`   // Object storage (S3-compatible)
	CDN         *InfraCDNConfig               `yaml:"cdn,omitempty" json:"cdn,omitempty"`           // CDN configuration
	State       *InfraStateConfig             `yaml:"state,omitempty" json:"state,omitempty"`       // State backend configuration (for multi-machine sync)
}

// InfraStateConfig defines where Pulumi state is stored
type InfraStateConfig struct {
	Backend   string `yaml:"backend,omitempty" json:"backend,omitempty"`     // "local" (default), "s3", or "manager"
	Bucket    string `yaml:"bucket,omitempty" json:"bucket,omitempty"`       // S3 bucket name for s3 backend
	Region    string `yaml:"region,omitempty" json:"region,omitempty"`       // S3 region (defaults to infra region)
	Endpoint  string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`   // Custom S3 endpoint (for DO Spaces, Linode Object Storage)
	Encrypt   bool   `yaml:"encrypt,omitempty" json:"encrypt,omitempty"`     // Enable state encryption (default: true for remote backends)
	AccessKey string `yaml:"accessKey,omitempty" json:"accessKey,omitempty"` // S3 access key (defaults to provider credentials)
	SecretKey string `yaml:"secretKey,omitempty" json:"secretKey,omitempty"` // S3 secret key (defaults to provider credentials)
}

// InfraCredentialsConfig holds provider authentication
type InfraCredentialsConfig struct {
	Token     string `yaml:"token,omitempty" json:"token,omitempty"`         // API token (DO, Hetzner, Linode) - defaults to env var
	AccessKey string `yaml:"accessKey,omitempty" json:"accessKey,omitempty"` // AWS access key
	SecretKey string `yaml:"secretKey,omitempty" json:"secretKey,omitempty"` // AWS secret key
	ProjectID string `yaml:"projectId,omitempty" json:"projectId,omitempty"` // Project/Account ID if needed
}

// InfraDefaultsConfig provides default values for server specs
type InfraDefaultsConfig struct {
	Size    string   `yaml:"size,omitempty" json:"size,omitempty"`         // Default size: small, medium, large, xlarge
	Image   string   `yaml:"image,omitempty" json:"image,omitempty"`       // Default OS image (auto-detected if empty)
	SSHKeys []string `yaml:"ssh_keys,omitempty" json:"ssh_keys,omitempty"` // Default SSH key fingerprints for cloud provider
	Tags    []string `yaml:"tags,omitempty" json:"tags,omitempty"`         // Default tags applied to all servers
}

// InfraServerSpec defines a server to be provisioned
type InfraServerSpec struct {
	Provider string   `yaml:"provider,omitempty" json:"provider,omitempty"` // Override provider (for multi-cloud setups)
	Region   string   `yaml:"region,omitempty" json:"region,omitempty"`     // Override region (for multi-region/multi-cloud)
	Size     string   `yaml:"size,omitempty" json:"size,omitempty"`         // Size: small, medium, large, xlarge (or provider-specific)
	Image    string   `yaml:"image,omitempty" json:"image,omitempty"`       // OS image (uses default if empty)
	Role     string   `yaml:"role,omitempty" json:"role,omitempty"`         // "manager" or "worker" (default: worker)
	Count    int      `yaml:"count,omitempty" json:"count,omitempty"`       // Number of servers (default: 1)
	SSHKeys  []string `yaml:"ssh_keys,omitempty" json:"ssh_keys,omitempty"` // SSH key fingerprints (uses defaults if empty)
	Tags     []string `yaml:"tags,omitempty" json:"tags,omitempty"`         // Server tags/labels
	UserData string   `yaml:"userData,omitempty" json:"userData,omitempty"` // Cloud-init script
}

// InfraNetworkingConfig defines network resources
type InfraNetworkingConfig struct {
	VPC      *InfraVPCConfig      `yaml:"vpc,omitempty" json:"vpc,omitempty"`
	Firewall *InfraFirewallConfig `yaml:"firewall,omitempty" json:"firewall,omitempty"`
}

// InfraVPCConfig defines VPC/private network settings
type InfraVPCConfig struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Name    string `yaml:"name,omitempty" json:"name,omitempty"`         // VPC name (auto-generated if empty)
	IPRange string `yaml:"ip_range,omitempty" json:"ip_range,omitempty"` // CIDR (e.g., 10.0.0.0/16)
}

// InfraFirewallConfig defines firewall rules
type InfraFirewallConfig struct {
	Enabled bool                `yaml:"enabled" json:"enabled"`
	Name    string              `yaml:"name,omitempty" json:"name,omitempty"` // Firewall name (auto-generated if empty)
	Rules   []InfraFirewallRule `yaml:"rules,omitempty" json:"rules,omitempty"`
}

// InfraFirewallRule defines a single firewall rule
type InfraFirewallRule struct {
	Protocol string   `yaml:"protocol" json:"protocol"`                 // tcp, udp, icmp
	Ports    []int    `yaml:"ports,omitempty" json:"ports,omitempty"`   // Port numbers (empty = all)
	Sources  []string `yaml:"sources,omitempty" json:"sources,omitempty"` // CIDR addresses (e.g., 0.0.0.0/0)
}

// InfraStorageConfig defines object storage (S3-compatible buckets)
type InfraStorageConfig struct {
	Buckets map[string]InfraBucketSpec `yaml:"buckets,omitempty" json:"buckets,omitempty"` // Named bucket definitions
}

// InfraBucketSpec defines a storage bucket
type InfraBucketSpec struct {
	Region string `yaml:"region,omitempty" json:"region,omitempty"` // Override region (defaults to infra region)
	ACL    string `yaml:"acl,omitempty" json:"acl,omitempty"`       // Access: private, public-read (default: private)
	CORS   bool   `yaml:"cors,omitempty" json:"cors,omitempty"`     // Enable CORS for web access
}

// InfraCDNConfig defines CDN configuration
type InfraCDNConfig struct {
	Enabled bool                      `yaml:"enabled" json:"enabled"`
	Origins map[string]InfraCDNOrigin `yaml:"origins,omitempty" json:"origins,omitempty"` // Named CDN origins
}

// InfraCDNOrigin defines a CDN origin (bucket or custom)
type InfraCDNOrigin struct {
	Bucket string `yaml:"bucket,omitempty" json:"bucket,omitempty"` // Reference to storage bucket
	Domain string `yaml:"domain,omitempty" json:"domain,omitempty"` // Custom origin domain
	TTL    int    `yaml:"ttl,omitempty" json:"ttl,omitempty"`       // Cache TTL in seconds (default: 86400)
}

// VolumeConfig defines a named volume configuration (Docker Compose style)
type VolumeConfig struct {
	Driver     string            `yaml:"driver,omitempty" json:"driver,omitempty"`           // Volume driver (default: "local")
	DriverOpts map[string]string `yaml:"driver_opts,omitempty" json:"driver_opts,omitempty"` // Driver-specific options
	Labels     map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`           // Volume labels
	External   bool              `yaml:"external,omitempty" json:"external,omitempty"`       // If true, volume must already exist
	Name       string            `yaml:"name,omitempty" json:"name,omitempty"`               // Override the auto-generated name (opt-out of prefix)
}

// StorageConfig defines shared storage configuration
type StorageConfig struct {
	NFS *NFSConfig `yaml:"nfs,omitempty" json:"nfs,omitempty"`
}

// NFSConfig defines NFS shared storage settings
type NFSConfig struct {
	Enabled bool              `yaml:"enabled" json:"enabled"`
	Server  string            `yaml:"server,omitempty" json:"server,omitempty"` // "auto" = use manager node, or specify server name
	Exports []NFSExportConfig `yaml:"exports,omitempty" json:"exports,omitempty"`
}

// NFSExportConfig defines an NFS export/share
type NFSExportConfig struct {
	Name    string   `yaml:"name" json:"name"`                       // Name of the export (used in volume references)
	Path    string   `yaml:"path" json:"path"`                       // Path on the NFS server
	Size    string   `yaml:"size,omitempty" json:"size,omitempty"`   // Optional: expected size for provisioning hints
	Options []string `yaml:"options,omitempty" json:"options,omitempty"` // NFS export options (e.g., rw, sync, no_subtree_check)
}

// NotificationsConfig defines notification settings
type NotificationsConfig struct {
	Slack   string `yaml:"slack,omitempty" json:"slack,omitempty"`     // Slack webhook URL
	Discord string `yaml:"discord,omitempty" json:"discord,omitempty"` // Discord webhook URL
	Webhook string `yaml:"webhook,omitempty" json:"webhook,omitempty"` // Generic webhook URL
}

// ProjectConfig defines project metadata
type ProjectConfig struct {
	Name    string `yaml:"name" json:"name"`
	Version string `yaml:"version" json:"version"`
}

// DeploymentConfig defines deployment optimization settings
type DeploymentConfig struct {
	Strategy string          `yaml:"strategy,omitempty" json:"strategy,omitempty"` // "parallel" or "sequential" (default: sequential)
	Parallel *ParallelConfig `yaml:"parallel,omitempty" json:"parallel,omitempty"`
	Cache    *CacheConfig    `yaml:"cache,omitempty" json:"cache,omitempty"`
}

// ParallelConfig defines parallel deployment settings
type ParallelConfig struct {
	MaxConcurrentBuilds  int    `yaml:"maxConcurrentBuilds,omitempty" json:"maxConcurrentBuilds,omitempty"`   // Default: 4
	MaxConcurrentDeploys int    `yaml:"maxConcurrentDeploys,omitempty" json:"maxConcurrentDeploys,omitempty"` // Default: 4
	Strategy             string `yaml:"strategy,omitempty" json:"strategy,omitempty"`                        // "dependency-aware" (default), "resource-aware", "round-robin"
}

// CacheConfig defines build caching settings
type CacheConfig struct {
	Enabled   bool   `yaml:"enabled,omitempty" json:"enabled,omitempty"`     // Enable build caching (default: true)
	Type      string `yaml:"type,omitempty" json:"type,omitempty"`           // "local" (default), "registry"
	Retention string `yaml:"retention,omitempty" json:"retention,omitempty"` // Cache retention period (e.g., "7d")
}

// ServerConfig defines server connection details
type ServerConfig struct {
	Host     string            `yaml:"host" json:"host"`
	User     string            `yaml:"user" json:"user"`
	Port     int               `yaml:"port,omitempty" json:"port,omitempty"`
	SSHKey   string            `yaml:"sshKey,omitempty" json:"sshKey,omitempty"`     // Path to SSH private key (mutually exclusive with password)
	Password string            `yaml:"password,omitempty" json:"password,omitempty"` // SSH password (mutually exclusive with sshKey, use env var for security)
	Role     string            `yaml:"role,omitempty" json:"role,omitempty"`         // "manager" or "worker" (auto-detect if not specified)
	Labels   map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`     // Custom labels for server selection
}

// ServiceConfig defines service deployment settings
type ServiceConfig struct {
	// Build or Image (mutually exclusive)
	Build string `yaml:"build,omitempty" json:"build,omitempty"` // Path to build context (auto-detects Dockerfile)
	Image string `yaml:"image,omitempty" json:"image,omitempty"` // Pre-built image (for postgres, redis, etc)

	// Basic settings
	Port     int               `yaml:"port,omitempty" json:"port,omitempty"`
	Command  string            `yaml:"command,omitempty" json:"command,omitempty"`
	Replicas int               `yaml:"replicas,omitempty" json:"replicas,omitempty"` // Default: 1
	Restart  string            `yaml:"restart,omitempty" json:"restart,omitempty"`   // Docker restart policy (always, unless-stopped, on-failure, no)
	Env      map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	EnvFile  string            `yaml:"envFile,omitempty" json:"envFile,omitempty"` // Path to .env file (e.g., .env.production)

	// Secrets: Can be either string array (new Tako secrets) or SecretConfig array (Docker Swarm secrets)
	// String format: ["DATABASE_URL", "JWT_SECRET"] or ["VAR_NAME:SECRET_KEY"]
	// SecretConfig format: [{name: "db_pass", source: "env:DB_PASSWORD"}]
	Secrets       []string       `yaml:"secrets,omitempty" json:"secrets,omitempty"`             // Tako secrets from .tako/secrets files
	DockerSecrets []SecretConfig `yaml:"dockerSecrets,omitempty" json:"dockerSecrets,omitempty"` // Docker Swarm secrets (for backward compatibility)
	Volumes       []string       `yaml:"volumes,omitempty" json:"volumes,omitempty"`

	// Service type flags
	Persistent bool `yaml:"persistent,omitempty" json:"persistent,omitempty"` // Don't remove on redeploy (databases, caches)

	// Per-service proxy settings (if present, service is exposed publicly)
	Proxy *ProxyConfig `yaml:"proxy,omitempty" json:"proxy,omitempty"`

	// Load balancing (for multi-replica services)
	LoadBalancer LoadBalancerConfig `yaml:"loadBalancer,omitempty" json:"loadBalancer,omitempty"`

	// Health checks
	HealthCheck HealthCheckConfig `yaml:"healthCheck,omitempty" json:"healthCheck,omitempty"`

	// Deployment strategy
	Deploy DeployConfig `yaml:"deploy,omitempty" json:"deploy,omitempty"`

	// Per-service hooks
	Hooks *HooksConfig `yaml:"hooks,omitempty" json:"hooks,omitempty"`

	// Per-service backup
	Backup *BackupConfig `yaml:"backup,omitempty" json:"backup,omitempty"`

	// Per-service monitoring
	Monitoring *MonitoringConfig `yaml:"monitoring,omitempty" json:"monitoring,omitempty"`

	// Cross-project networking
	Export  bool     `yaml:"export,omitempty" json:"export,omitempty"`   // Export this service to other projects
	Imports []string `yaml:"imports,omitempty" json:"imports,omitempty"` // Import services from other projects (format: "project.service")

	// Placement configuration (for Swarm multi-server deployments)
	Placement *PlacementConfig `yaml:"placement,omitempty" json:"placement,omitempty"` // Where to run service replicas

	// Service dependencies (controls deployment order)
	DependsOn []string `yaml:"dependsOn,omitempty" json:"dependsOn,omitempty"` // List of service names this service depends on

	// Init commands (run before service starts, useful for permissions)
	Init []string `yaml:"init,omitempty" json:"init,omitempty"` // Commands to run before service starts (e.g., chown, chmod)
}

// SecretConfig defines a Docker secret
type SecretConfig struct {
	Name   string `yaml:"name" json:"name"`                       // Secret name (e.g., "db_password")
	Source string `yaml:"source,omitempty" json:"source,omitempty"` // Source: "env:VAR" or "file:path" (default: env:NAME)
	Target string `yaml:"target,omitempty" json:"target,omitempty"` // Target path in container (default: /run/secrets/{name})
}

// HealthCheckConfig defines health check settings
type HealthCheckConfig struct {
	Path        string `yaml:"path" json:"path"`
	Interval    string `yaml:"interval" json:"interval"`
	Timeout     string `yaml:"timeout" json:"timeout"`
	Retries     int    `yaml:"retries" json:"retries"`
	StartPeriod string `yaml:"startPeriod,omitempty" json:"startPeriod,omitempty"` // Grace period before starting checks
}

// DeployConfig defines deployment strategy
type DeployConfig struct {
	Strategy       string `yaml:"strategy" json:"strategy"` // blue-green or rolling
	MaxUnavailable int    `yaml:"maxUnavailable,omitempty" json:"maxUnavailable,omitempty"`
}

// LoadBalancerConfig defines load balancing settings
type LoadBalancerConfig struct {
	Strategy    string                  `yaml:"strategy" json:"strategy"` // round_robin, least_conn, ip_hash, random
	HealthCheck LoadBalancerHealthCheck `yaml:"healthCheck,omitempty" json:"healthCheck,omitempty"`
}

// LoadBalancerHealthCheck defines load balancer health check settings
type LoadBalancerHealthCheck struct {
	Enabled  bool   `yaml:"enabled" json:"enabled"`
	Path     string `yaml:"path" json:"path"`
	Interval string `yaml:"interval" json:"interval"`
}

// ProxyConfig defines per-service Traefik reverse proxy settings
type ProxyConfig struct {
	// Domain is the primary domain where traffic is served (recommended)
	// Use this with RedirectFrom for cleaner configuration
	Domain string `yaml:"domain,omitempty" json:"domain,omitempty"`

	// RedirectFrom specifies domains that should redirect to the primary Domain
	// These domains will get their own TLS certificates and 301 redirect to Domain
	// Example: ["www.example.com", "old.example.com"] -> redirects to "example.com"
	RedirectFrom []string `yaml:"redirectFrom,omitempty" json:"redirectFrom,omitempty"`

	// Domains is the legacy field for backward compatibility
	// If Domain is not set, the first domain in Domains is treated as primary
	// Deprecated: Use Domain + RedirectFrom instead for clearer configuration
	Domains []string `yaml:"domains,omitempty" json:"domains,omitempty"`

	Email string    `yaml:"email,omitempty" json:"email,omitempty"` // Email for Let's Encrypt
	TLS   TLSConfig `yaml:"tls,omitempty" json:"tls,omitempty"`
}

// GetPrimaryDomain returns the primary domain for this service
func (p *ProxyConfig) GetPrimaryDomain() string {
	if p.Domain != "" {
		return p.Domain
	}
	if len(p.Domains) > 0 {
		return p.Domains[0]
	}
	return ""
}

// GetAllDomains returns all domains (primary + additional domains from Domains array)
// excluding redirect domains
func (p *ProxyConfig) GetAllDomains() []string {
	domains := []string{}

	if p.Domain != "" {
		domains = append(domains, p.Domain)
	}

	// Add domains from legacy Domains array (but skip the first if Domain is set)
	for i, d := range p.Domains {
		if p.Domain != "" || i > 0 {
			// Avoid duplicates
			isDuplicate := false
			for _, existing := range domains {
				if existing == d {
					isDuplicate = true
					break
				}
			}
			if !isDuplicate {
				domains = append(domains, d)
			}
		} else if i == 0 && p.Domain == "" {
			domains = append(domains, d)
		}
	}

	return domains
}

// GetRedirectDomains returns all domains that should redirect to the primary domain
func (p *ProxyConfig) GetRedirectDomains() []string {
	return p.RedirectFrom
}

// HasRedirects returns true if there are redirect domains configured
func (p *ProxyConfig) HasRedirects() bool {
	return len(p.RedirectFrom) > 0
}

// TLSConfig defines TLS settings
type TLSConfig struct {
	Provider string `yaml:"provider,omitempty" json:"provider,omitempty"` // letsencrypt, zerossl (default: letsencrypt)
	Staging  bool   `yaml:"staging,omitempty" json:"staging,omitempty"`
}

// BackupConfig defines per-service backup settings
type BackupConfig struct {
	Schedule string `yaml:"schedule" json:"schedule"` // cron format (e.g., "0 2 * * *")
	Retain   int    `yaml:"retain" json:"retain"`     // days to retain backups
}

// HooksConfig defines per-service pre/post deployment hooks
type HooksConfig struct {
	PreBuild   []string `yaml:"preBuild,omitempty" json:"preBuild,omitempty"`     // Before building Docker image
	PostBuild  []string `yaml:"postBuild,omitempty" json:"postBuild,omitempty"`   // After building Docker image
	PreDeploy  []string `yaml:"preDeploy,omitempty" json:"preDeploy,omitempty"`   // Before deploying service to swarm
	PostDeploy []string `yaml:"postDeploy,omitempty" json:"postDeploy,omitempty"` // After deploying service to swarm
	PostStart  []string `yaml:"postStart,omitempty" json:"postStart,omitempty"`   // After service is running (can use docker exec)
}

// MonitoringConfig defines per-service monitoring settings
type MonitoringConfig struct {
	Enabled   bool   `yaml:"enabled" json:"enabled"`                       // Enable monitoring for this service
	Interval  string `yaml:"interval,omitempty" json:"interval,omitempty"` // Check interval (e.g., "60s")
	Webhook   string `yaml:"webhook,omitempty" json:"webhook,omitempty"`   // Webhook URL for alerts
	CheckType string `yaml:"checkType,omitempty" json:"checkType,omitempty"` // "http" or "container" (default: auto-detect)
}

// EnvironmentConfig defines an environment (production, staging, etc.)
type EnvironmentConfig struct {
	Servers        []string                 `yaml:"servers" json:"servers"`                                 // List of server names to use
	ServerSelector *ServerSelector          `yaml:"serverSelector,omitempty" json:"serverSelector,omitempty"` // Label-based server selection
	Labels         map[string]string        `yaml:"labels,omitempty" json:"labels,omitempty"`               // Environment labels for Docker nodes
	Services       map[string]ServiceConfig `yaml:"services" json:"services"`                               // Services to deploy in this environment
}

// ServerSelector defines label-based server selection
type ServerSelector struct {
	Labels map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"` // Match servers with these labels
	Any    bool              `yaml:"any,omitempty" json:"any,omitempty"`       // Select any available server
}

// PlacementConfig defines where service replicas should run
type PlacementConfig struct {
	Strategy    string   `yaml:"strategy,omitempty" json:"strategy,omitempty"`       // "spread", "pinned", "any"
	Servers     []string `yaml:"servers,omitempty" json:"servers,omitempty"`         // Pin to specific servers (for "pinned" strategy)
	Constraints []string `yaml:"constraints,omitempty" json:"constraints,omitempty"` // Docker Swarm constraints (e.g., "node.labels.type==high-memory")
	Preferences []string `yaml:"preferences,omitempty" json:"preferences,omitempty"` // Docker Swarm placement preferences (e.g., "spread=node.labels.region")
}

// GetServiceType returns the auto-detected service type
func (s *ServiceConfig) GetServiceType() string {
	if s.Persistent {
		return "persistent" // Database, cache, etc.
	}
	if s.Proxy != nil {
		return "public" // Public web service
	}
	if s.Port > 0 {
		return "internal" // Internal API
	}
	return "worker" // Background worker
}

// IsPublic returns true if service should be exposed publicly
func (s *ServiceConfig) IsPublic() bool {
	return s.Proxy != nil
}

// IsInternal returns true if service is internal-only
func (s *ServiceConfig) IsInternal() bool {
	return s.Port > 0 && s.Proxy == nil
}

// IsWorker returns true if service is a background worker
func (s *ServiceConfig) IsWorker() bool {
	return s.Port == 0
}

// GetDefaultEnvironment returns the default environment name
// Returns "production" if it exists, otherwise the first environment
func (c *Config) GetDefaultEnvironment() string {
	// If only one environment, return it
	if len(c.Environments) == 1 {
		for name := range c.Environments {
			return name
		}
	}

	// Check if "production" exists
	if _, exists := c.Environments["production"]; exists {
		return "production"
	}

	// Return first environment (alphabetically)
	for name := range c.Environments {
		return name
	}

	return ""
}

// GetEnvironment retrieves a specific environment configuration
// If name is empty, returns the default environment
func (c *Config) GetEnvironment(name string) (*EnvironmentConfig, error) {
	if name == "" {
		name = c.GetDefaultEnvironment()
		if name == "" {
			return nil, fmt.Errorf("no environments configured")
		}
	}

	env, exists := c.Environments[name]
	if !exists {
		return nil, fmt.Errorf("environment '%s' not found", name)
	}
	return &env, nil
}

// GetServices returns services for a specific environment
func (c *Config) GetServices(envName string) (map[string]ServiceConfig, error) {
	env, err := c.GetEnvironment(envName)
	if err != nil {
		return nil, err
	}
	return env.Services, nil
}

// GetService returns a specific service from an environment
func (c *Config) GetService(envName string, serviceName string) (*ServiceConfig, error) {
	services, err := c.GetServices(envName)
	if err != nil {
		return nil, err
	}
	service, exists := services[serviceName]
	if !exists {
		return nil, fmt.Errorf("service '%s' not found in environment '%s'", serviceName, envName)
	}
	return &service, nil
}

// GetEnvironmentServers returns the list of servers for an environment
func (c *Config) GetEnvironmentServers(envName string) ([]string, error) {
	env, err := c.GetEnvironment(envName)
	if err != nil {
		return nil, err
	}

	// If specific servers are listed, return them
	if len(env.Servers) > 0 {
		return env.Servers, nil
	}

	// If server selector is configured, match servers by labels
	if env.ServerSelector != nil {
		if env.ServerSelector.Any {
			// Return all servers
			servers := make([]string, 0, len(c.Servers))
			for name := range c.Servers {
				servers = append(servers, name)
			}
			return servers, nil
		}

		// Match servers by labels
		var matchedServers []string
		for serverName, serverCfg := range c.Servers {
			if matchesLabels(serverCfg.Labels, env.ServerSelector.Labels) {
				matchedServers = append(matchedServers, serverName)
			}
		}
		if len(matchedServers) == 0 {
			return nil, fmt.Errorf("no servers match the selector labels for environment '%s'", envName)
		}
		return matchedServers, nil
	}

	return nil, fmt.Errorf("environment '%s' has no servers configured", envName)
}

// matchesLabels checks if server labels match all selector labels
func matchesLabels(serverLabels, selectorLabels map[string]string) bool {
	for key, value := range selectorLabels {
		if serverLabels[key] != value {
			return false
		}
	}
	return true
}

// GetManagerServer returns the manager server for a given environment
func (c *Config) GetManagerServer(envName string) (string, error) {
	servers, err := c.GetEnvironmentServers(envName)
	if err != nil {
		return "", err
	}

	// Look for explicitly marked manager
	for _, serverName := range servers {
		if server, exists := c.Servers[serverName]; exists {
			if server.Role == "manager" {
				return serverName, nil
			}
		}
	}

	// If no explicit manager, use first server in list
	if len(servers) > 0 {
		return servers[0], nil
	}

	return "", fmt.Errorf("no manager server found for environment '%s'", envName)
}

// IsMultiServer returns true if more than one server is configured
func (c *Config) IsMultiServer() bool {
	return len(c.Servers) > 1
}

// GetRegistryURL returns the auto-configured local registry URL
// Returns empty string for single-server deployments (no registry needed)
func (c *Config) GetRegistryURL() string {
	// TODO: Phase 2 - implement registry for multi-server deployments
	// For now, return empty string for all deployments
	// In Phase 2, detect multi-server and return registry URL on manager node
	return ""
}

// GetFullImageName returns the full image name with registry and environment tag
func (c *Config) GetFullImageName(serviceName string, envName string) string {
	registryURL := c.GetRegistryURL()

	// Environment-specific tag: project/service:version-env
	versionTag := fmt.Sprintf("%s-%s", c.Project.Version, envName)

	if registryURL != "" {
		// Multi-server setup with registry
		return fmt.Sprintf("%s/%s/%s:%s",
			registryURL,
			c.Project.Name,
			serviceName,
			versionTag,
		)
	}
	// Single-server setup without registry
	return fmt.Sprintf("%s/%s:%s",
		c.Project.Name,
		serviceName,
		versionTag,
	)
}

// expandEnvWithTrim expands environment variables and trims their values
// This handles Windows CMD quirk where "set VAR=value " includes trailing space
func expandEnvWithTrim(s string) string {
	return os.Expand(s, func(key string) string {
		value := os.Getenv(key)
		return strings.TrimSpace(value)
	})
}

// LoadConfig loads the configuration from a YAML or JSON file
func LoadConfig(configPath string) (*Config, error) {
	// Default to tako.yaml in current directory if not specified
	// Also check for tako.json if tako.yaml doesn't exist
	if configPath == "" {
		if _, err := os.Stat("tako.yaml"); err == nil {
			configPath = "tako.yaml"
		} else if _, err := os.Stat("tako.json"); err == nil {
			configPath = "tako.json"
		} else {
			configPath = "tako.yaml" // Default for error message
		}
	}

	// Check if config file exists
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("config file not found: %s", configPath)
	}

	// Determine file format from extension
	ext := strings.ToLower(filepath.Ext(configPath))
	isJSON := ext == ".json"

	// Load .env file if it exists (in the same directory as the config file)
	envPath := ".env"
	configDir := filepath.Dir(configPath)
	if configDir != "." && configDir != "" {
		envPath = filepath.Join(configDir, ".env")
	}

	// Try to load .env file and set environment variables
	if _, err := os.Stat(envPath); err == nil {
		envVars, err := LoadEnvFile(envPath)
		if err == nil {
			// Set environment variables from .env file
			for key, value := range envVars {
				os.Setenv(key, value)
			}
		}
	}

	// Read the config file
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// Expand environment variables in the content with trimming
	// This handles cases where environment variables have trailing spaces
	expandedData := expandEnvWithTrim(string(data))

	// Parse config into Config struct
	var config Config
	if isJSON {
		// Parse JSON
		if err := json.Unmarshal([]byte(expandedData), &config); err != nil {
			return nil, fmt.Errorf("failed to parse JSON config: %w", err)
		}
	} else {
		// Parse YAML
		decoder := yaml.NewDecoder(strings.NewReader(expandedData))
		decoder.KnownFields(true) // Strict mode - error on unknown fields
		if err := decoder.Decode(&config); err != nil {
			return nil, fmt.Errorf("failed to parse YAML config: %w", err)
		}
	}

	// Validate config
	if err := ValidateConfig(&config); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &config, nil
}

// SaveConfig writes the configuration to a YAML or JSON file based on extension
func SaveConfig(configPath string, cfg *Config) error {
	if configPath == "" {
		configPath = "tako.yaml"
	}

	// Determine format from extension
	ext := strings.ToLower(filepath.Ext(configPath))
	isJSON := ext == ".json"

	var data []byte
	var err error

	if isJSON {
		data, err = json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal JSON config: %w", err)
		}
		data = append(data, '\n') // Add trailing newline
	} else {
		data, err = yaml.Marshal(cfg)
		if err != nil {
			return fmt.Errorf("failed to marshal YAML config: %w", err)
		}
	}

	if err := os.WriteFile(configPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

// GetDeploymentStrategy returns the deployment strategy (parallel or sequential)
func (c *Config) GetDeploymentStrategy() string {
	if c.Deployment != nil && c.Deployment.Strategy != "" {
		return c.Deployment.Strategy
	}
	return "parallel" // Default to parallel deployment
}

// IsParallelDeployment returns true if parallel deployment is enabled
func (c *Config) IsParallelDeployment() bool {
	return c.GetDeploymentStrategy() == "parallel"
}

// GetMaxConcurrentBuilds returns the max concurrent builds configuration
func (c *Config) GetMaxConcurrentBuilds() int {
	if c.Deployment != nil && c.Deployment.Parallel != nil && c.Deployment.Parallel.MaxConcurrentBuilds > 0 {
		return c.Deployment.Parallel.MaxConcurrentBuilds
	}
	return 4 // Default
}

// GetMaxConcurrentDeploys returns the max concurrent deploys configuration
func (c *Config) GetMaxConcurrentDeploys() int {
	if c.Deployment != nil && c.Deployment.Parallel != nil && c.Deployment.Parallel.MaxConcurrentDeploys > 0 {
		return c.Deployment.Parallel.MaxConcurrentDeploys
	}
	return 4 // Default
}

// IsCacheEnabled returns true if build caching is enabled
func (c *Config) IsCacheEnabled() bool {
	if c.Deployment != nil && c.Deployment.Cache != nil {
		return c.Deployment.Cache.Enabled
	}
	return true // Default to enabled
}

// IsNFSEnabled returns true if NFS storage is enabled
func (c *Config) IsNFSEnabled() bool {
	return c.Storage != nil && c.Storage.NFS != nil && c.Storage.NFS.Enabled
}

// GetNFSConfig returns the NFS configuration, or nil if not enabled
func (c *Config) GetNFSConfig() *NFSConfig {
	if c.Storage != nil && c.Storage.NFS != nil {
		return c.Storage.NFS
	}
	return nil
}

// GetNFSServerName returns the NFS server name
// If "auto" or empty, returns the manager server name for the given environment
func (c *Config) GetNFSServerName(envName string) (string, error) {
	if !c.IsNFSEnabled() {
		return "", fmt.Errorf("NFS is not enabled")
	}

	nfsConfig := c.GetNFSConfig()
	if nfsConfig.Server == "" || nfsConfig.Server == "auto" {
		// Use manager node
		return c.GetManagerServer(envName)
	}

	// Verify the specified server exists
	if _, exists := c.Servers[nfsConfig.Server]; !exists {
		return "", fmt.Errorf("NFS server '%s' not found in servers configuration", nfsConfig.Server)
	}

	return nfsConfig.Server, nil
}

// GetNFSExport returns a specific NFS export by name
func (c *Config) GetNFSExport(name string) (*NFSExportConfig, error) {
	if !c.IsNFSEnabled() {
		return nil, fmt.Errorf("NFS is not enabled")
	}

	for _, export := range c.GetNFSConfig().Exports {
		if export.Name == name {
			return &export, nil
		}
	}

	return nil, fmt.Errorf("NFS export '%s' not found", name)
}

// GetNFSExports returns all NFS exports
func (c *Config) GetNFSExports() []NFSExportConfig {
	if !c.IsNFSEnabled() {
		return nil
	}
	return c.GetNFSConfig().Exports
}

// IsNFSVolume checks if a volume spec refers to an NFS volume (nfs:name:/path format)
func IsNFSVolume(volumeSpec string) bool {
	return strings.HasPrefix(volumeSpec, "nfs:")
}

// ParseNFSVolumeSpec parses an NFS volume specification
// Format: nfs:export_name:/container/path[:ro]
// Returns: exportName, containerPath, readOnly, error
func ParseNFSVolumeSpec(volumeSpec string) (exportName string, containerPath string, readOnly bool, err error) {
	if !IsNFSVolume(volumeSpec) {
		return "", "", false, fmt.Errorf("not an NFS volume spec: %s", volumeSpec)
	}

	// Remove the "nfs:" prefix
	spec := strings.TrimPrefix(volumeSpec, "nfs:")

	// Check for :ro suffix
	if strings.HasSuffix(spec, ":ro") {
		readOnly = true
		spec = strings.TrimSuffix(spec, ":ro")
	} else if strings.HasSuffix(spec, ":rw") {
		readOnly = false
		spec = strings.TrimSuffix(spec, ":rw")
	} else {
		// Default to read-only for safety
		readOnly = true
	}

	// Split into export name and container path
	parts := strings.SplitN(spec, ":", 2)
	if len(parts) != 2 {
		return "", "", false, fmt.Errorf("invalid NFS volume spec: %s (expected format: nfs:export_name:/container/path[:ro|:rw])", volumeSpec)
	}

	exportName = parts[0]
	containerPath = parts[1]

	if exportName == "" {
		return "", "", false, fmt.Errorf("NFS export name cannot be empty")
	}
	if containerPath == "" || !strings.HasPrefix(containerPath, "/") {
		return "", "", false, fmt.Errorf("NFS container path must be an absolute path")
	}

	return exportName, containerPath, readOnly, nil
}

// GetVolume returns a volume configuration by name
func (c *Config) GetVolume(name string) (*VolumeConfig, bool) {
	if c.Volumes == nil {
		return nil, false
	}
	vol, exists := c.Volumes[name]
	if !exists {
		return nil, false
	}
	return &vol, true
}

// GetVolumeName returns the actual Docker volume name for a defined volume
// If the volume has a custom name, use it; otherwise, apply project/env prefix
func (c *Config) GetVolumeName(volumeKey, envName string) string {
	if c.Volumes == nil {
		// No top-level volumes, use default naming
		return fmt.Sprintf("%s_%s_%s", c.Project.Name, envName, volumeKey)
	}

	vol, exists := c.Volumes[volumeKey]
	if !exists {
		// Volume not defined at top level, use default naming
		return fmt.Sprintf("%s_%s_%s", c.Project.Name, envName, volumeKey)
	}

	// If external or has custom name, use the specified name
	if vol.External || vol.Name != "" {
		if vol.Name != "" {
			return vol.Name
		}
		return volumeKey // External volumes use their key as-is
	}

	// Apply project/env prefix
	return fmt.Sprintf("%s_%s_%s", c.Project.Name, envName, volumeKey)
}

// IsVolumeExternal checks if a volume is marked as external
func (c *Config) IsVolumeExternal(name string) bool {
	if c.Volumes == nil {
		return false
	}
	vol, exists := c.Volumes[name]
	if !exists {
		return false
	}
	return vol.External
}

// GetAllDefinedVolumes returns all top-level volume definitions
func (c *Config) GetAllDefinedVolumes() map[string]VolumeConfig {
	if c.Volumes == nil {
		return make(map[string]VolumeConfig)
	}
	return c.Volumes
}

// PopulateServersFromInfrastructure populates the Servers map from infrastructure outputs
// This bridges the gap between infrastructure provisioning and server configuration
// It reads the infrastructure state from .tako/infra/state.json and creates ServerConfig entries
func (c *Config) PopulateServersFromInfrastructure(takoDir string) error {
	if c.Infrastructure == nil {
		return nil // No infrastructure defined, nothing to do
	}

	// Load infrastructure state
	statePath := filepath.Join(takoDir, "infra", "state.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No state file yet, infrastructure not provisioned
		}
		return fmt.Errorf("failed to read infrastructure state: %w", err)
	}

	var infraState struct {
		Servers map[string]struct {
			PublicIP  string `json:"public_ip"`
			PrivateIP string `json:"private_ip"`
			Role      string `json:"role"`
			Status    string `json:"status"`
		} `json:"servers"`
	}

	if err := json.Unmarshal(data, &infraState); err != nil {
		return fmt.Errorf("failed to parse infrastructure state: %w", err)
	}

	if len(infraState.Servers) == 0 {
		return nil // No servers provisioned yet
	}

	// Initialize Servers map if nil
	if c.Servers == nil {
		c.Servers = make(map[string]ServerConfig)
	}

	// Get SSH configuration from infrastructure
	sshUser := c.Infrastructure.SSHUser
	if sshUser == "" {
		sshUser = "root" // Default SSH user
	}

	sshKey := c.Infrastructure.SSHKey
	homeDir, _ := os.UserHomeDir()

	// Expand ~ in SSH key path
	if strings.HasPrefix(sshKey, "~/") {
		sshKey = filepath.Join(homeDir, sshKey[2:])
	}

	// Check for auto-generated SSH key first
	if sshKey == "" {
		sshKeysStatePath := filepath.Join(takoDir, "infra", "ssh_keys.json")
		if sshKeysData, err := os.ReadFile(sshKeysStatePath); err == nil {
			var sshKeyState struct {
				KeyPair struct {
					PrivateKeyPath string `json:"private_key_path"`
				} `json:"key_pair"`
			}
			if json.Unmarshal(sshKeysData, &sshKeyState) == nil && sshKeyState.KeyPair.PrivateKeyPath != "" {
				// Verify the key file exists
				if _, err := os.Stat(sshKeyState.KeyPair.PrivateKeyPath); err == nil {
					sshKey = sshKeyState.KeyPair.PrivateKeyPath
				}
			}
		}
	}

	if sshKey == "" {
		// Try common defaults
		for _, keyPath := range []string{
			filepath.Join(homeDir, ".ssh", "id_ed25519"),
			filepath.Join(homeDir, ".ssh", "id_rsa"),
		} {
			if _, err := os.Stat(keyPath); err == nil {
				sshKey = keyPath
				break
			}
		}
	}

	// Populate servers from infrastructure state
	for name, server := range infraState.Servers {
		// Skip if server already defined manually (manual takes precedence)
		if _, exists := c.Servers[name]; exists {
			continue
		}

		// Skip if server has no IP (not fully provisioned)
		if server.PublicIP == "" {
			continue
		}

		// Create ServerConfig from infrastructure output
		c.Servers[name] = ServerConfig{
			Host:   server.PublicIP,
			User:   sshUser,
			SSHKey: sshKey,
			Role:   server.Role,
			Port:   22,
		}
	}

	return nil
}

// LoadConfigWithInfra loads config and populates servers from infrastructure state
// This is the recommended way to load config when infrastructure is involved
func LoadConfigWithInfra(configPath string, takoDir string) (*Config, error) {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return nil, err
	}

	// Populate servers from infrastructure state if available
	if err := cfg.PopulateServersFromInfrastructure(takoDir); err != nil {
		// Log warning but don't fail - infrastructure might not be provisioned yet
		fmt.Fprintf(os.Stderr, "Warning: could not load infrastructure state: %v\n", err)
	}

	return cfg, nil
}
