package takodstate

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

const (
	DefaultDataDir = "/var/lib/tako"
	SchemaVersion  = 1
)

var ErrNotFound = errors.New("takod state not found")

type GitInfo struct {
	Commit      string `json:"commit,omitempty"`
	CommitShort string `json:"commitShort,omitempty"`
	Branch      string `json:"branch,omitempty"`
	Message     string `json:"message,omitempty"`
	Author      string `json:"author,omitempty"`
}

type DesiredRevision struct {
	SchemaVersion int                       `json:"schemaVersion"`
	RevisionID    string                    `json:"revisionId"`
	Project       string                    `json:"project"`
	Environment   string                    `json:"environment"`
	Source        string                    `json:"source"`
	TargetNodes   []string                  `json:"targetNodes"`
	Services      map[string]DesiredService `json:"services"`
	Git           GitInfo                   `json:"git,omitempty"`
	CreatedAt     time.Time                 `json:"createdAt"`
}

type DesiredService struct {
	Name           string                   `json:"name"`
	Type           string                   `json:"type"`
	Image          string                   `json:"image,omitempty"`
	Build          string                   `json:"build,omitempty"`
	Command        string                   `json:"command,omitempty"`
	Port           int                      `json:"port,omitempty"`
	Replicas       int                      `json:"replicas"`
	Restart        string                   `json:"restart,omitempty"`
	Persistent     bool                     `json:"persistent,omitempty"`
	Placement      *config.PlacementConfig  `json:"placement,omitempty"`
	Domains        []string                 `json:"domains,omitempty"`
	Volumes        []string                 `json:"volumes,omitempty"`
	EnvKeys        []string                 `json:"envKeys,omitempty"`
	EnvFile        bool                     `json:"envFile,omitempty"`
	SecretRefs     []string                 `json:"secretRefs,omitempty"`
	DependsOn      []string                 `json:"dependsOn,omitempty"`
	HealthCheck    config.HealthCheckConfig `json:"healthCheck,omitempty"`
	DeployStrategy string                   `json:"deployStrategy,omitempty"`
}

type ActualSnapshot struct {
	SchemaVersion int                      `json:"schemaVersion"`
	Project       string                   `json:"project"`
	Environment   string                   `json:"environment"`
	TargetNodes   []string                 `json:"targetNodes"`
	Services      map[string]ActualService `json:"services"`
	CapturedAt    time.Time                `json:"capturedAt"`
}

type ActualService struct {
	Name       string   `json:"name"`
	Image      string   `json:"image,omitempty"`
	Replicas   int      `json:"replicas"`
	Containers []string `json:"containers,omitempty"`
}

type Event struct {
	SchemaVersion int               `json:"schemaVersion"`
	Type          string            `json:"type"`
	Project       string            `json:"project"`
	Environment   string            `json:"environment"`
	RevisionID    string            `json:"revisionId,omitempty"`
	Service       string            `json:"service,omitempty"`
	Message       string            `json:"message,omitempty"`
	Details       map[string]string `json:"details,omitempty"`
	Time          time.Time         `json:"time"`
}

type Manager struct {
	client      *ssh.Client
	dataDir     string
	project     string
	environment string
}

func NewManager(client *ssh.Client, cfg *config.Config, environment string) *Manager {
	dataDir := DefaultDataDir
	if cfg.Runtime != nil && cfg.Runtime.Agent != nil && cfg.Runtime.Agent.DataDir != "" {
		dataDir = cfg.Runtime.Agent.DataDir
	}
	return &Manager{
		client:      client,
		dataDir:     dataDir,
		project:     cfg.Project.Name,
		environment: environment,
	}
}

func BuildDesiredRevision(cfg *config.Config, environment string, source string, services map[string]config.ServiceConfig, imageRefs map[string]string, targetNodes []string, git GitInfo) (*DesiredRevision, error) {
	now := time.Now().UTC()
	revision := &DesiredRevision{
		SchemaVersion: SchemaVersion,
		Project:       cfg.Project.Name,
		Environment:   environment,
		Source:        source,
		TargetNodes:   sortedCopy(targetNodes),
		Services:      make(map[string]DesiredService, len(services)),
		Git:           git,
		CreatedAt:     now,
	}

	for serviceName, service := range services {
		imageRef := imageRefs[serviceName]
		if imageRef == "" {
			if service.Image != "" {
				imageRef = service.Image
			} else {
				imageRef = cfg.GetFullImageName(serviceName, environment)
			}
		}

		revision.Services[serviceName] = sanitizeDesiredService(serviceName, service, imageRef)
	}

	revision.RevisionID = revisionID(revision)
	return revision, nil
}

