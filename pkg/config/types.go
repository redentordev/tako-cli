package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/alerttarget"
	"github.com/redentordev/tako-cli/pkg/envexpand"
	"github.com/redentordev/tako-cli/pkg/fileutil"
	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/projectbinding"
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
	Builds        map[string]SharedBuildConfig `yaml:"builds,omitempty" json:"builds,omitempty"`
	// Registries holds private image registry credentials keyed by host
	// (e.g. ghcr.io). Values must be ${ENV_VAR} references — literal
	// passwords in the config file are rejected at load time. Credentials
	// are request-scoped: they ride individual pull/build requests and are
	// never persisted on nodes.
	Registries map[string]RegistryConfig `yaml:"registries,omitempty" json:"registries,omitempty"`
	// SecretSources are external secret stores read at deploy time for every
	// environment; see SecretSource.
	SecretSources []SecretSource `yaml:"secretSources,omitempty" json:"secretSources,omitempty"`
	// Aliases are project-defined command shortcuts: `tako <alias>` expands
	// to the listed Tako arguments before command dispatch. Values are
	// literal (no ${...} expansion); see AliasMap and ValidateAliases.
	Aliases AliasMap `yaml:"aliases,omitempty" json:"aliases,omitempty"`
	// Hooks configures local lifecycle hooks run on the operator's machine
	// around deploy, rollback, promote, and setup; see HooksConfig.
	Hooks *HooksConfig `yaml:"hooks,omitempty" json:"hooks,omitempty"`

	Servers      map[string]ServerConfig      `yaml:"servers" json:"servers"`
	Environments map[string]EnvironmentConfig `yaml:"environments" json:"environments"`

	// Platform is trusted runtime context materialized from the node-local
	// platform inventory. It is never accepted from application config or
	// serialized back into tako.yaml/tako.json.
	Platform *PlatformContext `yaml:"-" json:"-"`
	// PlatformArtifacts records trusted node-local platform markers even when
	// enrollment is incomplete and no authoritative inventory can be
	// materialized. Project mutations fail closed while this is non-empty.
	PlatformArtifacts []string `yaml:"-" json:"-"`

	// resolution records environment inheritance (extends/abstract) and
	// value provenance when the document used it; nil otherwise.
	resolution *environmentResolution

	// preview is the preview instance this config was loaded for, or nil.
	preview *PreviewInstanceInfo

	// deferredVariables lists unset ${VAR} names that were referenced only
	// by environments or servers removed by environment-scoped loading.
	deferredVariables []string
}

// PlatformContext identifies the enrolled cluster that owns the runtime
// targets materialized for this invocation.
type PlatformContext = projectbinding.Context

const (
	RuntimeModeTakod = "takod"

	RuntimeProxyTako = "tako-proxy"

	ProxyVisibilityPublic   = "public"
	ProxyVisibilityInternal = "internal"
	ProxyCDNCloudflare      = "cloudflare"
	ProxyCDNGeneric         = "generic"

	ProxyTLSModeAuto            = "auto"
	ProxyTLSModeOff             = "off"
	ProxyTLSChallengeAuto       = "auto"
	ProxyTLSChallengeDNS        = "dns"
	ACMEDNSProviderCloudflare   = "cloudflare"
	ACMEDNSProviderHetzner      = "hetzner"
	ACMEDNSProviderDigitalOcean = "digitalocean"

	StateBackendReplicated = "replicated"

	StateDeployConsistencyLease = "lease"

	StateUnreachableBlock = "block"

	DeployStrategyRecreate  = "recreate"
	DeployStrategyRolling   = "rolling"
	DeployStrategyBlueGreen = "blue_green"

	DeployPromotionAutomatic = "automatic"
	DeployPromotionManual    = "manual"

	BuildStrategyRemote = "remote"
	BuildStrategyLocal  = "local"
	BuildStrategyAuto   = "auto"

	BackupStorageProviderS3           = "s3"
	BackupStorageProviderR2           = "r2"
	BackupStorageProviderS3Compatible = "s3-compatible"
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
	Owner      string            `yaml:"owner,omitempty" json:"owner,omitempty"`             // Creation-only numeric uid or uid:gid
}

// NotificationsConfig defines notification settings
type NotificationsConfig struct {
	Slack   string                 `yaml:"slack,omitempty" json:"slack,omitempty"`     // Slack webhook URL
	Discord string                 `yaml:"discord,omitempty" json:"discord,omitempty"` // Discord webhook URL
	Webhook string                 `yaml:"webhook,omitempty" json:"webhook,omitempty"` // Generic webhook URL
	Alerts  *SchedulerAlertsConfig `yaml:"alerts,omitempty" json:"alerts,omitempty"`
}

type SchedulerAlertsConfig struct {
	Jobs    JobAlertsConfig    `yaml:"jobs,omitempty" json:"jobs,omitempty"`
	Backups BackupAlertsConfig `yaml:"backups,omitempty" json:"backups,omitempty"`

	// Services, Host, Containers and Certificates configure takod-run
	// health monitoring (monitoring.health-v1). A nil section means "not
	// explicitly configured": defaults still apply when the alerts block
	// exists, but only explicitly set sections fail a deploy on nodes
	// without the capability.
	Services     *ServiceAlertsConfig     `yaml:"services,omitempty" json:"services,omitempty"`
	Host         *HostAlertsConfig        `yaml:"host,omitempty" json:"host,omitempty"`
	Containers   *ContainerAlertsConfig   `yaml:"containers,omitempty" json:"containers,omitempty"`
	Certificates *CertificateAlertsConfig `yaml:"certificates,omitempty" json:"certificates,omitempty"`
}

// ServiceAlertsConfig holds damping defaults for services with
// monitoring.enabled.
type ServiceAlertsConfig struct {
	AlertAfter   int `yaml:"alertAfter,omitempty" json:"alertAfter,omitempty"`     // consecutive failed checks before alerting (default 3)
	RecoverAfter int `yaml:"recoverAfter,omitempty" json:"recoverAfter,omitempty"` // consecutive passed checks before recovery (default 2)
}

// HostAlertsConfig holds node threshold alerts. Percent thresholds use
// 1..99; 0 disables. A host section also subscribes the environment to the
// node-wide alert kinds (hygiene and node conditions).
type HostAlertsConfig struct {
	Disk       *int    `yaml:"disk,omitempty" json:"disk,omitempty"`             // percent of a filesystem used (default 90)
	Inodes     *int    `yaml:"inodes,omitempty" json:"inodes,omitempty"`         // percent of inodes used (default 90)
	Memory     int     `yaml:"memory,omitempty" json:"memory,omitempty"`         // percent (default off)
	CPU        int     `yaml:"cpu,omitempty" json:"cpu,omitempty"`               // percent sustained for cpuFor (default off)
	CPUFor     string  `yaml:"cpuFor,omitempty" json:"cpuFor,omitempty"`         // default 10m
	Load       float64 `yaml:"load,omitempty" json:"load,omitempty"`             // 5-minute load per core (default off)
	Conditions *bool   `yaml:"conditions,omitempty" json:"conditions,omitempty"` // node-condition alerts (default true)
}

