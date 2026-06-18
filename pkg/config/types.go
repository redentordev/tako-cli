package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/envexpand"
	"github.com/redentordev/tako-cli/pkg/fileutil"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
	"gopkg.in/yaml.v3"
)

// Config represents the main configuration structure
type Config struct {
	Schema        string                       `yaml:"$schema,omitempty" json:"$schema,omitempty"` // JSON Schema reference
	Project       ProjectConfig                `yaml:"project" json:"project"`
	Runtime       *RuntimeConfig               `yaml:"runtime,omitempty" json:"runtime,omitempty"`
	Mesh          *MeshConfig                  `yaml:"mesh,omitempty" json:"mesh,omitempty"`
	State         *StateConfig                 `yaml:"state,omitempty" json:"state,omitempty"`
	Deployment    *DeploymentConfig            `yaml:"deployment,omitempty" json:"deployment,omitempty"`
	Notifications *NotificationsConfig         `yaml:"notifications,omitempty" json:"notifications,omitempty"`
	Volumes       map[string]VolumeConfig      `yaml:"volumes,omitempty" json:"volumes,omitempty"` // Top-level volume definitions
	Servers       map[string]ServerConfig      `yaml:"servers" json:"servers"`
	Environments  map[string]EnvironmentConfig `yaml:"environments" json:"environments"`
}

const (
	RuntimeModeTakod = "takod"

	RuntimeProxyTako = "tako-proxy"

	StateBackendReplicated = "replicated"

	StateDeployConsistencyLease = "lease"

	StateUnreachableBlock = "block"
)

// RuntimeConfig selects the orchestration runtime. Tako has one public runtime:
// takod. Single-node deployments are just one-node meshes.
type RuntimeConfig struct {
	Mode  string       `yaml:"mode,omitempty" json:"mode,omitempty"` // takod
	Proxy string       `yaml:"proxy,omitempty" json:"proxy,omitempty"`
	Agent *AgentConfig `yaml:"agent,omitempty" json:"agent,omitempty"`
}

// AgentConfig describes the takod node-local reconciler.
type AgentConfig struct {
	Enabled *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Socket  string `yaml:"socket,omitempty" json:"socket,omitempty"`
	DataDir string `yaml:"dataDir,omitempty" json:"dataDir,omitempty"`
}

// MeshConfig describes the private node mesh. A single node still uses the same
// model, with no remote peers.
type MeshConfig struct {
	Enabled      *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	NetworkCIDR  string `yaml:"networkCIDR,omitempty" json:"networkCIDR,omitempty"`
	Interface    string `yaml:"interface,omitempty" json:"interface,omitempty"`
	ListenPort   int    `yaml:"listenPort,omitempty" json:"listenPort,omitempty"`
	SubnetBits   int    `yaml:"subnetBits,omitempty" json:"subnetBits,omitempty"`
	NATTraversal bool   `yaml:"natTraversal,omitempty" json:"natTraversal,omitempty"`
}

// StateConfig controls how Tako treats local cache, remote runtime truth, and
// deployment consistency.
type StateConfig struct {
	Backend            string `yaml:"backend,omitempty" json:"backend,omitempty"`                       // replicated
	DeployConsistency  string `yaml:"deployConsistency,omitempty" json:"deployConsistency,omitempty"`   // lease
	OnUnreachableNode  string `yaml:"onUnreachableNode,omitempty" json:"onUnreachableNode,omitempty"`   // block
	RemoteCacheEnabled *bool  `yaml:"remoteCacheEnabled,omitempty" json:"remoteCacheEnabled,omitempty"` // must be true
}

// VolumeConfig defines a named service volume configuration.
type VolumeConfig struct {
	Driver     string            `yaml:"driver,omitempty" json:"driver,omitempty"`           // Volume driver (default: "local")
	DriverOpts map[string]string `yaml:"driver_opts,omitempty" json:"driver_opts,omitempty"` // Driver-specific options
	Labels     map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`           // Volume labels
	External   bool              `yaml:"external,omitempty" json:"external,omitempty"`       // If true, volume must already exist
	Name       string            `yaml:"name,omitempty" json:"name,omitempty"`               // Override the auto-generated name (opt-out of prefix)
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
	Strategy             string `yaml:"strategy,omitempty" json:"strategy,omitempty"`                         // "dependency-aware" (default), "resource-aware", "round-robin"
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
	Labels   map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`     // Custom labels for server selection
}