func BuildActualSnapshot(project string, environment string, targetNodes []string, actual map[string]*reconcile.ActualService) *ActualSnapshot {
	snapshot := &ActualSnapshot{
		SchemaVersion: SchemaVersion,
		Project:       project,
		Environment:   environment,
		TargetNodes:   sortedCopy(targetNodes),
		Services:      make(map[string]ActualService, len(actual)),
		CapturedAt:    time.Now().UTC(),
	}

	for serviceName, service := range actual {
		if service == nil {
			continue
		}
		containers := sortedCopy(service.Containers)
		snapshot.Services[serviceName] = ActualService{
			Name:       service.Name,
			Image:      service.Image,
			Replicas:   service.Replicas,
			Containers: containers,
		}
	}

	return snapshot
}

func NewEvent(project string, environment string, eventType string, revisionID string, message string, details map[string]string) Event {
	return Event{
		SchemaVersion: SchemaVersion,
		Type:          eventType,
		Project:       project,
		Environment:   environment,
		RevisionID:    revisionID,
		Message:       message,
		Details:       details,
		Time:          time.Now().UTC(),
	}
}

func PersistToServers(pool *ssh.Pool, cfg *config.Config, environment string, serverNames []string, desired *DesiredRevision, actual *ActualSnapshot, event Event, verbose bool) error {
	if pool == nil {
		return fmt.Errorf("ssh pool not initialized")
	}

	for _, serverName := range serverNames {
		server, ok := cfg.Servers[serverName]
		if !ok {
			return fmt.Errorf("server %s not found", serverName)
		}
		client, err := pool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return fmt.Errorf("failed to connect to %s for takod state persistence: %w", serverName, err)
		}
		manager := NewManager(client, cfg, environment)
		if err := manager.WriteDesired(desired); err != nil {
			return fmt.Errorf("%s: failed to write desired state: %w", serverName, err)
		}
		if err := manager.WriteActual(actual); err != nil {
			return fmt.Errorf("%s: failed to write actual state: %w", serverName, err)
		}
		if err := manager.AppendEvent(event); err != nil {
			return fmt.Errorf("%s: failed to append event: %w", serverName, err)
		}
		if verbose {
			fmt.Printf("  ✓ takod state persisted on %s\n", serverName)
		}
	}

	return nil
}

func (m *Manager) Ensure() error {
	dirs := []string{
		m.desiredDir(),
		m.desiredDir() + "/revisions",
		m.actualDir(),
		m.eventsDir(),
	}
	quoted := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		quoted = append(quoted, shellQuote(dir))
	}
	if _, err := m.client.Execute("mkdir -p " + strings.Join(quoted, " ")); err != nil {
		return err
	}
	return nil
}

