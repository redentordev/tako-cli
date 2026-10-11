package takod

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CapabilityNodeMaintenanceLeaseV1 means the agent serves the node maintenance
// lease at /v1/node-lease/maintenance and refuses environment leases while it
// is held.
const CapabilityNodeMaintenanceLeaseV1 = "lease.node-maintenance-v1"

// NodeMaintenanceLeaseVersion is the frozen on-disk format of
// <dataDir>/node-leases/maintenance.json. A new takod must read a lease that
// an older one wrote before its own restart, so the fields below never change
// meaning; a new format gets a new version.
const NodeMaintenanceLeaseVersion = 1

const maxNodeMaintenanceLeaseTTL = 2 * time.Hour

// NodeMaintenanceLease is the node-wide maintenance lease (format v1).
type NodeMaintenanceLease struct {
	Version   int       `json:"version"`
	ID        string    `json:"id"`
	Operation string    `json:"operation"`
	Who       string    `json:"who"`
	PID       int       `json:"pid"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// NodeMaintenanceLeaseRequest acquires or releases the maintenance lease.
type NodeMaintenanceLeaseRequest struct {
	ID         string `json:"id"`
	Operation  string `json:"operation,omitempty"`
	Who        string `json:"who,omitempty"`
	PID        int    `json:"pid,omitempty"`
	TTLSeconds int64  `json:"ttlSeconds,omitempty"`
}

// NodeMaintenanceLeaseResponse reports the maintenance lease, or the live
// environment leases that prevented it.
type NodeMaintenanceLeaseResponse struct {
	Acquired bool                  `json:"acquired"`
	Found    bool                  `json:"found"`
	Lease    *NodeMaintenanceLease `json:"lease,omitempty"`
	Holders  []LeaseInfo           `json:"holders,omitempty"`
	Message  string                `json:"message,omitempty"`
}

func nodeMaintenanceLeasePath(dataDir string) (string, error) {
	if dataDir == "" {
		return "", fmt.Errorf("data directory is required")
	}
	return filepath.Join(dataDir, "node-leases", "maintenance.json"), nil
}

// readNodeMaintenanceLeaseLocked returns the live maintenance lease, or nil.
// An expired lease is removed. A file that cannot be parsed, or has an
// unknown version, fails closed: the caller must not treat the node as free.
func readNodeMaintenanceLeaseLocked(dataDir string, now time.Time) (*NodeMaintenanceLease, error) {
	path, err := nodeMaintenanceLeasePath(dataDir)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read node maintenance lease: %w", err)
	}
	var lease NodeMaintenanceLease
	if err := json.Unmarshal(data, &lease); err != nil {
		return nil, fmt.Errorf("node maintenance lease %s is unreadable; remove it once no upgrade or setup is running: %w", path, err)
	}
	if lease.Version != NodeMaintenanceLeaseVersion {
		return nil, fmt.Errorf("node maintenance lease %s has unsupported version %d", path, lease.Version)
	}
	if !now.Before(lease.ExpiresAt) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove expired node maintenance lease: %w", err)
		}
		return nil, nil
	}
	return &lease, nil
}

// liveEnvironmentLeasesLocked lists the live environment leases of every
// project on this node, sorted by project and environment.
func liveEnvironmentLeasesLocked(dataDir string, now time.Time) ([]LeaseInfo, error) {
	files, err := filepath.Glob(filepath.Join(dataDir, "leases", "*", "*.json"))
	if err != nil {
		return nil, err
	}
	var holders []LeaseInfo
	for _, file := range files {
		lease, err := readLeaseFile(file)
		if err != nil {
			return nil, fmt.Errorf("read environment lease %s: %w", file, err)
		}
		if lease == nil || !now.Before(lease.ExpiresAt) {
			continue
		}
		holder := *lease
		holder.Fence = nil
		if holder.ProjectName == "" {
			holder.ProjectName = filepath.Base(filepath.Dir(file))
		}
		if holder.Environment == "" {
			holder.Environment = strings.TrimSuffix(filepath.Base(file), ".json")
		}
		holders = append(holders, holder)
	}
	sort.Slice(holders, func(i, j int) bool {
		if holders[i].ProjectName != holders[j].ProjectName {
			return holders[i].ProjectName < holders[j].ProjectName
		}
		return holders[i].Environment < holders[j].Environment
	})
	return holders, nil
}

// maintenanceBlockedEnvironmentLease is the response an environment lease
// acquisition gets while the node maintenance lease is live. The lease names
// the maintenance operation so the CLI's locked error does too.
func maintenanceBlockedEnvironmentLease(maintenance *NodeMaintenanceLease) *LeaseResponse {
	return &LeaseResponse{
		Acquired: false,
		Found:    true,
		Lease: &LeaseInfo{
			ID:        maintenance.ID,
			Operation: "node maintenance (" + maintenance.Operation + ")",
			Who:       maintenance.Who,
			PID:       maintenance.PID,
			CreatedAt: maintenance.CreatedAt,
			ExpiresAt: maintenance.ExpiresAt,
		},
		Message: "node maintenance lease is held",
	}
}

func validateNodeMaintenanceLeaseRequest(req NodeMaintenanceLeaseRequest, acquire bool) error {
	if req.ID == "" || !isSafeStateRevisionID(req.ID) {
		return fmt.Errorf("invalid lease ID")
	}
	if !acquire {
		return nil
	}
	if req.Operation == "" || !isSafeStateRevisionID(req.Operation) {
		return fmt.Errorf("invalid lease operation")
	}
	if req.Who == "" || len(req.Who) > 256 || hasControlChars(req.Who) {
		return fmt.Errorf("invalid lease owner")
	}
	if req.PID < 0 {
		return fmt.Errorf("invalid lease PID")
	}
	if req.TTLSeconds <= 0 || req.TTLSeconds > int64(maxNodeMaintenanceLeaseTTL/time.Second) {
		return fmt.Errorf("invalid lease TTL")
	}
	return nil
}

// AcquireNodeMaintenanceLease takes the node-wide maintenance lease. It fails
// without waiting while another maintenance holder or any live environment
// lease of any project exists. The check and the write run under the same
// mutex as environment lease acquisition, so the two kinds exclude each other.
func AcquireNodeMaintenanceLease(ctx context.Context, dataDir string, req NodeMaintenanceLeaseRequest) (*NodeMaintenanceLeaseResponse, error) {
	if err := validateNodeMaintenanceLeaseRequest(req, true); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := nodeMaintenanceLeasePath(dataDir)
	if err != nil {
		return nil, err
	}
	leaseMu.Lock()
	defer leaseMu.Unlock()
	now := time.Now().UTC()
	current, err := readNodeMaintenanceLeaseLocked(dataDir, now)
	if err != nil {
		return nil, err
	}
	if current != nil && current.ID != req.ID {
		return &NodeMaintenanceLeaseResponse{Acquired: false, Found: true, Lease: current, Message: "node maintenance lease is held"}, nil
	}
	holders, err := liveEnvironmentLeasesLocked(dataDir, now)
	if err != nil {
		return nil, err
	}
	if len(holders) > 0 {
		return &NodeMaintenanceLeaseResponse{Acquired: false, Found: current != nil, Lease: current, Holders: holders, Message: "environment leases are held"}, nil
	}
	lease := NodeMaintenanceLease{
		Version:   NodeMaintenanceLeaseVersion,
		ID:        req.ID,
		Operation: req.Operation,
		Who:       req.Who,
		PID:       req.PID,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Duration(req.TTLSeconds) * time.Second),
	}
	if current != nil {
		lease.CreatedAt = current.CreatedAt
	}
	content, err := json.MarshalIndent(&lease, "", "  ")
	if err != nil {
		return nil, err
	}
	content = append(content, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create node lease directory: %w", err)
	}
	if err := writeFileAtomic(path, content, 0600); err != nil {
		return nil, fmt.Errorf("write node maintenance lease: %w", err)
	}
	return &NodeMaintenanceLeaseResponse{Acquired: true, Found: true, Lease: &lease}, nil
}

// ReadNodeMaintenanceLease returns the live maintenance lease, if any.
func ReadNodeMaintenanceLease(ctx context.Context, dataDir string) (*NodeMaintenanceLeaseResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	leaseMu.Lock()
	defer leaseMu.Unlock()
	current, err := readNodeMaintenanceLeaseLocked(dataDir, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	return &NodeMaintenanceLeaseResponse{Found: current != nil, Lease: current}, nil
}

// ReleaseNodeMaintenanceLease removes the maintenance lease held by req.ID.
func ReleaseNodeMaintenanceLease(ctx context.Context, dataDir string, req NodeMaintenanceLeaseRequest) (*NodeMaintenanceLeaseResponse, error) {
	if err := validateNodeMaintenanceLeaseRequest(req, false); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := nodeMaintenanceLeasePath(dataDir)
	if err != nil {
		return nil, err
	}
	leaseMu.Lock()
	defer leaseMu.Unlock()
	current, err := readNodeMaintenanceLeaseLocked(dataDir, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if current == nil {
		return &NodeMaintenanceLeaseResponse{Found: false}, nil
	}
	if current.ID != req.ID {
		return nil, fmt.Errorf("cannot release node maintenance lease: held by %s (operation: %s)", current.Who, current.Operation)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove node maintenance lease: %w", err)
	}
	return &NodeMaintenanceLeaseResponse{Found: false, Lease: current}, nil
}

func (s *Server) handleNodeMaintenanceLease(w http.ResponseWriter, r *http.Request) {
	var (
		response *NodeMaintenanceLeaseResponse
		err      error
	)
	switch r.Method {
	case http.MethodGet:
		response, err = ReadNodeMaintenanceLease(r.Context(), s.dataDir)
	case http.MethodPost, http.MethodDelete:
		defer r.Body.Close()
		var request NodeMaintenanceLeaseRequest
		if decodeErr := decodeJSONRequest(w, r, &request); decodeErr != nil {
			http.Error(w, "invalid JSON body: "+decodeErr.Error(), http.StatusBadRequest)
			return
		}
		if r.Method == http.MethodPost {
			response, err = AcquireNodeMaintenanceLease(r.Context(), s.dataDir, request)
		} else {
			response, err = ReleaseNodeMaintenanceLease(r.Context(), s.dataDir, request)
		}
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}
