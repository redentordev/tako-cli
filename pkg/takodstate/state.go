package takodstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployplan"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/scheduler"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

const (
	SchemaVersion = 1

	stateDocumentDesired    = "desired"
	stateDocumentActual     = "actual"
	stateDocumentNodeActual = "actual-node"
	stateDocumentEvent      = "event"
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
	Builds        map[string]DesiredBuild   `json:"builds,omitempty"`
	Services      map[string]DesiredService `json:"services"`
	Git           GitInfo                   `json:"git,omitempty"`
	CreatedAt     time.Time                 `json:"createdAt"`
}

type DesiredBuild struct {
	Context    string   `json:"context"`
	ArgKeys    []string `json:"argKeys,omitempty"`
	Target     string   `json:"target,omitempty"`
	Dockerfile string   `json:"dockerfile,omitempty"`
}

type DesiredService struct {
	APIVersion      string                         `json:"apiVersion,omitempty"`
	Kind            string                         `json:"kind,omitempty"`
	Name            string                         `json:"name"`
	WorkloadKind    string                         `json:"workloadKind,omitempty"`
	Type            string                         `json:"type"`
	Image           string                         `json:"image,omitempty"`
	ImageFrom       string                         `json:"imageFrom,omitempty"`
	Build           string                         `json:"build,omitempty"`
	BuildArgKeys    []string                       `json:"buildArgKeys,omitempty"`
	BuildTarget     string                         `json:"buildTarget,omitempty"`
	Command         string                         `json:"command,omitempty"`
	CommandArgs     []string                       `json:"commandArgs,omitempty"`
	Entrypoint      string                         `json:"entrypoint,omitempty"`
	EntrypointArgs  []string                       `json:"entrypointArgs,omitempty"`
	Labels          map[string]string              `json:"labels,omitempty"`
	Port            int                            `json:"port,omitempty"`
	Replicas        int                            `json:"replicas"`
	Assignments     []scheduler.Assignment         `json:"assignments,omitempty"`
	Restart         string                         `json:"restart,omitempty"`
	Persistent      bool                           `json:"persistent,omitempty"`
	Placement       *config.PlacementConfig        `json:"placement,omitempty"`
	Domains         []string                       `json:"domains,omitempty"`
	Volumes         []string                       `json:"volumes,omitempty"`
	Files           []config.ServiceFileConfig     `json:"files,omitempty"`
	EnvKeys         []string                       `json:"envKeys,omitempty"`
	EnvFile         bool                           `json:"envFile,omitempty"`
	User            string                         `json:"user,omitempty"`
	WorkingDir      string                         `json:"workingDir,omitempty"`
	StopGracePeriod string                         `json:"stopGracePeriod,omitempty"`
	Init            bool                           `json:"init,omitempty"`
	ExtraHosts      []string                       `json:"extraHosts,omitempty"`
	Ulimits         map[string]config.UlimitConfig `json:"ulimits,omitempty"`
	ShmSize         string                         `json:"shmSize,omitempty"`
	SecretRefs      []string                       `json:"secretRefs,omitempty"`
	DependsOn       []string                       `json:"dependsOn,omitempty"`
	HealthCheck     config.HealthCheckConfig       `json:"healthCheck,omitempty"`
	DeployStrategy  string                         `json:"deployStrategy,omitempty"`
	RemovalPending  bool                           `json:"removalPending,omitempty"`
}

type ActualSnapshot struct {
	SchemaVersion int                           `json:"schemaVersion"`
	Project       string                        `json:"project"`
	Environment   string                        `json:"environment"`
	Node          string                        `json:"node,omitempty"`
	TargetNodes   []string                      `json:"targetNodes,omitempty"`
	Services      map[string]ActualService      `json:"services"`
	Nodes         map[string]ActualNodeSnapshot `json:"nodes,omitempty"`
	CapturedAt    time.Time                     `json:"capturedAt"`
}

type ActualNodeSnapshot struct {
	Node       string                   `json:"node"`
	Services   map[string]ActualService `json:"services"`
	CapturedAt time.Time                `json:"capturedAt"`
}

