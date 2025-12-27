package acmedns

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/ssh"
)

const (
	// DataDir is the directory for acme-dns data
	DataDir = "/data/tako/acme-dns"
	// ConfigDir is the directory for acme-dns config
	ConfigDir = "/data/tako/acme-dns/config"
	// ContainerName is the name of the acme-dns container
	ContainerName = "tako-acme-dns"
	// Image is the acme-dns Docker image
	Image = "joohoi/acme-dns:latest"
)

// Manager handles acme-dns container lifecycle and registrations
type Manager struct {
	client   *ssh.Client
	serverIP string
	verbose  bool
}

// NewManager creates a new acme-dns manager
func NewManager(client *ssh.Client, serverIP string, verbose bool) *Manager {
	return &Manager{
		client:   client,
		serverIP: serverIP,
		verbose:  verbose,
	}
}

// Setup ensures acme-dns container is running
func (m *Manager) Setup() error {
	// Check if container already exists and is running
	running, err := m.isContainerRunning()
	if err != nil {
		return fmt.Errorf("failed to check container status: %w", err)
	}

	if running {
		if m.verbose {
			fmt.Println("  acme-dns container already running")
		}
		return nil
	}

	if m.verbose {
		fmt.Println("  Setting up acme-dns for wildcard SSL certificates...")
	}

	// Create directories
	if err := m.createDirectories(); err != nil {
		return err
	}

	// Create configuration
	if err := m.createConfig(); err != nil {
		return err
	}

	// Deploy container
	if err := m.deployContainer(); err != nil {
		return err
	}

	// Wait for container to be ready
	time.Sleep(3 * time.Second)

	if m.verbose {
		fmt.Println("  ✓ acme-dns container deployed")
	}

	return nil
}

// isContainerRunning checks if the acme-dns container is running
func (m *Manager) isContainerRunning() (bool, error) {
	cmd := fmt.Sprintf("docker ps --filter name=%s --format '{{.Names}}'", ContainerName)
	output, err := m.client.Execute(cmd)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(output) == ContainerName, nil
}

// createDirectories creates necessary directories for acme-dns
func (m *Manager) createDirectories() error {
	dirs := []string{DataDir, ConfigDir, DataDir + "/data"}
	for _, dir := range dirs {
		cmd := fmt.Sprintf("sudo mkdir -p %s && sudo chmod 755 %s", dir, dir)
		if _, err := m.client.Execute(cmd); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}
	return nil
}

// createConfig creates the acme-dns configuration file
func (m *Manager) createConfig() error {
	// acme-dns config in TOML format
	config := fmt.Sprintf(`[general]
listen = "0.0.0.0:53"
protocol = "both"
domain = "acme.tako"
nsname = "acme.tako"
nsadmin = "admin.tako"
records = [
    "acme.tako. A %s",
    "acme.tako. NS acme.tako."
]
debug = false

[database]
engine = "sqlite3"
connection = "/var/lib/acme-dns/acme-dns.db"

[api]
ip = "0.0.0.0"
port = "80"
tls = "none"
corsorigins = ["*"]
use_header = false

[logconfig]
loglevel = "info"
logtype = "stdout"
logformat = "text"
`, m.serverIP)

	// Write config file
	cmd := fmt.Sprintf("echo '%s' | sudo tee %s/config.cfg > /dev/null",
		strings.ReplaceAll(config, "'", "'\\''"),
		ConfigDir)

	if _, err := m.client.Execute(cmd); err != nil {
		return fmt.Errorf("failed to write acme-dns config: %w", err)
	}

	return nil
}

// deployContainer deploys the acme-dns Docker container
func (m *Manager) deployContainer() error {
	// Remove existing container if any
	m.client.Execute(fmt.Sprintf("docker rm -f %s 2>/dev/null", ContainerName))

	// Deploy new container
	cmd := fmt.Sprintf(`docker run -d \
		--name %s \
		--restart unless-stopped \
		-p 53:53/udp \
		-p 53:53/tcp \
		-p 8053:80 \
		-v %s/config:/etc/acme-dns:ro \
		-v %s/data:/var/lib/acme-dns \
		%s`,
		ContainerName,
		DataDir,
		DataDir,
		Image)

	output, err := m.client.Execute(cmd)
	if err != nil {
		return fmt.Errorf("failed to deploy acme-dns container: %w, output: %s", err, output)
	}

	return nil
}

