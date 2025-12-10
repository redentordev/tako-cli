package drift

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/notification"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

// DriftType represents the type of drift detected
type DriftType string

const (
	DriftNone          DriftType = "none"
	DriftImageChanged  DriftType = "image_changed"
	DriftReplicasDown  DriftType = "replicas_down"
	DriftServiceGone   DriftType = "service_gone"
	DriftEnvChanged    DriftType = "env_changed"
	DriftConfigChanged DriftType = "config_changed"
	DriftUnexpected    DriftType = "unexpected_service"
)

// DriftReport represents a drift detection report
type DriftReport struct {
	Service     string            `json:"service"`
	Type        DriftType         `json:"type"`
	Expected    string            `json:"expected"`
	Actual      string            `json:"actual"`
	Details     map[string]string `json:"details,omitempty"`
	DetectedAt  time.Time         `json:"detected_at"`
	Severity    string            `json:"severity"` // low, medium, high, critical
}

// DriftState represents the current drift state
type DriftState struct {
	Project      string        `json:"project"`
	Environment  string        `json:"environment"`
	LastCheck    time.Time     `json:"last_check"`
	Drifts       []DriftReport `json:"drifts"`
	ServicesOK   []string      `json:"services_ok"`
	CheckDuration time.Duration `json:"check_duration"`
}

// Detector performs continuous drift detection
type Detector struct {
	client      *ssh.Client
	config      *config.Config
	environment string
	notifier    *notification.Notifier
	verbose     bool
	
	// State
	mu          sync.RWMutex
	lastState   *DriftState
	stopCh      chan struct{}
	running     bool
}

// NewDetector creates a new drift detector
func NewDetector(client *ssh.Client, cfg *config.Config, environment string, notifier *notification.Notifier, verbose bool) *Detector {
	return &Detector{
		client:      client,
		config:      cfg,
		environment: environment,
		notifier:    notifier,
		verbose:     verbose,
		stopCh:      make(chan struct{}),
	}
}

// CheckOnce performs a single drift detection check
func (d *Detector) CheckOnce() (*DriftState, error) {
	start := time.Now()
	
	env, err := d.config.GetEnvironment(d.environment)
	if err != nil {
		return nil, fmt.Errorf("failed to get environment: %w", err)
	}

	state := &DriftState{
		Project:     d.config.Project.Name,
		Environment: d.environment,
		LastCheck:   time.Now(),
		Drifts:      []DriftReport{},
		ServicesOK:  []string{},
	}

	// Get actual running services
	actualServices, err := d.getActualServices()
	if err != nil {
		return nil, fmt.Errorf("failed to get actual services: %w", err)
	}

	// Check each configured service
	for serviceName, serviceCfg := range env.Services {
		fullServiceName := fmt.Sprintf("%s_%s_%s", d.config.Project.Name, d.environment, serviceName)
		
		actual, exists := actualServices[fullServiceName]
		if !exists {
			// Service doesn't exist at all
			state.Drifts = append(state.Drifts, DriftReport{
				Service:    serviceName,
				Type:       DriftServiceGone,
				Expected:   fmt.Sprintf("%d replicas", serviceCfg.Replicas),
				Actual:     "service not found",
				DetectedAt: time.Now(),
				Severity:   "critical",
			})
			continue
		}

		// Check replicas
		expectedReplicas := serviceCfg.Replicas
		if expectedReplicas == 0 {
			expectedReplicas = 1
		}
		
		if actual.Replicas < expectedReplicas {
			state.Drifts = append(state.Drifts, DriftReport{
				Service:    serviceName,
				Type:       DriftReplicasDown,
				Expected:   fmt.Sprintf("%d replicas", expectedReplicas),
				Actual:     fmt.Sprintf("%d replicas", actual.Replicas),
				DetectedAt: time.Now(),
				Severity:   d.getReplicaSeverity(actual.Replicas, expectedReplicas),
			})
			continue
		}

		// Check image
		if serviceCfg.Image != "" && actual.Image != "" {
			expectedImage := serviceCfg.Image
			if !strings.Contains(actual.Image, expectedImage) && !strings.HasPrefix(actual.Image, d.config.Project.Name) {
				state.Drifts = append(state.Drifts, DriftReport{
					Service:    serviceName,
					Type:       DriftImageChanged,
					Expected:   expectedImage,
					Actual:     actual.Image,
					DetectedAt: time.Now(),
					Severity:   "high",
				})
				continue
			}
		}

		// Service is OK
		state.ServicesOK = append(state.ServicesOK, serviceName)
	}

	// Check for unexpected services
	for fullName := range actualServices {
		if !strings.HasPrefix(fullName, d.config.Project.Name+"_"+d.environment+"_") {
			continue
		}
		
		// Extract service name
		prefix := d.config.Project.Name + "_" + d.environment + "_"
		serviceName := strings.TrimPrefix(fullName, prefix)
		
		if _, exists := env.Services[serviceName]; !exists {
			state.Drifts = append(state.Drifts, DriftReport{
				Service:    serviceName,
				Type:       DriftUnexpected,
				Expected:   "not configured",
				Actual:     "running",
				DetectedAt: time.Now(),
				Severity:   "low",
			})
		}
	}

	state.CheckDuration = time.Since(start)
	
	// Store state
	d.mu.Lock()
	d.lastState = state
	d.mu.Unlock()

	// Send notifications for critical drifts
	if d.notifier != nil {
		for _, drift := range state.Drifts {
			if drift.Severity == "critical" || drift.Severity == "high" {
				d.notifier.Notify(notification.DriftDetectedEvent(
					d.config.Project.Name,
					d.environment,
					drift.Service,
					fmt.Sprintf("%s: expected %s, got %s", drift.Type, drift.Expected, drift.Actual),
				))
			}
		}
	}

	return state, nil
}