type ActualService struct {
	Name              string            `json:"name"`
	Image             string            `json:"image,omitempty"`
	RevisionImages    map[string]string `json:"revisionImages,omitempty"`
	Replicas          int               `json:"replicas"`
	Containers        []string          `json:"containers,omitempty"`
	ConfigHash        string            `json:"configHash,omitempty"`
	RuntimeID         string            `json:"runtimeId,omitempty"`
	Persistent        bool              `json:"persistent,omitempty"`
	CurrentRevision   string            `json:"currentRevision,omitempty"`
	PreviousRevision  string            `json:"previousRevision,omitempty"`
	WarmingRevisions  []string          `json:"warmingRevisions,omitempty"`
	DeployStrategy    string            `json:"deployStrategy,omitempty"`
	ActiveContainers  []string          `json:"activeContainers,omitempty"`
	WarmingContainers []string          `json:"warmingContainers,omitempty"`
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
	client         any
	socket         string
	project        string
	environment    string
	requestTimeout time.Duration
}

func NewManager(client any, cfg *config.Config, environment string) *Manager {
	socket := takodclient.DefaultSocket
	if cfg.Runtime != nil && cfg.Runtime.Agent != nil && cfg.Runtime.Agent.Socket != "" {
		socket = cfg.Runtime.Agent.Socket
	}
	return &Manager{
		client:      client,
		socket:      socket,
		project:     cfg.Project.Name,
		environment: environment,
	}
}

// WithRequestTimeout returns a shallow copy that uses a custom takod request
// deadline. A non-positive timeout keeps the package default.
func (m *Manager) WithRequestTimeout(timeout time.Duration) *Manager {
	copy := *m
	copy.requestTimeout = timeout
	return &copy
}

func BuildDesiredRevision(cfg *config.Config, environment string, source string, services map[string]config.ServiceConfig, imageRefs map[string]string, targetNodes []string, git GitInfo) (*DesiredRevision, error) {
	return BuildDesiredRevisionWithAssignments(cfg, environment, source, services, imageRefs, targetNodes, nil, git)
}

func BuildDesiredRevisionWithAssignments(cfg *config.Config, environment string, source string, services map[string]config.ServiceConfig, imageRefs map[string]string, targetNodes []string, assignments map[string][]scheduler.Assignment, git GitInfo) (*DesiredRevision, error) {
	return BuildDesiredRevisionWithPlacementIntent(cfg, environment, source, services, imageRefs, targetNodes, assignments, nil, git)
}

// BuildDesiredRevisionWithPlacementIntent retains removal-pending service
// bindings until cleanup succeeds, making interrupted removals retryable.
func BuildDesiredRevisionWithPlacementIntent(cfg *config.Config, environment string, source string, services map[string]config.ServiceConfig, imageRefs map[string]string, targetNodes []string, assignments map[string][]scheduler.Assignment, pendingRemovals map[string]config.ServiceConfig, git GitInfo) (*DesiredRevision, error) {
	return BuildDesiredRevisionWithPlacementSnapshot(cfg, environment, source, services, imageRefs, targetNodes, assignments, pendingRemovals, nil, git)
}

