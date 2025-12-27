package ssl

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/acmedns"
	"github.com/redentordev/tako-cli/pkg/notification"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

const (
	// PendingStateFile is where pending certificates are stored
	PendingStateFile = "/data/tako/ssl/pending.json"
	// CheckInterval is how often to check DNS propagation
	CheckInterval = 30 * time.Second
)

// PendingState represents the state of pending certificates
type PendingState struct {
	Certificates []acmedns.PendingCertificate `json:"certificates"`
	UpdatedAt    time.Time                    `json:"updated_at"`
}

// Monitor handles background SSL certificate monitoring
type Monitor struct {
	client       *ssh.Client
	projectName  string
	environment  string
	notifier     *notification.Notifier
	verbose      bool
}

// NewMonitor creates a new SSL monitor
func NewMonitor(client *ssh.Client, projectName, environment string, notifier *notification.Notifier, verbose bool) *Monitor {
	return &Monitor{
		client:      client,
		projectName: projectName,
		environment: environment,
		notifier:    notifier,
		verbose:     verbose,
	}
}

// AddPending adds a pending certificate to the monitoring list
func (m *Monitor) AddPending(pending acmedns.PendingCertificate) error {
	state, err := m.LoadState()
	if err != nil {
		state = &PendingState{}
	}

	// Check if already exists
	for i, existing := range state.Certificates {
		if existing.Domain == pending.Domain {
			// Update existing
			state.Certificates[i] = pending
			return m.SaveState(state)
		}
	}

	// Add new
	state.Certificates = append(state.Certificates, pending)
	return m.SaveState(state)
}

// LoadState loads the pending certificates state
func (m *Monitor) LoadState() (*PendingState, error) {
	cmd := fmt.Sprintf("cat %s 2>/dev/null", PendingStateFile)
	output, err := m.client.Execute(cmd)
	if err != nil || strings.TrimSpace(output) == "" {
		return &PendingState{}, nil
	}

	var state PendingState
	if err := json.Unmarshal([]byte(output), &state); err != nil {
		return nil, err
	}

	return &state, nil
}

// SaveState saves the pending certificates state
func (m *Monitor) SaveState(state *PendingState) error {
	state.UpdatedAt = time.Now()

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	// Ensure directory exists
	m.client.Execute("mkdir -p /data/tako/ssl")

	cmd := fmt.Sprintf("echo '%s' > %s",
		strings.ReplaceAll(string(data), "'", "'\\''"),
		PendingStateFile)

	_, err = m.client.Execute(cmd)
	return err
}

// CheckPending checks all pending certificates and issues them if DNS is ready
func (m *Monitor) CheckPending() (issued []string, stillPending []string, errors []error) {
	state, err := m.LoadState()
	if err != nil {
		errors = append(errors, err)
		return
	}

	if len(state.Certificates) == 0 {
		return
	}

	checker := NewDNSChecker()
	var remaining []acmedns.PendingCertificate

	for _, pending := range state.Certificates {
		pending.LastCheck = time.Now()
		pending.Attempts++

		// Check DNS propagation
		verified, err := checker.CheckCNAME(pending.Domain, pending.Registration.CNAMETarget)
		if err != nil {
			if m.verbose {
				fmt.Printf("  DNS check error for %s: %v\n", pending.Domain, err)
			}
			remaining = append(remaining, pending)
			stillPending = append(stillPending, pending.Domain)
			continue
		}

		if verified {
			// DNS is ready, certificate will be issued by Traefik automatically
			if m.verbose {
				fmt.Printf("  ✓ DNS verified for %s\n", pending.Domain)
			}
			issued = append(issued, pending.Domain)

			// Send notification
			if m.notifier != nil {
				m.notifier.Notify(notification.SSLIssuedEvent(
					m.projectName,
					m.environment,
					pending.Domain,
					time.Since(pending.StartedAt),
				))
			}
		} else {
			remaining = append(remaining, pending)
			stillPending = append(stillPending, pending.Domain)
		}
	}

	// Update state with remaining pending certificates
	state.Certificates = remaining
	if err := m.SaveState(state); err != nil {
		errors = append(errors, fmt.Errorf("failed to save state: %w", err))
	}

	return
}

// GetStatus returns the current status of all pending certificates
func (m *Monitor) GetStatus() ([]CertificateStatus, error) {
	state, err := m.LoadState()
	if err != nil {
		return nil, err
	}

	var statuses []CertificateStatus
	checker := NewDNSChecker()

	for _, pending := range state.Certificates {
		status := CertificateStatus{
			Domain:      pending.Domain,
			Status:      "pending",
			CNAMETarget: pending.Registration.CNAMETarget,
			StartedAt:   pending.StartedAt,
			LastCheck:   pending.LastCheck,
			Attempts:    pending.Attempts,
		}

		// Check current DNS status
		verified, _ := checker.CheckCNAME(pending.Domain, pending.Registration.CNAMETarget)
		if verified {
			status.Status = "dns_verified"
			status.DNSVerified = true
		}

		statuses = append(statuses, status)
	}

	return statuses, nil
}

// CertificateStatus represents the status of a certificate
type CertificateStatus struct {
	Domain      string    `json:"domain"`
	Status      string    `json:"status"` // "pending", "dns_verified", "issued", "failed"
	DNSVerified bool      `json:"dns_verified"`
	CNAMETarget string    `json:"cname_target"`
	StartedAt   time.Time `json:"started_at"`
	LastCheck   time.Time `json:"last_check"`
	Attempts    int       `json:"attempts"`
	Error       string    `json:"error,omitempty"`
}

// RemovePending removes a domain from the pending list
func (m *Monitor) RemovePending(domain string) error {
	state, err := m.LoadState()
	if err != nil {
		return err
	}

	var remaining []acmedns.PendingCertificate
	for _, pending := range state.Certificates {
		if pending.Domain != domain {
			remaining = append(remaining, pending)
		}
	}

	state.Certificates = remaining
	return m.SaveState(state)
}

// HasPending returns true if there are pending certificates
func (m *Monitor) HasPending() bool {
	state, err := m.LoadState()
	if err != nil {
		return false
	}
	return len(state.Certificates) > 0
}
