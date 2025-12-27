package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Manager handles local state persistence in .tako directory
type Manager struct {
	basePath string
	project  string
	env      string
	mu       sync.Mutex  // Protect concurrent access within process
	lock     *StateLock  // Cross-process file-based locking
	lockInfo *LockInfo   // Current lock if held
}

// GlobalState represents the overall project state
type GlobalState struct {
	Version string `json:"version"`
	Project struct {
		Name         string    `json:"name"`
		CreatedAt    time.Time `json:"created_at"`
		LastDeployed time.Time `json:"last_deployed"`
	} `json:"project"`
	Mode         string                       `json:"mode"` // "swarm" or "single"
	Environments map[string]*EnvironmentState `json:"environments"`
	Metadata     struct {
		TakoVersion string `json:"tako_version"`
		ConfigHash  string `json:"config_hash"`
	} `json:"metadata"`
}

// EnvironmentState represents state for a specific environment
type EnvironmentState struct {
	Active         bool      `json:"active"`
	Servers        []string  `json:"servers"`
	LastDeployment time.Time `json:"last_deployment"`
	DeploymentID   string    `json:"deployment_id"`
}

// DeploymentState represents a single deployment
type DeploymentState struct {
	DeploymentID    string                    `json:"deployment_id"`
	Timestamp       time.Time                 `json:"timestamp"`
	Environment     string                    `json:"environment"`
	Mode            string                    `json:"mode"`
	Status          string                    `json:"status"` // success, failed, partial
	DurationSeconds int                       `json:"duration_seconds"`
	Services        map[string]*ServiceDeploy `json:"services"`
	Network         *NetworkInfo              `json:"network"`
	Volumes         []VolumeInfo              `json:"volumes"`
	Config          ConfigInfo                `json:"config"`
	RollbackPoint   bool                      `json:"rollback_point"`
	Notes           string                    `json:"notes"`
	GitCommit       string                    `json:"git_commit,omitempty"`
	TriggeredBy     string                    `json:"triggered_by,omitempty"`
}

// ServiceDeploy represents a service in a deployment
type ServiceDeploy struct {
	Image      string          `json:"image"`
	ImageID    string          `json:"image_id"`
	Replicas   int             `json:"replicas"`
	Ports      []int           `json:"ports"`
	Domains    []string        `json:"domains"`
	Health     string          `json:"health"`
	Containers []ContainerInfo `json:"containers"`
	BuildTime  int             `json:"build_time_seconds,omitempty"`
}

// ContainerInfo represents a container instance
type ContainerInfo struct {
	ID        string    `json:"id"`
	Node      string    `json:"node,omitempty"`
	Status    string    `json:"status"`
	StartedAt time.Time `json:"started_at"`
}

// NetworkInfo represents network configuration
type NetworkInfo struct {
	Name   string `json:"name"`
	Driver string `json:"driver"`
	ID     string `json:"id"`
}

// VolumeInfo represents volume configuration
type VolumeInfo struct {
	Name       string `json:"name"`
	Driver     string `json:"driver"`
	MountPoint string `json:"mount_point"`
}

// ConfigInfo represents configuration metadata
type ConfigInfo struct {
	Hash   string `json:"hash"`
	Source string `json:"source"`
}

// ServiceState represents the current state of a service
type ServiceState struct {
	Name         string           `json:"name"`
	Current      ServiceCurrent   `json:"current"`
	History      []ServiceHistory `json:"history"`
	HealthChecks HealthCheck      `json:"health_checks"`
	Resources    Resources        `json:"resources"`
}

// ServiceCurrent represents current service state
type ServiceCurrent struct {
	Version    string    `json:"version"`
	Image      string    `json:"image"`
	DeployedAt time.Time `json:"deployed_at"`
	Replicas   int       `json:"replicas"`
	Status     string    `json:"status"`
}

// ServiceHistory represents historical service deployments
type ServiceHistory struct {
	Version    string    `json:"version"`
	DeployedAt time.Time `json:"deployed_at"`
	RetiredAt  time.Time `json:"retired_at"`
	Reason     string    `json:"reason"`
}