// ServiceConfig defines service deployment settings
type ServiceConfig struct {
	// Build or Image (mutually exclusive)
	Build      string `yaml:"build,omitempty" json:"build,omitempty"`           // Path to build context (auto-detects Dockerfile)
	Dockerfile string `yaml:"dockerfile,omitempty" json:"dockerfile,omitempty"` // Dockerfile path relative to build context
	Image      string `yaml:"image,omitempty" json:"image,omitempty"`           // Pre-built image (for postgres, redis, etc)

	// Basic settings
	Port     int               `yaml:"port,omitempty" json:"port,omitempty"`
	Command  string            `yaml:"command,omitempty" json:"command,omitempty"`
	Replicas int               `yaml:"replicas,omitempty" json:"replicas,omitempty"` // Default: 1
	Restart  string            `yaml:"restart,omitempty" json:"restart,omitempty"`   // Docker restart policy (always, unless-stopped, on-failure, no)
	Env      map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	EnvFile  string            `yaml:"envFile,omitempty" json:"envFile,omitempty"` // Path to .env file (e.g., .env.production)

	// Secrets: ["DATABASE_URL", "JWT_SECRET"] or ["VAR_NAME:SECRET_KEY"].
	Secrets []string `yaml:"secrets,omitempty" json:"secrets,omitempty"` // Tako secrets from .tako/secrets files
	Volumes []string `yaml:"volumes,omitempty" json:"volumes,omitempty"`

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

	// Per-service backup
	Backup *BackupConfig `yaml:"backup,omitempty" json:"backup,omitempty"`

	// Per-service monitoring
	Monitoring *MonitoringConfig `yaml:"monitoring,omitempty" json:"monitoring,omitempty"`

	// Cross-project networking
	Export  bool     `yaml:"export,omitempty" json:"export,omitempty"`   // Attach this service to a service-scoped export network
	Imports []string `yaml:"imports,omitempty" json:"imports,omitempty"` // Import same-environment services from other projects (format: "project.service")

	// Placement configuration for takod scheduling.
	Placement *PlacementConfig `yaml:"placement,omitempty" json:"placement,omitempty"` // Where to run service replicas

	// Service dependencies (controls deployment order)
	DependsOn []string `yaml:"dependsOn,omitempty" json:"dependsOn,omitempty"` // List of service names this service depends on
}

// HealthCheckConfig defines health check settings
type HealthCheckConfig struct {
	Path        string `yaml:"path" json:"path"`
	TCPPort     int    `yaml:"tcpPort,omitempty" json:"tcpPort,omitempty"`
	Interval    string `yaml:"interval" json:"interval"`
	Timeout     string `yaml:"timeout" json:"timeout"`
	Retries     int    `yaml:"retries" json:"retries"`
	StartPeriod string `yaml:"startPeriod,omitempty" json:"startPeriod,omitempty"` // Grace period before starting checks
}

// DeployConfig defines deployment strategy
type DeployConfig struct {
	Strategy       string `yaml:"strategy" json:"strategy"` // recreate
	MaxUnavailable int    `yaml:"maxUnavailable,omitempty" json:"maxUnavailable,omitempty"`
}

// LoadBalancerConfig defines load balancing settings
type LoadBalancerConfig struct {
	Strategy    string                  `yaml:"strategy" json:"strategy"` // round_robin, sticky
	HealthCheck LoadBalancerHealthCheck `yaml:"healthCheck,omitempty" json:"healthCheck,omitempty"`
}

// LoadBalancerHealthCheck defines load balancer health check settings
type LoadBalancerHealthCheck struct {
	Enabled  bool   `yaml:"enabled" json:"enabled"`
	Path     string `yaml:"path" json:"path"`
	Interval string `yaml:"interval" json:"interval"`
}

// ProxyConfig defines per-service public proxy settings.
type ProxyConfig struct {
	// Domain is where traffic is served.
	Domain string `yaml:"domain,omitempty" json:"domain,omitempty"`

	// RedirectFrom specifies domains that should redirect to the primary Domain
	// These domains will get their own TLS certificates and 301 redirect to Domain
	// Example: ["www.example.com", "old.example.com"] -> redirects to "example.com"
	RedirectFrom []string `yaml:"redirectFrom,omitempty" json:"redirectFrom,omitempty"`

	Email string    `yaml:"email,omitempty" json:"email,omitempty"` // Email for Let's Encrypt
	TLS   TLSConfig `yaml:"tls,omitempty" json:"tls,omitempty"`

	// DynamicDomains enables ask-gated on-demand TLS for customer domains.
	DynamicDomains *DynamicDomainsConfig `yaml:"dynamicDomains,omitempty" json:"dynamicDomains,omitempty"`
}

