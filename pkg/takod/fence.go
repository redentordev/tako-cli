package takod

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/platform"
	"github.com/redentordev/tako-cli/pkg/recovery"
)

const (
	OperationFenceHeader     = "X-Tako-Operation-Fence"
	OperationHolderHeader    = "X-Tako-Operation-Holder"
	CapabilityOperationFence = "operation.controller-fence-v1"
	maxControlHistory        = 256
	maxFenceStateBytes       = 1 << 20
)

type FenceRequest struct {
	Fence       nodeidentity.OperationFence `json:"fence"`
	HolderToken string                      `json:"holderToken,omitempty"`
	OperationID string                      `json:"operationId,omitempty"`
	Token       uint64                      `json:"token,omitempty"`
	Phase       string                      `json:"phase,omitempty"`
}

type FenceResponse struct {
	Active    bool                         `json:"active"`
	HighWater uint64                       `json:"highWater"`
	Fence     *nodeidentity.OperationFence `json:"fence,omitempty"`
	Phase     string                       `json:"phase,omitempty"`
}

type OperationStatusResponse struct {
	Active  *controlOperationRecord  `json:"active,omitempty"`
	History []controlOperationRecord `json:"history,omitempty"`
}

type controlOperationRecord struct {
	RequestID          string                       `json:"requestId"`
	RequestFingerprint string                       `json:"requestFingerprint"`
	HolderToken        string                       `json:"holderToken,omitempty"`
	Fence              nodeidentity.OperationFence  `json:"fence"`
	PreviousFence      *nodeidentity.OperationFence `json:"previousFence,omitempty"`
	Phase              string                       `json:"phase"`
	UpdatedAt          time.Time                    `json:"updatedAt"`
	CompletedAt        time.Time                    `json:"completedAt,omitempty"`
}

type controlOperationState struct {
	SchemaVersion int                      `json:"schemaVersion"`
	NextToken     uint64                   `json:"nextToken"`
	Active        *controlOperationRecord  `json:"active,omitempty"`
	History       []controlOperationRecord `json:"history,omitempty"`
	UpdatedAt     time.Time                `json:"updatedAt"`
}

type controlRequestBinding struct {
	SchemaVersion int       `json:"schemaVersion"`
	RequestID     string    `json:"requestId"`
	Fingerprint   string    `json:"fingerprint"`
	CreatedAt     time.Time `json:"createdAt"`
}

type workerFenceState struct {
	SchemaVersion int                          `json:"schemaVersion"`
	HighWater     uint64                       `json:"highWater"`
	Active        *nodeidentity.OperationFence `json:"active,omitempty"`
	Previous      *nodeidentity.OperationFence `json:"previous,omitempty"`
	UpdatedAt     time.Time                    `json:"updatedAt"`
}

var operationFenceMu sync.Mutex
var workerFenceAdmissionMu sync.RWMutex

