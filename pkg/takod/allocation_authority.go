package takod

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/platform"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

type AllocationAuthorizationRequest struct {
	Project      string                          `json:"project"`
	Environment  string                          `json:"environment"`
	Allocations  []nodeidentity.ActiveAllocation `json:"allocations"`
	Phase        string                          `json:"phase,omitempty"`
	ProposalID   string                          `json:"proposalId,omitempty"`
	TargetNodeID string                          `json:"targetNodeId,omitempty"`
}

type AllocationAuthorizationResponse struct {
	ProposalID            string                               `json:"proposalId,omitempty"`
	Snapshot              nodeidentity.SignedInventorySnapshot `json:"snapshot,omitempty"`
	RecoveryTargetNodeIDs []string                             `json:"recoveryTargetNodeIds,omitempty"`
	Recovered             bool                                 `json:"recovered,omitempty"`
}

type pendingAllocationAuthorization struct {
	ProposalID                  string                                `json:"proposalId"`
	RequestFingerprint          string                                `json:"requestFingerprint"`
	BaseGeneration              uint64                                `json:"baseGeneration"`
	BaseAllocationGeneration    uint64                                `json:"baseAllocationGeneration"`
	Request                     AllocationAuthorizationRequest        `json:"request"`
	Candidate                   platform.MembershipState              `json:"candidate"`
	Snapshot                    nodeidentity.SignedInventorySnapshot  `json:"snapshot"`
	CreatedAt                   time.Time                             `json:"createdAt"`
	OperationID                 string                                `json:"operationId"`
	FenceToken                  uint64                                `json:"fenceToken"`
	PublicationTargetNodeIDs    []string                              `json:"publicationTargetNodeIds,omitempty"`
	AcknowledgedNodeIDs         []string                              `json:"acknowledgedNodeIds,omitempty"`
	RecoverySnapshot            *nodeidentity.SignedInventorySnapshot `json:"recoverySnapshot,omitempty"`
	RecoveryAcknowledgedNodeIDs []string                              `json:"recoveryAcknowledgedNodeIds,omitempty"`
}

func (s *Server) supportsAuthoritativeRemoteMeshRoutes() bool {
	if s == nil || s.installation == nil {
		return false
	}
	inventory, err := nodeidentity.ReadInventory(s.inventoryFile)
	if err != nil || inventory.ClusterID != s.installation.ClusterID || inventory.ControllerNodeID == "" || inventory.Generation == 0 {
		return false
	}
	controller, ok := inventory.Node(inventory.ControllerNodeID)
	return ok && hasInventoryRole(controller.Roles, nodeidentity.RoleControlPlane) && controller.MeshCredentialStatus == nodeidentity.MeshCredentialActive
}

func (s *Server) supportsMembershipController() bool {
	if s == nil || s.installation == nil {
		return false
	}
	inventory, err := nodeidentity.ReadInventory(s.inventoryFile)
	if err != nil || inventory.ClusterID != s.installation.ClusterID || inventory.ControllerNodeID != s.installation.NodeID || inventory.Generation == 0 {
		return false
	}
	node, ok := inventory.Node(s.installation.NodeID)
	return ok && hasInventoryRole(node.Roles, nodeidentity.RoleControlPlane) && node.MeshCredentialStatus == nodeidentity.MeshCredentialActive
}