// HealthCheck represents health check information
type HealthCheck struct {
	Endpoint       string    `json:"endpoint"`
	LastCheck      time.Time `json:"last_check"`
	Status         string    `json:"status"`
	ResponseTimeMs int       `json:"response_time_ms"`
}

// Resources represents resource usage
type Resources struct {
	CPULimit      string `json:"cpu_limit"`
	MemoryLimit   string `json:"memory_limit"`
	CurrentCPU    string `json:"current_cpu"`
	CurrentMemory string `json:"current_memory"`
}

// NewManager creates a new state manager
func NewManager(projectPath, projectName, environment string) (*Manager, error) {
	basePath := filepath.Join(projectPath, ".tako")

	// Ensure .tako directory exists
	if err := os.MkdirAll(basePath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create .tako directory: %w", err)
	}

	// Create .gitignore if it doesn't exist
	gitignorePath := filepath.Join(basePath, ".gitignore")
	if _, err := os.Stat(gitignorePath); os.IsNotExist(err) {
		gitignoreContent := `# Tako local state - DO NOT COMMIT
*
!.gitignore
`
		if err := os.WriteFile(gitignorePath, []byte(gitignoreContent), 0644); err != nil {
			return nil, fmt.Errorf("failed to create .gitignore: %w", err)
		}
	}

	// Create directory structure
	dirs := []string{
		"deployments/" + environment + "/history",
		"deployments/" + environment + "/rollback",
		"services",
		"builds/cache",
		"builds/artifacts",
		"logs/deployments",
		"logs/builds",
		"logs/errors",
		"swarm",
		"config/traefik",
		"config/networks",
	}

	for _, dir := range dirs {
		dirPath := filepath.Join(basePath, dir)
		if err := os.MkdirAll(dirPath, 0755); err != nil {
			return nil, fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	return &Manager{
		basePath: basePath,
		project:  projectName,
		env:      environment,
		lock:     NewStateLock(basePath),
	}, nil
}

// AcquireLock acquires a cross-process lock for the given operation
// Returns an error if another process holds the lock
func (m *Manager) AcquireLock(operation string) error {
	lockInfo, err := m.lock.Acquire(operation)
	if err != nil {
		return err
	}
	m.lockInfo = lockInfo
	return nil
}

// AcquireLockWithWait acquires a cross-process lock, waiting if necessary
func (m *Manager) AcquireLockWithWait(operation string) error {
	lockInfo, err := m.lock.AcquireWithWait(operation)
	if err != nil {
		return err
	}
	m.lockInfo = lockInfo
	return nil
}

// ReleaseLock releases the cross-process lock
func (m *Manager) ReleaseLock() error {
	if m.lockInfo == nil {
		return nil
	}
	err := m.lock.Release(m.lockInfo)
	m.lockInfo = nil
	return err
}

// IsLocked returns true if the state is locked by another process
func (m *Manager) IsLocked() bool {
	return m.lock.IsLocked()
}

// GetLockHolder returns information about who holds the lock
func (m *Manager) GetLockHolder() (*LockInfo, error) {
	return m.lock.GetLockInfo()
}

// SaveDeployment saves deployment state
func (m *Manager) SaveDeployment(deployment *DeploymentState) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Save to current
	currentPath := filepath.Join(m.basePath, "deployments", m.env, "current.json")
	if err := m.saveJSON(currentPath, deployment); err != nil {
		return fmt.Errorf("failed to save current deployment: %w", err)
	}

	// Save to history with timestamp
	timestamp := deployment.Timestamp.Format("2006-01-02T15-04-05Z")
	historyPath := filepath.Join(m.basePath, "deployments", m.env, "history", timestamp+".json")
	if err := m.saveJSON(historyPath, deployment); err != nil {
		return fmt.Errorf("failed to save deployment history: %w", err)
	}

	// If marked as rollback point, save it
	if deployment.RollbackPoint {
		rollbackPath := filepath.Join(m.basePath, "deployments", m.env, "rollback", "last-stable.json")
		if err := m.saveJSON(rollbackPath, deployment); err != nil {
			return fmt.Errorf("failed to save rollback point: %w", err)
		}
	}

	// Prune old history files to prevent unbounded growth
	m.pruneHistory()

	// Update global state
	return m.updateGlobalState(deployment)
}

// MaxLocalHistoryEntries is the maximum number of deployment history files to keep
const MaxLocalHistoryEntries = 50

// pruneHistory removes old history files keeping only the most recent entries
func (m *Manager) pruneHistory() {
	historyDir := filepath.Join(m.basePath, "deployments", m.env, "history")

	entries, err := os.ReadDir(historyDir)
	if err != nil {
		return // Silently ignore errors during cleanup
	}

	// Filter to only .json files
	var jsonFiles []os.DirEntry
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			jsonFiles = append(jsonFiles, e)
		}
	}

	// If under limit, nothing to do
	if len(jsonFiles) <= MaxLocalHistoryEntries {
		return
	}

	// Sort by name (which is timestamp-based, so oldest first)
	// Files are named like "2006-01-02T15-04-05Z.json"
	// Sorting alphabetically gives chronological order
	type fileWithInfo struct {
		entry   os.DirEntry
		modTime time.Time
	}
	var files []fileWithInfo
	for _, e := range jsonFiles {
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fileWithInfo{entry: e, modTime: info.ModTime()})
	}

	// Sort by modification time (oldest first)
	for i := 0; i < len(files)-1; i++ {
		for j := i + 1; j < len(files); j++ {
			if files[i].modTime.After(files[j].modTime) {
				files[i], files[j] = files[j], files[i]
			}
		}
	}

	// Remove oldest files until we're at the limit
	toRemove := len(files) - MaxLocalHistoryEntries
	for i := 0; i < toRemove; i++ {
		path := filepath.Join(historyDir, files[i].entry.Name())
		os.Remove(path) // Ignore errors during cleanup
	}
}