func (s *Server) acquireControllerOperationLease(ctx context.Context, req LeaseRequest) (*LeaseResponse, error) {
	if s == nil || s.installation == nil || !s.supportsMembershipController() {
		return AcquireLease(ctx, s.dataDir, req)
	}
	inventory, err := nodeidentity.ReadInventory(s.inventoryFile)
	if err != nil {
		return nil, fmt.Errorf("read controller inventory for operation authority: %w", err)
	}
	if req.Renew {
		return s.renewControllerOperationLease(ctx, inventory, req)
	}
	if strings.TrimSpace(req.RequestID) == "" || len(req.RequestID) > 256 || hasControlChars(req.RequestID) {
		return nil, fmt.Errorf("controller operation request ID is required")
	}
	targets, err := validateFenceTargets(inventory, req.TargetNodeIDs, req.Operation)
	if err != nil {
		return nil, err
	}

	operationFenceMu.Lock()
	defer operationFenceMu.Unlock()
	statePath, err := controlOperationStatePath(s.dataDir, req.Project, req.Environment)
	if err != nil {
		return nil, err
	}
	state, err := readControlOperationState(statePath)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if state.Active != nil && !now.Before(state.Active.Fence.ExpiresAt) {
		retireControlOperation(&state, "expired-reconciled", now)
	}
	fingerprint, err := controllerOperationRequestFingerprint(req, targets)
	if err != nil {
		return nil, err
	}
	for _, completed := range state.History {
		if completed.RequestID == req.RequestID && (completed.RequestFingerprint == "" || completed.RequestFingerprint != fingerprint) {
			return nil, fmt.Errorf("controller operation request ID is durably bound to different immutable scope")
		}
	}
	bound, err := readControllerRequestBinding(statePath, req.RequestID)
	if err != nil {
		return nil, err
	}
	if bound != "" && bound != fingerprint {
		return nil, fmt.Errorf("controller operation request ID is durably bound to different immutable scope")
	}
	if state.Active != nil && state.Active.RequestID == req.RequestID {
		if state.Active.RequestFingerprint == "" || state.Active.RequestFingerprint != fingerprint {
			return nil, fmt.Errorf("controller operation request ID is already bound to different immutable scope")
		}
		if state.Active.HolderToken == "" {
			return nil, fmt.Errorf("controller operation holder credential is unavailable")
		}
		// An idempotent retry returns the original signed grant byte-for-byte.
		// Extending or changing it here would invalidate copies already installed
		// on workers; only the explicit renewal path may rotate the exact fence.
		existing, readErr := ReadLease(ctx, s.dataDir, LeaseRequest{Project: req.Project, Environment: req.Environment})
		if readErr != nil {
			return nil, readErr
		}
		if existing.Lease == nil || existing.Lease.ID != state.Active.Fence.OperationID {
			return nil, fmt.Errorf("controller operation authority lease is inconsistent with its active fence")
		}
		existing.Acquired = true
		existing.HolderToken = state.Active.HolderToken
		fence := state.Active.Fence
		existing.Lease.Fence = &fence
		return existing, nil
	}
	if state.Active != nil {
		activeLease, readErr := ReadLease(ctx, s.dataDir, LeaseRequest{Project: state.Active.Fence.Project, Environment: state.Active.Fence.Environment})
		if readErr != nil {
			return nil, readErr
		}
		if activeLease.Lease == nil {
			fence := state.Active.Fence
			activeLease.Lease = &LeaseInfo{
				ID: fence.OperationID, ProjectName: fence.Project, Environment: fence.Environment,
				Operation: fence.Operation, CreatedAt: fence.IssuedAt, ExpiresAt: fence.ExpiresAt,
			}
		}
		activeLease.Acquired = false
		activeLease.Found = true
		activeLease.Message = "another cluster-global controller operation is active"
		return activeLease, nil
	}
	operationID, err := newControllerOperationID()
	if err != nil {
		return nil, err
	}
	req.ID = operationID
	response, err := AcquireLease(ctx, s.dataDir, req)
	if err != nil || !response.Acquired || response.Lease == nil {
		return response, err
	}
	state.NextToken++
	if state.NextToken == 0 {
		return nil, fmt.Errorf("controller fencing token exhausted")
	}
	holderToken, err := newControllerHolderToken()
	if err != nil {
		_ = releaseLeaseLocked(ctx, s.dataDir, req)
		return nil, err
	}
	holderTokenHash, err := nodeidentity.OperationHolderTokenHash(holderToken)
	if err != nil {
		_ = releaseLeaseLocked(ctx, s.dataDir, req)
		return nil, err
	}
	fence := nodeidentity.OperationFence{
		Kind: nodeidentity.OperationFenceKind, ClusterID: inventory.ClusterID, ControllerNodeID: inventory.ControllerNodeID,
		MembershipGeneration: inventory.Generation, Project: req.Project, Environment: req.Environment,
		OperationID: operationID, Operation: req.Operation, Token: state.NextToken, HolderTokenHash: holderTokenHash, TargetNodeIDs: targets,
		IssuedAt: now, ExpiresAt: response.Lease.ExpiresAt,
	}
	if err := nodeidentity.SignOperationFence(&fence, s.installation); err != nil {
		_ = releaseLeaseLocked(ctx, s.dataDir, req)
		return nil, err
	}
	state.SchemaVersion = 1
	if err := bindControllerRequest(statePath, req.RequestID, fingerprint, now); err != nil {
		_ = releaseLeaseLocked(ctx, s.dataDir, req)
		return nil, err
	}
	state.Active = &controlOperationRecord{RequestID: req.RequestID, RequestFingerprint: fingerprint, HolderToken: holderToken, Fence: fence, Phase: "authority-acquired", UpdatedAt: now}
	state.UpdatedAt = now
	if err := writeControlOperationState(statePath, &state); err != nil {
		_ = releaseLeaseLocked(ctx, s.dataDir, req)
		return nil, fmt.Errorf("persist controller operation authority: %w", err)
	}
	response.Lease.Fence = &fence
	response.HolderToken = holderToken
	return response, nil
}

func (s *Server) renewControllerOperationLease(ctx context.Context, inventory *nodeidentity.ClusterInventory, req LeaseRequest) (*LeaseResponse, error) {
	operationFenceMu.Lock()
	defer operationFenceMu.Unlock()
	statePath, err := controlOperationStatePath(s.dataDir, req.Project, req.Environment)
	if err != nil {
		return nil, err
	}
	state, err := readControlOperationState(statePath)
	if err != nil {
		return nil, err
	}
	if state.Active == nil || state.Active.Fence.OperationID != req.ID {
		return &LeaseResponse{Acquired: false, Found: state.Active != nil, Message: "controller operation authority is not active"}, nil
	}
	if req.Project != state.Active.Fence.Project || req.Environment != state.Active.Fence.Environment {
		return nil, fmt.Errorf("controller operation request scope does not match the active fence")
	}
	if !holderTokensEqual(req.HolderToken, state.Active.HolderToken) {
		return nil, fmt.Errorf("controller operation holder credential is invalid")
	}
	if req.Fence == nil {
		return nil, fmt.Errorf("signed controller operation fence is required for renewal")
	}
	if !operationFencesEqual(*req.Fence, state.Active.Fence) {
		if state.Active.PreviousFence == nil {
			return nil, fmt.Errorf("controller operation fence is not the exact active grant")
		}
		if err := s.verifyExactActiveFence(*req.Fence, state.Active.PreviousFence, time.Time{}); err != nil {
			return nil, err
		}
		response, err := renewLeaseTo(ctx, s.dataDir, req, state.Active.Fence.ExpiresAt)
		if err != nil {
			return response, err
		}
		if response == nil || !response.Acquired || response.Lease == nil {
			return response, fmt.Errorf("controller backing lease could not be reconciled to the committed renewal")
		}
		fence := state.Active.Fence
		response.Lease.Fence = &fence
		response.HolderToken = state.Active.HolderToken
		return response, nil
	}
	if err := s.verifyExactActiveFence(*req.Fence, &state.Active.Fence, time.Now().UTC()); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	fence := state.Active.Fence
	if inventory.Generation != fence.MembershipGeneration {
		return nil, fmt.Errorf("membership changed during active cluster-global operation")
	}
	if err := validateLeaseRequest(req, true); err != nil {
		return nil, err
	}
	ttl := time.Duration(req.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = defaultLeaseTTL
	}
	candidateExpiry := now.Add(ttl)
	if !candidateExpiry.After(fence.ExpiresAt) {
		return nil, fmt.Errorf("controller operation renewal must extend the active fence")
	}
	fence.IssuedAt = now
	fence.ExpiresAt = candidateExpiry
	if err := nodeidentity.SignOperationFence(&fence, s.installation); err != nil {
		return nil, err
	}
	previous := state.Active.Fence
	state.Active.PreviousFence = &previous
	state.Active.Fence = fence
	state.Active.UpdatedAt = now
	state.UpdatedAt = now
	if err := writeControlOperationState(statePath, &state); err != nil {
		return nil, err
	}
	response, err := renewLeaseTo(ctx, s.dataDir, req, candidateExpiry)
	if err != nil {
		return response, err
	}
	if response == nil || !response.Acquired || response.Lease == nil {
		return response, fmt.Errorf("controller backing lease was lost after durable fence renewal")
	}
	response.Lease.Fence = &fence
	response.HolderToken = state.Active.HolderToken
	return response, nil
}