func (s *Server) handleAllocationAuthorization(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	if !s.supportsMembershipController() {
		http.Error(w, "allocation authorization requires the membership controller", http.StatusForbidden)
		return
	}
	var request AllocationAuthorizationRequest
	if err := decodeJSONRequest(w, r, &request); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(request.Phase) == "" || request.Phase == "prepare" {
		fence, ok := operationFenceFromContext(r.Context())
		if !ok {
			http.Error(w, "allocation authorization requires active controller operation context", http.StatusConflict)
			return
		}
		for _, allocation := range request.Allocations {
			if allocation.OperationID != fence.OperationID || allocation.FenceToken != fence.Token {
				http.Error(w, "worker allocation proof is not bound to this controller operation", http.StatusConflict)
				return
			}
		}
	}
	store, err := platform.NewMembershipStore(s.membershipFile, s.inventoryFile)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	phase := strings.TrimSpace(request.Phase)
	if phase == "" {
		phase = "prepare"
	}
	s.proxyAuthorityMu.Lock()
	defer s.proxyAuthorityMu.Unlock()
	var response *AllocationAuthorizationResponse
	switch phase {
	case "prepare":
		fence, _ := operationFenceFromContext(r.Context())
		response, err = s.prepareAllocationAuthorization(store, request, fence)
	case "track":
		response, err = s.trackAllocationPublicationTarget(r.Context(), request)
	case "ack":
		response, err = s.acknowledgeAllocationAuthorization(r.Context(), request)
	case "commit":
		response, err = s.commitAllocationAuthorization(r.Context(), store, request)
	case "recover":
		response, err = s.recoverAllocationAuthorization(r.Context(), store)
	case "recovery-ack":
		response, err = s.acknowledgeAllocationRecovery(r.Context(), request)
	case "finalize-recovery":
		response, err = s.finalizeAllocationRecovery(r.Context(), request)
	default:
		err = fmt.Errorf("unsupported allocation authorization phase")
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func (s *Server) prepareAllocationAuthorization(store *platform.MembershipStore, request AllocationAuthorizationRequest, fence nodeidentity.OperationFence) (*AllocationAuthorizationResponse, error) {
	current, err := store.Read()
	if err != nil {
		return nil, err
	}
	fingerprint, err := allocationAuthorizationFingerprint(request)
	if err != nil {
		return nil, err
	}
	path := s.pendingAllocationAuthorizationPath(request.Project, request.Environment)
	nextAllocationGeneration := current.AllocationGeneration + 1
	if nextAllocationGeneration == 0 {
		return nil, fmt.Errorf("allocation authority generation exhausted")
	}
	if pending, readErr := readPendingAllocationAuthorization(path); readErr != nil {
		return nil, readErr
	} else if pending != nil {
		if pending.BaseGeneration == current.Generation && pending.BaseAllocationGeneration == current.AllocationGeneration {
			if pending.RequestFingerprint == fingerprint && pending.OperationID == fence.OperationID && pending.FenceToken == fence.Token {
				return &AllocationAuthorizationResponse{ProposalID: pending.ProposalID, Snapshot: pending.Snapshot}, nil
			}
		}
		return nil, fmt.Errorf("pending allocation publication must be recovered before preparing another proposal")
	}
	candidate, err := store.PreviewAllocations(request.Project, request.Environment, request.Allocations)
	if err != nil {
		return nil, err
	}
	if candidate.AllocationGeneration == current.AllocationGeneration {
		snapshot := nodeidentity.SignedInventorySnapshot{Kind: nodeidentity.SignedInventoryKind, Inventory: current.Inventory(), IssuedAt: time.Now().UTC()}
		if err := nodeidentity.SignInventorySnapshot(&snapshot, s.installation); err != nil {
			return nil, err
		}
		return &AllocationAuthorizationResponse{Snapshot: snapshot}, nil
	}
	if candidate.AllocationGeneration < nextAllocationGeneration {
		candidate.AllocationGeneration = nextAllocationGeneration
		candidate.UpdatedAt = time.Now().UTC()
	}
	snapshot := nodeidentity.SignedInventorySnapshot{Kind: nodeidentity.SignedInventoryKind, Inventory: candidate.Inventory(), IssuedAt: time.Now().UTC()}
	if err := nodeidentity.SignInventorySnapshot(&snapshot, s.installation); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(encoded)
	proposalID := "allocation-" + hex.EncodeToString(digest[:16])
	request.Phase, request.ProposalID = "", ""
	pending := pendingAllocationAuthorization{ProposalID: proposalID, RequestFingerprint: fingerprint, BaseGeneration: current.Generation, BaseAllocationGeneration: current.AllocationGeneration, Request: request, Candidate: *candidate, Snapshot: snapshot, CreatedAt: time.Now().UTC(), OperationID: fence.OperationID, FenceToken: fence.Token}
	if err := writeJSONAtomic(path, pending, 0600); err != nil {
		return nil, err
	}
	return &AllocationAuthorizationResponse{ProposalID: proposalID, Snapshot: snapshot}, nil
}

func (s *Server) trackAllocationPublicationTarget(ctx context.Context, request AllocationAuthorizationRequest) (*AllocationAuthorizationResponse, error) {
	pending, path, fence, err := s.pendingAllocationForActiveOperation(ctx, request)
	if err != nil {
		return nil, err
	}
	nodeID := strings.ToLower(strings.TrimSpace(request.TargetNodeID))
	if nodeID == "" || !containsNodeID(fence.TargetNodeIDs, nodeID) {
		return nil, fmt.Errorf("allocation publication target is outside the active operation")
	}
	if !containsNodeID(pending.PublicationTargetNodeIDs, nodeID) {
		pending.PublicationTargetNodeIDs = append(pending.PublicationTargetNodeIDs, nodeID)
		sort.Strings(pending.PublicationTargetNodeIDs)
		if err := writeJSONAtomic(path, pending, 0600); err != nil {
			return nil, err
		}
	}
	return &AllocationAuthorizationResponse{ProposalID: pending.ProposalID, Snapshot: pending.Snapshot}, nil
}

func (s *Server) acknowledgeAllocationAuthorization(ctx context.Context, request AllocationAuthorizationRequest) (*AllocationAuthorizationResponse, error) {
	pending, path, _, err := s.pendingAllocationForActiveOperation(ctx, request)
	if err != nil {
		return nil, err
	}
	nodeID := strings.ToLower(strings.TrimSpace(request.TargetNodeID))
	if nodeID == "" || !containsNodeID(pending.PublicationTargetNodeIDs, nodeID) {
		return nil, fmt.Errorf("allocation acknowledgement has no durable publication intent")
	}
	if !containsNodeID(pending.AcknowledgedNodeIDs, nodeID) {
		pending.AcknowledgedNodeIDs = append(pending.AcknowledgedNodeIDs, nodeID)
		sort.Strings(pending.AcknowledgedNodeIDs)
		if err := writeJSONAtomic(path, pending, 0600); err != nil {
			return nil, err
		}
	}
	return &AllocationAuthorizationResponse{ProposalID: pending.ProposalID, Snapshot: pending.Snapshot}, nil
}

func (s *Server) pendingAllocationForActiveOperation(ctx context.Context, request AllocationAuthorizationRequest) (*pendingAllocationAuthorization, string, nodeidentity.OperationFence, error) {
	path := s.pendingAllocationAuthorizationPath(request.Project, request.Environment)
	pending, err := readPendingAllocationAuthorization(path)
	if err != nil {
		return nil, path, nodeidentity.OperationFence{}, err
	}
	fence, ok := operationFenceFromContext(ctx)
	if pending == nil || request.ProposalID == "" || pending.ProposalID != request.ProposalID || !ok || pending.OperationID != fence.OperationID || pending.FenceToken != fence.Token {
		return nil, path, nodeidentity.OperationFence{}, fmt.Errorf("allocation proposal is not bound to the active operation")
	}
	return pending, path, fence, nil
}

// recoverAllocationAuthorization commits a strictly newer copy of the
// controller's last durable authority before any ordinary inventory is sent
// to edges that may have accepted an abandoned proposal. Controller-first
// commit makes every later retry monotonic even if this recovery client dies.
func (s *Server) recoverAllocationAuthorization(ctx context.Context, store *platform.MembershipStore) (*AllocationAuthorizationResponse, error) {
	path := s.pendingAllocationAuthorizationPath("", "")
	pending, err := readPendingAllocationAuthorization(path)
	if err != nil || pending == nil {
		return &AllocationAuthorizationResponse{}, err
	}
	fence, ok := operationFenceFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("allocation recovery requires active controller operation context")
	}
	if pending.OperationID == fence.OperationID {
		return nil, fmt.Errorf("active operation must commit its own allocation proposal")
	}
	if pending.RecoverySnapshot == nil {
		current, err := store.Read()
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(current)
		if err != nil {
			return nil, err
		}
		var candidate platform.MembershipState
		if err := json.Unmarshal(encoded, &candidate); err != nil {
			return nil, err
		}
		next := pending.Candidate.AllocationGeneration + 1
		if next <= current.AllocationGeneration || next == 0 {
			next = current.AllocationGeneration + 1
		}
		if next == 0 {
			return nil, fmt.Errorf("allocation authority generation exhausted")
		}
		candidate.AllocationGeneration = next
		candidate.UpdatedAt = time.Now().UTC()
		if _, err := store.CommitPreparedAllocations(&candidate, current.Generation, current.AllocationGeneration); err != nil {
			return nil, err
		}
		snapshot := nodeidentity.SignedInventorySnapshot{Kind: nodeidentity.SignedInventoryKind, Inventory: candidate.Inventory(), IssuedAt: time.Now().UTC()}
		if err := nodeidentity.SignInventorySnapshot(&snapshot, s.installation); err != nil {
			return nil, err
		}
		pending.RecoverySnapshot = &snapshot
		if err := writeJSONAtomic(path, pending, 0600); err != nil {
			return nil, fmt.Errorf("persist committed allocation recovery: %w", err)
		}
	}
	targets := append([]string(nil), pending.PublicationTargetNodeIDs...)
	return &AllocationAuthorizationResponse{ProposalID: pending.ProposalID, Snapshot: *pending.RecoverySnapshot, RecoveryTargetNodeIDs: targets, Recovered: true}, nil
}

func (s *Server) acknowledgeAllocationRecovery(ctx context.Context, request AllocationAuthorizationRequest) (*AllocationAuthorizationResponse, error) {
	path := s.pendingAllocationAuthorizationPath("", "")
	pending, err := readPendingAllocationAuthorization(path)
	if err != nil {
		return nil, err
	}
	if _, ok := operationFenceFromContext(ctx); !ok || pending == nil || pending.RecoverySnapshot == nil || request.ProposalID != pending.ProposalID {
		return nil, fmt.Errorf("allocation recovery acknowledgement is not bound to active recovery")
	}
	nodeID := strings.ToLower(strings.TrimSpace(request.TargetNodeID))
	if !containsNodeID(pending.PublicationTargetNodeIDs, nodeID) {
		return nil, fmt.Errorf("allocation recovery acknowledgement target was not part of abandoned publication")
	}
	if !containsNodeID(pending.RecoveryAcknowledgedNodeIDs, nodeID) {
		pending.RecoveryAcknowledgedNodeIDs = append(pending.RecoveryAcknowledgedNodeIDs, nodeID)
		sort.Strings(pending.RecoveryAcknowledgedNodeIDs)
		if err := writeJSONAtomic(path, pending, 0600); err != nil {
			return nil, err
		}
	}
	return &AllocationAuthorizationResponse{ProposalID: pending.ProposalID, Snapshot: *pending.RecoverySnapshot, Recovered: true}, nil
}

func (s *Server) finalizeAllocationRecovery(ctx context.Context, request AllocationAuthorizationRequest) (*AllocationAuthorizationResponse, error) {
	path := s.pendingAllocationAuthorizationPath("", "")
	pending, err := readPendingAllocationAuthorization(path)
	if err != nil {
		return nil, err
	}
	if pending == nil {
		return &AllocationAuthorizationResponse{}, nil
	}
	if _, ok := operationFenceFromContext(ctx); !ok || pending.RecoverySnapshot == nil || request.ProposalID != pending.ProposalID {
		return nil, fmt.Errorf("allocation recovery is not ready to finalize")
	}
	if !sameNodeIDSet(pending.PublicationTargetNodeIDs, pending.RecoveryAcknowledgedNodeIDs) {
		return nil, fmt.Errorf("allocation recovery has unacknowledged convergence targets")
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return &AllocationAuthorizationResponse{ProposalID: pending.ProposalID, Snapshot: *pending.RecoverySnapshot, Recovered: true}, nil
}

func containsNodeID(values []string, wanted string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), wanted) {
			return true
		}
	}
	return false
}

func sameNodeIDSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !strings.EqualFold(strings.TrimSpace(left[index]), strings.TrimSpace(right[index])) {
			return false
		}
	}
	return true
}

func (s *Server) commitAllocationAuthorization(ctx context.Context, store *platform.MembershipStore, request AllocationAuthorizationRequest) (*AllocationAuthorizationResponse, error) {
	path := s.pendingAllocationAuthorizationPath(request.Project, request.Environment)
	pending, err := readPendingAllocationAuthorization(path)
	if err != nil {
		return nil, err
	}
	if pending == nil || request.ProposalID == "" || pending.ProposalID != request.ProposalID {
		return nil, fmt.Errorf("allocation proposal is missing or does not match")
	}
	fence, ok := operationFenceFromContext(ctx)
	if !ok || pending.OperationID != fence.OperationID || pending.FenceToken != fence.Token {
		return nil, fmt.Errorf("allocation proposal is bound to a different controller operation")
	}
	current, err := store.Read()
	if err != nil {
		return nil, err
	}
	if current.Generation != pending.BaseGeneration || current.AllocationGeneration != pending.BaseAllocationGeneration {
		return nil, fmt.Errorf("allocation proposal base generation changed before commit")
	}
	if !sameNodeIDSet(pending.PublicationTargetNodeIDs, pending.AcknowledgedNodeIDs) {
		return nil, fmt.Errorf("allocation proposal has unacknowledged publication targets")
	}
	if err := stopProxyAndVerifyAbsent(ctx); err != nil {
		return nil, fmt.Errorf("stop controller proxy before allocation transition: %w", err)
	}
	state, err := store.CommitPreparedAllocations(&pending.Candidate, pending.BaseGeneration, pending.BaseAllocationGeneration)
	if err != nil {
		return nil, err
	}
	wantJSON, _ := json.Marshal(pending.Snapshot.Inventory)
	gotJSON, _ := json.Marshal(state.Inventory())
	if string(wantJSON) != string(gotJSON) {
		return nil, fmt.Errorf("committed allocation state differs from edge-acknowledged proposal")
	}
	if err := s.reconcileAllStoredProxyAuthority(); err != nil {
		return nil, fmt.Errorf("allocation committed fail-closed but controller route reconciliation failed: %w", err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove committed allocation proposal: %w", err)
	}
	return &AllocationAuthorizationResponse{ProposalID: pending.ProposalID, Snapshot: pending.Snapshot}, nil
}

func (s *Server) pendingAllocationAuthorizationPath(project, environment string) string {
	_ = project
	_ = environment
	// AllocationGeneration is cluster-global, so proposals must be as well.
	// This prevents two app-scoped operations from publishing different
	// candidates at the same generation to disjoint edge subsets.
	return filepath.Join(s.dataDir, "control", "allocation-proposals", "pending.json")
}

func readPendingAllocationAuthorization(path string) (*pendingAllocationAuthorization, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pending pendingAllocationAuthorization
	if err := json.Unmarshal(data, &pending); err != nil {
		return nil, fmt.Errorf("decode pending allocation authorization: %w", err)
	}
	return &pending, nil
}

func allocationAuthorizationFingerprint(request AllocationAuthorizationRequest) (string, error) {
	request.Phase, request.ProposalID = "", ""
	data, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

// reconcileAllStoredProxyAuthority withdraws any manifest invalidated by a
// newer controller inventory before it can remain active after revocation.
func (s *Server) reconcileAllStoredProxyAuthority() error {
	if s == nil || s.installation == nil {
		return nil
	}
	entries, err := os.ReadDir(proxyRoutesDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	type rejected struct {
		name string
		data []byte
	}
	var invalid []rejected
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(proxyRoutesDir, entry.Name()))
		if readErr != nil {
			return readErr
		}
		if policyErr := validateEnrolledProxyRouteManifest(string(data), s.installation.ClusterID, s.inventoryFile); policyErr != nil {
			invalid = append(invalid, rejected{name: entry.Name(), data: data})
		}
	}
	if len(invalid) == 0 {
		return nil
	}
	if err := stopProxyAndVerifyAbsent(context.Background()); err != nil {
		return fmt.Errorf("stop proxy before allocation revocation: %w", err)
	}
	quarantineDir := filepath.Join(s.dataDir, "proxy", "quarantine")
	if err := os.MkdirAll(quarantineDir, 0700); err != nil {
		return err
	}
	stamp := time.Now().UTC().UnixNano()
	for index, manifest := range invalid {
		path := filepath.Join(quarantineDir, fmt.Sprintf("%s.%d.%d.quarantined", manifest.name, stamp, index))
		if err := writeFileAtomic(path, manifest.data, 0600); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(proxyRoutesDir, manifest.name)); err != nil {
			return err
		}
	}
	if err := syncDirectory(proxyRoutesDir); err != nil {
		return err
	}
	if err := syncDirectory(quarantineDir); err != nil {
		return err
	}
	if err := renderAndWriteCaddyfile(context.Background()); err != nil {
		return fmt.Errorf("render proxy after allocation revocation: %w", err)
	}
	return nil
}