// ContainerAlertsConfig holds container OOM and crash-loop detection.
type ContainerAlertsConfig struct {
	OOM             *bool  `yaml:"oom,omitempty" json:"oom,omitempty"`                         // default true
	CrashLoop       *int   `yaml:"crashLoop,omitempty" json:"crashLoop,omitempty"`             // restarts within crashLoopWindow; 0 disables (default 3)
	CrashLoopWindow string `yaml:"crashLoopWindow,omitempty" json:"crashLoopWindow,omitempty"` // default 10m
}

// CertificateAlertsConfig holds certificate expiry thresholds.
type CertificateAlertsConfig struct {
	WarnBefore     string `yaml:"warnBefore,omitempty" json:"warnBefore,omitempty"`         // default 336h
	CriticalBefore string `yaml:"criticalBefore,omitempty" json:"criticalBefore,omitempty"` // default 72h
}

type JobAlertsConfig struct {
	ConsecutiveFailures int    `yaml:"consecutiveFailures,omitempty" json:"consecutiveFailures,omitempty"`
	ConsecutiveTimeouts int    `yaml:"consecutiveTimeouts,omitempty" json:"consecutiveTimeouts,omitempty"`
	ConsecutiveSkipped  int    `yaml:"consecutiveSkipped,omitempty" json:"consecutiveSkipped,omitempty"`
	NoSuccessFor        string `yaml:"noSuccessFor,omitempty" json:"noSuccessFor,omitempty"`
}

type BackupAlertsConfig struct {
	MaxAge string `yaml:"maxAge,omitempty" json:"maxAge,omitempty"`
}

// ProjectConfig defines project metadata
// RegistryConfig holds credentials for one private image registry.
type RegistryConfig struct {
	Username string `yaml:"username,omitempty" json:"username,omitempty"` // Use ${ENV_VAR}
	Password string `yaml:"password,omitempty" json:"password,omitempty"` // Use ${ENV_VAR}
}

type ProjectConfig struct {
	Name    string `yaml:"name" json:"name"`
	Version string `yaml:"version" json:"version"`
}

// DeploymentConfig defines deployment settings.
type DeploymentConfig struct {
	// Deprecated: deployment.strategy has no effect. Services deploy
	// sequentially in dependency order. It is kept so existing configs decode
	// and is reported by ValidationWarnings until DeprecatedKeysRemovalRelease.
	Strategy string `yaml:"strategy,omitempty" json:"strategy,omitempty"`
	// Deprecated: deployment.parallel has no effect; see Strategy.
	Parallel *ParallelConfig `yaml:"parallel,omitempty" json:"parallel,omitempty"`
	// Deprecated: deployment.cache has no effect. Build cache is managed by
	// Docker (tako cleanup --docker-cache, takod --build-cache-keep-storage).
	Cache *CacheConfig `yaml:"cache,omitempty" json:"cache,omitempty"`
	Build *BuildConfig `yaml:"build,omitempty" json:"build,omitempty"`
}

// ParallelConfig is the shape of the deprecated deployment.parallel key.
//
// Deprecated: no code reads these settings.
type ParallelConfig struct {
	MaxConcurrentBuilds  int    `yaml:"maxConcurrentBuilds,omitempty" json:"maxConcurrentBuilds,omitempty"`   // Default: 4
	MaxConcurrentDeploys int    `yaml:"maxConcurrentDeploys,omitempty" json:"maxConcurrentDeploys,omitempty"` // Default: 4
	Strategy             string `yaml:"strategy,omitempty" json:"strategy,omitempty"`                         // "dependency-aware" (default), "resource-aware", "round-robin"
}

// CacheConfig is the shape of the deprecated deployment.cache key.
//
// Deprecated: no code reads these settings.
type CacheConfig struct {
	Enabled   bool   `yaml:"enabled,omitempty" json:"enabled,omitempty"`     // Enable build caching (default: true)
	Type      string `yaml:"type,omitempty" json:"type,omitempty"`           // "local" (default), "registry"
	Retention string `yaml:"retention,omitempty" json:"retention,omitempty"` // Cache retention period (e.g., "7d")
}

// BuildConfig selects where build-backed service images are produced.
type BuildConfig struct {
	Strategy string `yaml:"strategy,omitempty" json:"strategy,omitempty"` // remote, local, auto
}

// SharedBuildConfig declares one image build consumed by any number of
// services through imageFrom.
type SharedBuildConfig struct {
	Context         string            `yaml:"context" json:"context"`
	Args            map[string]string `yaml:"args,omitempty" json:"args,omitempty"`
	Target          string            `yaml:"target,omitempty" json:"target,omitempty"`
	Dockerfile      string            `yaml:"dockerfile,omitempty" json:"dockerfile,omitempty"`
	declaredContext string
}