func (s *Server) releaseControllerOperationLease(ctx context.Context, req LeaseRequest) (*LeaseResponse, error) {
	if s == nil || s.installation == nil || !s.supportsMembershipController() {
		return ReleaseLease(ctx, s.dataDir, req)
	}
	operationFenceMu.Lock()
	defer operationFenceMu.Unlock()
	path, pathErr := controlOperationStatePath(s.dataDir, req.Project, req.Environment)
	if pathErr != nil {
		return nil, pathErr
	}
	state, stateErr := readControlOperationState(path)
	if stateErr != nil {
		return nil, stateErr
	}
	if state.Active == nil || state.Active.Fence.OperationID != req.ID || req.Fence == nil {
		return nil, fmt.Errorf("signed active controller operation fence is required for release")
	}
	if req.Project != state.Active.Fence.Project || req.Environment != state.Active.Fence.Environment {
		return nil, fmt.Errorf("controller operation request scope does not match the active fence")
	}
	if !holderTokensEqual(req.HolderToken, state.Active.HolderToken) {
		return nil, fmt.Errorf("controller operation holder credential is invalid")
	}
	if err := s.verifyExactActiveFence(*req.Fence, &state.Active.Fence, time.Time{}); err != nil {
		if state.Active.PreviousFence == nil {
			return nil, err
		}
		if previousErr := s.verifyExactActiveFence(*req.Fence, state.Active.PreviousFence, time.Time{}); previousErr != nil {
			return nil, err
		}
	}
	response, err := ReleaseLease(ctx, s.dataDir, req)
	if err != nil {
		return response, err
	}
	if response == nil || response.Lease == nil || response.Lease.ID != req.ID {
		return nil, fmt.Errorf("controller operation lease was not removed; global authority remains active")
	}
	retireControlOperation(&state, "completed", time.Now().UTC())
	if err := writeControlOperationState(path, &state); err != nil {
		return nil, err
	}
	return response, nil
}

func releaseLeaseLocked(ctx context.Context, dataDir string, req LeaseRequest) error {
	_, err := ReleaseLease(ctx, dataDir, req)
	return err
}

func retireControlOperation(state *controlOperationState, phase string, now time.Time) {
	if state == nil || state.Active == nil {
		return
	}
	record := *state.Active
	record.HolderToken = ""
	record.PreviousFence = nil
	record.Phase = phase
	record.UpdatedAt = now
	record.CompletedAt = now
	state.History = append(state.History, record)
	if len(state.History) > maxControlHistory {
		state.History = append([]controlOperationRecord(nil), state.History[len(state.History)-maxControlHistory:]...)
	}
	state.Active = nil
	state.UpdatedAt = now
}

