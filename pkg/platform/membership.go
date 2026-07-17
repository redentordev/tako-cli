package platform

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/fileutil"
	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/recovery"
)

const (
	MembershipKind           = "PlatformMembership"
	DefaultMembershipName    = "membership.json"
	DefaultMembershipDirName = "control"
	DefaultJoinTokenTTL      = 15 * time.Minute
	MaximumJoinTokenTTL      = 24 * time.Hour
	DefaultPlatformMeshCIDR  = "10.210.0.0/16"
	joinTokenPrefix          = "tako_join_v1"
	membershipMaxBytes       = 8 << 20
)

var (
	errMembershipNoChange     = errors.New("membership unchanged")
	membershipNodeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	membershipMeshHostPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,252}$`)
)

type MembershipState struct {
	APIVersion           string                          `json:"apiVersion"`
	Kind                 string                          `json:"kind"`
	ClusterID            string                          `json:"clusterId"`
	Generation           uint64                          `json:"generation"`
	AllocationGeneration uint64                          `json:"allocationGeneration,omitempty"`
	ControllerNodeID     string                          `json:"controllerNodeId"`
	MeshCIDR             string                          `json:"meshCidr,omitempty"`
	Nodes                []MembershipNode                `json:"nodes"`
	Tombstones           []nodeidentity.NodeTombstone    `json:"tombstones,omitempty"`
	JoinTokens           []JoinTokenRecord               `json:"joinTokens,omitempty"`
	ActiveAllocations    []nodeidentity.ActiveAllocation `json:"activeAllocations,omitempty"`
	AllocationHighWater  []AllocationHighWater           `json:"allocationHighWater,omitempty"`
	RemovalOperations    []NodeRemovalOperation          `json:"removalOperations,omitempty"`
	UpdatedAt            time.Time                       `json:"updatedAt"`
}

// AllocationHighWater is a durable replay tombstone. It survives omission
// from ActiveAllocations so a withdrawn worker proof cannot later be replayed.
type AllocationHighWater struct {
	NodeID      string `json:"nodeId"`
	Key         string `json:"key"`
	Generation  uint64 `json:"generation"`
	Fingerprint string `json:"fingerprint"`
	Revoked     bool   `json:"revoked,omitempty"`
}

type MembershipNode struct {
	NodeID                string    `json:"nodeId"`
	NodeName              string    `json:"nodeName"`
	Lifecycle             string    `json:"lifecycle"`
	Roles                 []string  `json:"roles"`
	Schedulable           bool      `json:"schedulable"`
	MeshIP                string    `json:"meshIp,omitempty"`
	MeshEndpoint          string    `json:"meshEndpoint"`
	MeshCredentialID      string    `json:"meshCredentialId"`
	MeshPublicKey         string    `json:"meshPublicKey"`
	MeshCredentialStatus  string    `json:"meshCredentialStatus"`
	SSHHost               string    `json:"sshHost,omitempty"`
	SSHPort               int       `json:"sshPort,omitempty"`
	SSHUser               string    `json:"sshUser,omitempty"`
	SSHHostKeyType        string    `json:"sshHostKeyType,omitempty"`
	SSHHostKey            string    `json:"sshHostKey,omitempty"`
	SSHHostKeyFingerprint string    `json:"sshHostKeyFingerprint,omitempty"`
	AllocationPublicKey   string    `json:"allocationPublicKey"`
	JoinedAt              time.Time `json:"joinedAt"`
	UpdatedAt             time.Time `json:"updatedAt"`
}

const (
	RemovalPhaseRevokePeers   = "revoke-peers"
	RemovalPhaseRemoveMember  = "remove-membership"
	RemovalPhaseCleanupTarget = "cleanup-target"
)

// NodeRemovalOperation retains the immutable target identity and durable
// progress needed to resume removal after any controller or network failure.
type NodeRemovalOperation struct {
	Node       MembershipNode `json:"node"`
	Phase      string         `json:"phase"`
	Generation uint64         `json:"generation"`
	CreatedAt  time.Time      `json:"createdAt"`
	UpdatedAt  time.Time      `json:"updatedAt"`
}

type JoinTokenRecord struct {
	ID                   string    `json:"id"`
	ExpectedNodeID       string    `json:"expectedNodeId"`
	TokenHash            string    `json:"tokenHash"`
	CreatedAt            time.Time `json:"createdAt"`
	ExpiresAt            time.Time `json:"expiresAt"`
	ConsumedAt           time.Time `json:"consumedAt,omitempty"`
	ReservationHash      string    `json:"reservationHash,omitempty"`
	ReservedAt           time.Time `json:"reservedAt,omitempty"`
	ReservationExpiresAt time.Time `json:"reservationExpiresAt,omitempty"`
}

type JoinToken struct {
	Token          string    `json:"token"`
	ClusterID      string    `json:"clusterId"`
	ExpectedNodeID string    `json:"expectedNodeId"`
	ExpiresAt      time.Time `json:"expiresAt"`
}

type JoinTokenReservation struct {
	Reservation    string    `json:"reservation"`
	ExpectedNodeID string    `json:"expectedNodeId"`
	ExpiresAt      time.Time `json:"expiresAt"`
}

type EnrollWorkerRequest struct {
	Reservation                     string
	NodeID                          string
	NodeName                        string
	MeshIP                          string
	MeshEndpoint                    string
	SSHHost                         string
	SSHPort                         int
	SSHUser                         string
	SSHHostKeyType                  string
	SSHHostKey                      string
	SSHHostKeyFingerprint           string
	AllocationPublicKey             string
	MeshPublicKey                   string
	ControllerMeshEndpoint          string
	ControllerSSHHost               string
	ControllerSSHPort               int
	ControllerSSHUser               string
	ControllerSSHHostKeyType        string
	ControllerSSHHostKey            string
	ControllerSSHHostKeyFingerprint string
}

type MembershipStore struct {
	path          string
	inventoryPath string
	now           func() time.Time
	mu            sync.Mutex
}

func NewMembershipStore(path, inventoryPath string) (*MembershipStore, error) {
	if !filepath.IsAbs(path) || !filepath.IsAbs(inventoryPath) {
		return nil, fmt.Errorf("membership and inventory paths must be absolute")
	}
	return &MembershipStore{path: path, inventoryPath: inventoryPath, now: time.Now}, nil
}

func DefaultMembershipPath(stateDir string) string {
	if strings.TrimSpace(stateDir) == "" {
		stateDir = DefaultStateDir
	}
	return filepath.Join(filepath.Dir(filepath.Clean(stateDir)), DefaultMembershipDirName, DefaultMembershipName)
}

// OpenControllerMembership opens the protected local controller store only
// when the caller is root on the exact immutable controller node.
func OpenControllerMembership(stateDir, identityPath, inventoryPath string) (*MembershipStore, *MembershipState, error) {
	if !runningAsRoot() {
		return nil, nil, fmt.Errorf("platform membership commands must run as root on the controller node")
	}
	if strings.TrimSpace(identityPath) == "" {
		identityPath = nodeidentity.DefaultPath
	}
	if strings.TrimSpace(inventoryPath) == "" {
		inventoryPath = nodeidentity.DefaultInventoryPath
	}
	installation, err := nodeidentity.Read(identityPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read local controller identity: %w", err)
	}
	store, err := NewMembershipStore(DefaultMembershipPath(stateDir), inventoryPath)
	if err != nil {
		return nil, nil, err
	}
	state, err := store.Read()
	if err != nil {
		return nil, nil, err
	}
	if state.ClusterID != installation.ClusterID || state.ControllerNodeID != installation.NodeID {
		return nil, nil, fmt.Errorf("local installation is not the authoritative membership controller")
	}
	return store, state, nil
}

// ValidateControllerRecoverySnapshot proves that the protected membership and
// published inventory are the exact authority owned by this controller key.
// The caller must hold the node-wide exclusive recovery snapshot lock.
func ValidateControllerRecoverySnapshot(membershipPath, inventoryPath string, installation *nodeidentity.Installation) error {
	if installation == nil || !installation.HasEnrollmentRole(nodeidentity.RoleControlPlane) {
		return fmt.Errorf("platform recovery backup requires the enrolled controller identity")
	}
	if err := installation.Validate(); err != nil {
		return err
	}
	state, err := readMembershipState(membershipPath)
	if err != nil {
		return fmt.Errorf("read authoritative controller membership: %w", err)
	}
	if err := state.Validate(time.Now().UTC()); err != nil {
		return err
	}
	if state.ClusterID != installation.ClusterID || state.ControllerNodeID != installation.NodeID {
		return fmt.Errorf("membership is owned by a different controller identity")
	}
	controller := state.node(state.ControllerNodeID)
	if controller == nil || !containsRole(controller.Roles, nodeidentity.RoleControlPlane) || controller.AllocationPublicKey != installation.AllocationPublicKey {
		return fmt.Errorf("membership controller key or role does not match the local installation")
	}
	inventory, err := nodeidentity.ReadInventory(inventoryPath)
	if err != nil {
		return fmt.Errorf("read published controller inventory: %w", err)
	}
	expected := state.Inventory()
	if !reflect.DeepEqual(expected, *inventory) {
		return fmt.Errorf("published inventory does not exactly match authoritative membership")
	}
	snapshot := nodeidentity.SignedInventorySnapshot{Kind: nodeidentity.SignedInventoryKind, Inventory: *inventory, IssuedAt: time.Now().UTC()}
	if err := nodeidentity.SignInventorySnapshot(&snapshot, installation); err != nil {
		return fmt.Errorf("sign controller inventory proof: %w", err)
	}
	if err := nodeidentity.VerifyInventorySnapshot(snapshot, controller.AllocationPublicKey, time.Now().UTC()); err != nil {
		return fmt.Errorf("verify controller inventory proof: %w", err)
	}
	return nil
}

func RunningAsRoot() bool { return runningAsRoot() }

func (s *MembershipStore) InitializeFirstNode(installation nodeidentity.Installation, meshCIDR, meshPublicKey, meshEndpoint string) (*MembershipState, error) {
	if err := installation.Validate(); err != nil {
		return nil, err
	}
	if !installation.HasEnrollmentRole(nodeidentity.RoleControlPlane) {
		return nil, fmt.Errorf("first membership node must be a controller")
	}
	return s.mutate(true, func(state *MembershipState) error {
		if state.ClusterID != "" {
			if state.ClusterID != installation.ClusterID || state.ControllerNodeID != installation.NodeID {
				return fmt.Errorf("existing membership belongs to another controller")
			}
			existing := state.node(installation.NodeID)
			if existing == nil || existing.MeshPublicKey != strings.TrimSpace(meshPublicKey) {
				return fmt.Errorf("existing controller membership is bound to another mesh identity")
			}
			return errMembershipNoChange
		}
		now := s.now().UTC()
		meshIP, err := firstPlatformMeshIP(meshCIDR)
		if err != nil {
			return err
		}
		credentialID, err := nodeidentity.MeshCredentialID(meshPublicKey)
		if err != nil {
			return err
		}
		roles := append([]string(nil), installation.EnrollmentRoles...)
		sort.Strings(roles)
		*state = MembershipState{
			APIVersion: APIVersion, Kind: MembershipKind, ClusterID: installation.ClusterID,
			Generation: 1, ControllerNodeID: installation.NodeID, MeshCIDR: strings.TrimSpace(meshCIDR), UpdatedAt: now,
			Nodes: []MembershipNode{{
				NodeID: installation.NodeID, NodeName: installation.NodeName,
				Lifecycle: nodeidentity.NodeLifecycleSchedulable, Roles: roles, Schedulable: true,
				MeshIP: meshIP, MeshEndpoint: strings.TrimSpace(meshEndpoint), MeshCredentialID: credentialID, MeshPublicKey: meshPublicKey, MeshCredentialStatus: nodeidentity.MeshCredentialActive,
				AllocationPublicKey: installation.AllocationPublicKey, JoinedAt: now, UpdatedAt: now,
			}},
		}
		return nil
	})
}

func firstPlatformMeshIP(cidr string) (string, error) {
	if strings.TrimSpace(cidr) == "" {
		return "", nil
	}
	prefix, err := netip.ParsePrefix(strings.TrimSpace(cidr))
	if err != nil || !prefix.Addr().Is4() {
		return "", fmt.Errorf("platform mesh CIDR must be valid IPv4")
	}
	address := prefix.Masked().Addr().Next()
	if !address.IsValid() || !prefix.Contains(address) {
		return "", fmt.Errorf("platform mesh CIDR has no usable node address")
	}
	return address.String(), nil
}

func (s *MembershipStore) Read() (*MembershipState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	releaseSnapshot, err := s.acquireSnapshotMutation()
	if err != nil {
		return nil, err
	}
	defer releaseSnapshot()
	lock, unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer func() { unlock(); _ = lock.Close() }()
	state, err := readMembershipState(s.path)
	if err != nil {
		return nil, err
	}
	if err := s.reconcileInventory(state); err != nil {
		return nil, err
	}
	return state, nil
}

func (s *MembershipStore) CreateJoinToken(expectedNodeID string, ttl time.Duration) (*JoinToken, error) {
	if err := nodeidentity.ValidateNodeID(expectedNodeID); err != nil {
		return nil, err
	}
	if ttl == 0 {
		ttl = DefaultJoinTokenTTL
	}
	if ttl < time.Minute || ttl > MaximumJoinTokenTTL {
		return nil, fmt.Errorf("join token TTL must be between 1m and %s", MaximumJoinTokenTTL)
	}
	var issued *JoinToken
	_, err := s.mutate(false, func(state *MembershipState) error {
		if state.node(expectedNodeID) != nil || state.tombstoned(expectedNodeID) {
			return fmt.Errorf("node ID is already active or permanently tombstoned")
		}
		id, err := randomIdentifier(12)
		if err != nil {
			return err
		}
		secret, err := randomIdentifier(32)
		if err != nil {
			return err
		}
		token := joinTokenPrefix + "." + id + "." + secret
		now := s.now().UTC()
		expires := now.Add(ttl)
		state.JoinTokens = append(state.JoinTokens, JoinTokenRecord{
			ID: id, ExpectedNodeID: strings.ToLower(expectedNodeID), TokenHash: hashJoinToken(token), CreatedAt: now, ExpiresAt: expires,
		})
		issued = &JoinToken{Token: token, ClusterID: state.ClusterID, ExpectedNodeID: strings.ToLower(expectedNodeID), ExpiresAt: expires}
		return nil
	})
	return issued, err
}

// ReserveJoinToken authenticates the bound join request before any create-once
// candidate identity is mutated. Re-presenting the original token replaces an
// abandoned reservation without weakening single-use consumption.
func (s *MembershipStore) ReserveJoinToken(token, nodeID string) (*JoinTokenReservation, error) {
	nodeID = strings.ToLower(strings.TrimSpace(nodeID))
	if err := nodeidentity.ValidateNodeID(nodeID); err != nil {
		return nil, err
	}
	var issued *JoinTokenReservation
	_, err := s.mutate(false, func(state *MembershipState) error {
		now := s.now().UTC()
		record, err := validateJoinToken(state, token, nodeID, now)
		if err != nil {
			return err
		}
		secret, err := randomIdentifier(32)
		if err != nil {
			return err
		}
		reservation := "tako_reserve_v1." + record.ID + "." + secret
		expires := now.Add(5 * time.Minute)
		if expires.After(record.ExpiresAt) {
			expires = record.ExpiresAt
		}
		record.ReservationHash = hashJoinToken(reservation)
		record.ReservedAt = now
		record.ReservationExpiresAt = expires
		issued = &JoinTokenReservation{Reservation: reservation, ExpectedNodeID: nodeID, ExpiresAt: expires}
		return nil
	})
	return issued, err
}

func (s *MembershipStore) EnrollWorker(request EnrollWorkerRequest) (*MembershipNode, error) {
	var enrolled *MembershipNode
	_, err := s.mutate(false, func(state *MembershipState) error {
		request.NodeID = strings.ToLower(strings.TrimSpace(request.NodeID))
		request.NodeName = strings.TrimSpace(request.NodeName)
		request.SSHHost = strings.TrimSpace(request.SSHHost)
		request.MeshIP = strings.TrimSpace(request.MeshIP)
		if err := nodeidentity.ValidateNodeID(request.NodeID); err != nil {
			return err
		}
		if state.node(request.NodeID) != nil || state.tombstoned(request.NodeID) {
			return fmt.Errorf("node ID is already active or permanently tombstoned")
		}
		if err := validateEnrollmentRequest(request, state.MeshCIDR); err != nil {
			return err
		}
		now := s.now().UTC()
		controller := state.node(state.ControllerNodeID)
		if controller == nil {
			return fmt.Errorf("membership controller is not active")
		}
		controller.MeshEndpoint = strings.TrimSpace(request.ControllerMeshEndpoint)
		controller.SSHHost = strings.TrimSpace(request.ControllerSSHHost)
		controller.SSHPort = request.ControllerSSHPort
		controller.SSHUser = strings.TrimSpace(request.ControllerSSHUser)
		controller.SSHHostKeyType = request.ControllerSSHHostKeyType
		controller.SSHHostKey = request.ControllerSSHHostKey
		controller.SSHHostKeyFingerprint = request.ControllerSSHHostKeyFingerprint
		controller.UpdatedAt = now
		tokenRecord, err := consumeJoinReservation(state, request.Reservation, request.NodeID, now)
		if err != nil {
			return err
		}
		credentialID, err := nodeidentity.MeshCredentialID(request.MeshPublicKey)
		if err != nil {
			return err
		}
		node := MembershipNode{
			NodeID: request.NodeID, NodeName: request.NodeName, Lifecycle: nodeidentity.NodeLifecycleJoining,
			Roles: []string{nodeidentity.RoleWorker}, Schedulable: false,
			MeshIP: request.MeshIP, MeshEndpoint: request.MeshEndpoint, MeshCredentialID: credentialID, MeshPublicKey: request.MeshPublicKey, MeshCredentialStatus: nodeidentity.MeshCredentialActive,
			SSHHost: request.SSHHost, SSHPort: request.SSHPort, SSHUser: request.SSHUser, SSHHostKeyType: request.SSHHostKeyType,
			SSHHostKey: request.SSHHostKey, SSHHostKeyFingerprint: request.SSHHostKeyFingerprint,
			AllocationPublicKey: request.AllocationPublicKey, JoinedAt: now, UpdatedAt: now,
		}
		tokenRecord.ConsumedAt = now
		tokenRecord.ReservationHash = ""
		tokenRecord.ReservedAt = time.Time{}
		tokenRecord.ReservationExpiresAt = time.Time{}
		state.Nodes = append(state.Nodes, node)
		enrolled = &node
		return nil
	})
	return enrolled, err
}

func (s *MembershipStore) MarkReady(nodeID string) (*MembershipNode, error) {
	return s.transition(nodeID, []string{nodeidentity.NodeLifecycleJoining}, nodeidentity.NodeLifecycleReady, false)
}

func (s *MembershipStore) SetNodeMeshEndpoint(nodeID, endpoint string) (*MembershipNode, error) {
	endpoint = strings.TrimSpace(endpoint)
	if !validMeshEndpoint(endpoint) {
		return nil, fmt.Errorf("invalid platform mesh endpoint")
	}
	var changed *MembershipNode
	_, err := s.mutate(false, func(state *MembershipState) error {
		node := state.node(nodeID)
		if node == nil {
			return fmt.Errorf("node %s is not active", nodeID)
		}
		if node.MeshEndpoint == endpoint {
			copy := *node
			copy.Roles = append([]string(nil), node.Roles...)
			changed = &copy
			return errMembershipNoChange
		}
		node.MeshEndpoint, node.UpdatedAt = endpoint, s.now().UTC()
		copy := *node
		copy.Roles = append([]string(nil), node.Roles...)
		changed = &copy
		return nil
	})
	return changed, err
}

// PrepareRemoval durably records enough immutable target state to resume a
// revoke-first removal even after membership deletion or a controller crash.
func (s *MembershipStore) PrepareRemoval(nodeID string) (*NodeRemovalOperation, error) {
	var prepared *NodeRemovalOperation
	_, err := s.mutate(false, func(state *MembershipState) error {
		if operation := state.removalOperation(nodeID); operation != nil {
			copy := *operation
			copy.Node.Roles = append([]string(nil), operation.Node.Roles...)
			prepared = &copy
			return errMembershipNoChange
		}
		node := state.node(nodeID)
		if node == nil {
			return fmt.Errorf("node %s is not active", nodeID)
		}
		if node.Lifecycle != nodeidentity.NodeLifecycleDraining {
			return fmt.Errorf("node %s must be draining before removal", nodeID)
		}
		if containsRole(node.Roles, nodeidentity.RoleControlPlane) && state.controllerCount() <= 1 {
			return fmt.Errorf("refusing to remove the final controller")
		}
		now := s.now().UTC()
		operation := NodeRemovalOperation{
			Node: *node, Phase: RemovalPhaseRevokePeers, Generation: state.Generation + 1,
			CreatedAt: now, UpdatedAt: now,
		}
		operation.Node.Roles = append([]string(nil), node.Roles...)
		state.RemovalOperations = append(state.RemovalOperations, operation)
		prepared = &operation
		return nil
	})
	return prepared, err
}

func (s *MembershipStore) MarkRemovalPeersRevoked(nodeID string) (*NodeRemovalOperation, error) {
	var changed *NodeRemovalOperation
	_, err := s.mutate(false, func(state *MembershipState) error {
		operation := state.removalOperation(nodeID)
		if operation == nil {
			return fmt.Errorf("node %s has no pending removal", nodeID)
		}
		if operation.Phase == RemovalPhaseRemoveMember || operation.Phase == RemovalPhaseCleanupTarget {
			copy := *operation
			copy.Node.Roles = append([]string(nil), operation.Node.Roles...)
			changed = &copy
			return errMembershipNoChange
		}
		if operation.Phase != RemovalPhaseRevokePeers {
			return fmt.Errorf("node %s removal has invalid phase %q", nodeID, operation.Phase)
		}
		operation.Phase = RemovalPhaseRemoveMember
		operation.UpdatedAt = s.now().UTC()
		copy := *operation
		copy.Node.Roles = append([]string(nil), operation.Node.Roles...)
		changed = &copy
		return nil
	})
	return changed, err
}

func (s *MembershipStore) MarkSchedulable(nodeID string) (*MembershipNode, error) {
	return s.transition(nodeID, []string{nodeidentity.NodeLifecycleReady, nodeidentity.NodeLifecycleCordoned}, nodeidentity.NodeLifecycleSchedulable, true)
}

func (s *MembershipStore) Cordon(nodeID string) (*MembershipNode, error) {
	return s.transition(nodeID, []string{nodeidentity.NodeLifecycleReady, nodeidentity.NodeLifecycleSchedulable}, nodeidentity.NodeLifecycleCordoned, false)
}

func (s *MembershipStore) BeginDrain(nodeID string) (*MembershipNode, error) {
	var changed *MembershipNode
	_, err := s.mutate(false, func(state *MembershipState) error {
		node := state.node(nodeID)
		if node == nil {
			return fmt.Errorf("node %s is not active", nodeID)
		}
		if node.Lifecycle == nodeidentity.NodeLifecycleDraining && !node.Schedulable {
			copy := *node
			copy.Roles = append([]string(nil), node.Roles...)
			changed = &copy
			return errMembershipNoChange
		}
		if node.Lifecycle != nodeidentity.NodeLifecycleCordoned {
			return fmt.Errorf("node %s cannot transition from %s to %s", nodeID, node.Lifecycle, nodeidentity.NodeLifecycleDraining)
		}
		if containsRole(node.Roles, nodeidentity.RoleControlPlane) && state.controllerCount() <= 1 {
			return fmt.Errorf("refusing to drain the final controller")
		}
		now := s.now().UTC()
		node.Lifecycle, node.Schedulable, node.UpdatedAt = nodeidentity.NodeLifecycleDraining, false, now
		revokeNodeAllocations(state, node.NodeID, now)
		copy := *node
		copy.Roles = append([]string(nil), node.Roles...)
		changed = &copy
		return nil
	})
	return changed, err
}

func (s *MembershipStore) Remove(nodeID string) (*nodeidentity.NodeTombstone, error) {
	var removed *nodeidentity.NodeTombstone
	_, err := s.mutate(false, func(state *MembershipState) error {
		if existing := state.tombstone(nodeID); existing != nil {
			copy := *existing
			removed = &copy
			return errMembershipNoChange
		}
		index := state.nodeIndex(nodeID)
		if index < 0 {
			return fmt.Errorf("node %s is not active", nodeID)
		}
		node := state.Nodes[index]
		if node.Lifecycle != nodeidentity.NodeLifecycleDraining {
			return fmt.Errorf("node %s must be draining before removal", nodeID)
		}
		if containsRole(node.Roles, nodeidentity.RoleControlPlane) && state.controllerCount() <= 1 {
			return fmt.Errorf("refusing to remove the final controller")
		}
		operation := state.removalOperation(nodeID)
		if operation == nil || operation.Phase != RemovalPhaseRemoveMember {
			return fmt.Errorf("node %s mesh peers must be durably revoked before membership removal", nodeID)
		}
		now := s.now().UTC()
		fingerprint := sha256.Sum256([]byte(node.AllocationPublicKey))
		tombstone := nodeidentity.NodeTombstone{
			NodeID: node.NodeID, NodeName: node.NodeName, RemovedAt: now,
			RemovedGeneration: state.Generation + 1, RevokedMeshCredentialID: node.MeshCredentialID,
			RevokedMeshPublicKey:     node.MeshPublicKey,
			AllocationKeyFingerprint: hex.EncodeToString(fingerprint[:]),
		}
		state.Nodes = append(state.Nodes[:index], state.Nodes[index+1:]...)
		state.Tombstones = append(state.Tombstones, tombstone)
		revokeNodeAllocations(state, node.NodeID, now)
		operation.Phase = RemovalPhaseCleanupTarget
		operation.Generation = state.Generation + 1
		operation.UpdatedAt = now
		removed = &tombstone
		return nil
	})
	return removed, err
}

func (s *MembershipStore) CompleteRemoval(nodeID string) error {
	_, err := s.mutate(false, func(state *MembershipState) error {
		index := state.removalOperationIndex(nodeID)
		if index < 0 {
			if state.tombstoned(nodeID) {
				return errMembershipNoChange
			}
			return fmt.Errorf("node %s has no pending removal", nodeID)
		}
		if state.RemovalOperations[index].Phase != RemovalPhaseCleanupTarget {
			return fmt.Errorf("node %s removal is not awaiting target cleanup", nodeID)
		}
		state.RemovalOperations = append(state.RemovalOperations[:index], state.RemovalOperations[index+1:]...)
		return nil
	})
	return err
}

func (s *MembershipStore) transition(nodeID string, from []string, to string, schedulable bool) (*MembershipNode, error) {
	var changed *MembershipNode
	_, err := s.mutate(false, func(state *MembershipState) error {
		node := state.node(nodeID)
		if node == nil {
			return fmt.Errorf("node %s is not active", nodeID)
		}
		if node.Lifecycle == to && node.Schedulable == schedulable {
			copy := *node
			copy.Roles = append([]string(nil), node.Roles...)
			changed = &copy
			return errMembershipNoChange
		}
		allowed := false
		for _, value := range from {
			allowed = allowed || node.Lifecycle == value
		}
		if !allowed {
			return fmt.Errorf("node %s cannot transition from %s to %s", nodeID, node.Lifecycle, to)
		}
		node.Lifecycle, node.Schedulable, node.UpdatedAt = to, schedulable, s.now().UTC()
		if !schedulable {
			revokeNodeAllocations(state, node.NodeID, s.now().UTC())
		}
		copy := *node
		copy.Roles = append([]string(nil), node.Roles...)
		changed = &copy
		return nil
	})
	return changed, err
}

func (s *MembershipStore) mutate(allowMissing bool, mutation func(*MembershipState) error) (*MembershipState, error) {
	if s == nil || mutation == nil {
		return nil, fmt.Errorf("membership store and mutation are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	releaseSnapshot, err := s.acquireSnapshotMutation()
	if err != nil {
		return nil, err
	}
	defer releaseSnapshot()
	lock, unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer func() { unlock(); _ = lock.Close() }()
	state, err := readMembershipState(s.path)
	if errors.Is(err, os.ErrNotExist) && allowMissing {
		state = &MembershipState{}
	} else if err != nil {
		return nil, err
	}
	if state.ClusterID != "" {
		if err := state.Validate(s.now().UTC()); err != nil {
			return nil, err
		}
	}
	before := *state
	before.Nodes = append([]MembershipNode(nil), state.Nodes...)
	before.Tombstones = append([]nodeidentity.NodeTombstone(nil), state.Tombstones...)
	before.JoinTokens = append([]JoinTokenRecord(nil), state.JoinTokens...)
	before.ActiveAllocations = append([]nodeidentity.ActiveAllocation(nil), state.ActiveAllocations...)
	before.AllocationHighWater = append([]AllocationHighWater(nil), state.AllocationHighWater...)
	before.RemovalOperations = cloneRemovalOperations(state.RemovalOperations)
	initializing := state.ClusterID == ""
	if err := mutation(state); errors.Is(err, errMembershipNoChange) {
		if err := s.reconcileInventory(state); err != nil {
			return nil, err
		}
		return state, nil
	} else if err != nil {
		return nil, err
	}
	if reflect.DeepEqual(before, *state) {
		return state, nil
	}
	if state.Generation == 0 {
		return nil, fmt.Errorf("membership mutation did not initialize generation")
	}
	if !initializing {
		allocationsChanged := !reflect.DeepEqual(before.ActiveAllocations, state.ActiveAllocations) || !reflect.DeepEqual(before.AllocationHighWater, state.AllocationHighWater)
		beforeWithoutAllocations := before
		afterWithoutAllocations := *state
		beforeWithoutAllocations.ActiveAllocations = nil
		afterWithoutAllocations.ActiveAllocations = nil
		beforeWithoutAllocations.AllocationHighWater = nil
		afterWithoutAllocations.AllocationHighWater = nil
		beforeWithoutAllocations.AllocationGeneration = 0
		afterWithoutAllocations.AllocationGeneration = 0
		if !reflect.DeepEqual(beforeWithoutAllocations, afterWithoutAllocations) {
			state.Generation++
		}
		if allocationsChanged {
			state.AllocationGeneration++
			if state.AllocationGeneration == 0 {
				return nil, fmt.Errorf("allocation authority generation exhausted")
			}
		}
		state.UpdatedAt = s.now().UTC()
	}
	state.pruneTokens(s.now().UTC())
	if err := state.Validate(s.now().UTC()); err != nil {
		return nil, err
	}
	if err := writeMembershipState(s.path, state); err != nil {
		return nil, err
	}
	if err := s.reconcileInventory(state); err != nil {
		return nil, fmt.Errorf("membership committed at generation %d but inventory publication failed: %w", state.Generation, err)
	}
	copy := *state
	return &copy, nil
}

// AuthorizeAllocations replaces the complete controller-authorized allocation
// set for one project/environment. Omission is intentional revocation, so a
// stale node proof cannot remain routable after the desired scope changes.
func (s *MembershipStore) AuthorizeAllocations(project, environment string, allocations []nodeidentity.ActiveAllocation) (*MembershipState, error) {
	project = strings.TrimSpace(project)
	environment = strings.TrimSpace(environment)
	if !safeAllocationScopeName(project) || !safeAllocationScopeName(environment) {
		return nil, fmt.Errorf("allocation project/environment is invalid")
	}
	return s.mutate(false, func(state *MembershipState) error {
		return applyAllocationAuthorization(state, project, environment, allocations, s.now().UTC())
	})
}

// PreviewAllocations produces the exact next membership state without
// committing it. The controller uses this for publish-before-commit routing
// transitions: every edge must acknowledge the signed candidate first.
func (s *MembershipStore) PreviewAllocations(project, environment string, allocations []nodeidentity.ActiveAllocation) (*MembershipState, error) {
	project, environment = strings.TrimSpace(project), strings.TrimSpace(environment)
	if !safeAllocationScopeName(project) || !safeAllocationScopeName(environment) {
		return nil, fmt.Errorf("allocation project/environment is invalid")
	}
	current, err := s.Read()
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(current)
	if err != nil {
		return nil, err
	}
	var candidate MembershipState
	if err := json.Unmarshal(data, &candidate); err != nil {
		return nil, err
	}
	beforeAllocations := append([]nodeidentity.ActiveAllocation(nil), candidate.ActiveAllocations...)
	beforeHighWater := append([]AllocationHighWater(nil), candidate.AllocationHighWater...)
	now := s.now().UTC()
	if err := applyAllocationAuthorization(&candidate, project, environment, allocations, now); err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(beforeAllocations, candidate.ActiveAllocations) || !reflect.DeepEqual(beforeHighWater, candidate.AllocationHighWater) {
		candidate.AllocationGeneration++
		if candidate.AllocationGeneration == 0 {
			return nil, fmt.Errorf("allocation authority generation exhausted")
		}
		candidate.UpdatedAt = now
	}
	if err := candidate.Validate(now); err != nil {
		return nil, err
	}
	return &candidate, nil
}

// CommitPreparedAllocations atomically persists the exact state previously
// signed and acknowledged by edge nodes. The base generations provide the
// compare-and-swap boundary against unrelated controller changes.
func (s *MembershipStore) CommitPreparedAllocations(candidate *MembershipState, baseGeneration, baseAllocationGeneration uint64) (*MembershipState, error) {
	if candidate == nil {
		return nil, fmt.Errorf("prepared allocation state is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	releaseSnapshot, err := s.acquireSnapshotMutation()
	if err != nil {
		return nil, err
	}
	defer releaseSnapshot()
	file, unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer file.Close()
	defer unlock()
	current, err := readMembershipState(s.path)
	if err != nil {
		return nil, err
	}
	if current.Generation != baseGeneration || current.AllocationGeneration != baseAllocationGeneration {
		return nil, fmt.Errorf("prepared allocation base generation changed")
	}
	if err := candidate.Validate(s.now().UTC()); err != nil {
		return nil, err
	}
	if err := writeMembershipState(s.path, candidate); err != nil {
		return nil, err
	}
	if err := s.reconcileInventory(candidate); err != nil {
		return nil, fmt.Errorf("prepared allocations committed but inventory publication failed: %w", err)
	}
	copy := *candidate
	return &copy, nil
}

func applyAllocationAuthorization(state *MembershipState, project, environment string, allocations []nodeidentity.ActiveAllocation, now time.Time) error {
	inventory := state.Inventory()
	next := make([]nodeidentity.ActiveAllocation, 0, len(state.ActiveAllocations)+len(allocations))
	for _, existing := range state.ActiveAllocations {
		if existing.Project != project || existing.Environment != environment {
			next = append(next, existing)
		}
	}
	seen := make(map[string]struct{}, len(allocations))
	incoming := make(map[string]struct{}, len(allocations))
	for _, candidate := range allocations {
		incoming[candidate.NodeID+"\x00"+candidate.Key] = struct{}{}
	}
	for index := range state.AllocationHighWater {
		high := &state.AllocationHighWater[index]
		for _, existing := range state.ActiveAllocations {
			if existing.NodeID != high.NodeID || existing.Key != high.Key || existing.Project != project || existing.Environment != environment {
				continue
			}
			if _, retained := incoming[high.NodeID+"\x00"+high.Key]; !retained {
				high.Revoked = true
			}
		}
	}
	for _, candidate := range allocations {
		if candidate.Project != project || candidate.Environment != environment || candidate.ClusterID != state.ClusterID || candidate.Kind != "mesh-upstream" {
			return fmt.Errorf("allocation %s is outside the controller authorization scope", candidate.Key)
		}
		if !safeAllocationScopeName(candidate.Service) || (candidate.Revision != "" && !safeAllocationScopeName(candidate.Revision)) {
			return fmt.Errorf("allocation %s has invalid service/revision identity", candidate.Key)
		}
		expectedKey := fmt.Sprintf("%s/%s/%s/%s/%d", candidate.Kind, project, environment, candidate.Service, candidate.Slot)
		if candidate.Revision != "" {
			expectedKey = fmt.Sprintf("%s/%s/%s/%s/%s/%d", candidate.Kind, project, environment, candidate.Service, candidate.Revision, candidate.Slot)
		}
		if candidate.Key != expectedKey {
			return fmt.Errorf("allocation %s has an invalid durable key", candidate.Key)
		}
		node, ok := inventory.Node(candidate.NodeID)
		if !ok || !node.Schedulable || node.Lifecycle != nodeidentity.NodeLifecycleSchedulable || node.MeshCredentialStatus != nodeidentity.MeshCredentialActive || candidate.HostIP != node.MeshIP {
			return fmt.Errorf("allocation %s belongs to a non-schedulable destination", candidate.Key)
		}
		if err := nodeidentity.VerifyActiveAllocationOrigin(candidate, node.AllocationPublicKey); err != nil {
			return fmt.Errorf("verify worker allocation %s: %w", candidate.Key, err)
		}
		compound := candidate.NodeID + "\x00" + candidate.Key
		if _, duplicate := seen[compound]; duplicate {
			return fmt.Errorf("allocation %s is duplicated", candidate.Key)
		}
		seen[compound] = struct{}{}
		fingerprint, err := allocationEvidenceFingerprint(candidate)
		if err != nil {
			return err
		}
		index := allocationHighWaterIndex(state.AllocationHighWater, candidate.NodeID, candidate.Key)
		if index >= 0 {
			high := state.AllocationHighWater[index]
			if candidate.Generation < high.Generation {
				return fmt.Errorf("allocation %s generation %d is behind durable high-water %d", candidate.Key, candidate.Generation, high.Generation)
			}
			if candidate.Generation == high.Generation && (high.Revoked || fingerprint != high.Fingerprint) {
				return fmt.Errorf("allocation %s generation %d was revoked or conflicts with its durable proof", candidate.Key, candidate.Generation)
			}
			if candidate.Generation > high.Generation {
				state.AllocationHighWater[index] = AllocationHighWater{NodeID: candidate.NodeID, Key: candidate.Key, Generation: candidate.Generation, Fingerprint: fingerprint}
			}
		} else {
			state.AllocationHighWater = append(state.AllocationHighWater, AllocationHighWater{NodeID: candidate.NodeID, Key: candidate.Key, Generation: candidate.Generation, Fingerprint: fingerprint})
		}
		candidate.AuthorizedAt = now
		next = append(next, candidate)
	}
	state.ActiveAllocations = next
	sort.Slice(state.AllocationHighWater, func(i, j int) bool {
		if state.AllocationHighWater[i].NodeID != state.AllocationHighWater[j].NodeID {
			return state.AllocationHighWater[i].NodeID < state.AllocationHighWater[j].NodeID
		}
		return state.AllocationHighWater[i].Key < state.AllocationHighWater[j].Key
	})
	return nil
}

func allocationEvidenceFingerprint(allocation nodeidentity.ActiveAllocation) (string, error) {
	allocation.AuthorizedAt = time.Time{}
	data, err := json.Marshal(allocation)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func allocationHighWaterIndex(values []AllocationHighWater, nodeID, key string) int {
	for index := range values {
		if values[index].NodeID == nodeID && values[index].Key == key {
			return index
		}
	}
	return -1
}

func safeAllocationScopeName(value string) bool {
	if value == "" || len(value) > 128 || value == "." || value == ".." {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' {
			continue
		}
		return false
	}
	return true
}

func (s *MembershipStore) lock() (*os.File, func(), error) {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return nil, nil, err
	}
	dirInfo, err := os.Lstat(filepath.Dir(s.path))
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 || dirInfo.Mode().Perm()&0022 != 0 {
		return nil, nil, fmt.Errorf("membership directory must be a protected real directory")
	}
	if err := validateMembershipDirectoryOwner(dirInfo); err != nil {
		return nil, nil, err
	}
	lock, err := openJournalAppend(s.path + ".lock")
	if err != nil {
		return nil, nil, fmt.Errorf("open membership lock: %w", err)
	}
	unlock, err := lockJournalFile(lock)
	if err != nil {
		_ = lock.Close()
		return nil, nil, fmt.Errorf("lock membership store: %w", err)
	}
	return lock, unlock, nil
}

// acquireSnapshotMutation joins the node-wide recovery barrier when this store
// uses the production <data-dir>/control layout. Tests and explicitly custom
// stores remain independent because their parent directory is not "control".
func (s *MembershipStore) acquireSnapshotMutation() (func(), error) {
	controlDir := filepath.Dir(s.path)
	if filepath.Base(controlDir) != DefaultMembershipDirName {
		return func() {}, nil
	}
	return recovery.AcquireMutationLock(filepath.Dir(controlDir))
}

func (s *MembershipStore) reconcileInventory(state *MembershipState) error {
	inventory := state.Inventory()
	existing, err := nodeidentity.ReadInventoryOptional(s.inventoryPath)
	if err != nil {
		return fmt.Errorf("read trusted inventory: %w", err)
	}
	if existing != nil && (existing.Generation > inventory.Generation || existing.AllocationGeneration > inventory.AllocationGeneration) {
		return fmt.Errorf("trusted inventory generation %d is ahead of controller membership %d", existing.Generation, inventory.Generation)
	}
	if existing == nil {
		return nodeidentity.CreateInventory(s.inventoryPath, inventory)
	}
	if existing.Generation == inventory.Generation && existing.AllocationGeneration == inventory.AllocationGeneration {
		return nil
	}
	return nodeidentity.ReplaceInventory(s.inventoryPath, inventory)
}

func (s MembershipState) Inventory() nodeidentity.ClusterInventory {
	inventory := nodeidentity.ClusterInventory{
		APIVersion: nodeidentity.InventoryAPIVersion, Kind: nodeidentity.InventoryKind,
		ClusterID: s.ClusterID, Generation: s.Generation, ControllerNodeID: s.ControllerNodeID,
		AllocationGeneration: s.AllocationGeneration,
		MeshCIDR:             s.MeshCIDR, UpdatedAt: s.UpdatedAt,
		Tombstones:        append([]nodeidentity.NodeTombstone(nil), s.Tombstones...),
		ActiveAllocations: append([]nodeidentity.ActiveAllocation(nil), s.ActiveAllocations...),
	}
	for _, node := range s.Nodes {
		inventory.Nodes = append(inventory.Nodes, nodeidentity.InventoryNode{
			NodeID: node.NodeID, NodeName: node.NodeName, Lifecycle: node.Lifecycle,
			Roles: append([]string(nil), node.Roles...), Schedulable: node.Schedulable,
			MeshIP: node.MeshIP, MeshEndpoint: node.MeshEndpoint, MeshCredentialID: node.MeshCredentialID, MeshCredentialStatus: node.MeshCredentialStatus,
			MeshPublicKey: node.MeshPublicKey,
			SSHHost:       node.SSHHost, SSHPort: node.SSHPort, SSHUser: node.SSHUser, SSHHostKeyType: node.SSHHostKeyType,
			SSHHostKey: node.SSHHostKey, SSHHostKeyFingerprint: node.SSHHostKeyFingerprint,
			AllocationPublicKey: node.AllocationPublicKey, JoinedAt: node.JoinedAt, UpdatedAt: node.UpdatedAt,
		})
	}
	return inventory
}

func (s MembershipState) Validate(now time.Time) error {
	if s.APIVersion != APIVersion || s.Kind != MembershipKind {
		return fmt.Errorf("platform membership apiVersion/kind is invalid")
	}
	if err := nodeidentity.ValidateClusterID(s.ClusterID); err != nil {
		return err
	}
	if s.Generation == 0 || s.UpdatedAt.IsZero() || s.UpdatedAt.After(now.Add(time.Minute)) {
		return fmt.Errorf("platform membership generation/timestamp is invalid")
	}
	if err := s.Inventory().Validate(); err != nil {
		return err
	}
	seenAllocationHighWater := map[string]struct{}{}
	for _, high := range s.AllocationHighWater {
		if err := nodeidentity.ValidateNodeID(high.NodeID); err != nil || high.Key == "" || high.Generation == 0 || len(high.Fingerprint) != sha256.Size*2 {
			return fmt.Errorf("allocation high-water record is invalid")
		}
		compound := high.NodeID + "\x00" + high.Key
		if _, duplicate := seenAllocationHighWater[compound]; duplicate {
			return fmt.Errorf("allocation high-water record is duplicated")
		}
		seenAllocationHighWater[compound] = struct{}{}
	}
	seenTokens := map[string]struct{}{}
	for _, token := range s.JoinTokens {
		if token.ID == "" || token.TokenHash == "" || token.CreatedAt.IsZero() || token.ExpiresAt.Before(token.CreatedAt) {
			return fmt.Errorf("join token record is invalid")
		}
		if err := nodeidentity.ValidateNodeID(token.ExpectedNodeID); err != nil {
			return fmt.Errorf("join token expected node ID is invalid")
		}
		if _, ok := seenTokens[token.ID]; ok {
			return fmt.Errorf("join token ID is duplicated")
		}
		if (token.ReservationHash == "") != token.ReservedAt.IsZero() || (token.ReservationHash == "") != token.ReservationExpiresAt.IsZero() || (!token.ReservationExpiresAt.IsZero() && token.ReservationExpiresAt.Before(token.ReservedAt)) {
			return fmt.Errorf("join token reservation record is invalid")
		}
		seenTokens[token.ID] = struct{}{}
	}
	seenRemovals := map[string]struct{}{}
	for _, operation := range s.RemovalOperations {
		if operation.Node.NodeID == "" || operation.CreatedAt.IsZero() || operation.UpdatedAt.Before(operation.CreatedAt) || operation.Generation == 0 {
			return fmt.Errorf("node removal operation is invalid")
		}
		switch operation.Phase {
		case RemovalPhaseRevokePeers, RemovalPhaseRemoveMember, RemovalPhaseCleanupTarget:
		default:
			return fmt.Errorf("node removal operation %s has invalid phase", operation.Node.NodeID)
		}
		if _, ok := seenRemovals[operation.Node.NodeID]; ok {
			return fmt.Errorf("node removal operation is duplicated")
		}
		seenRemovals[operation.Node.NodeID] = struct{}{}
		if operation.Phase == RemovalPhaseCleanupTarget {
			if !s.tombstoned(operation.Node.NodeID) {
				return fmt.Errorf("cleanup-phase removal must have a tombstone")
			}
		} else if s.node(operation.Node.NodeID) == nil {
			return fmt.Errorf("pre-removal operation must reference an active node")
		}
	}
	return nil
}

func (s MembershipState) ActiveNode(nodeID string) (*MembershipNode, bool) {
	for index := range s.Nodes {
		if strings.EqualFold(s.Nodes[index].NodeID, strings.TrimSpace(nodeID)) {
			copy := s.Nodes[index]
			copy.Roles = append([]string(nil), copy.Roles...)
			return &copy, true
		}
	}
	return nil, false
}

func (s MembershipState) IsTombstoned(nodeID string) bool {
	return s.tombstoned(nodeID)
}

func (s *MembershipState) node(nodeID string) *MembershipNode {
	index := s.nodeIndex(nodeID)
	if index < 0 {
		return nil
	}
	return &s.Nodes[index]
}

func (s *MembershipState) nodeIndex(nodeID string) int {
	for index := range s.Nodes {
		if strings.EqualFold(s.Nodes[index].NodeID, strings.TrimSpace(nodeID)) {
			return index
		}
	}
	return -1
}

func (s *MembershipState) tombstoned(nodeID string) bool {
	for _, value := range s.Tombstones {
		if strings.EqualFold(value.NodeID, strings.TrimSpace(nodeID)) {
			return true
		}
	}
	return false
}

func (s *MembershipState) controllerCount() int {
	count := 0
	for _, node := range s.Nodes {
		if containsRole(node.Roles, nodeidentity.RoleControlPlane) {
			count++
		}
	}
	return count
}

func (s *MembershipState) pruneTokens(now time.Time) {
	kept := s.JoinTokens[:0]
	for _, token := range s.JoinTokens {
		if token.ConsumedAt.IsZero() && now.Before(token.ExpiresAt) || !token.ConsumedAt.IsZero() && now.Sub(token.ConsumedAt) < 24*time.Hour {
			kept = append(kept, token)
		}
	}
	s.JoinTokens = kept
}

func validateEnrollmentRequest(request EnrollWorkerRequest, meshCIDR string) error {
	if !membershipNodeNamePattern.MatchString(request.NodeName) {
		return fmt.Errorf("invalid worker node name")
	}
	if err := nodeidentity.ValidateAllocationPublicKey(request.AllocationPublicKey); err != nil {
		return fmt.Errorf("invalid allocation public key")
	}
	if err := nodeidentity.ValidateMeshPublicKey(request.MeshPublicKey); err != nil {
		return fmt.Errorf("invalid worker mesh public key: %w", err)
	}
	if !validMeshEndpoint(request.MeshEndpoint) {
		return fmt.Errorf("invalid worker mesh endpoint")
	}
	if !validMeshEndpoint(request.ControllerMeshEndpoint) {
		return fmt.Errorf("invalid controller mesh endpoint")
	}
	if request.SSHHost == "" || request.SSHPort < 1 || request.SSHPort > 65535 || request.SSHUser == "" || request.SSHHostKeyType == "" || request.SSHHostKey == "" || request.SSHHostKeyFingerprint == "" {
		return fmt.Errorf("a complete pinned SSH host identity is required")
	}
	if _, err := base64.StdEncoding.DecodeString(request.SSHHostKey); err != nil {
		return fmt.Errorf("pinned SSH host key is invalid")
	}
	if request.ControllerSSHHost == "" || request.ControllerSSHPort < 1 || request.ControllerSSHPort > 65535 || request.ControllerSSHUser == "" || request.ControllerSSHHostKeyType == "" || request.ControllerSSHHostKey == "" || request.ControllerSSHHostKeyFingerprint == "" {
		return fmt.Errorf("a complete pinned controller SSH host identity is required")
	}
	if _, err := base64.StdEncoding.DecodeString(request.ControllerSSHHostKey); err != nil {
		return fmt.Errorf("pinned controller SSH host key is invalid")
	}
	if meshCIDR != "" {
		_, network, err := net.ParseCIDR(meshCIDR)
		if err != nil || net.ParseIP(request.MeshIP) == nil || !network.Contains(net.ParseIP(request.MeshIP)) {
			return fmt.Errorf("worker mesh IP is outside the cluster mesh CIDR")
		}
	}
	return nil
}

func validMeshEndpoint(value string) bool {
	value = strings.TrimSpace(value)
	return net.ParseIP(value) != nil || membershipMeshHostPattern.MatchString(value)
}

func validateJoinToken(state *MembershipState, token, nodeID string, now time.Time) (*JoinTokenRecord, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 || parts[0] != joinTokenPrefix {
		return nil, fmt.Errorf("join token is malformed")
	}
	hash := hashJoinToken(token)
	for index := range state.JoinTokens {
		record := &state.JoinTokens[index]
		if record.ID != parts[1] {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(record.TokenHash), []byte(hash)) != 1 {
			return nil, fmt.Errorf("join token is invalid")
		}
		if !record.ConsumedAt.IsZero() {
			return nil, fmt.Errorf("join token was already consumed")
		}
		if !now.Before(record.ExpiresAt) {
			return nil, fmt.Errorf("join token has expired")
		}
		if !strings.EqualFold(record.ExpectedNodeID, nodeID) {
			return nil, fmt.Errorf("join token is bound to another node ID")
		}
		return record, nil
	}
	return nil, fmt.Errorf("join token is unknown")
}

func consumeJoinReservation(state *MembershipState, reservation, nodeID string, now time.Time) (*JoinTokenRecord, error) {
	parts := strings.Split(strings.TrimSpace(reservation), ".")
	if len(parts) != 3 || parts[0] != "tako_reserve_v1" {
		return nil, fmt.Errorf("join reservation is malformed")
	}
	hash := hashJoinToken(reservation)
	for index := range state.JoinTokens {
		record := &state.JoinTokens[index]
		if record.ID != parts[1] {
			continue
		}
		if !record.ConsumedAt.IsZero() {
			return nil, fmt.Errorf("join token was already consumed")
		}
		if record.ReservationHash == "" || subtle.ConstantTimeCompare([]byte(record.ReservationHash), []byte(hash)) != 1 {
			return nil, fmt.Errorf("join reservation is invalid")
		}
		if !now.Before(record.ReservationExpiresAt) {
			return nil, fmt.Errorf("join reservation has expired")
		}
		if !strings.EqualFold(record.ExpectedNodeID, nodeID) {
			return nil, fmt.Errorf("join reservation is bound to another node ID")
		}
		return record, nil
	}
	return nil, fmt.Errorf("join reservation is unknown")
}

func hashJoinToken(token string) string {
	hash := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(hash[:])
}

func randomIdentifier(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func readMembershipState(path string) (*MembershipState, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() || pathInfo.Mode().Perm()&0077 != 0 || pathInfo.Size() > membershipMaxBytes {
		return nil, fmt.Errorf("membership state must be a private regular file")
	}
	if err := validateMembershipFileOwner(pathInfo); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !os.SameFile(pathInfo, info) || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("membership state must be a private regular file")
	}
	if err := validateMembershipFileOwner(info); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(io.LimitReader(file, membershipMaxBytes+1))
	decoder.DisallowUnknownFields()
	var state MembershipState
	if err := decoder.Decode(&state); err != nil {
		return nil, fmt.Errorf("decode membership state: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("decode membership state: multiple JSON values are not allowed")
	}
	return &state, nil
}

func writeMembershipState(path string, state *MembershipState) error {
	sort.Slice(state.Nodes, func(i, j int) bool { return state.Nodes[i].NodeID < state.Nodes[j].NodeID })
	sort.Slice(state.Tombstones, func(i, j int) bool { return state.Tombstones[i].NodeID < state.Tombstones[j].NodeID })
	sort.Slice(state.JoinTokens, func(i, j int) bool { return state.JoinTokens[i].ID < state.JoinTokens[j].ID })
	sort.Slice(state.RemovalOperations, func(i, j int) bool {
		return state.RemovalOperations[i].Node.NodeID < state.RemovalOperations[j].Node.NodeID
	})
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > membershipMaxBytes {
		return fmt.Errorf("membership state exceeds %d bytes", membershipMaxBytes)
	}
	if err := fileutil.WriteFileAtomic(path, data, 0600); err != nil {
		return fmt.Errorf("write membership state: %w", err)
	}
	return nil
}

func containsRole(roles []string, wanted string) bool {
	for _, role := range roles {
		if role == wanted {
			return true
		}
	}
	return false
}

func removeNodeAllocations(values []nodeidentity.ActiveAllocation, nodeID string) []nodeidentity.ActiveAllocation {
	kept := values[:0]
	for _, value := range values {
		if value.NodeID != nodeID {
			kept = append(kept, value)
		}
	}
	return kept
}

func cloneRemovalOperations(values []NodeRemovalOperation) []NodeRemovalOperation {
	result := append([]NodeRemovalOperation(nil), values...)
	for index := range result {
		result[index].Node.Roles = append([]string(nil), result[index].Node.Roles...)
	}
	return result
}

func (s *MembershipState) removalOperationIndex(nodeID string) int {
	for index := range s.RemovalOperations {
		if strings.EqualFold(s.RemovalOperations[index].Node.NodeID, strings.TrimSpace(nodeID)) {
			return index
		}
	}
	return -1
}

func (s *MembershipState) removalOperation(nodeID string) *NodeRemovalOperation {
	if index := s.removalOperationIndex(nodeID); index >= 0 {
		return &s.RemovalOperations[index]
	}
	return nil
}

func (s MembershipState) RemovalOperation(nodeID string) (*NodeRemovalOperation, bool) {
	for _, operation := range s.RemovalOperations {
		if strings.EqualFold(operation.Node.NodeID, strings.TrimSpace(nodeID)) {
			copy := operation
			copy.Node.Roles = append([]string(nil), operation.Node.Roles...)
			return &copy, true
		}
	}
	return nil, false
}

func (s *MembershipState) tombstone(nodeID string) *nodeidentity.NodeTombstone {
	for index := range s.Tombstones {
		if strings.EqualFold(s.Tombstones[index].NodeID, strings.TrimSpace(nodeID)) {
			return &s.Tombstones[index]
		}
	}
	return nil
}

func revokeNodeAllocations(state *MembershipState, nodeID string, _ time.Time) {
	state.ActiveAllocations = removeNodeAllocations(state.ActiveAllocations, nodeID)
}