// BuildDesiredRevisionWithPlacementSnapshot carries prior desired-service
// records that are outside the current workflow's scope. This is necessary for
// targeted and non-removal operations: rewriting desired state must not clear
// an unrelated removalPending marker or forget a workload removed from the
// operator's current config before a full deploy reconciles it.
func BuildDesiredRevisionWithPlacementSnapshot(cfg *config.Config, environment string, source string, services map[string]config.ServiceConfig, imageRefs map[string]string, targetNodes []string, assignments map[string][]scheduler.Assignment, pendingRemovals map[string]config.ServiceConfig, preservedServices map[string]DesiredService, git GitInfo) (*DesiredRevision, error) {
	now := time.Now().UTC()
	revision := &DesiredRevision{
		SchemaVersion: SchemaVersion,
		Project:       cfg.Project.Name,
		Environment:   environment,
		Source:        source,
		TargetNodes:   sortedCopy(targetNodes),
		Builds:        make(map[string]DesiredBuild, len(cfg.Builds)),
		Services:      make(map[string]DesiredService, len(services)+len(pendingRemovals)+len(preservedServices)),
		Git:           git,
		CreatedAt:     now,
	}
	for name, build := range cfg.Builds {
		revision.Builds[name] = DesiredBuild{Context: build.DeclaredContext(), ArgKeys: sortedKeys(build.Args), Target: build.Target, Dockerfile: build.Dockerfile}
	}
	for serviceName, preserved := range preservedServices {
		if _, desired := services[serviceName]; desired {
			return nil, fmt.Errorf("service %s cannot be both current and preserved desired state", serviceName)
		}
		if _, pending := pendingRemovals[serviceName]; pending {
			return nil, fmt.Errorf("service %s cannot be both removal-pending and preserved desired state", serviceName)
		}
		if preserved.Name != "" && preserved.Name != serviceName {
			return nil, fmt.Errorf("preserved desired service %s has mismatched name %s", serviceName, preserved.Name)
		}
		if adopted := scheduler.Stable(assignments[serviceName]); len(adopted) > 0 {
			preserved.Assignments = adopted
		} else {
			preserved.Assignments = scheduler.Stable(preserved.Assignments)
		}
		if err := validateDesiredAssignments(cfg, serviceName, preserved.Assignments, "preserved desired"); err != nil {
			return nil, err
		}
		revision.Services[serviceName] = preserved
	}

	for serviceName, service := range services {
		imageRef := imageRefs[serviceName]
		if imageRef == "" {
			if service.Image != "" {
				imageRef = service.Image
			} else if _, ok := cfg.Builds[service.ImageFrom]; ok {
				imageRef = deployplan.SharedBuildImageRef(cfg, environment, service.ImageFrom, "")
			} else {
				imageRef = cfg.GetFullImageName(serviceName, environment)
			}
		}

		desired := sanitizeDesiredService(serviceName, service, imageRef)
		serviceAssignments := scheduler.Stable(assignments[serviceName])
		if err := validateDesiredAssignments(cfg, serviceName, serviceAssignments, "service"); err != nil {
			return nil, err
		}
		desired.Assignments = serviceAssignments
		revision.Services[serviceName] = desired
	}
	for serviceName, service := range pendingRemovals {
		if _, stillDesired := revision.Services[serviceName]; stillDesired {
			return nil, fmt.Errorf("service %s cannot be desired and removal-pending", serviceName)
		}
		imageRef := imageRefs[serviceName]
		if imageRef == "" {
			imageRef = service.Image
		}
		desired := sanitizeDesiredService(serviceName, service, imageRef)
		desired.RemovalPending = true
		serviceAssignments := scheduler.Stable(assignments[serviceName])
		if len(serviceAssignments) == 0 {
			return nil, fmt.Errorf("removal-pending service %s has no authoritative assignment", serviceName)
		}
		if err := validateDesiredAssignments(cfg, serviceName, serviceAssignments, "removal-pending service"); err != nil {
			return nil, err
		}
		desired.Assignments = serviceAssignments
		revision.Services[serviceName] = desired
	}

	revision.RevisionID = revisionID(revision)
	return revision, nil
}

func validateDesiredAssignments(cfg *config.Config, serviceName string, assignments []scheduler.Assignment, label string) error {
	if err := scheduler.ValidateAssignments(assignments); err != nil {
		return fmt.Errorf("%s %s assignments are invalid: %w", label, serviceName, err)
	}
	for _, assignment := range assignments {
		server, ok := cfg.Servers[assignment.Node]
		if len(cfg.Servers) > 0 && !ok {
			return fmt.Errorf("%s %s assignment references unknown node %s", label, serviceName, assignment.Node)
		}
		if ok && assignment.NodeID != server.NodeID && (assignment.NodeID != "" || server.NodeID != "") {
			return fmt.Errorf("%s %s assignment for node %s does not match immutable node identity", label, serviceName, assignment.Node)
		}
	}
	return nil
}

func BuildActualSnapshot(project string, environment string, targetNodes []string, actual map[string]*reconcile.ActualService) *ActualSnapshot {
	return BuildActualSnapshotWithNodes(project, environment, targetNodes, actual, nil)
}