func (b SharedBuildConfig) Fingerprint() string {
	contextPath := b.DeclaredContext()
	data, _ := json.Marshal(struct {
		Context    string            `json:"context"`
		Args       map[string]string `json:"args,omitempty"`
		Target     string            `json:"target,omitempty"`
		Dockerfile string            `json:"dockerfile,omitempty"`
	}{Context: contextPath, Args: b.Args, Target: b.Target, Dockerfile: b.Dockerfile})
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (b SharedBuildConfig) DeclaredContext() string {
	contextPath := b.declaredContext
	if contextPath == "" {
		contextPath = strings.TrimSpace(b.Context)
	}
	return filepath.ToSlash(filepath.Clean(contextPath))
}

// ServerConfig defines server connection details
type ServerConfig struct {
	Host        string            `yaml:"host" json:"host"`
	PrivateHost string            `yaml:"privateHost,omitempty" json:"privateHost,omitempty"`
	User        string            `yaml:"user" json:"user"`
	Port        int               `yaml:"port,omitempty" json:"port,omitempty"`
	SSHKey      string            `yaml:"sshKey,omitempty" json:"sshKey,omitempty"`     // Path to SSH private key (mutually exclusive with password)
	Password    string            `yaml:"password,omitempty" json:"password,omitempty"` // SSH password (mutually exclusive with sshKey, use env var for security)
	Labels      map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`     // Custom labels for server selection
	// Transport controls routine takod traffic only. The empty value preserves
	// the historical SSH-first behavior. auto/local require the immutable IDs
	// below; privileged setup, join, repair, and upgrade remain SSH operations.
	Transport string `yaml:"transport,omitempty" json:"transport,omitempty"`
	ClusterID string `yaml:"clusterId,omitempty" json:"clusterId,omitempty"`
	NodeID    string `yaml:"nodeId,omitempty" json:"nodeId,omitempty"`
	WorkerUID int    `yaml:"workerUid,omitempty" json:"workerUid,omitempty"`
	// Platform-owned fields are materialized from the protected cluster
	// inventory and can never be supplied by application YAML/JSON.
	MeshIP                string   `yaml:"-" json:"-"`
	Lifecycle             string   `yaml:"-" json:"-"`
	Roles                 []string `yaml:"-" json:"-"`
	SSHHostKeyType        string   `yaml:"-" json:"-"`
	SSHHostKey            string   `yaml:"-" json:"-"`
	SSHHostKeyFingerprint string   `yaml:"-" json:"-"`
}

func (s ServerConfig) Schedulable() bool {
	return s.Lifecycle == "" || s.Lifecycle == nodeidentity.NodeLifecycleSchedulable
}

// HasPlatformRole preserves legacy configs (which have no controller-owned
// roles) while enforcing roles on enrolled inventory members.
func (s ServerConfig) HasPlatformRole(role string) bool {
	if len(s.Roles) == 0 {
		return true
	}
	for _, candidate := range s.Roles {
		if candidate == role {
			return true
		}
	}
	return false
}

// Service kinds.
const (
	ServiceKindService = "service"
	ServiceKindJob     = "job"
	ServiceKindRun     = "run"
)

// ServiceConfig defines service deployment settings
type ServiceConfig struct {
	buildStructured bool

	// Kind selects the workload type: "service" (default, long-running
	// containers) or "job" (a command run on a cron schedule by takod).
	Kind string `yaml:"kind,omitempty" json:"kind,omitempty"`
	// Schedule is the cron expression for kind: job (five-field cron or
	// descriptors like @hourly). Evaluated in UTC unless timezone is set.
	Schedule string `yaml:"schedule,omitempty" json:"schedule,omitempty"`
	// Timezone is the IANA zone the schedule is evaluated in (kind: job).
	Timezone string `yaml:"timezone,omitempty" json:"timezone,omitempty"`
	// Timeout kills a job run after this duration (kind: job, default 1h).
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty"`

	// Build or Image (mutually exclusive)
	Build       string            `yaml:"build,omitempty" json:"build,omitempty"` // Path to build context (auto-detects Dockerfile)
	BuildArgs   map[string]string `yaml:"-" json:"-"`
	BuildTarget string            `yaml:"-" json:"-"`
	Dockerfile  string            `yaml:"dockerfile,omitempty" json:"dockerfile,omitempty"` // Dockerfile path relative to build context
	Image       string            `yaml:"image,omitempty" json:"image,omitempty"`           // Pre-built image (for postgres, redis, etc)
	ImageFrom   string            `yaml:"imageFrom,omitempty" json:"imageFrom,omitempty"`   // shared build, or source service for kind: run
	// SharedBuildHash fingerprints the resolved top-level build definition
	// without duplicating it into each service's public config shape.
	SharedBuildHash string `yaml:"-" json:"-"`

	// Basic settings
	Port int `yaml:"port,omitempty" json:"port,omitempty"`
	// Ports publishes raw TCP/UDP host ports directly on the node, bypassing
	// tako-proxy (docker-compose syntax: "PORT", "HOST:CONTAINER",
	// "IP:HOST:CONTAINER", optional "/tcp" or "/udp"). Requires the recreate
	// deploy strategy and at most one replica.
	Ports      []string          `yaml:"ports,omitempty" json:"ports,omitempty"`
	Command    StringOrList      `yaml:"command,omitempty" json:"command,omitempty,omitzero"`
	Entrypoint StringOrList      `yaml:"entrypoint,omitempty" json:"entrypoint,omitempty,omitzero"`
	Labels     map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
	Replicas   int               `yaml:"replicas,omitempty" json:"replicas,omitempty"` // Default: 1
	Restart    string            `yaml:"restart,omitempty" json:"restart,omitempty"`   // Docker restart policy (always, unless-stopped, on-failure, no)
	Env        map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	EnvFile    string            `yaml:"envFile,omitempty" json:"envFile,omitempty"` // Legacy single .env file path
	EnvFiles   []string          `yaml:"envFiles,omitempty" json:"envFiles,omitempty"`
	// RunInputHash is an internal digest of the fully resolved env/secret file
	// used to fingerprint kind:run executions without persisting secret values.
	RunInputHash    string                  `yaml:"-" json:"-"`
	User            string                  `yaml:"user,omitempty" json:"user,omitempty"`
	WorkingDir      string                  `yaml:"workingDir,omitempty" json:"workingDir,omitempty"`
	StopGracePeriod string                  `yaml:"stopGracePeriod,omitempty" json:"stopGracePeriod,omitempty"`
	Init            bool                    `yaml:"init,omitempty" json:"init,omitempty"`
	ExtraHosts      []string                `yaml:"extraHosts,omitempty" json:"extraHosts,omitempty"`
	Ulimits         map[string]UlimitConfig `yaml:"ulimits,omitempty" json:"ulimits,omitempty"`
	ShmSize         string                  `yaml:"shmSize,omitempty" json:"shmSize,omitempty"`
	Logging         *ServiceLoggingConfig   `yaml:"logging,omitempty" json:"logging,omitempty"`

	// Secrets: ["DATABASE_URL", "JWT_SECRET"] or ["VAR_NAME:SECRET_KEY"].
	Secrets             []string            `yaml:"secrets,omitempty" json:"secrets,omitempty"` // Tako secrets from .tako/secrets files
	Volumes             []string            `yaml:"volumes,omitempty" json:"volumes,omitempty"`
	VolumeOwnershipHash string              `yaml:"-" json:"-"`
	Files               []ServiceFileConfig `yaml:"files,omitempty" json:"files,omitempty"`
	// FilesContentHash is an internal digest of fully resolved operator file
	// metadata and bytes; file contents never enter desired state or labels.
	FilesContentHash string `yaml:"-" json:"-"`

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

	// SelfHealing overrides the environment's self-healing policy for this
	// service. It is not part of container identity.
	SelfHealing *SelfHealingConfig `yaml:"selfHealing,omitempty" json:"selfHealing,omitempty"`

	// Container resource limits.
	Resources *ResourceLimitsConfig `yaml:"resources,omitempty" json:"resources,omitempty"`

	// Cross-project networking
	Export  bool     `yaml:"export,omitempty" json:"export,omitempty"`   // Attach this service to a service-scoped export network
	Imports []string `yaml:"imports,omitempty" json:"imports,omitempty"` // Import same-environment services from other projects (format: "project.service")

	// Placement configuration for takod scheduling.
	Placement *PlacementConfig `yaml:"placement,omitempty" json:"placement,omitempty"` // Where to run service replicas

	// Service dependencies (controls deployment order)
	DependsOn []string `yaml:"dependsOn,omitempty" json:"dependsOn,omitempty"` // List of service names this service depends on

	// ReuseFiles tells rollback paths to mount an already-published immutable
	// FilesContentHash instead of reading today's local sources.
	ReuseFiles bool `yaml:"-" json:"-"`
}

// ServiceLoggingConfig bounds Docker's per-container application logs while
// preserving `docker logs` support.
type ServiceLoggingConfig struct {
	Driver   string `yaml:"driver" json:"driver"`
	MaxSize  string `yaml:"maxSize" json:"maxSize"`
	MaxFiles int    `yaml:"maxFiles" json:"maxFiles"`
}

func (c *Config) ServiceVolumeOwnershipFingerprint(environment string, service *ServiceConfig) string {
	if c == nil || service == nil {
		return ""
	}
	type entry struct {
		LogicalName string `json:"logicalName"`
		DockerName  string `json:"dockerName"`
		Owner       string `json:"owner"`
	}
	entries := make([]entry, 0)
	seen := make(map[string]bool)
	for _, mount := range service.Volumes {
		parts := strings.SplitN(mount, ":", 2)
		logicalName := parts[0]
		if strings.HasPrefix(logicalName, "/") || seen[logicalName] {
			continue
		}
		volume, ok := c.GetVolume(logicalName)
		if !ok || volume.Owner == "" {
			continue
		}
		seen[logicalName] = true
		entries = append(entries, entry{LogicalName: logicalName, DockerName: c.GetVolumeName(logicalName, environment), Owner: volume.Owner})
	}
	if len(entries) == 0 {
		return ""
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].LogicalName < entries[j].LogicalName })
	data, _ := json.Marshal(entries)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// ServiceFileConfig distributes one local regular file or directory into a