func (m *Manager) WriteDesired(revision *DesiredRevision) error {
	if revision == nil {
		return fmt.Errorf("desired revision is nil")
	}
	if err := m.Ensure(); err != nil {
		return err
	}
	if err := uploadJSON(m.client, m.desiredRevisionPath(), revision, 0600); err != nil {
		return err
	}
	if revision.RevisionID != "" {
		if err := uploadJSON(m.client, m.desiredRevisionArchivePath(revision.RevisionID), revision, 0600); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) WriteActual(snapshot *ActualSnapshot) error {
	if snapshot == nil {
		return fmt.Errorf("actual snapshot is nil")
	}
	if err := m.Ensure(); err != nil {
		return err
	}
	return uploadJSON(m.client, m.actualSnapshotPath(), snapshot, 0600)
}

func (m *Manager) ReadDesired() (*DesiredRevision, error) {
	var revision DesiredRevision
	if err := m.readJSON(m.desiredRevisionPath(), &revision); err != nil {
		return nil, err
	}
	return &revision, nil
}

func (m *Manager) ReadActual() (*ActualSnapshot, error) {
	var snapshot ActualSnapshot
	if err := m.readJSON(m.actualSnapshotPath(), &snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func (m *Manager) AppendEvent(event Event) error {
	if err := m.Ensure(); err != nil {
		return err
	}
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	if event.SchemaVersion == 0 {
		event.SchemaVersion = SchemaVersion
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	encoded := base64.StdEncoding.EncodeToString(data)
	cmd := fmt.Sprintf("echo '%s' | base64 -d >> %s", encoded, shellQuote(m.eventsPath()))
	_, err = m.client.Execute(cmd)
	return err
}

func (m *Manager) readJSON(remotePath string, value any) error {
	cmd := fmt.Sprintf("if test -f %s; then cat %s; else printf '__TAKO_STATE_NOT_FOUND__'; fi", shellQuote(remotePath), shellQuote(remotePath))
	output, err := m.client.Execute(cmd)
	if err != nil {
		return fmt.Errorf("failed to read %s: %w", remotePath, err)
	}
	if strings.TrimSpace(output) == "__TAKO_STATE_NOT_FOUND__" {
		return ErrNotFound
	}
	if strings.TrimSpace(output) == "" {
		return fmt.Errorf("empty state file %s", remotePath)
	}
	if err := json.Unmarshal([]byte(output), value); err != nil {
		return fmt.Errorf("failed to parse %s: %w", remotePath, err)
	}
	return nil
}

func sanitizeDesiredService(serviceName string, service config.ServiceConfig, imageRef string) DesiredService {
	replicas := service.Replicas
	if replicas < 0 {
		replicas = 1
	}

	var domains []string
	if service.Proxy != nil {
		domains = sortedCopy(service.Proxy.GetAllDomains())
	}

	return DesiredService{
		Name:           serviceName,
		Type:           service.GetServiceType(),
		Image:          imageRef,
		Build:          service.Build,
		Command:        service.Command,
		Port:           service.Port,
		Replicas:       replicas,
		Restart:        service.Restart,
		Persistent:     service.Persistent,
		Placement:      service.Placement,
		Domains:        domains,
		Volumes:        sortedCopy(service.Volumes),
		EnvKeys:        sortedKeys(service.Env),
		EnvFile:        service.EnvFile != "",
		SecretRefs:     sortedCopy(service.Secrets),
		DependsOn:      sortedCopy(service.DependsOn),
		HealthCheck:    service.HealthCheck,
		DeployStrategy: service.Deploy.Strategy,
	}
}

func revisionID(revision *DesiredRevision) string {
	copyForHash := *revision
	copyForHash.RevisionID = ""
	data, _ := json.Marshal(copyForHash)
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%s-%s", revision.CreatedAt.Format("20060102T150405Z"), hex.EncodeToString(sum[:])[:12])
}

func (m *Manager) desiredDir() string {
	return fmt.Sprintf("%s/desired/%s/%s", m.dataDir, m.project, m.environment)
}

func (m *Manager) actualDir() string {
	return fmt.Sprintf("%s/actual/%s/%s", m.dataDir, m.project, m.environment)
}

func (m *Manager) eventsDir() string {
	return fmt.Sprintf("%s/events/%s", m.dataDir, m.project)
}

func (m *Manager) desiredRevisionPath() string {
	return fmt.Sprintf("%s/revision.json", m.desiredDir())
}

func (m *Manager) desiredRevisionArchivePath(revisionID string) string {
	return fmt.Sprintf("%s/revisions/%s.json", m.desiredDir(), revisionID)
}

func (m *Manager) actualSnapshotPath() string {
	return fmt.Sprintf("%s/containers.json", m.actualDir())
}

func (m *Manager) eventsPath() string {
	return fmt.Sprintf("%s/%s.jsonl", m.eventsDir(), m.environment)
}

func uploadJSON(client *ssh.Client, remotePath string, value any, mode os.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return client.UploadReader(strings.NewReader(string(data)), remotePath, mode)
}

func sortedKeys(values map[string]string) []string {
	if len(values) == 0 {
		return nil
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedCopy(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