// GetCurrentDeployment returns the current deployment state
func (m *Manager) GetCurrentDeployment() (*DeploymentState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	path := filepath.Join(m.basePath, "deployments", m.env, "current.json")

	var deployment DeploymentState
	if err := m.loadJSON(path, &deployment); err != nil {
		if os.IsNotExist(err) {
			return nil, nil // No current deployment
		}
		return nil, err
	}

	return &deployment, nil
}

// GetLastStableDeployment returns the last stable deployment for rollback
func (m *Manager) GetLastStableDeployment() (*DeploymentState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	path := filepath.Join(m.basePath, "deployments", m.env, "rollback", "last-stable.json")

	var deployment DeploymentState
	if err := m.loadJSON(path, &deployment); err != nil {
		if os.IsNotExist(err) {
			return nil, nil // No rollback point
		}
		return nil, err
	}

	return &deployment, nil
}

// SaveServiceState saves service-specific state
func (m *Manager) SaveServiceState(serviceName string, state *ServiceState) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	servicePath := filepath.Join(m.basePath, "services", serviceName)
	if err := os.MkdirAll(servicePath, 0755); err != nil {
		return err
	}

	path := filepath.Join(servicePath, "state.json")
	return m.saveJSON(path, state)
}

// GetServiceState retrieves service state
func (m *Manager) GetServiceState(serviceName string) (*ServiceState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	path := filepath.Join(m.basePath, "services", serviceName, "state.json")

	var state ServiceState
	if err := m.loadJSON(path, &state); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	return &state, nil
}

// LogDeployment logs deployment messages
func (m *Manager) LogDeployment(message string) error {
	timestamp := time.Now().Format("2006-01-02T15-04-05Z")
	logPath := filepath.Join(m.basePath, "logs", "deployments", timestamp+".log")

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return err
	}

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	logEntry := fmt.Sprintf("[%s] %s\n", time.Now().Format(time.RFC3339), message)
	_, err = f.WriteString(logEntry)
	return err
}

// LogError logs error messages
func (m *Manager) LogError(err error) error {
	timestamp := time.Now().Format("2006-01-02")
	logPath := filepath.Join(m.basePath, "logs", "errors", timestamp+".log")

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return err
	}

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	logEntry := fmt.Sprintf("[%s] ERROR: %v\n", time.Now().Format(time.RFC3339), err)
	_, err = f.WriteString(logEntry)
	return err
}

