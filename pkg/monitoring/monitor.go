package monitoring

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/verification"
)

// Monitor handles continuous service monitoring
type Monitor struct {
	config   *config.Config
	sshPool  *ssh.Pool
	verbose  bool
	envName  string
	services map[string]config.ServiceConfig
}

// WebhookPayload represents the structure sent to webhook endpoints
type WebhookPayload struct {
	Event     string                 `json:"event"`
	Timestamp time.Time              `json:"timestamp"`
	Project   string                 `json:"project"`
	Service   string                 `json:"service"`
	Server    string                 `json:"server"`
	Details   map[string]interface{} `json:"details"`
	Severity  string                 `json:"severity"`
}

// NewMonitor creates a new service monitor
func NewMonitor(cfg *config.Config, sshPool *ssh.Pool, verbose bool) *Monitor {
	return &Monitor{
		config:  cfg,
		sshPool: sshPool,
		verbose: verbose,
	}
}

// Start begins monitoring all enabled services
func (m *Monitor) Start(envName string) error {
	fmt.Printf("🔍 Starting service monitoring for environment: %s...\n\n", envName)

	// Get services for the environment
	services, err := m.config.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services: %w", err)
	}

	// Store for use in monitoring loop
	m.envName = envName
	m.services = services

	// Show monitoring configuration
	monitoredCount := 0
	for serviceName, service := range services {
		if service.Monitoring != nil && service.Monitoring.Enabled {
			monitoredCount++
			fmt.Printf("  ✓ Monitoring %s (%s check every %s)\n",
				serviceName,
				service.Monitoring.CheckType,
				service.Monitoring.Interval,
			)
		}
	}

	if monitoredCount == 0 {
		return fmt.Errorf("no services have monitoring enabled")
	}

	fmt.Printf("\n✨ Monitoring %d service(s)\n", monitoredCount)
	fmt.Println("Press Ctrl+C to stop monitoring")

	// Start monitoring loop
	ticker := time.NewTicker(10 * time.Second) // Check every 10 seconds
	defer ticker.Stop()

	// Run first check immediately
	m.checkAllServices()

	for range ticker.C {
		m.checkAllServices()
	}

	return nil
}

// CheckOnce runs a single monitoring check on all services
func (m *Monitor) CheckOnce(envName string) error {
	fmt.Printf("🔍 Running single monitoring check for environment: %s...\n\n", envName)

	// Get services for the environment
	services, err := m.config.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services: %w", err)
	}

	// Store for use in check
	m.envName = envName
	m.services = services

	m.checkAllServices()

	fmt.Println("\n✓ Check complete")
	return nil
}

// checkAllServices checks all monitored services
func (m *Monitor) checkAllServices() {
	now := time.Now()

	for serviceName, service := range m.services {
		// Skip if monitoring not enabled
		if service.Monitoring == nil || !service.Monitoring.Enabled {
			continue
		}

		if m.verbose {
			fmt.Printf("[%s] Checking %s...\n", now.Format("15:04:05"), serviceName)
		}

		// Check the service
		if err := m.checkService(serviceName, &service); err != nil {
			fmt.Printf("[%s] ✗ %s: %v\n", now.Format("15:04:05"), serviceName, err)

			// Send webhook alert
			m.sendWebhook(serviceName, &service, "prod", err.Error(), "critical")
		} else if m.verbose {
			fmt.Printf("[%s] ✓ %s: healthy\n", now.Format("15:04:05"), serviceName)
		}
	}
}

// checkService checks a single service based on its monitoring config
func (m *Monitor) checkService(serviceName string, service *config.ServiceConfig) error {
	if service.Monitoring.CheckType == "http" {
		return m.checkPublicService(serviceName, service)
	}

	// Default: check containers
	return m.checkInternalService(serviceName, service)
}