func (s *Server) handleFence(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.handleFenceStatus(w, r)
		return
	}
	defer r.Body.Close()
	var req FenceRequest
	if err := decodeJSONRequest(w, r, &req); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var response *FenceResponse
	var err error
	switch r.Method {
	case http.MethodPost:
		if req.Phase != "" {
			response, err = s.updateControllerFencePhase(req)
		} else {
			response, err = s.activateWorkerFence(req.Fence, req.HolderToken)
		}
	case http.MethodDelete:
		response, err = s.revokeWorkerFenceWithFence(req.Fence, req.HolderToken)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func (s *Server) handleFenceStatus(w http.ResponseWriter, r *http.Request) {
	if !s.supportsMembershipController() {
		http.Error(w, "operation status is available only from the controller", http.StatusForbidden)
		return
	}
	project, environment := strings.TrimSpace(r.URL.Query().Get("project")), strings.TrimSpace(r.URL.Query().Get("environment"))
	path, err := controlOperationStatePath(s.dataDir, project, environment)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	operationFenceMu.Lock()
	state, err := readControlOperationState(path)
	if err == nil && state.Active != nil && !time.Now().UTC().Before(state.Active.Fence.ExpiresAt) {
		retireControlOperation(&state, "expired-reconciled", time.Now().UTC())
		err = writeControlOperationState(path, &state)
	}
	operationFenceMu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	response := OperationStatusResponse{History: append([]controlOperationRecord(nil), state.History...)}
	for index := range response.History {
		response.History[index].RequestID = ""
		response.History[index].RequestFingerprint = ""
		response.History[index].HolderToken = ""
		response.History[index].PreviousFence = nil
		redactOperationFence(&response.History[index].Fence)
	}
	if state.Active != nil {
		active := *state.Active
		active.RequestID = ""
		active.RequestFingerprint = ""
		active.HolderToken = ""
		redactOperationFence(&active.Fence)
		redactOperationFence(active.PreviousFence)
		response.Active = &active
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func redactOperationFence(fence *nodeidentity.OperationFence) {
	if fence != nil {
		fence.Signature = ""
	}
}

func (s *Server) handleInventoryAuthority(w http.ResponseWriter, r *http.Request) {
	var snapshot nodeidentity.SignedInventorySnapshot
	var err error
	switch r.Method {
	case http.MethodGet:
		if !s.supportsMembershipController() {
			http.Error(w, "signed inventory is available only from the controller", http.StatusForbidden)
			return
		}
		store, storeErr := platform.NewMembershipStore(s.membershipFile, s.inventoryFile)
		if storeErr != nil {
			err = storeErr
			break
		}
		state, readErr := store.Read()
		if readErr != nil {
			err = readErr
			break
		}
		snapshot = nodeidentity.SignedInventorySnapshot{Kind: nodeidentity.SignedInventoryKind, Inventory: state.Inventory(), IssuedAt: time.Now().UTC()}
		err = nodeidentity.SignInventorySnapshot(&snapshot, s.installation)
	case http.MethodPost:
		defer r.Body.Close()
		if decodeErr := decodeJSONRequest(w, r, &snapshot); decodeErr != nil {
			http.Error(w, "invalid JSON body: "+decodeErr.Error(), http.StatusBadRequest)
			return
		}
		err = s.acceptSignedInventory(snapshot)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snapshot)
}

func (s *Server) acceptSignedInventory(snapshot nodeidentity.SignedInventorySnapshot) error {
	workerFenceAdmissionMu.Lock()
	defer workerFenceAdmissionMu.Unlock()
	if s.installation == nil {
		return fmt.Errorf("signed inventory publication requires an enrolled node")
	}
	current, err := nodeidentity.ReadInventory(s.inventoryFile)
	if err != nil {
		return err
	}
	if snapshot.Inventory.ClusterID != current.ClusterID || snapshot.Inventory.ControllerNodeID != current.ControllerNodeID || snapshot.Inventory.Generation < current.Generation || snapshot.Inventory.AllocationGeneration < current.AllocationGeneration {
		return fmt.Errorf("signed inventory does not advance the trusted controller generation")
	}
	controller, ok := current.Node(current.ControllerNodeID)
	if !ok || !hasInventoryRole(controller.Roles, nodeidentity.RoleControlPlane) {
		return fmt.Errorf("current inventory has no trusted controller key")
	}
	if err := nodeidentity.VerifyInventorySnapshot(snapshot, controller.AllocationPublicKey, time.Now().UTC()); err != nil {
		return err
	}
	s.proxyAuthorityMu.Lock()
	defer s.proxyAuthorityMu.Unlock()
	if snapshot.Inventory.Generation == current.Generation && snapshot.Inventory.AllocationGeneration == current.AllocationGeneration {
		currentJSON, _ := json.Marshal(current)
		candidateJSON, _ := json.Marshal(snapshot.Inventory)
		if string(currentJSON) != string(candidateJSON) {
			return fmt.Errorf("signed inventory content changed without a generation advance")
		}
		return s.reconcileAllStoredProxyAuthority()
	}
	if snapshot.Inventory.AllocationGeneration != current.AllocationGeneration {
		invalid, err := s.storedProxyAuthorityInvalidForInventory(&snapshot.Inventory)
		if err != nil {
			return err
		}
		if invalid {
			// The old in-memory Caddy config can outlive revoked on-disk authority.
			// Stop it before committing only when at least one stored route would
			// become invalid. A monotonic crash-recovery generation with unchanged
			// routes therefore does not strand an otherwise healthy edge proxy.
			if err := stopProxyAndVerifyAbsent(context.Background()); err != nil {
				return fmt.Errorf("stop proxy before allocation authority transition: %w", err)
			}
		}
	}
	if err := nodeidentity.ReplaceInventory(s.inventoryFile, snapshot.Inventory); err != nil {
		return err
	}
	return s.reconcileAllStoredProxyAuthority()
}

func (s *Server) updateControllerFencePhase(req FenceRequest) (*FenceResponse, error) {
	if !s.supportsMembershipController() {
		return nil, fmt.Errorf("operation phases may be updated only on the controller")
	}
	if req.Phase != "targets-fenced" && req.Phase != "mutating" {
		return nil, fmt.Errorf("unsupported controller operation phase")
	}
	operationFenceMu.Lock()
	defer operationFenceMu.Unlock()
	path, err := controlOperationStatePath(s.dataDir, req.Fence.Project, req.Fence.Environment)
	if err != nil {
		return nil, err
	}
	state, err := readControlOperationState(path)
	if err != nil {
		return nil, err
	}
	if state.Active == nil {
		return nil, fmt.Errorf("controller operation is not active")
	}
	if !holderTokensEqual(req.HolderToken, state.Active.HolderToken) {
		return nil, fmt.Errorf("controller operation holder credential is invalid")
	}
	if err := s.verifyExactActiveFence(req.Fence, &state.Active.Fence, time.Now().UTC()); err != nil {
		return nil, err
	}
	state.Active.Phase = req.Phase
	state.Active.UpdatedAt = time.Now().UTC()
	state.UpdatedAt = state.Active.UpdatedAt
	if err := writeControlOperationState(path, &state); err != nil {
		return nil, err
	}
	fence := state.Active.Fence
	return &FenceResponse{Active: true, HighWater: fence.Token, Fence: &fence, Phase: req.Phase}, nil
}

func (s *Server) activateWorkerFence(fence nodeidentity.OperationFence, holderToken string) (*FenceResponse, error) {
	workerFenceAdmissionMu.Lock()
	defer workerFenceAdmissionMu.Unlock()
	if s.installation == nil {
		return nil, fmt.Errorf("operation fencing requires an enrolled node")
	}
	inventory, err := nodeidentity.ReadInventory(s.inventoryFile)
	if err != nil {
		return nil, err
	}
	if inventory.ClusterID != fence.ClusterID || inventory.ControllerNodeID != fence.ControllerNodeID || inventory.Generation != fence.MembershipGeneration {
		return nil, fmt.Errorf("operation fence does not match current controller membership generation")
	}
	controller, ok := inventory.Node(inventory.ControllerNodeID)
	if !ok || !hasInventoryRole(controller.Roles, nodeidentity.RoleControlPlane) {
		return nil, fmt.Errorf("operation fence controller is not authoritative")
	}
	if !fence.Targets(s.installation.NodeID) {
		return nil, fmt.Errorf("operation fence does not target this node")
	}
	if err := nodeidentity.VerifyOperationFence(fence, controller.AllocationPublicKey, time.Now().UTC()); err != nil {
		return nil, err
	}
	if err := verifyOperationHolderToken(holderToken, fence.HolderTokenHash); err != nil {
		return nil, err
	}
	path, err := workerFencePath(s.dataDir, fence.Project, fence.Environment)
	if err != nil {
		return nil, err
	}
	operationFenceMu.Lock()
	defer operationFenceMu.Unlock()
	state, err := readWorkerFenceState(path)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if fence.Token < state.HighWater {
		return nil, fmt.Errorf("operation fence token %d is behind local high-water %d", fence.Token, state.HighWater)
	}
	if fence.Token == state.HighWater {
		if state.Active == nil {
			return nil, fmt.Errorf("operation fence token %d was already revoked", fence.Token)
		}
		if state.Active.OperationID != fence.OperationID {
			return nil, fmt.Errorf("operation fence token is already bound to another operation")
		}
		if operationFencesEqual(fence, *state.Active) {
			if _, err := s.holdOperationBarrier(fence); err != nil {
				return nil, fmt.Errorf("reacquire node-global operation barrier: %w", err)
			}
			s.bindOperationBarrier(fence)
			s.expireOperationBarrier(fence)
			return &FenceResponse{Active: true, HighWater: state.HighWater, Fence: state.Active}, nil
		}
		if !validFenceRenewal(*state.Active, fence) {
			return nil, fmt.Errorf("operation fence renewal is not strictly monotonic")
		}
	} else if state.Active != nil && now.Before(state.Active.ExpiresAt) {
		return nil, fmt.Errorf("another operation fence remains active")
	} else if state.Active != nil {
		s.releaseOperationBarrier(state.Active.OperationID)
		state.Active = nil
		state.Previous = nil
	}
	barrierAcquired, err := s.holdOperationBarrier(fence)
	if err != nil {
		return nil, fmt.Errorf("acquire node-global operation barrier: %w", err)
	}
	state.SchemaVersion = 1
	state.HighWater = fence.Token
	if state.Active != nil {
		previous := *state.Active
		state.Previous = &previous
	} else {
		state.Previous = nil
	}
	state.Active = &fence
	state.UpdatedAt = now
	if err := writeJSONAtomic(path, state, 0600); err != nil {
		if barrierAcquired {
			s.releaseOperationBarrier(fence.OperationID)
		}
		return nil, err
	}
	s.bindOperationBarrier(fence)
	s.expireOperationBarrier(fence)
	return &FenceResponse{Active: true, HighWater: state.HighWater, Fence: &fence}, nil
}

func (s *Server) revokeWorkerFenceWithFence(fence nodeidentity.OperationFence, holderToken string) (*FenceResponse, error) {
	workerFenceAdmissionMu.Lock()
	defer workerFenceAdmissionMu.Unlock()
	if s.installation == nil || !fence.Targets(s.installation.NodeID) {
		return nil, fmt.Errorf("operation fence does not target this node")
	}
	path, err := workerFencePath(s.dataDir, fence.Project, fence.Environment)
	if err != nil {
		return nil, err
	}
	operationFenceMu.Lock()
	defer operationFenceMu.Unlock()
	state, err := readWorkerFenceState(path)
	if err != nil {
		return nil, err
	}
	if state.Active == nil {
		s.releaseOperationBarrier(fence.OperationID)
		return &FenceResponse{Active: false, HighWater: state.HighWater}, nil
	}
	if err := verifyOperationHolderToken(holderToken, state.Active.HolderTokenHash); err != nil {
		return nil, err
	}
	// Revocation is authenticated cleanup, not mutation admission. Permit the
	// exact stored grant (or immediate predecessor) after expiry so a partition
	// cannot strand a renewed worker until the newer fence also expires.
	if err := s.verifyExactActiveFence(fence, state.Active, time.Time{}); err != nil {
		if state.Previous == nil {
			return nil, err
		}
		if previousErr := s.verifyExactActiveFence(fence, state.Previous, time.Time{}); previousErr != nil {
			return nil, err
		}
	}
	if state.Active.OperationID == fence.OperationID && state.Active.Token == fence.Token {
		state.Active = nil
		state.Previous = nil
		state.UpdatedAt = time.Now().UTC()
		if err := writeJSONAtomic(path, state, 0600); err != nil {
			return nil, err
		}
		s.releaseOperationBarrier(fence.OperationID)
	}
	return &FenceResponse{Active: false, HighWater: state.HighWater}, nil
}

func (s *Server) holdOperationBarrier(fence nodeidentity.OperationFence) (bool, error) {
	s.operationBarrierMu.Lock()
	defer s.operationBarrierMu.Unlock()
	if s.operationBarrierUnlock != nil {
		return false, nil
	}
	unlock, err := recovery.AcquireOperationBarrier(s.dataDir)
	if err != nil {
		return false, err
	}
	s.operationBarrierUnlock = unlock
	s.operationBarrierID = fence.OperationID
	s.operationBarrierExpires = fence.ExpiresAt
	return true, nil
}

func (s *Server) bindOperationBarrier(fence nodeidentity.OperationFence) {
	s.operationBarrierMu.Lock()
	defer s.operationBarrierMu.Unlock()
	if s.operationBarrierUnlock != nil {
		s.operationBarrierID = fence.OperationID
		s.operationBarrierExpires = fence.ExpiresAt
	}
}

func (s *Server) releaseOperationBarrier(operationID string) {
	s.operationBarrierMu.Lock()
	defer s.operationBarrierMu.Unlock()
	if s.operationBarrierUnlock == nil || s.operationBarrierID != operationID {
		return
	}
	s.operationBarrierUnlock()
	s.operationBarrierUnlock = nil
	s.operationBarrierID = ""
	s.operationBarrierExpires = time.Time{}
}

func (s *Server) releaseAnyOperationBarrier() {
	s.operationBarrierMu.Lock()
	defer s.operationBarrierMu.Unlock()
	if s.operationBarrierUnlock != nil {
		s.operationBarrierUnlock()
		s.operationBarrierUnlock = nil
	}
	s.operationBarrierID = ""
	s.operationBarrierExpires = time.Time{}
}

func (s *Server) expireOperationBarrier(fence nodeidentity.OperationFence) {
	delay := time.Until(fence.ExpiresAt)
	if delay < 0 {
		delay = 0
	}
	go func(operationID string, delay time.Duration) {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		<-timer.C
		s.operationBarrierMu.Lock()
		if s.operationBarrierUnlock != nil && s.operationBarrierID == operationID && !time.Now().UTC().Before(s.operationBarrierExpires) {
			s.operationBarrierUnlock()
			s.operationBarrierUnlock = nil
			s.operationBarrierID = ""
			s.operationBarrierExpires = time.Time{}
		}
		s.operationBarrierMu.Unlock()
	}(fence.OperationID, delay)
}

type operationFenceContextKey struct{}
type placementCleanupContextKey struct{}

func withOperationFence(ctx context.Context, fence nodeidentity.OperationFence) context.Context {
	return context.WithValue(ctx, operationFenceContextKey{}, fence)
}

func operationFenceFromContext(ctx context.Context) (nodeidentity.OperationFence, bool) {
	fence, ok := ctx.Value(operationFenceContextKey{}).(nodeidentity.OperationFence)
	return fence, ok
}

func withPlacementCleanupOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, placementCleanupContextKey{}, true)
}

func placementCleanupOnly(ctx context.Context) bool {
	value, _ := ctx.Value(placementCleanupContextKey{}).(bool)
	return value
}

func (s *Server) validateMutationFence(r *http.Request) (context.Context, error) {
	encoded := strings.TrimSpace(r.Header.Get(OperationFenceHeader))
	if encoded == "" {
		return r.Context(), fmt.Errorf("controller operation fence is required")
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(data) > 32<<10 {
		return r.Context(), fmt.Errorf("controller operation fence header is invalid")
	}
	var fence nodeidentity.OperationFence
	if err := json.Unmarshal(data, &fence); err != nil {
		return r.Context(), fmt.Errorf("controller operation fence header is invalid")
	}
	if s.installation == nil || !fence.Targets(s.installation.NodeID) {
		return r.Context(), fmt.Errorf("controller operation fence does not target this node")
	}
	path, err := workerFencePath(s.dataDir, fence.Project, fence.Environment)
	if err != nil {
		return r.Context(), err
	}
	operationFenceMu.Lock()
	state, err := readWorkerFenceState(path)
	operationFenceMu.Unlock()
	if err != nil {
		return r.Context(), err
	}
	accepted := state.Active
	if err := s.verifyExactActiveFence(fence, accepted, time.Now().UTC()); err != nil {
		accepted = state.Previous
		if previousErr := s.verifyExactActiveFence(fence, accepted, time.Now().UTC()); previousErr != nil {
			return r.Context(), err
		}
	}
	if err := verifyOperationHolderToken(r.Header.Get(OperationHolderHeader), accepted.HolderTokenHash); err != nil {
		return r.Context(), err
	}
	if err := validateFencedRequestQuery(r, *accepted); err != nil {
		return r.Context(), err
	}
	// Downstream authorization consumes only the durable, cryptographically
	// verified record. Caller-controlled header fields never enter context.
	return withOperationFence(r.Context(), *accepted), nil
}

func validateFencedPayloadScope(ctx context.Context, value any) error {
	fence, ok := operationFenceFromContext(ctx)
	if !ok || value == nil {
		return nil
	}
	return validateEndpointPayloadScope(value, fence)
}

func (s *Server) verifyExactActiveFence(candidate nodeidentity.OperationFence, active *nodeidentity.OperationFence, now time.Time) error {
	if active == nil || !operationFencesEqual(candidate, *active) {
		return fmt.Errorf("controller operation fence is not the exact active grant on this node")
	}
	inventory, err := nodeidentity.ReadInventory(s.inventoryFile)
	if err != nil {
		return fmt.Errorf("read trusted controller inventory: %w", err)
	}
	if inventory.ClusterID != active.ClusterID || inventory.ControllerNodeID != active.ControllerNodeID || inventory.Generation != active.MembershipGeneration {
		return fmt.Errorf("controller operation fence does not match current membership authority")
	}
	controller, ok := inventory.Node(inventory.ControllerNodeID)
	if !ok || !hasInventoryRole(controller.Roles, nodeidentity.RoleControlPlane) {
		return fmt.Errorf("current inventory has no trusted controller key")
	}
	if err := nodeidentity.VerifyOperationFence(*active, controller.AllocationPublicKey, now); err != nil {
		return fmt.Errorf("verify active controller operation fence: %w", err)
	}
	return nil
}

func operationFencesEqual(a, b nodeidentity.OperationFence) bool {
	aJSON, aErr := json.Marshal(a)
	bJSON, bErr := json.Marshal(b)
	return aErr == nil && bErr == nil && string(aJSON) == string(bJSON)
}

func validFenceRenewal(active, candidate nodeidentity.OperationFence) bool {
	if candidate.Kind != active.Kind || candidate.ClusterID != active.ClusterID || candidate.ControllerNodeID != active.ControllerNodeID ||
		candidate.MembershipGeneration != active.MembershipGeneration || candidate.Project != active.Project || candidate.Environment != active.Environment ||
		candidate.OperationID != active.OperationID || candidate.Operation != active.Operation || candidate.Token != active.Token || candidate.HolderTokenHash != active.HolderTokenHash ||
		!equalStrings(candidate.TargetNodeIDs, active.TargetNodeIDs) {
		return false
	}
	return candidate.IssuedAt.After(active.IssuedAt) && candidate.ExpiresAt.After(active.ExpiresAt)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

func controllerOperationRequestFingerprint(req LeaseRequest, targets []string) (string, error) {
	immutable := struct {
		Project, Environment, Operation, Who string
		PID                                  int
		TargetNodeIDs                        []string
	}{strings.TrimSpace(req.Project), strings.TrimSpace(req.Environment), strings.TrimSpace(req.Operation), strings.TrimSpace(req.Who), req.PID, append([]string(nil), targets...)}
	data, err := json.Marshal(immutable)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func validateFencedRequestQuery(r *http.Request, fence nodeidentity.OperationFence) error {
	query := r.URL.Query()
	project, environment := strings.TrimSpace(query.Get("project")), strings.TrimSpace(query.Get("environment"))
	if project != "" && project != fence.Project {
		return fmt.Errorf("request project is outside the controller operation fence")
	}
	if environment != "" && environment != fence.Environment {
		return fmt.Errorf("request environment is outside the controller operation fence")
	}
	switch r.URL.Path {
	case "/v1/images/build", "/v1/images/import", "/v1/proxy-file", "/v1/certs", "/v1/acme-dns", "/v1/backups", "/v1/backups/restore", "/v1/backups/cleanup", "/v1/backup-schedule":
		// Body-scoped forms are validated after decoding. Query-scoped forms
		// must carry both dimensions so opaque identifiers cannot cross fences.
		if (r.Method == http.MethodDelete || r.URL.Path == "/v1/images/build" || r.URL.Path == "/v1/images/import") && (project == "" || environment == "") {
			return fmt.Errorf("mutation query has no unambiguous project/environment scope")
		}
	}
	return nil
}

func validateFenceTargets(inventory *nodeidentity.ClusterInventory, targets []string, operation string) ([]string, error) {
	if inventory == nil || inventory.ControllerNodeID == "" || inventory.Generation == 0 {
		return nil, fmt.Errorf("authoritative controller inventory is required")
	}
	seen := make(map[string]struct{}, len(targets))
	out := make([]string, 0, len(targets))
	for _, nodeID := range targets {
		nodeID = strings.ToLower(strings.TrimSpace(nodeID))
		if _, duplicate := seen[nodeID]; duplicate {
			continue
		}
		node, ok := inventory.Node(nodeID)
		lifecycleAllowed := node.Lifecycle == nodeidentity.NodeLifecycleSchedulable
		if operation == "placement-apply" {
			lifecycleAllowed = lifecycleAllowed || node.Lifecycle == nodeidentity.NodeLifecycleCordoned || node.Lifecycle == nodeidentity.NodeLifecycleDraining
		}
		if !ok || node.MeshCredentialStatus != nodeidentity.MeshCredentialActive || !lifecycleAllowed {
			return nil, fmt.Errorf("operation target %s is not an active schedulable member", nodeID)
		}
		seen[nodeID] = struct{}{}
		out = append(out, nodeID)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("controller operation requires at least one target node")
	}
	sort.Strings(out)
	return out, nil
}

func newControllerOperationID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return "op-" + hex.EncodeToString(random), nil
}

func newControllerHolderToken() (string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(random), nil
}

func holderTokensEqual(candidate, expected string) bool {
	if candidate == "" || expected == "" || len(candidate) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(expected)) == 1
}

func verifyOperationHolderToken(holderToken, expectedHash string) error {
	hash, err := nodeidentity.OperationHolderTokenHash(holderToken)
	if err != nil || !holderTokensEqual(hash, expectedHash) {
		return fmt.Errorf("controller operation holder credential is invalid")
	}
	return nil
}

func controlOperationStatePath(dataDir, project, environment string) (string, error) {
	_, err := leasePath(dataDir, project, environment)
	if err != nil {
		return "", err
	}
	// Enrolled operations are cluster-global, even when their payload is scoped
	// to one application. Docker image tags, pruning, proxy state, allocation
	// generations, and node lifecycle are shared resources, so two project
	// leases must never mutate a node concurrently.
	return filepath.Join(dataDir, "control", "control-operation.json"), nil
}

func workerFencePath(dataDir, project, environment string) (string, error) {
	_, err := leasePath(dataDir, project, environment)
	if err != nil {
		return "", err
	}
	return filepath.Join(dataDir, "control", "worker-fence.json"), nil
}

func controllerRequestBindingPath(statePath, requestID string) string {
	digest := sha256.Sum256([]byte(requestID))
	return filepath.Join(filepath.Dir(statePath), "request-bindings", hex.EncodeToString(digest[:])+".json")
}

func readControllerRequestBinding(statePath, requestID string) (string, error) {
	path := controllerRequestBindingPath(statePath, requestID)
	var binding controlRequestBinding
	if err := readJSONFile(path, &binding); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read controller request binding: %w", err)
	}
	if binding.SchemaVersion != 1 || binding.RequestID != requestID || binding.Fingerprint == "" || binding.CreatedAt.IsZero() {
		return "", fmt.Errorf("controller request binding is invalid")
	}
	return binding.Fingerprint, nil
}

func bindControllerRequest(statePath, requestID, fingerprint string, now time.Time) error {
	existing, err := readControllerRequestBinding(statePath, requestID)
	if err != nil {
		return err
	}
	if existing != "" {
		if existing != fingerprint {
			return fmt.Errorf("controller operation request ID is durably bound to different immutable scope")
		}
		return nil
	}
	binding := controlRequestBinding{SchemaVersion: 1, RequestID: requestID, Fingerprint: fingerprint, CreatedAt: now}
	if err := writeJSONAtomic(controllerRequestBindingPath(statePath, requestID), binding, 0600); err != nil {
		return fmt.Errorf("persist controller request binding: %w", err)
	}
	return nil
}

func activeWorkerOperation(dataDir string, now time.Time) (bool, error) {
	path := filepath.Join(dataDir, "control", "worker-fence.json")
	var state workerFenceState
	if err := readJSONFile(path, &state); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return state.Active != nil && now.Before(state.Active.ExpiresAt), nil
}

func readControlOperationState(path string) (controlOperationState, error) {
	var state controlOperationState
	if err := readJSONFile(path, &state); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return controlOperationState{SchemaVersion: 1}, nil
		}
		return state, err
	}
	if state.SchemaVersion != 1 {
		return state, fmt.Errorf("unsupported controller operation state schema")
	}
	return state, nil
}

func readWorkerFenceState(path string) (workerFenceState, error) {
	var state workerFenceState
	if err := readJSONFile(path, &state); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return workerFenceState{SchemaVersion: 1}, nil
		}
		return state, err
	}
	if state.SchemaVersion != 1 {
		return state, fmt.Errorf("unsupported worker fence state schema")
	}
	return state, nil
}

func readJSONFile(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) > maxFenceStateBytes {
		return fmt.Errorf("state file exceeds size limit")
	}
	return json.Unmarshal(data, value)
}

func writeJSONAtomic(path string, value any, mode os.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return writeFileAtomic(path, data, mode)
}

func writeControlOperationState(path string, state *controlOperationState) error {
	if state == nil {
		return fmt.Errorf("controller operation state is required")
	}
	for {
		data, err := json.MarshalIndent(state, "", "  ")
		if err != nil {
			return err
		}
		if len(data)+1 <= maxFenceStateBytes {
			return writeJSONAtomic(path, state, 0600)
		}
		if len(state.History) == 0 {
			return fmt.Errorf("active controller operation state exceeds size limit")
		}
		state.History = append([]controlOperationRecord(nil), state.History[1:]...)
	}
}