// Start begins continuous drift detection
func (d *Detector) Start(ctx context.Context, interval time.Duration) error {
	d.mu.Lock()
	if d.running {
		d.mu.Unlock()
		return fmt.Errorf("detector already running")
	}
	d.running = true
	d.stopCh = make(chan struct{})
	d.mu.Unlock()

	if d.verbose {
		fmt.Printf("→ Starting drift detector (interval: %s)\n", interval)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Initial check
	if _, err := d.CheckOnce(); err != nil {
		if d.verbose {
			fmt.Printf("  ⚠ Initial drift check failed: %v\n", err)
		}
	}

	for {
		select {
		case <-ctx.Done():
			d.mu.Lock()
			d.running = false
			d.mu.Unlock()
			return ctx.Err()
		case <-d.stopCh:
			d.mu.Lock()
			d.running = false
			d.mu.Unlock()
			return nil
		case <-ticker.C:
			state, err := d.CheckOnce()
			if err != nil {
				if d.verbose {
					fmt.Printf("  ⚠ Drift check failed: %v\n", err)
				}
				continue
			}
			
			if d.verbose && len(state.Drifts) > 0 {
				fmt.Printf("  ⚠ Detected %d drifts\n", len(state.Drifts))
				for _, drift := range state.Drifts {
					fmt.Printf("    - %s: %s (%s)\n", drift.Service, drift.Type, drift.Severity)
				}
			}
		}
	}
}

// Stop stops continuous drift detection
func (d *Detector) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	
	if d.running {
		close(d.stopCh)
	}
}

// GetLastState returns the last drift state
func (d *Detector) GetLastState() *DriftState {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.lastState
}

// SaveState saves the drift state to a file
func (d *Detector) SaveState(path string) error {
	d.mu.RLock()
	state := d.lastState
	d.mu.RUnlock()

	if state == nil {
		return fmt.Errorf("no state to save")
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	return os.WriteFile(path, data, 0644)
}

// ActualService represents a running service
type ActualService struct {
	Name     string
	Image    string
	Replicas int
	Running  int
}

// getActualServices gets the actual running services from Docker Swarm
func (d *Detector) getActualServices() (map[string]ActualService, error) {
	// Get service list with details
	cmd := "docker service ls --format '{{.Name}}|{{.Image}}|{{.Replicas}}'"
	output, err := d.client.Execute(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to list services: %w", err)
	}

	services := make(map[string]ActualService)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.Split(line, "|")
		if len(parts) < 3 {
			continue
		}

		name := parts[0]
		image := parts[1]
		replicaStr := parts[2]

		// Parse replicas (format: "2/3" or "3/3")
		var running, desired int
		fmt.Sscanf(replicaStr, "%d/%d", &running, &desired)

		services[name] = ActualService{
			Name:     name,
			Image:    image,
			Replicas: desired,
			Running:  running,
		}
	}

	return services, nil
}

// getReplicaSeverity determines severity based on replica count
func (d *Detector) getReplicaSeverity(actual, expected int) string {
	if actual == 0 {
		return "critical"
	}
	
	ratio := float64(actual) / float64(expected)
	if ratio < 0.5 {
		return "high"
	} else if ratio < 1.0 {
		return "medium"
	}
	
	return "low"
}
