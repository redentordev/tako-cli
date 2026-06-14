package takod

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	defaultLeaseTTL = 30 * time.Minute
	maxLeaseTTL     = 24 * time.Hour
)

type LeaseInfo struct {
	ID          string    `json:"id"`
	ProjectName string    `json:"projectName"`
	Environment string    `json:"environment"`
	Operation   string    `json:"operation"`
	Who         string    `json:"who"`
	PID         int       `json:"pid"`
	CreatedAt   time.Time `json:"createdAt"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

type LeaseRequest struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	ID          string `json:"id,omitempty"`
	Operation   string `json:"operation,omitempty"`
	Who         string `json:"who,omitempty"`
	PID         int    `json:"pid,omitempty"`
	TTLSeconds  int64  `json:"ttlSeconds,omitempty"`
}

type LeaseResponse struct {
	Acquired bool       `json:"acquired"`
	Found    bool       `json:"found"`
	Lease    *LeaseInfo `json:"lease,omitempty"`
	Message  string     `json:"message,omitempty"`
}

func AcquireLease(ctx context.Context, dataDir string, req LeaseRequest) (*LeaseResponse, error) {
	if err := validateLeaseRequest(req, true); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	ttl := time.Duration(req.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = defaultLeaseTTL
	}
	now := time.Now().UTC()
	lease := &LeaseInfo{
		ID:          req.ID,
		ProjectName: req.Project,
		Environment: req.Environment,
		Operation:   req.Operation,
		Who:         req.Who,
		PID:         req.PID,
		CreatedAt:   now,
		ExpiresAt:   now.Add(ttl),
	}

	path, err := leasePath(dataDir, req.Project, req.Environment)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("failed to create lease directory: %w", err)
	}

	content, err := json.MarshalIndent(lease, "", "  ")
	if err != nil {
		return nil, err
	}
	content = append(content, '\n')

	for attempt := 0; attempt < 2; attempt++ {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err == nil {
			if _, writeErr := file.Write(content); writeErr != nil {
				_ = file.Close()
				_ = os.Remove(path)
				return nil, fmt.Errorf("failed to write lease: %w", writeErr)
			}
			if closeErr := file.Close(); closeErr != nil {
				_ = os.Remove(path)
				return nil, fmt.Errorf("failed to close lease: %w", closeErr)
			}
			return &LeaseResponse{Acquired: true, Found: true, Lease: lease}, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("failed to create lease: %w", err)
		}

		current, err := readLeaseFile(path)
		if err != nil {
			return nil, fmt.Errorf("lease is held but metadata could not be read: %w", err)
		}
		if current == nil {
			continue
		}
		if now.After(current.ExpiresAt) {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("failed to remove expired lease: %w", err)
			}
			continue
		}
		return &LeaseResponse{
			Acquired: false,
			Found:    true,
			Lease:    current,
			Message:  "lease is held",
		}, nil
	}

	return nil, fmt.Errorf("failed to acquire lease after removing expired lease")
}

func ReadLease(ctx context.Context, dataDir string, req LeaseRequest) (*LeaseResponse, error) {
	if err := validateLeaseRequest(req, false); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := leasePath(dataDir, req.Project, req.Environment)
	if err != nil {
		return nil, err
	}
	lease, err := readLeaseFile(path)
	if err != nil {
		return nil, err
	}
	if lease == nil {
		return &LeaseResponse{Found: false}, nil
	}
	if time.Now().UTC().After(lease.ExpiresAt) {
		_ = os.Remove(path)
		return &LeaseResponse{Found: false}, nil
	}
	return &LeaseResponse{Found: true, Lease: lease}, nil
}

func ReleaseLease(ctx context.Context, dataDir string, req LeaseRequest) (*LeaseResponse, error) {
	if err := validateLeaseRequest(req, false); err != nil {
		return nil, err
	}
	if req.ID == "" {
		return nil, fmt.Errorf("lease ID is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := leasePath(dataDir, req.Project, req.Environment)
	if err != nil {
		return nil, err
	}
	current, err := readLeaseFile(path)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return &LeaseResponse{Found: false}, nil
	}
	if current.ID != req.ID {
		return nil, fmt.Errorf("cannot release lease: held by %s (operation: %s)", current.Who, current.Operation)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to remove lease: %w", err)
	}
	return &LeaseResponse{Found: false, Lease: current}, nil
}

func readLeaseFile(path string) (*LeaseInfo, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var lease LeaseInfo
	if err := json.Unmarshal(data, &lease); err != nil {
		return nil, err
	}
	return &lease, nil
}

func validateLeaseRequest(req LeaseRequest, requireAcquireFields bool) error {
	if !isSafeProjectName(req.Project) {
		return fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(req.Environment) {
		return fmt.Errorf("invalid environment name")
	}
	if requireAcquireFields {
		if req.ID == "" || !isSafeStateRevisionID(req.ID) {
			return fmt.Errorf("invalid lease ID")
		}
		if req.Operation == "" || !isSafeStateRevisionID(req.Operation) {
			return fmt.Errorf("invalid lease operation")
		}
		if req.Who == "" || len(req.Who) > 256 {
			return fmt.Errorf("invalid lease owner")
		}
		if hasControlChars(req.Who) {
			return fmt.Errorf("invalid lease owner")
		}
		if req.PID < 0 {
			return fmt.Errorf("invalid lease PID")
		}
		if req.TTLSeconds < 0 || req.TTLSeconds > int64(maxLeaseTTL/time.Second) {
			return fmt.Errorf("invalid lease TTL")
		}
	}
	return nil
}

func leasePath(dataDir string, project string, environment string) (string, error) {
	if dataDir == "" {
		return "", fmt.Errorf("data directory is required")
	}
	return filepath.Join(dataDir, "leases", project, environment+".json"), nil
}