func BuildActualSnapshotWithNodes(project string, environment string, targetNodes []string, actual map[string]*reconcile.ActualService, nodeActual map[string]map[string]*reconcile.ActualService) *ActualSnapshot {
	now := time.Now().UTC()
	snapshot := &ActualSnapshot{
		SchemaVersion: SchemaVersion,
		Project:       project,
		Environment:   environment,
		TargetNodes:   sortedCopy(targetNodes),
		Services:      make(map[string]ActualService, len(actual)),
		CapturedAt:    now,
	}

	for serviceName, service := range actual {
		if service == nil {
			continue
		}
		snapshot.Services[serviceName] = actualServiceFromReconcile(service)
	}

	if len(nodeActual) > 0 {
		snapshot.Nodes = make(map[string]ActualNodeSnapshot, len(nodeActual))
		for node, services := range nodeActual {
			nodeSnapshot := BuildNodeActualSnapshot(project, environment, node, services)
			nodeSnapshot.CapturedAt = now
			snapshot.Nodes[node] = ActualNodeSnapshot{
				Node:       node,
				Services:   nodeSnapshot.Services,
				CapturedAt: nodeSnapshot.CapturedAt,
			}
		}
	}

	return snapshot
}

func BuildNodeActualSnapshot(project string, environment string, node string, actual map[string]*reconcile.ActualService) *ActualSnapshot {
	snapshot := &ActualSnapshot{
		SchemaVersion: SchemaVersion,
		Project:       project,
		Environment:   environment,
		Node:          node,
		Services:      make(map[string]ActualService, len(actual)),
		CapturedAt:    time.Now().UTC(),
	}

	for serviceName, service := range actual {
		if service == nil {
			continue
		}
		snapshot.Services[serviceName] = actualServiceFromReconcile(service)
	}
	return snapshot
}

