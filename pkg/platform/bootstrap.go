package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/fileutil"
	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

var firstNodeRoles = []string{
	nodeidentity.RoleBuilder,
	nodeidentity.RoleControlPlane,
	nodeidentity.RoleEdge,
	nodeidentity.RoleWorker,
}

type Host interface {
	ResolveDockerDataRoot(context.Context, string) (string, error)
	EnsurePlatformAccounts(context.Context, string, string, string) (PlatformAccountIDs, error)
	StageBinary(context.Context, string, string) error
	InstallUnit(context.Context, string, string) error
	ReloadEnableRestart(context.Context, ...string) error
	WaitReady(context.Context, ReadinessCheck) error
}

type PlatformAccountIDs struct {
	WorkerUID      int
	WorkerGID      int
	SocketGroupGID int
}

type ReadinessCheck struct {
	SocketPath       string
	WorkerSocketPath string
	WorkerUID        int
	ClusterID        string
	NodeID           string
	StateDir         string
	Since            time.Time
}

type Bootstrapper struct {
	host Host
}

func NewBootstrapper(host Host) (*Bootstrapper, error) {
	if host == nil {
		return nil, fmt.Errorf("platform bootstrap host is required")
	}
	return &Bootstrapper{host: host}, nil
}

type ConfigDocument struct {
	State  BootstrapState `json:"state"`
	Policy ResourcePolicy `json:"policy"`
}