// checkPublicService checks a public service via HTTP
func (m *Monitor) checkPublicService(serviceName string, service *config.ServiceConfig) error {
	if service.Proxy == nil || len(service.Proxy.Domains) == 0 {
		return fmt.Errorf("service has no domains configured")
	}

	domain := service.Proxy.Domains[0]
	path := service.HealthCheck.Path
	if path == "" {
		path = "/"
	}

	url := fmt.Sprintf("https://%s%s", domain, path)

	// Make HTTP request
	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("health check returned status %d", resp.StatusCode)
	}

	return nil
}

// checkInternalService checks internal service by verifying containers
func (m *Monitor) checkInternalService(serviceName string, service *config.ServiceConfig) error {
	// Connect to servers and check containers
	for serverName, serverCfg := range m.config.Servers {
		client, err := m.sshPool.GetOrCreateWithAuth(serverCfg.Host, serverCfg.Port, serverCfg.User, serverCfg.SSHKey, serverCfg.Password)
		if err != nil {
			return fmt.Errorf("failed to connect to %s: %w", serverName, err)
		}

		// Check each replica
		failures := 0
		for replica := 1; replica <= service.Replicas; replica++ {
			containerName := fmt.Sprintf("%s_%s_%d", m.config.Project.Name, serviceName, replica)

			// Create verifier
			verifier := verification.NewVerifier(client, false)

			// Check container health
			if err := verifier.CheckContainerHealth(containerName, service); err != nil {
				failures++
				if m.verbose {
					fmt.Printf("    Replica %d/%d failed: %v\n", replica, service.Replicas, err)
				}
			}
		}

		if failures > 0 {
			return fmt.Errorf("%d/%d replicas unhealthy", failures, service.Replicas)
		}
	}

	return nil
}

// sendWebhook sends a webhook notification
func (m *Monitor) sendWebhook(serviceName string, service *config.ServiceConfig, serverName string, errorMsg string, severity string) {
	webhook := service.Monitoring.Webhook
	if webhook == "" {
		if m.verbose {
			fmt.Printf("  No webhook configured for %s\n", serviceName)
		}
		return
	}

	payload := WebhookPayload{
		Event:     "service_down",
		Timestamp: time.Now(),
		Project:   m.config.Project.Name,
		Service:   serviceName,
		Server:    serverName,
		Details: map[string]interface{}{
			"error":          errorMsg,
			"replicas_total": service.Replicas,
			"check_type":     service.Monitoring.CheckType,
		},
		Severity: severity,
	}

	// Marshal to JSON
	jsonData, err := json.Marshal(payload)
	if err != nil {
		if m.verbose {
			fmt.Printf("  Failed to marshal webhook payload: %v\n", err)
		}
		return
	}

	// Send POST request
	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	resp, err := client.Post(webhook, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		if m.verbose {
			fmt.Printf("  Failed to send webhook: %v\n", err)
		}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		fmt.Printf("  ✓ Alert sent to webhook\n")
	} else {
		if m.verbose {
			fmt.Printf("  Webhook returned status %d\n", resp.StatusCode)
		}
	}
}

// CheckDomain checks if a domain is accessible
func CheckDomain(domain string) error {
	url := fmt.Sprintf("https://%s", domain)

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		return fmt.Errorf("server error: status %d", resp.StatusCode)
	}

	return nil
}

// FormatWebhookPayload returns a formatted string representation of the payload
func FormatWebhookPayload(payload WebhookPayload) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("🚨 Service Alert\n\n"))
	b.WriteString(fmt.Sprintf("Event: %s\n", payload.Event))
	b.WriteString(fmt.Sprintf("Project: %s\n", payload.Project))
	b.WriteString(fmt.Sprintf("Service: %s\n", payload.Service))
	b.WriteString(fmt.Sprintf("Server: %s\n", payload.Server))
	b.WriteString(fmt.Sprintf("Severity: %s\n", payload.Severity))
	b.WriteString(fmt.Sprintf("Time: %s\n\n", payload.Timestamp.Format(time.RFC3339)))

	b.WriteString("Details:\n")
	for k, v := range payload.Details {
		b.WriteString(fmt.Sprintf("  %s: %v\n", k, v))
	}

	return b.String()
}