// service container as a read-only bind mount. Secret forces private modes.
type ServiceFileConfig struct {
	Source string `yaml:"source" json:"source"`
	Target string `yaml:"target" json:"target"`
	Secret bool   `yaml:"secret,omitempty" json:"secret,omitempty"`
	Owner  string `yaml:"owner,omitempty" json:"owner,omitempty"`
}

// IsJob reports whether the service is a scheduled job workload.
func (s *ServiceConfig) IsJob() bool {
	return s.Kind == ServiceKindJob
}

// IsRun reports whether the service is a deploy-time run-to-completion step.
func (s *ServiceConfig) IsRun() bool {
	return s.Kind == ServiceKindRun
}

// ClearBuild removes the complete build definition when an image override
// changes the service to a prebuilt image.
func (s *ServiceConfig) ClearBuild() {
	s.Build = ""
	s.BuildArgs = nil
	s.BuildTarget = ""
	s.Dockerfile = ""
	s.buildStructured = false
}

// HealthCheckConfig defines health check settings
type HealthCheckConfig struct {
	Command     string `yaml:"command,omitempty" json:"command,omitempty"`
	Path        string `yaml:"path" json:"path"`
	TCPPort     int    `yaml:"tcpPort,omitempty" json:"tcpPort,omitempty"`
	Interval    string `yaml:"interval" json:"interval"`
	Timeout     string `yaml:"timeout" json:"timeout"`
	Retries     int    `yaml:"retries" json:"retries"`
	StartPeriod string `yaml:"startPeriod,omitempty" json:"startPeriod,omitempty"` // Grace period before starting checks
}

// DeployConfig defines deployment strategy
type DeployConfig struct {
	Strategy string `yaml:"strategy,omitempty" json:"strategy,omitempty"` // recreate, rolling, blue_green
	// StrategyDefaulted records that validation selected Strategy because the
	// config omitted it. It is never serialized, so it stays out of the config
	// hash and the schema; a JSON/YAML round trip degrades it to explicit.
	StrategyDefaulted bool `yaml:"-" json:"-"`
	MaxUnavailable    int  `yaml:"maxUnavailable,omitempty" json:"maxUnavailable,omitempty"`
	MaxSurge          int  `yaml:"maxSurge,omitempty" json:"maxSurge,omitempty"`
	// RollbackOnFailure is tri-state: nil means the default (true). True keeps
	// the previous revision serving when a rollout fails; false disables
	// recreate restore and keeps failed new containers stopped for debugging.
	RollbackOnFailure *bool                 `yaml:"rollbackOnFailure,omitempty" json:"rollbackOnFailure,omitempty"`
	Readiness         DeployReadinessConfig `yaml:"readiness,omitempty" json:"readiness,omitempty"`
	SmokeTest         DeploySmokeTestConfig `yaml:"smokeTest,omitempty" json:"smokeTest,omitempty"`
	Promotion         string                `yaml:"promotion,omitempty" json:"promotion,omitempty"` // automatic, manual
	GracePeriod       string                `yaml:"gracePeriod,omitempty" json:"gracePeriod,omitempty"`
	Release           *ReleaseConfig        `yaml:"release,omitempty" json:"release,omitempty"`
}