// DynamicDomainsConfig describes Caddy on-demand TLS for customer domains.
type DynamicDomainsConfig struct {
	Enabled *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Ask     string `yaml:"ask,omitempty" json:"ask,omitempty"` // "<service>:<path>"
}

// IsEnabled returns true when dynamic domain handling is active.
func (d *DynamicDomainsConfig) IsEnabled() bool {
	return d != nil && (d.Enabled == nil || *d.Enabled)
}

// GetPrimaryDomain returns the primary domain for this service
func (p *ProxyConfig) GetPrimaryDomain() string {
	return p.Domain
}

// GetAllDomains returns all serving domains, excluding redirect domains.
func (p *ProxyConfig) GetAllDomains() []string {
	if p.Domain == "" {
		return nil
	}
	return []string{p.Domain}
}

// GetRedirectDomains returns all domains that should redirect to the primary domain
func (p *ProxyConfig) GetRedirectDomains() []string {
	return p.RedirectFrom
}

// HasRedirects returns true if there are redirect domains configured
func (p *ProxyConfig) HasRedirects() bool {
	return len(p.RedirectFrom) > 0
}

// NormalizeProxyDomain trims and validates a domain before it is used in proxy
// routing configuration.
func NormalizeProxyDomain(domain string) (string, error) {
	trimmed := strings.TrimSpace(domain)
	if trimmed == "" {
		return "", fmt.Errorf("domain is required")
	}
	if !isValidDomain(trimmed) {
		return "", fmt.Errorf("invalid domain: %s", trimmed)
	}
	return trimmed, nil
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

// MonitoringConfig defines per-service monitoring settings
type MonitoringConfig struct {
	Enabled   bool   `yaml:"enabled" json:"enabled"`                         // Enable monitoring for this service
	Interval  string `yaml:"interval,omitempty" json:"interval,omitempty"`   // Check interval (e.g., "60s")
	Webhook   string `yaml:"webhook,omitempty" json:"webhook,omitempty"`     // Webhook URL for alerts
	CheckType string `yaml:"checkType,omitempty" json:"checkType,omitempty"` // "http" or "container" (default: auto-detect)
}

// EnvironmentConfig defines an environment (production, staging, etc.)
type EnvironmentConfig struct {
	Servers        []string                 `yaml:"servers" json:"servers"`                                   // List of server names to use
	ServerSelector *ServerSelector          `yaml:"serverSelector,omitempty" json:"serverSelector,omitempty"` // Label-based server selection
	Proxy          *EnvironmentProxyConfig  `yaml:"proxy,omitempty" json:"proxy,omitempty"`                   // Environment-level proxy placement
	Labels         map[string]string        `yaml:"labels,omitempty" json:"labels,omitempty"`                 // Environment labels for nodes
	Services       map[string]ServiceConfig `yaml:"services" json:"services"`                                 // Services to deploy in this environment
}

// EnvironmentProxyConfig controls where environment-level public proxy routes
// are reconciled. Services still use their own placement for containers.
type EnvironmentProxyConfig struct {
	Placement *PlacementConfig `yaml:"placement,omitempty" json:"placement,omitempty"`
}

// ServerSelector defines label-based server selection
type ServerSelector struct {
	Labels map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"` // Match servers with these labels
	Any    bool              `yaml:"any,omitempty" json:"any,omitempty"`       // Select any available server
}

// PlacementConfig defines where service replicas should run
type PlacementConfig struct {
	Strategy    string   `yaml:"strategy,omitempty" json:"strategy,omitempty"`       // "spread", "pinned", "any", "global"
	Servers     []string `yaml:"servers,omitempty" json:"servers,omitempty"`         // Pin to specific servers (for "pinned" strategy)
	Constraints []string `yaml:"constraints,omitempty" json:"constraints,omitempty"` // Node label constraints (e.g., "node.labels.type==high-memory")
	Preferences []string `yaml:"preferences,omitempty" json:"preferences,omitempty"` // Placement preferences (e.g., "spread=node.labels.region")
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
			sort.Strings(servers)
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
		sort.Strings(matchedServers)
		return matchedServers, nil
	}

	return nil, fmt.Errorf("environment '%s' has no servers configured", envName)
}