func actualServiceFromReconcile(service *reconcile.ActualService) ActualService {
	containers := sortedCopy(service.Containers)
	activeContainers := sortedCopy(service.ActiveContainers)
	warmingContainers := sortedCopy(service.WarmingContainers)
	warmingRevisions := sortedCopy(service.WarmingRevisions)
	return ActualService{
		Name:              service.Name,
		Image:             service.Image,
		RevisionImages:    cloneStringMap(service.RevisionImages),
		Replicas:          service.Replicas,
		Containers:        containers,
		ConfigHash:        service.ConfigHash,
		RuntimeID:         service.RuntimeID,
		Persistent:        service.Persistent,
		CurrentRevision:   service.CurrentRevision,
		PreviousRevision:  service.PreviousRevision,
		WarmingRevisions:  warmingRevisions,
		DeployStrategy:    service.DeployStrategy,
		ActiveContainers:  activeContainers,
		WarmingContainers: warmingContainers,
	}
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
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

func PersistToServers(pool *ssh.Pool, cfg *config.Config, environment string, serverNames []string, desired *DesiredRevision, actual *ActualSnapshot, nodeActual map[string]*ActualSnapshot, event Event, verbose bool) error {
	if pool == nil {
		return fmt.Errorf("ssh pool not initialized")
	}
	factory, err := nodeclient.NewFactory(cfg, pool, takodSocket(cfg))
	if err != nil {
		return err
	}

	targets := make([]statePersistTarget, len(serverNames))
	for index, serverName := range serverNames {
		server, ok := cfg.Servers[serverName]
		if !ok {
			return fmt.Errorf("server %s not found", serverName)
		}
		targets[index] = statePersistTarget{
			index:      index,
			serverName: serverName,
			server:     server,
		}
	}

	results := make([]statePersistResult, len(targets))
	resultCh := make(chan statePersistResult, len(targets))
	for _, target := range targets {
		go func(target statePersistTarget) {
			resultCh <- statePersistResult{
				index:      target.index,
				serverName: target.serverName,
				err:        persistToServer(factory, cfg, environment, target.serverName, target.server, desired, actual, nodeActual, event),
			}
		}(target)
	}

	for range targets {
		result := <-resultCh
		results[result.index] = result
	}

	if err := statePersistError(results); err != nil {
		return err
	}
	if verbose {
		for _, result := range results {
			fmt.Printf("  ✓ takod state persisted on %s\n", result.serverName)
		}
	}

	return nil
}

// PersistDesiredToServers durably records placement/config intent before any
// workload mutation. Actual snapshots remain untouched until reconciliation
// finishes, so an interrupted operation is visible as desired/actual drift.
func PersistDesiredToServers(pool *ssh.Pool, cfg *config.Config, environment string, serverNames []string, desired *DesiredRevision, event Event, verbose bool) error {
	if pool == nil {
		return fmt.Errorf("ssh pool not initialized")
	}
	if desired == nil {
		return fmt.Errorf("desired revision is required")
	}
	factory, err := nodeclient.NewFactory(cfg, pool, takodSocket(cfg))
	if err != nil {
		return err
	}
	targets := make([]statePersistTarget, len(serverNames))
	for index, serverName := range serverNames {
		server, ok := cfg.Servers[serverName]
		if !ok {
			return fmt.Errorf("server %s not found", serverName)
		}
		targets[index] = statePersistTarget{index: index, serverName: serverName, server: server}
	}
	results := make([]statePersistResult, len(targets))
	resultCh := make(chan statePersistResult, len(targets))
	for _, target := range targets {
		go func(target statePersistTarget) {
			result := statePersistResult{index: target.index, serverName: target.serverName}
			client, _, err := factory.Client(context.Background(), target.serverName)
			if err != nil {
				result.err = fmt.Errorf("failed to connect to %s for desired-state persistence: %w", target.serverName, err)
			} else {
				manager := NewManager(client, cfg, environment)
				if err := manager.WriteDesired(desired); err != nil {
					result.err = fmt.Errorf("%s: failed to write desired placement intent: %w", target.serverName, err)
				} else if err := manager.AppendEvent(event); err != nil {
					result.err = fmt.Errorf("%s: desired placement intent persisted but event append failed: %w", target.serverName, err)
				}
			}
			resultCh <- result
		}(target)
	}
	for range targets {
		result := <-resultCh
		results[result.index] = result
	}
	if err := statePersistError(results); err != nil {
		return err
	}
	if verbose {
		for _, result := range results {
			fmt.Printf("  ✓ desired placement intent persisted on %s\n", result.serverName)
		}
	}
	return nil
}

type statePersistTarget struct {
	index      int
	serverName string
	server     config.ServerConfig
}

type statePersistResult struct {
	index      int
	serverName string
	err        error
}

func persistToServer(factory *nodeclient.Factory, cfg *config.Config, environment string, serverName string, _ config.ServerConfig, desired *DesiredRevision, actual *ActualSnapshot, nodeActual map[string]*ActualSnapshot, event Event) error {
	client, _, err := factory.Client(context.Background(), serverName)
	if err != nil {
		return fmt.Errorf("failed to connect to %s for takod state persistence: %w", serverName, err)
	}
	manager := NewManager(client, cfg, environment)
	previousActual, err := manager.ReadActual()
	if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%s: failed to read previous actual state before pruning stale node state: %w", serverName, err)
	}
	if err := manager.WriteDesired(desired); err != nil {
		return fmt.Errorf("%s: failed to write desired state: %w", serverName, err)
	}
	if err := manager.WriteActual(actual); err != nil {
		return fmt.Errorf("%s: failed to write actual state: %w", serverName, err)
	}
	for nodeName, snapshot := range nodeActual {
		if err := manager.WriteNodeActual(nodeName, snapshot); err != nil {
			return fmt.Errorf("%s: failed to write actual state for node %s: %w", serverName, nodeName, err)
		}
	}
	for _, staleNode := range StaleNodeActualNames(previousActual, actual, nodeActual) {
		if err := manager.DeleteNodeActual(staleNode); err != nil {
			return fmt.Errorf("%s: failed to delete stale actual state for node %s: %w", serverName, staleNode, err)
		}
	}
	if err := manager.AppendEvent(event); err != nil {
		return fmt.Errorf("%s: failed to append event: %w", serverName, err)
	}
	return nil
}

func takodSocket(cfg *config.Config) string {
	if cfg != nil && cfg.Runtime != nil && cfg.Runtime.Agent != nil && cfg.Runtime.Agent.Socket != "" {
		return cfg.Runtime.Agent.Socket
	}
	return takodclient.DefaultSocket
}

func statePersistError(results []statePersistResult) error {
	var errs []error
	for _, result := range results {
		if result.err != nil {
			errs = append(errs, result.err)
		}
	}
	return errors.Join(errs...)
}

