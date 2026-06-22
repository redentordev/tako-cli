package state

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

const DefaultLeaseTTL = 30 * time.Minute
const leaseReleaseTimeout = 10 * time.Second

// LeaseInfo describes a remote takod deployment lease.
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

// AcquireLease acquires the remote deployment lease for this project.
func (s *StateManager) AcquireLease(operation, environment string, ttl time.Duration) (*LeaseInfo, error) {
	if environment == "" {
		environment = s.environment
	}
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}

	request := takod.LeaseRequest{
		Project:     s.projectName,
		Environment: environment,
		ID:          fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano()),
		Operation:   operation,
		Who:         currentPrincipal(),
		PID:         os.Getpid(),
		TTLSeconds:  int64(ttl.Seconds()),
	}
	output, err := s.requestJSON("POST", "/v1/lease", request)
	if err != nil {
		return nil, err
	}

	response, err := decodeLeaseResponse(output)
	if err != nil {
		return nil, err
	}
	if !response.Acquired {
		if response.Lease == nil {
			return nil, fmt.Errorf("remote state is locked but lease metadata is missing")
		}
		return nil, fmt.Errorf("remote state is locked by %s (operation: %s, expires in %s)",
			response.Lease.Who,
			response.Lease.Operation,
			time.Until(response.Lease.ExpiresAt).Round(time.Second),
		)
	}
	if response.Lease == nil {
		return nil, fmt.Errorf("remote lease was acquired but metadata is missing")
	}
	return leaseFromTakod(response.Lease), nil
}

// ReadLease returns the currently held remote lease, or nil if none exists.
func (s *StateManager) ReadLease() (*LeaseInfo, error) {
	output, err := s.requestJSON("GET", takodclient.LeaseEndpoint(s.projectName, s.environment), nil)
	if err != nil {
		return nil, err
	}
	response, err := decodeLeaseResponse(output)
	if err != nil {
		return nil, err
	}
	if !response.Found || response.Lease == nil {
		return nil, nil
	}
	return leaseFromTakod(response.Lease), nil
}

// ReleaseLease releases the remote lease if it is still owned by this process.
func (s *StateManager) ReleaseLease(lease *LeaseInfo) error {
	if lease == nil {
		return nil
	}
	request := takod.LeaseRequest{
		Project:     s.projectName,
		Environment: lease.Environment,
		ID:          lease.ID,
	}
	_, err := s.requestJSONWithTimeout("DELETE", "/v1/lease", request, leaseReleaseTimeout)
	return err
}

func decodeLeaseResponse(output string) (*takod.LeaseResponse, error) {
	var response takod.LeaseResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("failed to parse takod lease response: %w", err)
	}
	return &response, nil
}

func leaseFromTakod(lease *takod.LeaseInfo) *LeaseInfo {
	if lease == nil {
		return nil
	}
	return &LeaseInfo{
		ID:          lease.ID,
		ProjectName: lease.ProjectName,
		Environment: lease.Environment,
		Operation:   lease.Operation,
		Who:         lease.Who,
		PID:         lease.PID,
		CreatedAt:   lease.CreatedAt,
		ExpiresAt:   lease.ExpiresAt,
	}
}

func currentPrincipal() string {
	hostname, _ := os.Hostname()
	who := GetCurrentUser()
	if hostname == "" {
		return who
	}
	return fmt.Sprintf("%s@%s", who, hostname)
}