func (b *Bootstrapper) Bootstrap(ctx context.Context, config BootstrapConfig) (*BootstrapResult, error) {
	config = config.withDefaults()
	identityPath := config.hostPath(config.IdentityPath)
	configPath := filepath.Join(config.hostPath(config.ConfigDir), "platform.json")
	existingIdentity, err := nodeidentity.ReadOptional(identityPath)
	if err != nil {
		return nil, fmt.Errorf("read installation identity: %w", err)
	}
	existingConfig, configErr := readConfigDocument(configPath)
	if configErr != nil && !errors.Is(configErr, os.ErrNotExist) {
		return nil, fmt.Errorf("read existing platform config: %w", configErr)
	}
	if existingConfig != nil && existingIdentity == nil {
		return nil, fmt.Errorf("platform config exists without an installation identity")
	}
	if existingIdentity != nil {
		if strings.TrimSpace(config.NodeName) == "" {
			config.NodeName = existingIdentity.NodeName
		}
		if existingConfig != nil {
			if existingConfig.State.ClusterID != existingIdentity.ClusterID || existingConfig.State.NodeID != existingIdentity.NodeID {
				return nil, fmt.Errorf("existing platform config belongs to another cluster member")
			}
			if config.WorkerUserExplicit && config.WorkerUser != existingConfig.State.WorkerUser {
				return nil, fmt.Errorf("platform worker user differs from persisted config; use an explicit reconfigure workflow")
			}
			if config.WorkerGroupExplicit && config.WorkerGroup != existingConfig.State.WorkerGroup {
				return nil, fmt.Errorf("platform worker group differs from persisted config; use an explicit reconfigure workflow")
			}
			if config.PolicyExplicit && config.Policy != existingConfig.Policy {
				return nil, fmt.Errorf("platform resource policy differs from persisted config; use an explicit reconfigure workflow")
			}
			config.WorkerUser = existingConfig.State.WorkerUser
			config.WorkerGroup = existingConfig.State.WorkerGroup
			config.SocketGroup = existingConfig.State.SocketGroup
			config.DockerDataRoot = existingConfig.State.DockerDataRoot
			config.ServiceBinaryPath = existingConfig.State.ServiceBinaryPath
			config.Policy = existingConfig.Policy
		}
	} else if strings.TrimSpace(config.NodeName) == "" {
		config.NodeName = "node-1"
	}
	if config.RequireRoot && !runningAsRoot() {
		return nil, fmt.Errorf("platform init must run as root")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if existingIdentity != nil {
		if existingIdentity.NodeName != config.NodeName || !slices.Equal(existingIdentity.EnrollmentRoles, firstNodeRoles) {
			return nil, fmt.Errorf("existing installation identity is not the requested first-node enrollment")
		}
		if strings.TrimSpace(config.ClusterID) != "" && !strings.EqualFold(existingIdentity.ClusterID, config.ClusterID) {
			return nil, fmt.Errorf("existing cluster ID does not match requested cluster ID")
		}
		if strings.TrimSpace(config.NodeID) != "" && !strings.EqualFold(existingIdentity.NodeID, config.NodeID) {
			return nil, fmt.Errorf("existing node ID does not match requested node ID")
		}
	}
	dockerDataRoot, err := b.host.ResolveDockerDataRoot(ctx, config.DockerDataRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve Docker data root: %w", err)
	}
	config.DockerDataRoot = dockerDataRoot
	if err := config.Validate(); err != nil {
		return nil, err
	}
	accountIDs, err := b.host.EnsurePlatformAccounts(ctx, config.WorkerUser, config.WorkerGroup, config.SocketGroup)
	if err != nil {
		return nil, fmt.Errorf("ensure platform worker account: %w", err)
	}
	if existingConfig != nil && (existingConfig.State.WorkerUID != accountIDs.WorkerUID || existingConfig.State.WorkerGID != accountIDs.WorkerGID || existingConfig.State.SocketGroupGID != accountIDs.SocketGroupGID) {
		return nil, fmt.Errorf("platform account numeric identities changed; use an explicit repair workflow")
	}
	uid, gid := accountIDs.WorkerUID, accountIDs.WorkerGID
	stateDir := config.hostPath(config.StateDir)
	configDir := config.hostPath(config.ConfigDir)
	auditDir := config.hostPath(config.AuditDir)
	protectedWriteDirs := map[string]os.FileMode{
		filepath.Dir(identityPath): 0755,
		configDir:                  0755,
		stateDir:                   0750,
		filepath.Dir(DefaultMembershipPath(stateDir)): 0700,
		auditDir:                               0750,
		config.hostPath("/etc/tako/proxy"):     0750,
		config.hostPath("/var/log/tako/proxy"): 0750,
		config.hostPath("/var/lib/tako/certs"): 0750,
	}
	for path, mode := range protectedWriteDirs {
		if err := os.MkdirAll(path, mode); err != nil {
			return nil, fmt.Errorf("create protected platform directory %s: %w", path, err)
		}
		if err := os.Chmod(path, mode); err != nil {
			return nil, fmt.Errorf("secure platform directory %s: %w", path, err)
		}
	}
	if err := os.Chown(stateDir, uid, gid); err != nil {
		return nil, fmt.Errorf("own platform state directory: %w", err)
	}
	auditUID := 0
	if !config.RequireRoot {
		auditUID = uid
	}
	if err := os.Chown(auditDir, auditUID, gid); err != nil {
		return nil, fmt.Errorf("own platform audit directory: %w", err)
	}

	installation, resumed, err := ensureFirstNodeIdentity(identityPath, config)
	if err != nil {
		return nil, err
	}
	now := config.Now().UTC()
	operationID := fmt.Sprintf("platform-init-%d-%s", now.UnixNano(), installation.NodeID)
	journalPath := filepath.Join(stateDir, DefaultJournalName)
	auditPath := filepath.Join(auditDir, DefaultAuditLogName)
	if err := ensureOwnedDurableFile(journalPath, uid, gid); err != nil {
		return nil, err
	}
	if err := fileutil.SyncDirectory(filepath.Dir(journalPath)); err != nil {
		return nil, fmt.Errorf("sync journal directory: %w", err)
	}
	if err := ensureOwnedDurableFile(auditPath, auditUID, gid); err != nil {
		return nil, err
	}
	if err := fileutil.SyncDirectory(filepath.Dir(auditPath)); err != nil {
		return nil, fmt.Errorf("sync audit directory: %w", err)
	}
	journal, _ := NewJournal(journalPath)
	record := func(phase string, status string, message string) error {
		return journal.Append(OperationRecord{
			OperationID: operationID,
			Operation:   "platform.init",
			Phase:       phase,
			Status:      status,
			NodeID:      installation.NodeID,
			Message:     message,
			Timestamp:   config.Now().UTC(),
		})
	}
	if err := record("identity", "completed", "first-node identity verified"); err != nil {
		return nil, err
	}
	if strings.TrimSpace(installation.AllocationPublicKey) == "" {
		return nil, fmt.Errorf("first-node identity predates allocation signing; explicit re-enrollment is required")
	}
	inventoryPath := config.hostPath(nodeidentity.DefaultInventoryPath)
	inventory := nodeidentity.ClusterInventory{
		APIVersion: nodeidentity.InventoryAPIVersion, Kind: nodeidentity.InventoryKind, ClusterID: installation.ClusterID,
		Nodes: []nodeidentity.InventoryNode{{NodeID: installation.NodeID, AllocationPublicKey: installation.AllocationPublicKey}},
	}
	if existing, readErr := nodeidentity.ReadInventoryOptional(inventoryPath); readErr != nil {
		return nil, fmt.Errorf("read trusted cluster inventory: %w", readErr)
	} else if existing == nil {
		if err := nodeidentity.CreateInventory(inventoryPath, inventory); err != nil {
			return nil, fmt.Errorf("create trusted cluster inventory: %w", err)
		}
	} else if existing.ClusterID != installation.ClusterID {
		return nil, fmt.Errorf("trusted cluster inventory belongs to another cluster")
	}
	membershipPath := DefaultMembershipPath(stateDir)
	meshPublicKey, err := EnsureMeshPublicKey(config.hostPath(DefaultPlatformMeshKeyDir))
	if err != nil {
		return nil, fmt.Errorf("initialize first-node mesh identity: %w", err)
	}
	membership, err := NewMembershipStore(membershipPath, inventoryPath)
	if err != nil {
		return nil, err
	}
	if _, err := membership.InitializeFirstNode(*installation, DefaultPlatformMeshCIDR, meshPublicKey, installation.NodeName); err != nil {
		return nil, fmt.Errorf("initialize controller membership: %w", err)
	}
	if err := nodeidentity.WriteLocalBinding(config.hostPath(nodeidentity.DefaultLocalBindingPath), nodeidentity.LocalBinding{
		APIVersion: nodeidentity.InventoryAPIVersion, Kind: nodeidentity.LocalBindingKind,
		ClusterID: installation.ClusterID, NodeID: installation.NodeID, NodeName: installation.NodeName, WorkerUID: accountIDs.WorkerUID,
	}); err != nil {
		return nil, fmt.Errorf("publish local node binding: %w", err)
	}

	state := BootstrapState{
		APIVersion:        APIVersion,
		Kind:              BootstrapKind,
		ClusterID:         installation.ClusterID,
		NodeID:            installation.NodeID,
		NodeName:          installation.NodeName,
		ControllerMode:    "single-writer",
		EnrollmentRoles:   append([]string(nil), installation.EnrollmentRoles...),
		IdentityPath:      config.IdentityPath,
		InventoryPath:     nodeidentity.DefaultInventoryPath,
		MembershipPath:    DefaultMembershipPath(config.StateDir),
		StateDir:          config.StateDir,
		AuditDir:          config.AuditDir,
		SocketPath:        config.SocketPath,
		WorkerSocketPath:  config.WorkerSocketPath,
		DockerDataRoot:    config.DockerDataRoot,
		SocketGroup:       config.SocketGroup,
		ServiceBinaryPath: config.ServiceBinaryPath,
		WorkerUser:        config.WorkerUser,
		WorkerGroup:       config.WorkerGroup,
		WorkerUID:         accountIDs.WorkerUID,
		WorkerGID:         accountIDs.WorkerGID,
		SocketGroupGID:    accountIDs.SocketGroupGID,
		InitializedAt:     now,
	}
	if err := state.Validate(); err != nil {
		return nil, err
	}
	document := ConfigDocument{State: state, Policy: config.Policy}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	configPath = filepath.Join(configDir, "platform.json")
	if existing, readErr := readConfigDocument(configPath); readErr == nil {
		if existing.State.ClusterID != state.ClusterID || existing.State.NodeID != state.NodeID {
			return nil, fmt.Errorf("existing platform config belongs to another cluster member")
		}
		state.InitializedAt = existing.State.InitializedAt
		document.State.InitializedAt = existing.State.InitializedAt
		data, _ = json.MarshalIndent(document, "", "  ")
		data = append(data, '\n')
		resumed = true
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return nil, fmt.Errorf("read existing platform config: %w", readErr)
	}
	if err := fileutil.WriteFileAtomic(configPath, data, 0640); err != nil {
		return nil, fmt.Errorf("write platform config: %w", err)
	}
	if err := os.Chown(configPath, 0, gid); err != nil && config.RequireRoot {
		return nil, fmt.Errorf("own platform config: %w", err)
	}
	if err := record("configuration", "completed", "protected platform configuration published"); err != nil {
		return nil, err
	}
	if err := b.host.StageBinary(ctx, config.BinaryPath, config.ServiceBinaryPath); err != nil {
		_ = record("binary", "failed", err.Error())
		return nil, fmt.Errorf("stage protected Tako service binary: %w", err)
	}
	if err := record("binary", "completed", "protected service binary staged"); err != nil {
		return nil, err
	}

	takodUnit, err := RenderTakodUnit(config.ServiceBinaryPath, config.SocketPath, config.StateDir, config.NodeName, config.IdentityPath, config.DockerDataRoot, config.WorkerGroup, config.Policy)
	if err != nil {
		return nil, err
	}
	workerUnit, err := RenderWorkerUnit(config)
	if err != nil {
		return nil, err
	}
	if err := b.host.InstallUnit(ctx, TakodUnitName, takodUnit); err != nil {
		_ = record("services", "failed", err.Error())
		return nil, fmt.Errorf("install takod unit: %w", err)
	}
	if err := b.host.InstallUnit(ctx, WorkerUnitName, workerUnit); err != nil {
		_ = record("services", "failed", err.Error())
		return nil, fmt.Errorf("install platform worker unit: %w", err)
	}
	if err := b.host.ReloadEnableRestart(ctx, TakodUnitName, WorkerUnitName); err != nil {
		_ = record("services", "failed", err.Error())
		return nil, fmt.Errorf("activate platform services: %w", err)
	}
	if err := b.host.WaitReady(ctx, ReadinessCheck{
		SocketPath: config.SocketPath, ClusterID: installation.ClusterID, NodeID: installation.NodeID,
		WorkerSocketPath: config.WorkerSocketPath, WorkerUID: accountIDs.WorkerUID, StateDir: config.StateDir, Since: now,
	}); err != nil {
		_ = record("services", "failed", err.Error())
		return nil, fmt.Errorf("verify platform service readiness: %w", err)
	}
	if err := record("services", "completed", "takod and durable deployment worker activated"); err != nil {
		return nil, err
	}
	audit, _ := NewJournal(auditPath)
	if err := audit.Append(OperationRecord{
		OperationID: operationID, Operation: "platform.init", Phase: "audit", Status: "completed",
		NodeID: installation.NodeID, Message: "first controller initialized", Timestamp: config.Now().UTC(),
		Metadata: map[string]any{"controllerMode": "single-writer", "roles": installation.EnrollmentRoles},
	}); err != nil {
		return nil, fmt.Errorf("append platform audit: %w", err)
	}
	return &BootstrapResult{State: state, Policy: config.Policy, Resumed: resumed, JournalPath: config.StateDir + "/" + DefaultJournalName, AuditPath: config.AuditDir + "/" + DefaultAuditLogName}, nil
}

func ensureFirstNodeIdentity(path string, config BootstrapConfig) (*nodeidentity.Installation, bool, error) {
	existing, err := nodeidentity.ReadOptional(path)
	if err != nil {
		return nil, false, fmt.Errorf("read installation identity: %w", err)
	}
	if existing != nil {
		if existing.NodeName != config.NodeName || !slices.Equal(existing.EnrollmentRoles, firstNodeRoles) {
			return nil, false, fmt.Errorf("existing installation identity is not the requested first-node enrollment")
		}
		if strings.TrimSpace(config.ClusterID) != "" && !strings.EqualFold(existing.ClusterID, config.ClusterID) {
			return nil, false, fmt.Errorf("existing cluster ID does not match requested cluster ID")
		}
		if strings.TrimSpace(config.NodeID) != "" && !strings.EqualFold(existing.NodeID, config.NodeID) {
			return nil, false, fmt.Errorf("existing node ID does not match requested node ID")
		}
		return existing, true, nil
	}
	installation, err := nodeidentity.New(config.ClusterID, config.NodeID, config.NodeName, firstNodeRoles, config.Now())
	if err != nil {
		return nil, false, err
	}
	if err := nodeidentity.Create(path, *installation); err != nil {
		return nil, false, fmt.Errorf("create installation identity: %w", err)
	}
	return installation, false, nil
}

func readConfigDocument(path string) (*ConfigDocument, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var document ConfigDocument
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("platform config must contain one JSON value")
		}
		return nil, err
	}
	if strings.TrimSpace(document.State.WorkerSocketPath) == "" {
		document.State.WorkerSocketPath = DefaultWorkerSocket
	}
	if strings.TrimSpace(document.State.MembershipPath) == "" {
		document.State.MembershipPath = DefaultMembershipPath(document.State.StateDir)
	}
	if err := document.State.Validate(); err != nil {
		return nil, err
	}
	if err := document.Policy.Validate(); err != nil {
		return nil, err
	}
	return &document, nil
}