// StaleNodeActualNames returns previous node snapshots that are no longer active.
func StaleNodeActualNames(previous *ActualSnapshot, current *ActualSnapshot, currentNodeActual map[string]*ActualSnapshot) []string {
	if previous == nil {
		return nil
	}

	active := make(map[string]struct{})
	if current != nil {
		for _, nodeName := range current.TargetNodes {
			if nodeName != "" {
				active[nodeName] = struct{}{}
			}
		}
	}
	for nodeName := range currentNodeActual {
		if nodeName != "" {
			active[nodeName] = struct{}{}
		}
	}
	if len(active) == 0 {
		return nil
	}

	previousNodes := make(map[string]struct{})
	for _, nodeName := range previous.TargetNodes {
		if nodeName != "" {
			previousNodes[nodeName] = struct{}{}
		}
	}
	for nodeName := range previous.Nodes {
		if nodeName != "" {
			previousNodes[nodeName] = struct{}{}
		}
	}

	stale := make([]string, 0)
	for nodeName := range previousNodes {
		if _, ok := active[nodeName]; !ok {
			stale = append(stale, nodeName)
		}
	}
	sort.Strings(stale)
	return stale
}

func (m *Manager) WriteDesired(revision *DesiredRevision) error {
	if revision == nil {
		return fmt.Errorf("desired revision is nil")
	}
	content, err := marshalStateDocument(revision)
	if err != nil {
		return err
	}
	return m.writeDocument(stateDocumentDesired, revision.RevisionID, content)
}

func (m *Manager) WriteActual(snapshot *ActualSnapshot) error {
	if snapshot == nil {
		return fmt.Errorf("actual snapshot is nil")
	}
	content, err := marshalStateDocument(snapshot)
	if err != nil {
		return err
	}
	return m.writeDocument(stateDocumentActual, "", content)
}

func (m *Manager) WriteNodeActual(node string, snapshot *ActualSnapshot) error {
	if node == "" {
		return fmt.Errorf("node name is required")
	}
	if snapshot == nil {
		return fmt.Errorf("node actual snapshot is nil")
	}
	if snapshot.Node == "" {
		snapshot.Node = node
	}
	content, err := marshalStateDocument(snapshot)
	if err != nil {
		return err
	}
	return m.writeNodeDocument(stateDocumentNodeActual, node, content)
}

func (m *Manager) DeleteNodeActual(node string) error {
	if node == "" {
		return fmt.Errorf("node name is required")
	}
	request := m.documentRequest(stateDocumentNodeActual, "", "")
	request.Node = node
	_, err := m.requestJSON("DELETE", "/v1/state", request)
	return err
}

func (m *Manager) ReadDesired() (*DesiredRevision, error) {
	var revision DesiredRevision
	if err := m.readDocument(stateDocumentDesired, &revision); err != nil {
		return nil, err
	}
	return &revision, nil
}