// GetDeploymentHistory returns deployment history
func (m *Manager) GetDeploymentHistory(limit int) ([]*DeploymentState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	historyPath := filepath.Join(m.basePath, "deployments", m.env, "history")

	files, err := os.ReadDir(historyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []*DeploymentState{}, nil
		}
		return nil, err
	}

	var deployments []*DeploymentState

	// Get files in reverse order (newest first)
	for i := len(files) - 1; i >= 0 && len(deployments) < limit; i-- {
		if files[i].IsDir() {
			continue
		}

		var deployment DeploymentState
		path := filepath.Join(historyPath, files[i].Name())
		if err := m.loadJSON(path, &deployment); err != nil {
			continue // Skip corrupted files
		}

		deployments = append(deployments, &deployment)
	}

	return deployments, nil
}

// CheckBuildCache checks if rebuild is needed based on file checksums
func (m *Manager) CheckBuildCache(serviceName string, files map[string]string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cachePath := filepath.Join(m.basePath, "builds", "cache", serviceName, "checksums.json")

	var cache struct {
		LastBuild time.Time         `json:"last_build"`
		Files     map[string]string `json:"files"`
	}

	if err := m.loadJSON(cachePath, &cache); err != nil {
		if os.IsNotExist(err) {
			return true, nil // No cache, rebuild needed
		}
		return true, err
	}

	// Compare checksums
	for file, checksum := range files {
		if cache.Files[file] != checksum {
			return true, nil // File changed, rebuild needed
		}
	}

	return false, nil // No changes, skip rebuild
}

// SaveBuildCache saves build cache information
func (m *Manager) SaveBuildCache(serviceName string, files map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cache := struct {
		LastBuild time.Time         `json:"last_build"`
		Files     map[string]string `json:"files"`
	}{
		LastBuild: time.Now(),
		Files:     files,
	}

	cachePath := filepath.Join(m.basePath, "builds", "cache", serviceName)
	if err := os.MkdirAll(cachePath, 0755); err != nil {
		return err
	}

	return m.saveJSON(filepath.Join(cachePath, "checksums.json"), cache)
}

// Helper methods

func (m *Manager) updateGlobalState(deployment *DeploymentState) error {
	globalPath := filepath.Join(m.basePath, "state.json")

	var global GlobalState
	// Try to load existing state
	m.loadJSON(globalPath, &global)

	// Update or initialize
	if global.Version == "" {
		global.Version = "1.0.0"
	}

	global.Project.Name = m.project
	if global.Project.CreatedAt.IsZero() {
		global.Project.CreatedAt = time.Now()
	}
	global.Project.LastDeployed = deployment.Timestamp

	global.Mode = deployment.Mode

	if global.Environments == nil {
		global.Environments = make(map[string]*EnvironmentState)
	}

	global.Environments[m.env] = &EnvironmentState{
		Active:         true,
		Servers:        []string{}, // TODO: Extract from deployment
		LastDeployment: deployment.Timestamp,
		DeploymentID:   deployment.DeploymentID,
	}

	return m.saveJSON(globalPath, global)
}

func (m *Manager) saveJSON(path string, data interface{}) error {
	// Ensure directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	bytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}

	// Use atomic write: write to temp file, then rename
	// This prevents corruption if the process crashes mid-write
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, bytes, 0644); err != nil {
		return err
	}

	// Rename is atomic on POSIX systems
	if err := os.Rename(tmpPath, path); err != nil {
		// Clean up temp file on failure
		os.Remove(tmpPath)
		return err
	}

	return nil
}

func (m *Manager) loadJSON(path string, target interface{}) error {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	return json.Unmarshal(bytes, target)
}

// GetBasePath returns the base .tako directory path
func (m *Manager) GetBasePath() string {
	return m.basePath
}

// Exists checks if state directory exists
func (m *Manager) Exists() bool {
	_, err := os.Stat(m.basePath)
	return !os.IsNotExist(err)
}