// ReleaseConfig runs a command from the new revision's image exactly once
// per applied deploy, before traffic cutover; a non-zero exit aborts the
// rollout. Making the command itself re-runnable (e.g. migrations) is the
// application's responsibility.
type ReleaseConfig struct {
	Command []string `yaml:"command,omitempty" json:"command,omitempty"`
	Timeout string   `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	// Volumes opts the release container into the service's volume mounts.
	Volumes bool `yaml:"volumes,omitempty" json:"volumes,omitempty"`
}

// DeployReadinessConfig defines service readiness checks for rollout strategies.
type DeployReadinessConfig struct {
	Path     string `yaml:"path,omitempty" json:"path,omitempty"`
	TCPPort  int    `yaml:"tcpPort,omitempty" json:"tcpPort,omitempty"`
	Timeout  string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	Interval string `yaml:"interval,omitempty" json:"interval,omitempty"`
	Retries  int    `yaml:"retries,omitempty" json:"retries,omitempty"`
}

// DeploySmokeTestConfig defines post-readiness checks for blue-green promotion.
type DeploySmokeTestConfig struct {
	Path           string `yaml:"path,omitempty" json:"path,omitempty"`
	ExpectedStatus int    `yaml:"expectedStatus,omitempty" json:"expectedStatus,omitempty"`
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

// ProxyConfig defines per-service proxy settings.
type ProxyConfig struct {
	// Domain is where public traffic is served.
	Domain string `yaml:"domain,omitempty" json:"domain,omitempty"`

	// Domains adds co-equal serving hostnames beside Domain. Each serves
	// the same upstreams with its own ACME certificate; Domain stays the
	// primary for URL display and as the redirect target.
	Domains []string `yaml:"domains,omitempty" json:"domains,omitempty"`

	// Host is where internal traffic is served when visibility is internal.
	Host string `yaml:"host,omitempty" json:"host,omitempty"`

	// Visibility controls whether the route is public ACME-backed ingress or
	// private HTTP-only ingress intended for LAN/VPN/hosts-file resolution.
	Visibility string `yaml:"visibility,omitempty" json:"visibility,omitempty"`

	// CDN declares that public DNS intentionally terminates at an external
	// CDN. Domain readiness heuristics never substitute for this declaration.
	CDN string `yaml:"cdn,omitempty" json:"cdn,omitempty"`

	// RedirectFrom specifies domains that should redirect to the primary Domain
	// These domains will get their own TLS certificates and a 308 permanent redirect
	// to Domain that preserves the request path and method.
	// Example: ["www.example.com", "old.example.com"] -> redirects to "example.com"
	RedirectFrom []string `yaml:"redirectFrom,omitempty" json:"redirectFrom,omitempty"`

	Email string    `yaml:"email,omitempty" json:"email,omitempty"` // Email for Let's Encrypt
	TLS   TLSConfig `yaml:"tls,omitempty" json:"tls,omitempty"`

	// DynamicDomains enables ask-gated on-demand TLS for customer domains.
	DynamicDomains *DynamicDomainsConfig `yaml:"dynamicDomains,omitempty" json:"dynamicDomains,omitempty"`

	// BasicAuth protects every serving domain of this route with HTTP
	// basic authentication before requests reach the service.
	BasicAuth *ProxyBasicAuthConfig `yaml:"basicAuth,omitempty" json:"basicAuth,omitempty"`

	// AllowIps restricts the route to the listed client IPs/CIDRs; other
	// addresses receive 403. It matches the TCP peer unless TrustedProxies
	// is configured, in which case it matches Caddy's parsed client IP.
	AllowIps []string `yaml:"allowIps,omitempty" json:"allowIps,omitempty"`

	// TrustedProxies declares the explicit proxy/CDN CIDRs whose forwarded
	// client IP headers Caddy may trust for this route. CIDRs only.
	TrustedProxies []string `yaml:"trustedProxies,omitempty" json:"trustedProxies,omitempty"`

	// DenyPathPrefixes returns 404 for an exact path prefix or any child path
	// before authentication, body reads, or proxying.
	DenyPathPrefixes []string `yaml:"denyPathPrefixes,omitempty" json:"denyPathPrefixes,omitempty"`

	// AllowPathPrefixes restricts the route to exact prefixes and child paths;
	// other paths receive 404. DenyPathPrefixes always takes precedence.
	AllowPathPrefixes []string `yaml:"allowPathPrefixes,omitempty" json:"allowPathPrefixes,omitempty"`

	// MaxRequestBody limits request bodies accepted by this route, for example
	// 10MB. Oversized declared bodies receive 413 before proxying.
	MaxRequestBody string `yaml:"maxRequestBody,omitempty" json:"maxRequestBody,omitempty"`

	// ResponseHeaders adds or replaces response headers for this route.
	ResponseHeaders map[string]string `yaml:"responseHeaders,omitempty" json:"responseHeaders,omitempty"`
}

// ProxyBasicAuthConfig protects a proxy route with HTTP basic auth.
type ProxyBasicAuthConfig struct {
	Username string `yaml:"username" json:"username"`
	// PasswordBcrypt is the pre-computed bcrypt hash of the password —
	// never the plaintext. Mint one with `tako proxy hash-password`.
	// A pre-computed hash keeps redeploys idempotent (hashing at deploy
	// time would salt fresh every run and churn the proxy config).
	PasswordBcrypt string `yaml:"passwordBcrypt" json:"passwordBcrypt"`
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

func (p *ProxyConfig) EffectiveVisibility() string {
	if p == nil {
		return ProxyVisibilityPublic
	}
	switch strings.ToLower(strings.TrimSpace(p.Visibility)) {
	case ProxyVisibilityInternal:
		return ProxyVisibilityInternal
	default:
		return ProxyVisibilityPublic
	}
}

func (p *ProxyConfig) IsInternal() bool {
	return p != nil && p.EffectiveVisibility() == ProxyVisibilityInternal
}

func (p *ProxyConfig) IsPublic() bool {
	return p != nil && p.EffectiveVisibility() == ProxyVisibilityPublic
}

// GetPrimaryDomain returns the primary domain for this service
func (p *ProxyConfig) GetPrimaryDomain() string {
	if p.IsInternal() {
		return ""
	}
	return p.Domain
}

// GetAllDomains returns all serving domains — the primary first, then the
// additional `domains` entries — excluding redirect domains.
func (p *ProxyConfig) GetAllDomains() []string {
	if p == nil || p.IsInternal() || p.Domain == "" {
		return nil
	}
	domains := []string{p.Domain}
	seen := map[string]bool{strings.ToLower(p.Domain): true}
	for _, domain := range p.Domains {
		key := strings.ToLower(strings.TrimSpace(domain))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		domains = append(domains, strings.TrimSpace(domain))
	}
	return domains
}

func (p *ProxyConfig) GetPrimaryHost() string {
	if p == nil {
		return ""
	}
	if p.IsInternal() {
		return p.Host
	}
	return p.Domain
}

func (p *ProxyConfig) GetAllHosts() []string {
	if p == nil {
		return nil
	}
	if p.IsInternal() {
		if p.Host == "" {
			return nil
		}
		return []string{p.Host}
	}
	return p.GetAllDomains()
}

// GetRedirectDomains returns all domains that should redirect to the primary domain
func (p *ProxyConfig) GetRedirectDomains() []string {
	if p == nil || p.IsInternal() {
		return nil
	}
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
	Mode      string `yaml:"mode,omitempty" json:"mode,omitempty"`         // auto, off
	Provider  string `yaml:"provider,omitempty" json:"provider,omitempty"` // letsencrypt, zerossl (default: letsencrypt)
	Staging   bool   `yaml:"staging,omitempty" json:"staging,omitempty"`
	Challenge string `yaml:"challenge,omitempty" json:"challenge,omitempty"` // auto, dns
}

// BackupConfig defines per-service backup settings.
type BackupConfig struct {
	Schedule string               `yaml:"schedule,omitempty" json:"schedule,omitempty"` // cron format (e.g., "0 2 * * *")
	Retain   int                  `yaml:"retain,omitempty" json:"retain,omitempty"`     // days to retain backups
	Volumes  []string             `yaml:"volumes,omitempty" json:"volumes,omitempty"`   // optional logical service volumes to back up
	Storage  *BackupStorageConfig `yaml:"storage,omitempty" json:"storage,omitempty"`   // optional object storage target
}

// BackupStorageConfig defines an S3-compatible object storage target for
// off-node backup copies. R2, MinIO, B2, and Spaces use the s3-compatible API.
type BackupStorageConfig struct {
	Provider        string `yaml:"provider,omitempty" json:"provider,omitempty"`               // s3, r2, s3-compatible
	Bucket          string `yaml:"bucket,omitempty" json:"bucket,omitempty"`                   // Object storage bucket
	Region          string `yaml:"region,omitempty" json:"region,omitempty"`                   // AWS region or "auto" for R2
	Endpoint        string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`               // Required for r2/s3-compatible
	Prefix          string `yaml:"prefix,omitempty" json:"prefix,omitempty"`                   // Optional object key prefix
	AccessKeyID     string `yaml:"accessKeyId,omitempty" json:"accessKeyId,omitempty"`         // Use ${ENV_VAR}
	SecretAccessKey string `yaml:"secretAccessKey,omitempty" json:"secretAccessKey,omitempty"` // Use ${ENV_VAR}
	SessionToken    string `yaml:"sessionToken,omitempty" json:"sessionToken,omitempty"`       // Optional temporary credential token
	ForcePathStyle  bool   `yaml:"forcePathStyle,omitempty" json:"forcePathStyle,omitempty"`   // Needed by some S3-compatible stores
	Versioned       bool   `yaml:"versioned,omitempty" json:"versioned,omitempty"`             // Bucket uses object versioning APIs
}

