package state

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

const DefaultLeaseTTL = 30 * time.Minute
const leaseReleaseTimeout = 10 * time.Second

// ErrLeaseLost means a renewal could not prove continued ownership of the
// existing, unexpired holder token.
var ErrLeaseLost = errors.New("remote lease lost")

// ErrLeaseRenewalUnsupported means the contacted takod predates explicit
// renewal. The original lease remains valid until its confirmed expiry, which
// gives normal deploy setup a bounded window to upgrade the agent in place.
var ErrLeaseRenewalUnsupported = errors.New("takod does not support explicit lease renewal; upgrade takod")

// LeaseInfo describes a remote takod deployment lease.
type LeaseInfo struct {
	ID          string                       `json:"id"`
	ProjectName string                       `json:"projectName"`
	Environment string                       `json:"environment"`
	Operation   string                       `json:"operation"`
	Who         string                       `json:"who"`
	PID         int                          `json:"pid"`
	CreatedAt   time.Time                    `json:"createdAt"`
	ExpiresAt   time.Time                    `json:"expiresAt"`
	Fence       *nodeidentity.OperationFence `json:"fence,omitempty"`
	HolderToken string                       `json:"-"`
}

// AcquireLease acquires the remote deployment lease for this project.
func (s *StateManager) AcquireLease(operation, environment string, ttl time.Duration) (*LeaseInfo, error) {
	return s.AcquireLeaseContext(context.Background(), operation, environment, ttl)
}

// AcquireLeaseContext acquires the remote deployment lease bounded by ctx.
func (s *StateManager) AcquireLeaseContext(ctx context.Context, operation, environment string, ttl time.Duration) (*LeaseInfo, error) {
	return s.AcquireControllerLeaseContext(ctx, operation, environment, ttl, nil)
}

// AcquireControllerLeaseContext requests controller-authoritative operation
// identity and target fencing. Legacy takod ignores the additive fields and
// continues to use the client-generated ID.
func (s *StateManager) AcquireControllerLeaseContext(ctx context.Context, operation, environment string, ttl time.Duration, targetNodeIDs []string) (*LeaseInfo, error) {
	if environment == "" {
		environment = s.environment
	}
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}

	requestID, err := newLeaseRequestID()
	if err != nil {
		return nil, fmt.Errorf("create remote lease request identity: %w", err)
	}
	request := takod.LeaseRequest{
		Project:       s.projectName,
		Environment:   environment,
		ID:            requestID,
		RequestID:     requestID,
		TargetNodeIDs: append([]string(nil), targetNodeIDs...),
		Operation:     operation,
		Who:           currentPrincipal(),
		PID:           os.Getpid(),
		TTLSeconds:    int64(ttl.Seconds()),
	}
	output, err := s.requestJSONContext(ctx, "POST", "/v1/lease", request)
	if err != nil && retryUncertainLeaseAcquire(ctx, err) {
		// Reuse the exact cryptographically random RequestID once. If the first
		// response was lost after commit, takod returns the original signed fence
		// and private holder credential instead of creating a competing writer.
		output, err = s.requestJSONContext(ctx, "POST", "/v1/lease", request)
	}
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
	return leaseFromTakod(response.Lease, response.HolderToken), nil
}

func retryUncertainLeaseAcquire(ctx context.Context, err error) bool {
	if err == nil || ctx == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var httpErr *takodclient.HTTPError
	return !errors.As(err, &httpErr)
}

func newLeaseRequestID() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return "req-" + hex.EncodeToString(value), nil
}

// RenewLease extends a lease still held by the same ID token.
func (s *StateManager) RenewLease(lease *LeaseInfo, ttl time.Duration) (*LeaseInfo, error) {
	return s.RenewLeaseContext(context.Background(), lease, ttl)
}

// RenewLeaseContext extends a lease still held by the same ID token, bounded
// by ctx. Takod preserves the original holder metadata and creation time.
func (s *StateManager) RenewLeaseContext(ctx context.Context, lease *LeaseInfo, ttl time.Duration) (*LeaseInfo, error) {
	if lease == nil || lease.ID == "" {
		return nil, fmt.Errorf("lease is required")
	}
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	environment := lease.Environment
	if environment == "" {
		environment = s.environment
	}
	request := takod.LeaseRequest{
		Project:     s.projectName,
		Environment: environment,
		ID:          lease.ID,
		Operation:   lease.Operation,
		Who:         lease.Who,
		PID:         lease.PID,
		TTLSeconds:  int64(ttl.Seconds()),
		Renew:       true,
		Fence:       lease.Fence,
		HolderToken: lease.HolderToken,
	}
	output, err := s.requestJSONContext(ctx, "POST", "/v1/lease", request)
	if err != nil && retryUncertainLeaseAcquire(ctx, err) {
		// The controller accepts the immediate predecessor fence from this
		// authenticated holder and returns any already-committed renewal exactly.
		output, err = s.requestJSONContext(ctx, "POST", "/v1/lease", request)
	}
	if err != nil {
		return nil, err
	}
	response, err := decodeLeaseResponse(output)
	if err != nil {
		return nil, err
	}
	if !response.Acquired || response.Lease == nil || response.Lease.ID != lease.ID {
		if response.Lease != nil {
			if response.Lease.ID == lease.ID && response.Message == "lease is held" {
				return nil, fmt.Errorf("%w", ErrLeaseRenewalUnsupported)
			}
			return nil, fmt.Errorf("%w: %s (requested %s, current %s)", ErrLeaseLost, response.Message, lease.ID, response.Lease.ID)
		}
		return nil, fmt.Errorf("%w: %s", ErrLeaseLost, response.Message)
	}
	return leaseFromTakod(response.Lease, response.HolderToken), nil
}

// ReadLease returns the currently held remote lease, or nil if none exists.
func (s *StateManager) ReadLease() (*LeaseInfo, error) {
	return s.ReadLeaseContext(context.Background())
}

// ReadLeaseContext returns the currently held remote lease bounded by ctx, or nil if none exists.
func (s *StateManager) ReadLeaseContext(ctx context.Context) (*LeaseInfo, error) {
	output, err := s.requestJSONContext(ctx, "GET", takodclient.LeaseEndpoint(s.projectName, s.environment), nil)
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
	return leaseFromTakod(response.Lease, ""), nil
}

// ReleaseLease releases the remote lease if it is still owned by this process.
func (s *StateManager) ReleaseLease(lease *LeaseInfo) error {
	return s.ReleaseLeaseContext(context.Background(), lease)
}

// ReleaseLeaseContext releases the remote lease if it is still owned by this process, bounded by ctx and the cleanup timeout.
func (s *StateManager) ReleaseLeaseContext(ctx context.Context, lease *LeaseInfo) error {
	if lease == nil {
		return nil
	}
	request := takod.LeaseRequest{
		Project:     s.projectName,
		Environment: lease.Environment,
		ID:          lease.ID,
		Fence:       lease.Fence,
		HolderToken: lease.HolderToken,
	}
	_, err := s.requestJSONWithTimeoutContext(ctx, "DELETE", "/v1/lease", request, leaseReleaseTimeout)
	return err
}

func decodeLeaseResponse(output string) (*takod.LeaseResponse, error) {
	var response takod.LeaseResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("failed to parse takod lease response: %w", err)
	}
	return &response, nil
}

func leaseFromTakod(lease *takod.LeaseInfo, holderToken string) *LeaseInfo {
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
		Fence:       lease.Fence,
		HolderToken: holderToken,
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