// CheckPort53 verifies port 53 is accessible from the internet
func (m *Manager) CheckPort53() error {
	// Check if port 53 is listening locally
	cmd := "ss -uln | grep ':53 ' || netstat -uln | grep ':53 ' 2>/dev/null"
	output, err := m.client.Execute(cmd)
	if err != nil || strings.TrimSpace(output) == "" {
		return fmt.Errorf("port 53 is not listening")
	}

	// We can't easily verify external access from inside the server
	// The actual verification will happen when Let's Encrypt tries to query
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

	// Call acme-dns register endpoint
	cmd := "curl -s -X POST http://localhost:8053/register"
	output, err := m.client.Execute(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to register with acme-dns: %w", err)
	}

	// Parse response
	var response struct {
		Subdomain  string `json:"subdomain"`
		Username   string `json:"username"`
		Password   string `json:"password"`
		FullDomain string `json:"fulldomain"`
	}

	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("failed to parse acme-dns response: %w, output: %s", err, output)
	}

	// Build registration
	reg := &Registration{
		Domain:      baseDomain,
		Subdomain:   response.Subdomain,
		Username:    response.Username,
		Password:    response.Password,
		FullDomain:  response.FullDomain,
		CNAMETarget: response.FullDomain,
		ServerIP:    m.serverIP,
		CreatedAt:   time.Now(),
	}

	// Save registration
	if err := m.saveRegistration(reg); err != nil {
		return nil, fmt.Errorf("failed to save registration: %w", err)
	}

	return reg, nil
}

// loadRegistration loads an existing registration for a domain
func (m *Manager) loadRegistration(baseDomain string) (*Registration, error) {
	credPath := DataDir + "/credentials.json"
	cmd := fmt.Sprintf("cat %s 2>/dev/null", credPath)
	output, err := m.client.Execute(cmd)
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

// saveRegistration saves a registration to the credentials file
func (m *Manager) saveRegistration(reg *Registration) error {
	credPath := DataDir + "/credentials.json"

	// Load existing credentials
	var creds Credentials
	cmd := fmt.Sprintf("cat %s 2>/dev/null", credPath)
	output, _ := m.client.Execute(cmd)
	if output != "" {
		// Try to parse existing credentials, but don't fail if the file is corrupted
		// We'll just start fresh with an empty credentials object
		if err := json.Unmarshal([]byte(output), &creds); err != nil {
			if m.verbose {
				fmt.Printf("  Warning: existing credentials file is corrupted, starting fresh: %v\n", err)
			}
			creds = Credentials{}
		}
	}

	if creds.Registrations == nil {
		creds.Registrations = make(map[string]*Registration)
	}
	creds.Registrations[reg.Domain] = reg

	// Save credentials
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal credentials: %w", err)
	}

	cmd = fmt.Sprintf("echo '%s' | sudo tee %s > /dev/null",
		strings.ReplaceAll(string(data), "'", "'\\''"),
		credPath)

	if _, err := m.client.Execute(cmd); err != nil {
		return fmt.Errorf("failed to save credentials: %w", err)
	}

	return nil
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
	credPath := DataDir + "/credentials.json"
	cmd := fmt.Sprintf("cat %s 2>/dev/null", credPath)
	output, err := m.client.Execute(cmd)
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
	cmd := fmt.Sprintf("docker stop %s 2>/dev/null", ContainerName)
	if _, err := m.client.Execute(cmd); err != nil {
		// Ignore "no such container" errors, but return other errors
		if !strings.Contains(err.Error(), "No such container") {
			return fmt.Errorf("failed to stop acme-dns container: %w", err)
		}
	}
	return nil
}

// Remove removes the acme-dns container
func (m *Manager) Remove() error {
	cmd := fmt.Sprintf("docker rm -f %s 2>/dev/null", ContainerName)
	if _, err := m.client.Execute(cmd); err != nil {
		// Ignore "no such container" errors, but return other errors
		if !strings.Contains(err.Error(), "No such container") {
			return fmt.Errorf("failed to remove acme-dns container: %w", err)
		}
	}
	return nil
}
