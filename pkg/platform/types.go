// Package platform owns the single-controller PaaS foundation installed on
// the first Tako node. It is separate from application desired state.
package platform

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

const (
	APIVersion         = "tako.io/v1"
	BootstrapKind      = "PlatformBootstrap"
	PolicyKind         = "PlatformResourcePolicy"
	DefaultStateDir    = "/var/lib/tako/platform"
	DefaultConfigDir   = "/etc/tako"
	DefaultAuditDir    = "/var/log/tako"
	DefaultWorkerUser  = "tako-platform"
	DefaultWorkerGroup = "tako-platform"
	// Keep the established deployer group during the bootstrap milestone so
	// existing SSH deployments remain available until the durable ingress is
	// introduced and migrated in the next milestone.
	DefaultSocketGroup  = "tako"
	DefaultSocket       = "/run/tako/takod.sock"
	DefaultBinaryPath   = "/usr/local/lib/tako/tako"
	DefaultJournalName  = "operations.jsonl"
	DefaultAuditLogName = "platform-audit.jsonl"
)

var systemIdentifierPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,30}$`)

type ResourcePolicy struct {
	APIVersion              string `json:"apiVersion"`
	Kind                    string `json:"kind"`
	ReservedMemoryBytes     int64  `json:"reservedMemoryBytes"`
	ReservedDiskBytes       int64  `json:"reservedDiskBytes"`
	MinimumFreeDiskBytes    int64  `json:"minimumFreeDiskBytes"`
	MaximumConcurrentBuilds int    `json:"maximumConcurrentBuilds"`
	MaximumConcurrentOps    int    `json:"maximumConcurrentOperations"`
}

func DefaultResourcePolicy() ResourcePolicy {
	return ResourcePolicy{
		APIVersion:              APIVersion,
		Kind:                    PolicyKind,
		ReservedMemoryBytes:     512 << 20,
		ReservedDiskBytes:       5 << 30,
		MinimumFreeDiskBytes:    10 << 30,
		MaximumConcurrentBuilds: 1,
		MaximumConcurrentOps:    2,
	}
}

func (p ResourcePolicy) Validate() error {
	if p.APIVersion != APIVersion || p.Kind != PolicyKind {
		return fmt.Errorf("platform resource policy apiVersion/kind is invalid")
	}
	if p.ReservedMemoryBytes < 256<<20 {
		return fmt.Errorf("controller memory reservation must be at least 256 MiB")
	}
	if p.ReservedDiskBytes < 1<<30 {
		return fmt.Errorf("controller disk reservation must be at least 1 GiB")
	}
	if p.MinimumFreeDiskBytes < p.ReservedDiskBytes {
		return fmt.Errorf("minimum free disk must be at least the controller disk reservation")
	}
	if p.MaximumConcurrentBuilds < 1 || p.MaximumConcurrentBuilds > 32 {
		return fmt.Errorf("maximum concurrent builds must be between 1 and 32")
	}
	if p.MaximumConcurrentOps < 1 || p.MaximumConcurrentOps > 128 {
		return fmt.Errorf("maximum concurrent operations must be between 1 and 128")
	}
	return nil
}

type BootstrapConfig struct {
	RootDir             string
	NodeName            string
	ClusterID           string
	NodeID              string
	IdentityPath        string
	StateDir            string
	ConfigDir           string
	AuditDir            string
	SocketPath          string
	DockerDataRoot      string
	BinaryPath          string
	ServiceBinaryPath   string
	WorkerUser          string
	WorkerGroup         string
	SocketGroup         string
	Policy              ResourcePolicy
	WorkerUserExplicit  bool
	WorkerGroupExplicit bool
	PolicyExplicit      bool
	RequireRoot         bool
	Now                 func() time.Time
}

func (c BootstrapConfig) withDefaults() BootstrapConfig {
	if strings.TrimSpace(c.IdentityPath) == "" {
		c.IdentityPath = nodeidentity.DefaultPath
	}
	if strings.TrimSpace(c.StateDir) == "" {
		c.StateDir = DefaultStateDir
	}
	if strings.TrimSpace(c.ConfigDir) == "" {
		c.ConfigDir = DefaultConfigDir
	}
	if strings.TrimSpace(c.AuditDir) == "" {
		c.AuditDir = DefaultAuditDir
	}
	if strings.TrimSpace(c.SocketPath) == "" {
		c.SocketPath = DefaultSocket
	}
	if strings.TrimSpace(c.WorkerUser) == "" {
		c.WorkerUser = DefaultWorkerUser
	}
	if strings.TrimSpace(c.WorkerGroup) == "" {
		c.WorkerGroup = DefaultWorkerGroup
	}
	if strings.TrimSpace(c.SocketGroup) == "" {
		c.SocketGroup = DefaultSocketGroup
	}
	if strings.TrimSpace(c.ServiceBinaryPath) == "" {
		c.ServiceBinaryPath = DefaultBinaryPath
	}
	if c.Policy.APIVersion == "" {
		c.Policy = DefaultResourcePolicy()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

func (c BootstrapConfig) Validate() error {
	if strings.TrimSpace(c.NodeName) == "" {
		return fmt.Errorf("node name is required")
	}
	if strings.TrimSpace(c.BinaryPath) == "" || !filepath.IsAbs(c.BinaryPath) {
		return fmt.Errorf("an absolute Tako source binary path is required")
	}
	if !filepath.IsAbs(c.ServiceBinaryPath) {
		return fmt.Errorf("the installed Tako service binary path must be absolute")
	}
	if !systemIdentifierPattern.MatchString(c.WorkerUser) || !systemIdentifierPattern.MatchString(c.WorkerGroup) || !systemIdentifierPattern.MatchString(c.SocketGroup) {
		return fmt.Errorf("platform worker user and groups must be safe system identifiers")
	}
	if c.WorkerUser == "root" || c.WorkerGroup == "root" || c.SocketGroup == "root" {
		return fmt.Errorf("platform worker must use dedicated non-root accounts")
	}
	if c.SocketGroup != DefaultSocketGroup {
		return fmt.Errorf("platform socket group must be %q", DefaultSocketGroup)
	}
	if c.WorkerGroup == c.SocketGroup {
		return fmt.Errorf("platform worker group must be dedicated and distinct from the deployer socket group")
	}
	for label, value := range map[string]string{
		"identity path":    c.IdentityPath,
		"state directory":  c.StateDir,
		"config directory": c.ConfigDir,
		"audit directory":  c.AuditDir,
		"socket path":      c.SocketPath,
	} {
		if !filepath.IsAbs(value) {
			return fmt.Errorf("%s must be absolute", label)
		}
	}
	return c.Policy.Validate()
}

func (c BootstrapConfig) hostPath(path string) string {
	if strings.TrimSpace(c.RootDir) == "" {
		return path
	}
	return filepath.Join(c.RootDir, strings.TrimPrefix(filepath.Clean(path), string(filepath.Separator)))
}

type BootstrapState struct {
	APIVersion        string    `json:"apiVersion"`
	Kind              string    `json:"kind"`
	ClusterID         string    `json:"clusterId"`
	NodeID            string    `json:"nodeId"`
	NodeName          string    `json:"nodeName"`
	ControllerMode    string    `json:"controllerMode"`
	EnrollmentRoles   []string  `json:"enrollmentRoles"`
	IdentityPath      string    `json:"identityPath"`
	StateDir          string    `json:"stateDir"`
	AuditDir          string    `json:"auditDir"`
	SocketPath        string    `json:"socketPath"`
	DockerDataRoot    string    `json:"dockerDataRoot"`
	SocketGroup       string    `json:"socketGroup"`
	ServiceBinaryPath string    `json:"serviceBinaryPath"`
	WorkerUser        string    `json:"workerUser"`
	WorkerGroup       string    `json:"workerGroup"`
	WorkerUID         int       `json:"workerUid"`
	WorkerGID         int       `json:"workerGid"`
	SocketGroupGID    int       `json:"socketGroupGid"`
	InitializedAt     time.Time `json:"initializedAt"`
}

func (s BootstrapState) Validate() error {
	if s.APIVersion != APIVersion || s.Kind != BootstrapKind {
		return fmt.Errorf("platform bootstrap apiVersion/kind is invalid")
	}
	if err := (nodeidentity.Reference{ClusterID: s.ClusterID, NodeID: s.NodeID}).Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(s.NodeName) == "" || s.ControllerMode != "single-writer" {
		return fmt.Errorf("platform bootstrap must name a single-writer controller")
	}
	if !slices.Equal(s.EnrollmentRoles, firstNodeRoles) {
		return fmt.Errorf("first-node enrollment roles are invalid")
	}
	for _, path := range []string{s.IdentityPath, s.StateDir, s.AuditDir, s.SocketPath, s.DockerDataRoot, s.ServiceBinaryPath} {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("platform bootstrap paths must be absolute")
		}
	}
	if !systemIdentifierPattern.MatchString(s.WorkerUser) || !systemIdentifierPattern.MatchString(s.WorkerGroup) || !systemIdentifierPattern.MatchString(s.SocketGroup) {
		return fmt.Errorf("platform worker account identifiers are invalid")
	}
	if s.WorkerUser == "root" || s.WorkerGroup == "root" || s.SocketGroup == "root" {
		return fmt.Errorf("platform worker accounts must be non-root")
	}
	if s.WorkerUID <= 0 || s.WorkerGID <= 0 || s.SocketGroupGID <= 0 {
		return fmt.Errorf("platform worker numeric account identities must be non-root")
	}
	if s.SocketGroup != DefaultSocketGroup {
		return fmt.Errorf("platform socket group is invalid")
	}
	if s.WorkerGroup == s.SocketGroup {
		return fmt.Errorf("platform worker group must be dedicated")
	}
	if s.InitializedAt.IsZero() {
		return fmt.Errorf("platform initialization time is required")
	}
	return nil
}

type BootstrapResult struct {
	State       BootstrapState `json:"state"`
	Policy      ResourcePolicy `json:"policy"`
	Resumed     bool           `json:"resumed"`
	JournalPath string         `json:"journalPath"`
	AuditPath   string         `json:"auditPath"`
}