func (m *Manager) ReadActual() (*ActualSnapshot, error) {
	var snapshot ActualSnapshot
	if err := m.readDocument(stateDocumentActual, &snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func (m *Manager) ReadNodeActual(node string) (*ActualSnapshot, error) {
	if node == "" {
		return nil, fmt.Errorf("node name is required")
	}
	var snapshot ActualSnapshot
	if err := m.readNodeDocument(stateDocumentNodeActual, node, &snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func (m *Manager) AppendEvent(event Event) error {
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
	request := m.documentRequest(stateDocumentEvent, "", string(data))
	_, err = m.requestJSON("POST", "/v1/state", request)
	return err
}

func (m *Manager) writeDocument(document string, revisionID string, content string) error {
	request := m.documentRequest(document, revisionID, content)
	_, err := m.requestJSON("PUT", "/v1/state", request)
	return err
}

func (m *Manager) writeNodeDocument(document string, node string, content string) error {
	request := m.documentRequest(document, "", content)
	request.Node = node
	_, err := m.requestJSON("PUT", "/v1/state", request)
	return err
}

func (m *Manager) readDocument(document string, value any) error {
	output, err := m.requestJSON("GET", takodclient.StateEndpoint(m.project, m.environment, document), nil)
	if err != nil {
		return err
	}
	return decodeStateDocumentResponse(output, m.project, m.environment, document, value)
}

func (m *Manager) readNodeDocument(document string, node string, value any) error {
	output, err := m.requestJSON("GET", takodclient.StateNodeEndpoint(m.project, m.environment, document, node), nil)
	if err != nil {
		return err
	}
	return decodeStateDocumentResponse(output, m.project, m.environment, document, value)
}

func (m *Manager) requestJSON(method string, endpoint string, value any) (string, error) {
	if m.requestTimeout > 0 {
		return takodclient.RequestJSONWithTimeout(m.client, m.socket, method, endpoint, value, m.requestTimeout)
	}
	return takodclient.RequestJSON(m.client, m.socket, method, endpoint, value)
}

func decodeStateDocumentResponse(output string, project string, environment string, document string, value any) error {
	var response takod.StateDocumentResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return fmt.Errorf("failed to parse takod state response: %w", err)
	}
	if !response.Found {
		return ErrNotFound
	}
	if response.Content == "" {
		return fmt.Errorf("empty takod state document %s/%s/%s", project, environment, document)
	}
	if err := json.Unmarshal([]byte(response.Content), value); err != nil {
		return fmt.Errorf("failed to parse takod state document %s/%s/%s: %w", project, environment, document, err)
	}
	return nil
}

func (m *Manager) documentRequest(document string, revisionID string, content string) takod.StateDocumentRequest {
	return takod.StateDocumentRequest{
		Project:     m.project,
		Environment: m.environment,
		Document:    document,
		RevisionID:  revisionID,
		Content:     content,
	}
}

func marshalStateDocument(value any) (string, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	return string(data), nil
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

	command, commandArgs := stateStringOrList(service.Command)
	entrypoint, entrypointArgs := stateStringOrList(service.Entrypoint)
	return DesiredService{
		APIVersion:      takoapi.APIVersionCurrent,
		Kind:            takoapi.KindDesiredServiceDocument,
		Name:            serviceName,
		WorkloadKind:    service.Kind,
		Type:            service.GetServiceType(),
		Image:           imageRef,
		ImageFrom:       service.ImageFrom,
		Build:           service.Build,
		BuildArgKeys:    sortedKeys(service.BuildArgs),
		BuildTarget:     service.BuildTarget,
		Command:         command,
		CommandArgs:     commandArgs,
		Entrypoint:      entrypoint,
		EntrypointArgs:  entrypointArgs,
		Labels:          cloneStringMap(service.Labels),
		Port:            service.Port,
		Replicas:        replicas,
		Restart:         service.Restart,
		Persistent:      service.Persistent,
		Placement:       service.Placement,
		Domains:         domains,
		Volumes:         sortedCopy(service.Volumes),
		Files:           append([]config.ServiceFileConfig(nil), service.Files...),
		EnvKeys:         sortedKeys(service.Env),
		EnvFile:         service.EnvFile != "" || len(service.EnvFiles) > 0,
		User:            service.User,
		WorkingDir:      service.WorkingDir,
		StopGracePeriod: service.StopGracePeriod,
		Init:            service.Init,
		ExtraHosts:      sortedCopy(service.ExtraHosts),
		Ulimits:         cloneUlimits(service.Ulimits),
		ShmSize:         service.ShmSize,
		SecretRefs:      sortedCopy(service.Secrets),
		DependsOn:       sortedCopy(service.DependsOn),
		HealthCheck:     service.HealthCheck,
		DeployStrategy:  service.Deploy.Strategy,
	}
}

func stateStringOrList(value config.StringOrList) (string, []string) {
	if scalar, ok := value.Scalar(); ok {
		return scalar, nil
	}
	if value.IsList() {
		return "", value.Arguments()
	}
	return "", nil
}

func cloneUlimits(source map[string]config.UlimitConfig) map[string]config.UlimitConfig {
	if len(source) == 0 {
		return nil
	}
	copy := make(map[string]config.UlimitConfig, len(source))
	for name, limit := range source {
		copy[name] = limit
	}
	return copy
}

func revisionID(revision *DesiredRevision) string {
	copyForHash := *revision
	copyForHash.RevisionID = ""
	data, _ := json.Marshal(copyForHash)
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%s-%s", revision.CreatedAt.Format("20060102T150405Z"), hex.EncodeToString(sum[:])[:12])
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
