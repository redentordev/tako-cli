package state

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	DefaultLeaseTTL = 30 * time.Minute
)

// LeaseInfo describes a remote takod deployment lease. The lease is backed by
// an atomic directory create on the state node, so separate laptops and CI jobs
// contend for the same lock.
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
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	if err := s.Initialize(); err != nil {
		return nil, err
	}

	lease := &LeaseInfo{
		ID:          fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano()),
		ProjectName: s.projectName,
		Environment: environment,
		Operation:   operation,
		Who:         currentPrincipal(),
		PID:         os.Getpid(),
		CreatedAt:   time.Now().UTC(),
		ExpiresAt:   time.Now().UTC().Add(ttl),
	}

	for attempt := 0; attempt < 2; attempt++ {
		acquired, err := s.tryCreateLeaseDir()
		if err != nil {
			return nil, err
		}
		if acquired {
			if err := s.writeLeaseInfo(lease); err != nil {
				_ = s.ReleaseLease(lease)
				return nil, err
			}
			return lease, nil
		}

		existing, err := s.readLeaseWithGrace()
		if err != nil {
			return nil, fmt.Errorf("remote lease is held but metadata could not be read: %w", err)
		}
		if existing == nil {
			return nil, fmt.Errorf("remote lease is held but metadata is missing; remove %s manually if it is stale", s.leasePath())
		}
		if time.Now().UTC().After(existing.ExpiresAt) {
			if err := s.forceRemoveLease(); err != nil {
				return nil, fmt.Errorf("failed to remove expired remote lease: %w", err)
			}
			continue
		}

		return nil, fmt.Errorf("remote state is locked by %s (operation: %s, expires in %s)",
			existing.Who,
			existing.Operation,
			time.Until(existing.ExpiresAt).Round(time.Second),
		)
	}

	return nil, fmt.Errorf("failed to acquire remote lease after removing expired lease")
}

func (s *StateManager) readLeaseWithGrace() (*LeaseInfo, error) {
	var lastErr error
	for i := 0; i < 5; i++ {
		lease, err := s.ReadLease()
		if err != nil {
			lastErr = err
		} else if lease != nil {
			return lease, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, nil
}

// ReadLease returns the currently held remote lease, or nil if none exists.
func (s *StateManager) ReadLease() (*LeaseInfo, error) {
	cmd := fmt.Sprintf("sudo cat %s 2>/dev/null || true", shellQuote(s.leaseInfoPath()))
	output, err := s.client.Execute(cmd)
	if err != nil {
		return nil, err
	}
	output = strings.TrimSpace(output)
	if output == "" {
		return nil, nil
	}

	var lease LeaseInfo
	if err := json.Unmarshal([]byte(output), &lease); err != nil {
		return nil, err
	}
	return &lease, nil
}

// ReleaseLease releases the remote lease if it is still owned by this process.
func (s *StateManager) ReleaseLease(lease *LeaseInfo) error {
	if lease == nil {
		return nil
	}

	current, err := s.ReadLease()
	if err != nil {
		return err
	}
	if current == nil {
		return nil
	}
	if current.ID != lease.ID {
		return fmt.Errorf("cannot release remote lease: held by %s (operation: %s)", current.Who, current.Operation)
	}

	cmd := fmt.Sprintf("sudo rm -rf %s", shellQuote(s.leasePath()))
	_, err = s.client.Execute(cmd)
	return err
}

func (s *StateManager) tryCreateLeaseDir() (bool, error) {
	cmd := fmt.Sprintf(
		"sudo mkdir -p %s && if sudo mkdir %s 2>/dev/null; then echo acquired; else echo held; fi",
		shellQuote(s.getStatePath()),
		shellQuote(s.leasePath()),
	)
	output, err := s.client.Execute(cmd)
	if err != nil {
		return false, fmt.Errorf("failed to create remote lease: %w", err)
	}
	return strings.Contains(output, "acquired"), nil
}

func (s *StateManager) writeLeaseInfo(lease *LeaseInfo) error {
	data, err := json.MarshalIndent(lease, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmpFile := fmt.Sprintf("/tmp/tako-lease-%s.json", lease.ID)
	encoded := base64.StdEncoding.EncodeToString(data)
	cmd := fmt.Sprintf("echo '%s' | base64 -d > %s && sudo mv %s %s && sudo chmod 644 %s",
		encoded,
		shellQuote(tmpFile),
		shellQuote(tmpFile),
		shellQuote(s.leaseInfoPath()),
		shellQuote(s.leaseInfoPath()),
	)
	if _, err := s.client.Execute(cmd); err != nil {
		_, _ = s.client.Execute("rm -f " + shellQuote(tmpFile))
		return fmt.Errorf("failed to write remote lease metadata: %w", err)
	}
	return nil
}

func (s *StateManager) forceRemoveLease() error {
	cmd := fmt.Sprintf("sudo rm -rf %s", shellQuote(s.leasePath()))
	_, err := s.client.Execute(cmd)
	return err
}

func (s *StateManager) leasePath() string {
	return fmt.Sprintf("%s/lease", s.getStatePath())
}

func (s *StateManager) leaseInfoPath() string {
	return fmt.Sprintf("%s/info.json", s.leasePath())
}

func currentPrincipal() string {
	hostname, _ := os.Hostname()
	who := GetCurrentUser()
	if hostname == "" {
		return who
	}
	return fmt.Sprintf("%s@%s", who, hostname)
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