// ResourceLimitsConfig defines container runtime resource limits.
type ResourceLimitsConfig struct {
	Memory string `yaml:"memory,omitempty" json:"memory,omitempty"` // Docker memory limit, for example 512m or 1g
	CPUs   string `yaml:"cpus,omitempty" json:"cpus,omitempty"`     // Docker --cpus limit, for example 0.5 or 2
}

// MonitoringConfig defines per-service monitoring settings
type MonitoringConfig struct {
	Enabled      bool   `yaml:"enabled" json:"enabled"`                               // Enable monitoring for this service
	Interval     string `yaml:"interval,omitempty" json:"interval,omitempty"`         // Check interval (10s..1h, default 60s)
	Timeout      string `yaml:"timeout,omitempty" json:"timeout,omitempty"`           // Check timeout (1s..60s, default 10s, < interval)
	AlertAfter   int    `yaml:"alertAfter,omitempty" json:"alertAfter,omitempty"`     // Overrides notifications.alerts.services.alertAfter
	RecoverAfter int    `yaml:"recoverAfter,omitempty" json:"recoverAfter,omitempty"` // Overrides notifications.alerts.services.recoverAfter
	Webhook      string `yaml:"webhook,omitempty" json:"webhook,omitempty"`           // Extra webhook target for this service only
	CheckType    string `yaml:"checkType,omitempty" json:"checkType,omitempty"`       // "http" or "container" (default: auto-detect)
}

// DeliverableWebhook returns the normalized per-service webhook when it
// passes the alert target rules (public HTTPS), and "" when it is unset or
// fails them. A failing value was accepted by earlier releases; it is
// reported as a deprecation warning and is never sent to takod or used for
// local delivery.
func (m *MonitoringConfig) DeliverableWebhook() string {
	if m == nil || strings.TrimSpace(m.Webhook) == "" {
		return ""
	}
	normalized, err := alerttarget.NormalizeAndValidate("webhook", m.Webhook)
	if err != nil {
		return ""
	}
	return normalized
}

// WebhookDeprecated reports whether the configured webhook fails the alert
// target rules and is therefore ignored for delivery.
func (m *MonitoringConfig) WebhookDeprecated() bool {
	return m != nil && strings.TrimSpace(m.Webhook) != "" && m.DeliverableWebhook() == ""
}

// ExplicitHealthKeys reports whether this block sets a monitoring key that
// only node-side health monitoring understands.
func (m *MonitoringConfig) ExplicitHealthKeys() bool {
	return m != nil && (strings.TrimSpace(m.Timeout) != "" || m.AlertAfter != 0 || m.RecoverAfter != 0)
}

// EnvironmentConfig defines an environment (production, staging, etc.)
type EnvironmentConfig struct {
	// Extends names the environment this one inherits from and Abstract
	// marks a template-only environment. Both are consumed while the config
	// document is resolved, so they are always empty on a loaded config and
	// are never written back out.
	Extends  string `yaml:"extends,omitempty" json:"extends,omitempty"`
	Abstract bool   `yaml:"abstract,omitempty" json:"abstract,omitempty"`

	Servers        []string                `yaml:"servers" json:"servers"`                                   // List of server names to use
	ServerSelector *ServerSelector         `yaml:"serverSelector,omitempty" json:"serverSelector,omitempty"` // Label-based server selection
	Proxy          *EnvironmentProxyConfig `yaml:"proxy,omitempty" json:"proxy,omitempty"`                   // Environment-level proxy placement
	// Deprecated: environments.<env>.labels has no effect. Set
	// servers.<name>.labels and select them with serverSelector.labels or
	// placement constraints. Reported by ValidationWarnings until
	// DeprecatedKeysRemovalRelease.
	Labels        map[string]string        `yaml:"labels,omitempty" json:"labels,omitempty"`
	Services      map[string]ServiceConfig `yaml:"services" json:"services"`                               // Services to deploy in this environment
	SecretSources []SecretSource           `yaml:"secretSources,omitempty" json:"secretSources,omitempty"` // Environment-specific external secret stores

	// SelfHealing sets the default self-healing policy for this environment's
	// services. Persistent services ignore the environment-level enabled flag.
	SelfHealing *SelfHealingConfig `yaml:"selfHealing,omitempty" json:"selfHealing,omitempty"`

	// Preview makes this environment a preview base (see PreviewConfig).
	// Like Extends and Abstract it is consumed while the document is
	// resolved: it is not inherited and is always nil on a loaded config.
	Preview *PreviewConfig `yaml:"preview,omitempty" json:"preview,omitempty"`
}

// EnvironmentProxyConfig controls where environment-level proxy routes are
// reconciled. Services still use their own placement for containers.
type EnvironmentProxyConfig struct {
	Placement *PlacementConfig `yaml:"placement,omitempty" json:"placement,omitempty"`
	// ACME is the environment-scoped DNS-01 provider used by routes that
	// explicitly opt in. Raw credential values must be whole ${ENV_VAR}
	// references and never enter route manifests or replicated state.
	ACME *EnvironmentACMEConfig `yaml:"acme,omitempty" json:"acme,omitempty"`
}