func (s *Server) storedProxyAuthorityInvalidForInventory(inventory *nodeidentity.ClusterInventory) (bool, error) {
	entries, err := os.ReadDir(proxyRoutesDir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(proxyRoutesDir, entry.Name()))
		if err != nil {
			return false, err
		}
		manifest, err := ParseProxyRouteManifest(string(data))
		if err != nil || validateEnrolledProxyRouteManifestWithInventory(manifest, s.installation.ClusterID, inventory) != nil {
			return true, nil
		}
	}
	return false, nil
}

func (s *Server) authorizeProxyFileCandidate(name, content string) error {
	if _, err := validateProxyFileName(name); err != nil {
		return err
	}
	manifest, err := ParseProxyRouteManifest(content)
	if err != nil {
		return err
	}
	ownedName := name == runtimeid.ProxyConfigFileName(manifest.Project, manifest.Environment)
	if len(manifest.Routes) == 1 && strings.HasSuffix(manifest.Routes[0].Service, "-maintenance") {
		service := strings.TrimSuffix(manifest.Routes[0].Service, "-maintenance")
		if service != "" {
			ownedName = ownedName || name == runtimeid.MaintenanceProxyConfigFileName(manifest.Project, manifest.Environment, service)
		}
	}
	if !ownedName {
		return fmt.Errorf("proxy manifest filename does not match its project/environment ownership")
	}
	return s.authorizeProxyManifestAllocations(content)
}