// GetEnvironmentProxyServers returns the nodes that should reconcile public
// proxy routes for an environment. Without an explicit environment proxy
// placement, every selected environment server remains a proxy node.
func (c *Config) GetEnvironmentProxyServers(envName string) ([]string, error) {
	env, err := c.GetEnvironment(envName)
	if err != nil {
		return nil, err
	}
	environmentServers, err := c.GetEnvironmentServers(envName)
	if err != nil {
		return nil, err
	}
	return ResolveEnvironmentProxyTargets(env.Proxy, c.Servers, environmentServers, envName)
}

// ResolveEnvironmentProxyTargets applies environment proxy placement to the
// selected environment node set.
func ResolveEnvironmentProxyTargets(proxy *EnvironmentProxyConfig, servers map[string]ServerConfig, environmentServers []string, environment string) ([]string, error) {
	if proxy == nil || proxy.Placement == nil {
		return append([]string(nil), environmentServers...), nil
	}
	return ResolvePlacementTargets(proxy.Placement, servers, environmentServers, environment)
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

// GetDefaultServer returns the deterministic default node for an environment.
func (c *Config) GetDefaultServer(envName string) (string, error) {
	servers, err := c.GetEnvironmentServers(envName)
	if err != nil {
		return "", err
	}

	if len(servers) > 0 {
		return servers[0], nil
	}

	return "", fmt.Errorf("no servers found for environment '%s'", envName)
}

// IsMultiServer returns true if more than one server is configured
func (c *Config) IsMultiServer() bool {
	return len(c.Servers) > 1
}

// GetRuntimeMode returns the configured orchestration runtime.
func (c *Config) GetRuntimeMode() string {
	if c.Runtime == nil || c.Runtime.Mode == "" {
		return RuntimeModeTakod
	}
	return c.Runtime.Mode
}

// GetRuntimeProxy returns the internal ingress proxy implementation.
func (c *Config) GetRuntimeProxy() string {
	if c.Runtime == nil || c.Runtime.Proxy == "" {
		return RuntimeProxyTako
	}
	return c.Runtime.Proxy
}

// IsTakodRuntime returns true when the current runtime is the takod mesh runtime.
func (c *Config) IsTakodRuntime() bool {
	return c.GetRuntimeMode() == RuntimeModeTakod
}

// IsMeshEnabled returns true when the private node mesh is enabled.
func (c *Config) IsMeshEnabled() bool {
	return c.Mesh == nil || c.Mesh.Enabled == nil || *c.Mesh.Enabled
}

// GetStateBackend returns the configured state backend.
func (c *Config) GetStateBackend() string {
	if c.State == nil || c.State.Backend == "" {
		return StateBackendReplicated
	}
	return c.State.Backend
}

// GetDeployConsistency returns the deployment consistency policy.
func (c *Config) GetDeployConsistency() string {
	if c.State == nil || c.State.DeployConsistency == "" {
		return StateDeployConsistencyLease
	}
	return c.State.DeployConsistency
}

// GetOnUnreachableNode returns the policy used when a node cannot be reached.
func (c *Config) GetOnUnreachableNode() string {
	if c.State == nil || c.State.OnUnreachableNode == "" {
		return StateUnreachableBlock
	}
	return c.State.OnUnreachableNode
}

// IsRemoteCacheEnabled returns whether deployment history is replicated to takod.
func (c *Config) IsRemoteCacheEnabled() bool {
	if c.State == nil || c.State.RemoteCacheEnabled == nil {
		return true
	}
	return *c.State.RemoteCacheEnabled
}

// GetRegistryURL returns the auto-configured local registry URL
// Returns empty string for single-server deployments (no registry needed)
func (c *Config) GetRegistryURL() string {
	// Takod deployments use direct peer transfer for built images unless a service
	// explicitly references an external image.
	return ""
}

// GetFullImageNameWithTag returns the full image name for an explicit tag.
func (c *Config) GetFullImageNameWithTag(serviceName string, tag string) string {
	registryURL := c.GetRegistryURL()

	if registryURL != "" {
		// Multi-server setup with registry
		return fmt.Sprintf("%s/%s/%s:%s",
			registryURL,
			c.Project.Name,
			serviceName,
			tag,
		)
	}
	// Single-server setup without registry
	return fmt.Sprintf("%s/%s:%s",
		c.Project.Name,
		serviceName,
		tag,
	)
}

// GetFullImageName returns the legacy config-version image name.
func (c *Config) GetFullImageName(serviceName string, envName string) string {
	versionTag := fmt.Sprintf("%s-%s", c.Project.Version, envName)
	return c.GetFullImageNameWithTag(serviceName, versionTag)
}

// expandEnvWithTrim expands ${VAR} placeholders and trims their values.
// It intentionally does not expand bare $VAR so keys like $schema remain intact.
// For YAML, comment text is ignored so commented examples do not require env vars.
func expandEnvWithTrim(s string, ignoreYAMLComments bool) (string, error) {
	var result strings.Builder
	missing := make([]string, 0)
	seenMissing := map[string]bool{}

	for _, line := range strings.SplitAfter(s, "\n") {
		content := line
		comment := ""
		if ignoreYAMLComments {
			content, comment = splitYAMLComment(line)
		}

		expanded, lineMissing := envexpand.BracedFromOS(content)
		for _, key := range lineMissing {
			if !seenMissing[key] {
				seenMissing[key] = true
				missing = append(missing, key)
			}
		}
		result.WriteString(expanded)
		result.WriteString(comment)
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		return "", fmt.Errorf("missing environment variable(s): %s", strings.Join(missing, ", "))
	}
	return result.String(), nil
}

func splitYAMLComment(line string) (string, string) {
	inSingle := false
	inDouble := false

	for idx := 0; idx < len(line); idx++ {
		switch line[idx] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle && (idx == 0 || line[idx-1] != '\\') {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble {
				return line[:idx], line[idx:]
			}
		}
	}
	return line, ""
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
	expandedData, err := expandEnvWithTrim(string(data), !isJSON)
	if err != nil {
		return nil, fmt.Errorf("failed to expand config environment variables: %w", err)
	}

	// Parse config into Config struct
	var config Config
	if isJSON {
		decoder := json.NewDecoder(strings.NewReader(expandedData))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&config); err != nil {
			return nil, fmt.Errorf("failed to parse JSON config: %w", err)
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			if err == nil {
				return nil, fmt.Errorf("failed to parse JSON config: multiple JSON values")
			}
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

	normalizeConfigRelativePaths(&config, configDir)

	// Validate config
	if err := ValidateConfig(&config); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &config, nil
}

func normalizeConfigRelativePaths(cfg *Config, configDir string) {
	if cfg == nil || configDir == "" || configDir == "." {
		return
	}
	absConfigDir, err := filepath.Abs(configDir)
	if err != nil {
		return
	}
	cwd, err := os.Getwd()
	if err == nil {
		absCWD, err := filepath.Abs(cwd)
		if err == nil && absConfigDir == absCWD {
			return
		}
	}

	for name, server := range cfg.Servers {
		server.SSHKey = resolveConfigRelativePath(absConfigDir, server.SSHKey)
		cfg.Servers[name] = server
	}
	for envName, env := range cfg.Environments {
		for serviceName, service := range env.Services {
			service.Build = resolveConfigRelativePath(absConfigDir, service.Build)
			service.EnvFile = resolveConfigRelativePath(absConfigDir, service.EnvFile)
			env.Services[serviceName] = service
		}
		cfg.Environments[envName] = env
	}
}

func resolveConfigRelativePath(baseDir string, path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" || filepath.IsAbs(trimmed) || strings.HasPrefix(trimmed, "~") {
		return path
	}
	return filepath.Join(baseDir, trimmed)
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

	if err := fileutil.WriteFileAtomic(configPath, data, 0644); err != nil {
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

// IsNFSVolume checks if a volume spec uses the removed nfs: prefix.
func IsNFSVolume(volumeSpec string) bool {
	return strings.HasPrefix(volumeSpec, "nfs:")
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
		return runtimeid.VolumeName(c.Project.Name, envName, volumeKey)
	}

	vol, exists := c.Volumes[volumeKey]
	if !exists {
		// Volume not defined at top level, use default naming
		return runtimeid.VolumeName(c.Project.Name, envName, volumeKey)
	}

	// If external or has custom name, use the specified name
	if vol.External || vol.Name != "" {
		if vol.Name != "" {
			return vol.Name
		}
		return volumeKey // External volumes use their key as-is
	}

	// Apply project/env prefix
	return runtimeid.VolumeName(c.Project.Name, envName, volumeKey)
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
