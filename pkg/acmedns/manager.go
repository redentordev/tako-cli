package acmedns

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

const (
	// DataDir is the directory for acme-dns data
	DataDir = "/data/tako/acme-dns"
	// ConfigDir is the directory for acme-dns config
	ConfigDir = "/data/tako/acme-dns/config"
	// ContainerName is the name of the acme-dns container
	ContainerName = "tako-acme-dns"
	// Image is the acme-dns Docker image
	Image = "joohoi/acme-dns:v1.0"
)

// Manager handles acme-dns runtime reconciliation and registrations.
type Manager struct {
	client   *ssh.Client
	serverIP string
	socket   string
	verbose  bool
}

// NewManager creates a new acme-dns manager
func NewManager(client *ssh.Client, serverIP string, socket string, verbose bool) *Manager {
	return &Manager{
		client:   client,
		serverIP: serverIP,
		socket:   socket,
		verbose:  verbose,
	}
}

// Setup ensures acme-dns container is running
func (m *Manager) Setup() error {
	if m.verbose {
		fmt.Println("  Setting up acme-dns for wildcard SSL certificates...")
	}

	if _, err := takodclient.RequestJSON(m.client, m.socket, "POST", "/v1/acme-dns", takod.ReconcileAcmeDNSRequest{
		ServerIP: m.serverIP,
		Image:    Image,
	}); err != nil {
		return fmt.Errorf("failed to reconcile acme-dns through takod: %w", err)
	}

	// Wait for container to be ready
	time.Sleep(3 * time.Second)

	if m.verbose {
		fmt.Println("  ✓ acme-dns container deployed")
	}

	return nil
}

// Register creates a new acme-dns registration for a domain
func (m *Manager) Register(baseDomain string) (*Registration, error) {
	// Check if already registered
	existing, err := m.loadRegistration(baseDomain)
	if err == nil && existing != nil {
		if m.verbose {
			fmt.Printf("  Using existing registration for %s\n", baseDomain)
		}
		return existing, nil
	}

	if m.verbose {
		fmt.Printf("  Registering %s with acme-dns...\n", baseDomain)
	}

	output, err := takodclient.RequestJSON(m.client, m.socket, "POST", "/v1/acme-dns/register", takod.AcmeDNSRegisterRequest{
		Domain:   baseDomain,
		ServerIP: m.serverIP,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to register with acme-dns through takod: %w", err)
	}
	var response takod.AcmeDNSRegisterResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("failed to parse acme-dns registration response: %w", err)
	}

	reg := &Registration{
		Domain:      baseDomain,
		Subdomain:   response.Registration.Subdomain,
		Username:    response.Registration.Username,
		Password:    response.Registration.Password,
		FullDomain:  response.Registration.FullDomain,
		CNAMETarget: response.Registration.CNAMETarget,
		ServerIP:    response.Registration.ServerIP,
		CreatedAt:   response.Registration.CreatedAt,
	}

	return reg, nil
}

// loadRegistration loads an existing registration for a domain
func (m *Manager) loadRegistration(baseDomain string) (*Registration, error) {
	output, err := m.readCredentials()
	if err != nil {
		return nil, err
	}

	var creds Credentials
	if err := json.Unmarshal([]byte(output), &creds); err != nil {
		return nil, err
	}

	reg, exists := creds.Registrations[baseDomain]
	if !exists {
		return nil, fmt.Errorf("registration not found for %s", baseDomain)
	}

	return reg, nil
}

// GetCNAMEInstructions returns formatted CNAME setup instructions
func (m *Manager) GetCNAMEInstructions(registrations []*Registration) string {
	var sb strings.Builder

	sb.WriteString("\n┌─────────────────────────────────────────────────────────────────┐\n")
	sb.WriteString("│  ADD THESE CNAME RECORDS TO YOUR DNS                           │\n")
	sb.WriteString("├─────────────────────────────────────────────────────────────────┤\n")

	for _, reg := range registrations {
		sb.WriteString(fmt.Sprintf("│                                                                 │\n"))
		sb.WriteString(fmt.Sprintf("│  Domain: *.%s\n", reg.Domain))
		sb.WriteString(fmt.Sprintf("│  ─────────────────────────────────────────────────────────────  │\n"))
		sb.WriteString(fmt.Sprintf("│  Name:   _acme-challenge.%s\n", reg.Domain))
		sb.WriteString(fmt.Sprintf("│  Type:   CNAME\n"))
		sb.WriteString(fmt.Sprintf("│  Target: %s\n", reg.CNAMETarget))
		sb.WriteString(fmt.Sprintf("│                                                                 │\n"))
	}

	sb.WriteString("└─────────────────────────────────────────────────────────────────┘\n")

	return sb.String()
}

// LoadAllRegistrations loads all saved registrations
func (m *Manager) LoadAllRegistrations() (map[string]*Registration, error) {
	output, err := m.readCredentials()
	if err != nil || strings.TrimSpace(output) == "" {
		return make(map[string]*Registration), nil
	}

	var creds Credentials
	if err := json.Unmarshal([]byte(output), &creds); err != nil {
		return nil, err
	}

	if creds.Registrations == nil {
		return make(map[string]*Registration), nil
	}

	return creds.Registrations, nil
}

// Stop stops the acme-dns container
func (m *Manager) Stop() error {
	return m.Remove()
}

// Remove removes the acme-dns container
func (m *Manager) Remove() error {
	if _, err := takodclient.RequestJSON(m.client, m.socket, "DELETE", "/v1/acme-dns", nil); err != nil {
		return fmt.Errorf("failed to remove acme-dns through takod: %w", err)
	}
	return nil
}

func (m *Manager) readCredentials() (string, error) {
	output, err := takodclient.RequestJSON(m.client, m.socket, "GET", "/v1/acme-dns/credentials", nil)
	if err != nil {
		return "", err
	}
	var response takod.AcmeDNSCredentialsResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return "", fmt.Errorf("failed to parse acme-dns credentials response: %w", err)
	}
	return response.Content, nil
}