func (s *Server) reconcileStoredProxyScope(_, _, _ string) error {
	return s.reconcileAllStoredProxyAuthority()
}

func (s *Server) authorizeProxyManifestAllocations(content string) error {
	manifest, err := ParseProxyRouteManifest(content)
	if err != nil {
		return err
	}
	var inventory *nodeidentity.ClusterInventory
	for _, route := range manifest.Routes {
		destinations := append([]ProxyDestination(nil), route.Destinations...)
		if route.DynamicDomain != nil && route.DynamicDomain.Destination != nil {
			destinations = append(destinations, *route.DynamicDomain.Destination)
		}
		for _, destination := range destinations {
			if destination.Kind == ProxyDestinationMesh {
				if inventory == nil {
					inventory, err = nodeidentity.ReadInventory(s.inventoryFile)
					if err != nil {
						return fmt.Errorf("read controller allocation authority: %w", err)
					}
				}
				if err := validateAuthorizedRemoteAllocation(inventory, destination); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateAuthorizedRemoteAllocation(inventory *nodeidentity.ClusterInventory, destination ProxyDestination) error {
	if inventory == nil || inventory.ControllerNodeID == "" || inventory.Generation == 0 || destination.ClusterID != inventory.ClusterID {
		return fmt.Errorf("authoritative remote allocation inventory is unavailable")
	}
	allocation, ok := inventory.Allocation(destination.NodeID, destination.AllocationKey, destination.Generation)
	if !ok {
		return fmt.Errorf("remote allocation %s generation %d is not controller-authorized", destination.AllocationKey, destination.Generation)
	}
	if allocation.Kind != PortAllocationKindMeshUpstream || allocation.Project != destination.Project || allocation.Environment != destination.Environment || allocation.Service != destination.Service || allocation.Revision != destination.Revision || allocation.Slot != destination.Slot || allocation.HostIP != destination.HostIP || allocation.HostPort != destination.HostPort || allocation.ContainerPort != destination.ContainerPort || allocation.ClusterID != destination.ClusterID || allocation.NodeID != destination.NodeID || !allocation.IssuedAt.Equal(destination.IssuedAt) || allocation.OperationID != destination.OperationID || allocation.FenceToken != destination.FenceToken || allocation.Signature != destination.Signature || allocation.AuthorizedAt.IsZero() {
		return fmt.Errorf("remote allocation %s does not match the exact controller-authorized generation", destination.AllocationKey)
	}
	node, ok := inventory.Node(allocation.NodeID)
	if !ok || !node.Schedulable || node.Lifecycle != nodeidentity.NodeLifecycleSchedulable || node.MeshCredentialStatus != nodeidentity.MeshCredentialActive || node.MeshIP != allocation.HostIP {
		return fmt.Errorf("remote allocation %s belongs to an unavailable node", destination.AllocationKey)
	}
	if err := nodeidentity.VerifyActiveAllocationOrigin(allocation, node.AllocationPublicKey); err != nil {
		return fmt.Errorf("verify remote allocation %s: %w", destination.AllocationKey, err)
	}
	return nil
}

func hasInventoryRole(roles []string, wanted string) bool {
	for _, role := range roles {
		if role == wanted {
			return true
		}
	}
	return false
}