// EnvironmentACMEConfig configures takod-managed DNS-01 issuance.
type EnvironmentACMEConfig struct {
	DNSProvider string            `yaml:"dnsProvider" json:"dnsProvider"`
	Credentials map[string]string `yaml:"credentials" json:"credentials"`
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
	if s.IsRun() {
		return ServiceKindRun
	}
	if s.IsJob() {
		return ServiceKindJob
	}
	if s.Persistent {
		return "persistent" // Database, cache, etc.
	}
	if s.IsPublic() {
		return "public" // Public web service
	}
	if s.Port > 0 {
		return "internal" // Internal API
	}
	return "worker" // Background worker
}

// IsPublic returns true if service should be exposed publicly
func (s *ServiceConfig) IsPublic() bool {
	return s.Proxy != nil && s.Proxy.IsPublic()
}

// IsProxied returns true when the service should be routed through tako-proxy.
func (s *ServiceConfig) IsProxied() bool {
	return s.Proxy != nil
}

// IsInternal returns true if service is internal-only
func (s *ServiceConfig) IsInternal() bool {
	return s.Port > 0 && (s.Proxy == nil || s.Proxy.IsInternal())
}

// IsWorker returns true if service is a background worker
func (s *ServiceConfig) IsWorker() bool {
	return s.Port == 0
}

// GetDefaultEnvironment returns the environment used when none is selected;
// see ResolveDefaultEnvironment.
func (c *Config) GetDefaultEnvironment() string {
	name, _ := c.ResolveDefaultEnvironment()
	return name
}

// ResolveDefaultEnvironment deterministically picks the environment used when
// none is selected, from the deployable (non-abstract) environments: the only
// one, else "production", else the first name in ascending order. implicit is
// true only in that last case, so callers can tell the user to pass -e. It
// returns "" when no environments are configured.
func (c *Config) ResolveDefaultEnvironment() (name string, implicit bool) {
	if c == nil || len(c.Environments) == 0 {
		return "", false
	}
	names := make([]string, 0, len(c.Environments))
	for envName := range c.Environments {
		names = append(names, envName)
	}
	if len(names) == 1 {
		return names[0], false
	}
	if _, exists := c.Environments["production"]; exists {
		return "production", false
	}
	sort.Strings(names)
	return names[0], true
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
		if c.IsAbstractEnvironment(name) {
			if children := c.resolution.concreteDescendants(name); len(children) > 0 {
				return nil, fmt.Errorf("environment '%s' is abstract and cannot be deployed; use one of: %s", name, strings.Join(children, ", "))
			}
			return nil, fmt.Errorf("environment '%s' is abstract and cannot be deployed; no environment extends it", name)
		}
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
	var placement *PlacementConfig
	if proxy != nil {
		placement = proxy.Placement
	}
	targets, err := ResolvePlacementTargets(placement, servers, environmentServers, environment)
	if err != nil {
		return nil, err
	}
	edges := make([]string, 0, len(targets))
	for _, target := range targets {
		server := servers[target]
		if !server.Schedulable() {
			if placement != nil && strings.TrimSpace(placement.Strategy) == "pinned" {
				return nil, fmt.Errorf("proxy placement target %s is %s and cannot receive proxy mutations", target, server.Lifecycle)
			}
			continue
		}
		if server.HasPlatformRole(nodeidentity.RoleEdge) {
			edges = append(edges, target)
			continue
		}
		if placement != nil && strings.TrimSpace(placement.Strategy) == "pinned" {
			return nil, fmt.Errorf("proxy placement target %s does not have the edge role", target)
		}
	}
	if len(edges) == 0 {
		return nil, fmt.Errorf("environment %s has no edge-role proxy nodes", environment)
	}
	return edges, nil
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

// GetBuildStrategy returns where build-backed service images should be built.
func (c *Config) GetBuildStrategy() string {
	if c == nil || c.Deployment == nil || c.Deployment.Build == nil || c.Deployment.Build.Strategy == "" {
		return BuildStrategyRemote
	}
	return c.Deployment.Build.Strategy
}

// SetBuildStrategy overrides the configured image build strategy.
func (c *Config) SetBuildStrategy(strategy string) error {
	normalized, err := NormalizeBuildStrategy(strategy)
	if err != nil {
		return err
	}
	if c.Deployment == nil {
		c.Deployment = &DeploymentConfig{}
	}
	if c.Deployment.Build == nil {
		c.Deployment.Build = &BuildConfig{}
	}
	c.Deployment.Build.Strategy = normalized
	return nil
}

// NormalizeBuildStrategy validates and canonicalizes a build strategy value.
func NormalizeBuildStrategy(strategy string) (string, error) {
	strategy = strings.ToLower(strings.TrimSpace(strategy))
	if strategy == "" {
		return BuildStrategyRemote, nil
	}
	switch strategy {
	case BuildStrategyRemote, BuildStrategyLocal, BuildStrategyAuto:
		return strategy, nil
	default:
		return "", fmt.Errorf("deployment.build.strategy must be remote, local, or auto")
	}
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
	expanded, missing := expandConfigText(s, ignoreYAMLComments, false)
	if len(missing) > 0 {
		return "", missingEnvironmentVariablesError(missing)
	}
	return expanded, nil
}

func missingEnvironmentVariablesError(missing []string) error {
	return fmt.Errorf("missing environment variable(s): %s", strings.Join(missing, ", "))
}

// expandConfigText expands ${VAR} references from the process environment,
// trimming values and skipping YAML comment text when ignoreYAMLComments is
// set. It returns the sorted, unique names that are not set. With
// keepMissing, an unset reference stays in the output as the literal
// ${VAR} text so callers can tell where it was used; otherwise it expands
// to the empty string.
func expandConfigText(s string, ignoreYAMLComments bool, keepMissing bool) (string, []string) {
	var result strings.Builder
	missing := make([]string, 0)
	seenMissing := map[string]bool{}
	lookup := func(key string) (string, bool) {
		if value, ok := os.LookupEnv(key); ok {
			return strings.TrimSpace(value), true
		}
		if !seenMissing[key] {
			seenMissing[key] = true
			missing = append(missing, key)
		}
		if keepMissing {
			return "${" + key + "}", true
		}
		return "", false
	}

	for _, line := range strings.SplitAfter(s, "\n") {
		content := line
		comment := ""
		if ignoreYAMLComments {
			content, comment = splitYAMLComment(line)
		}

		expanded, _ := envexpand.Braced(content, lookup)
		result.WriteString(expanded)
		result.WriteString(comment)
	}

	sort.Strings(missing)
	return result.String(), missing
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

// LoadOptions changes how LoadConfigWithOptions builds the config.
type LoadOptions struct {
	// Preview synthesizes the preview environment preview-<name> from its
	// base before validation; see PreviewInstance.
	Preview *PreviewInstance
	// Environment selects the one environment the command works on and
	// turns on environment-scoped ${VAR} expansion (see
	// LoadConfigForEnvironment). With Preview set, the scope is the
	// preview's base (Preview.Base, or Environment when Base is empty) and
	// its extends chain: the preview environment is synthesized from it, so
	// the base is never pruned and its references are always required.
	// Empty requires every reference in the file.
	Environment string
}

// LoadConfig loads the configuration from a YAML or JSON file. Every
// ${VAR} reference in the file must be set.
func LoadConfig(configPath string) (*Config, error) {
	return LoadConfigWithOptions(configPath, LoadOptions{})
}

// LoadConfigForEnvironment loads the configuration for a command that works
// on one explicitly selected environment. Top-level sections, the selected
// environment, the environments it extends, and the servers it can use must
// have every ${VAR} set. A reference that is unset and appears only in other
// environments, or in servers the selected environment cannot use, does not
// fail the load: those environments and servers are removed from the
// returned config so their unexpanded values can never be used, and the
// skipped names are reported by DeferredEnvironmentVariables. With an empty
// environment this is LoadConfig.
func LoadConfigForEnvironment(configPath string, environment string) (*Config, error) {
	return LoadConfigWithOptions(configPath, LoadOptions{Environment: environment})
}

// LoadConfigWithOptions is LoadConfig with options. With zero options it
// behaves exactly like LoadConfig.
func LoadConfigWithOptions(configPath string, opts LoadOptions) (*Config, error) {
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

	// Registry passwords must be env refs, not literals; the check runs on
	// the raw content because expansion below erases the distinction.
	if err := validateRawRegistryCredentials(data, isJSON); err != nil {
		return nil, err
	}
	if err := validateRawACMEDNSCredentials(data, isJSON); err != nil {
		return nil, err
	}
	if err := validateRawAliases(data, isJSON); err != nil {
		return nil, err
	}
	if err := validateRawHookCommands(data, isJSON); err != nil {
		return nil, err
	}

	// Expand environment variables in the content with trimming, then
	// resolve environment inheritance and decode into the Config struct.
	var parsed *Config
	if scope := opts.scopeEnvironment(); scope == "" {
		expandedData, err := expandEnvWithTrim(string(data), !isJSON)
		if err != nil {
			return nil, fmt.Errorf("failed to expand config environment variables: %w", err)
		}
		parsed, err = ParseConfigDocumentWithOptions([]byte(expandedData), isJSON, opts)
		if err != nil {
			return nil, err
		}
	} else {
		parsed, err = parseEnvironmentScopedDocument(string(data), isJSON, scope, opts)
		if err != nil {
			return nil, err
		}
	}
	config := *parsed

	normalizeConfigRelativePaths(&config, configDir)
	if err := materializeDefaultPlatformInventory(&config); err != nil {
		return nil, fmt.Errorf("materialize platform inventory: %w", err)
	}
	// Validate config
	if err := ValidateConfig(&config); err != nil {
		return nil, fmt.Errorf("invalid config: %w", config.annotateEnvironmentChain(err))
	}
	if err := validateExistingProjectClusterBinding(&config, configPath); err != nil {
		return nil, err
	}

	return &config, nil
}

// ParseConfigDocument strictly decodes a tako.yaml (isJSON false) or
// tako.json document without validating it. Top-level x- keys are dropped
// and environments are resolved through extends/abstract first, so every
// environment in the result is flattened and deployable. A document that
// uses none of these features is decoded exactly as written. Errors carry
// the "failed to parse YAML config:"/"failed to parse JSON config:" prefix
// for decode failures and "invalid config:" for inheritance failures.
func ParseConfigDocument(data []byte, isJSON bool) (*Config, error) {
	return ParseConfigDocumentWithOptions(data, isJSON, LoadOptions{})
}

// ParseConfigDocumentWithOptions is ParseConfigDocument with load options.
// With opts.Preview set, the preview environment is synthesized from its
// base and the preview policy is applied to it before the config is
// returned; validation is still the caller's job.
func ParseConfigDocumentWithOptions(data []byte, isJSON bool, opts LoadOptions) (*Config, error) {
	format := "YAML"
	if isJSON {
		format = "JSON"
	}
	var instance *previewRequest
	if opts.Preview != nil {
		name, err := NormalizePreviewName(opts.Preview.Name)
		if err != nil {
			return nil, fmt.Errorf("invalid config: %w", err)
		}
		instance = &previewRequest{name: name, identity: PreviewIdentity(name), base: strings.TrimSpace(opts.Preview.Base)}
	}
	resolved, resolution, err := resolveConfigDocumentWithPreview(data, isJSON, instance)
	if err != nil {
		var decodeErr *configDecodeError
		if errors.As(err, &decodeErr) {
			return nil, fmt.Errorf("failed to parse %s config: %w", format, decodeErr.err)
		}
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	var config Config
	if isJSON {
		decoder := json.NewDecoder(bytes.NewReader(resolved))
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
		decoder := yaml.NewDecoder(bytes.NewReader(resolved))
		decoder.KnownFields(true) // Strict mode - error on unknown fields
		if err := decoder.Decode(&config); err != nil {
			if resolution != nil {
				err = remapYAMLErrorLines(err, resolution.encodedLines)
			}
			return nil, fmt.Errorf("failed to parse YAML config: %w", err)
		}
	}
	for name, env := range config.Environments {
		if env.Extends != "" || env.Abstract || env.Preview != nil {
			return nil, fmt.Errorf("invalid config: environment %s: extends/abstract/preview could not be resolved", name)
		}
	}
	config.resolution = resolution
	if instance != nil {
		if err := applyPreviewPolicy(&config); err != nil {
			return nil, fmt.Errorf("invalid config: %w", err)
		}
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
	for name, build := range cfg.Builds {
		if build.declaredContext == "" {
			build.declaredContext = filepath.ToSlash(filepath.Clean(strings.TrimSpace(build.Context)))
		}
		build.Context = resolveConfigRelativePath(absConfigDir, build.Context)
		cfg.Builds[name] = build
	}
	for envName, env := range cfg.Environments {
		for serviceName, service := range env.Services {
			service.Build = resolveConfigRelativePath(absConfigDir, service.Build)
			service.EnvFile = resolveConfigRelativePath(absConfigDir, service.EnvFile)
			for i := range service.EnvFiles {
				service.EnvFiles[i] = resolveConfigRelativePath(absConfigDir, service.EnvFiles[i])
			}
			for i := range service.Files {
				service.Files[i].Source = resolveConfigRelativePath(absConfigDir, service.Files[i].Source)
			}
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
