package takodstate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/ssh"
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
	Services      map[string]DesiredService `json:"services"`
	Imports       map[string]DesiredImport  `json:"imports,omitempty"`
	Exports       []DesiredExportRecord     `json:"exports,omitempty"`
	Git           GitInfo                   `json:"git,omitempty"`
	CreatedAt     time.Time                 `json:"createdAt"`
}

type DesiredService struct {
	Name           string                      `json:"name"`
	Type           string                      `json:"type"`
	Image          string                      `json:"image,omitempty"`
	Build          string                      `json:"build,omitempty"`
	Dockerfile     string                      `json:"dockerfile,omitempty"`
	Platform       string                      `json:"platform,omitempty"`
	Command        string                      `json:"command,omitempty"`
	Port           int                         `json:"port,omitempty"`
	Ports          []config.PortConfig         `json:"ports,omitempty"`
	Replicas       int                         `json:"replicas"`
	Restart        string                      `json:"restart,omitempty"`
	Persistent     bool                        `json:"persistent,omitempty"`
	Placement      *config.PlacementConfig     `json:"placement,omitempty"`
	Domains        []string                    `json:"domains,omitempty"`
	Volumes        []string                    `json:"volumes,omitempty"`
	Configs        []DesiredConfigFileMount    `json:"configs,omitempty"`
	EnvKeys        []string                    `json:"envKeys,omitempty"`
	EnvFile        bool                        `json:"envFile,omitempty"`
	SecretRefs     []string                    `json:"secretRefs,omitempty"`
	DependsOn      []string                    `json:"dependsOn,omitempty"`
	Export         *config.ServiceExportConfig `json:"export,omitempty"`
	HealthCheck    config.HealthCheckConfig    `json:"healthCheck,omitempty"`
	DeployStrategy string                      `json:"deployStrategy,omitempty"`
	DeployOrder    string                      `json:"deployOrder,omitempty"`
	MaxUnavailable int                         `json:"maxUnavailable,omitempty"`
	DeployMonitor  string                      `json:"deployMonitor,omitempty"`
	Hooks          config.HooksConfig          `json:"hooks,omitempty"`
}

type DesiredImport struct {
	Project     string   `json:"project"`
	Environment string   `json:"environment"`
	Service     string   `json:"service"`
	Port        string   `json:"port"`
	Servers     []string `json:"servers,omitempty"`
}

type DesiredExportRecord struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Service     string `json:"service"`
	Port        string `json:"port"`
	Target      int    `json:"target"`
	Protocol    string `json:"protocol,omitempty"`
}

type DesiredConfigFileMount struct {
	Source      string `json:"source"`
	Target      string `json:"target"`
	Mode        string `json:"mode,omitempty"`
	ContentHash string `json:"contentHash,omitempty"`
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
	Name                  string   `json:"name"`
	Image                 string   `json:"image,omitempty"`
	Replicas              int      `json:"replicas"`
	Containers            []string `json:"containers,omitempty"`
	ConfigHash            string   `json:"configHash,omitempty"`
	RuntimeID             string   `json:"runtimeId,omitempty"`
	HealthyReplicas       int      `json:"healthyReplicas,omitempty"`
	UnhealthyReplicas     int      `json:"unhealthyReplicas,omitempty"`
	StartingReplicas      int      `json:"startingReplicas,omitempty"`
	NoHealthcheckReplicas int      `json:"noHealthcheckReplicas,omitempty"`
	UnknownHealthReplicas int      `json:"unknownHealthReplicas,omitempty"`
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
	socket      string
	project     string
	environment string
}

func NewManager(client *ssh.Client, cfg *config.Config, environment string) *Manager {
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

func BuildDesiredRevision(cfg *config.Config, environment string, source string, services map[string]config.ServiceConfig, imageRefs map[string]string, targetNodes []string, git GitInfo) (*DesiredRevision, error) {
	now := time.Now().UTC()
	revision := &DesiredRevision{
		SchemaVersion: SchemaVersion,
		Project:       cfg.Project.Name,
		Environment:   environment,
		Source:        source,
		TargetNodes:   sortedCopy(targetNodes),
		Services:      make(map[string]DesiredService, len(services)),
		Imports:       sanitizeDesiredImports(cfg.Imports),
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

	revision.Exports = buildDesiredExportRecords(cfg.Project.Name, environment, services)
	revision.RevisionID = revisionID(revision)
	return revision, nil
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
	return ActualService{
		Name:                  service.Name,
		Image:                 service.Image,
		Replicas:              service.Replicas,
		Containers:            containers,
		ConfigHash:            service.ConfigHash,
		RuntimeID:             service.RuntimeID,
		HealthyReplicas:       service.HealthyReplicas,
		UnhealthyReplicas:     service.UnhealthyReplicas,
		StartingReplicas:      service.StartingReplicas,
		NoHealthcheckReplicas: service.NoHealthcheckReplicas,
		UnknownHealthReplicas: service.UnknownHealthReplicas,
	}
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
				err:        persistToServer(pool, cfg, environment, target.serverName, target.server, desired, actual, nodeActual, event),
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

func persistToServer(pool *ssh.Pool, cfg *config.Config, environment string, serverName string, server config.ServerConfig, desired *DesiredRevision, actual *ActualSnapshot, nodeActual map[string]*ActualSnapshot, event Event) error {
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
	for nodeName, snapshot := range nodeActual {
		if err := manager.WriteNodeActual(nodeName, snapshot); err != nil {
			return fmt.Errorf("%s: failed to write actual state for node %s: %w", serverName, nodeName, err)
		}
	}
	if err := manager.AppendEvent(event); err != nil {
		return fmt.Errorf("%s: failed to append event: %w", serverName, err)
	}
	return nil
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
	_, err = takodclient.RequestJSON(m.client, m.socket, "POST", "/v1/state", request)
	return err
}

func (m *Manager) writeDocument(document string, revisionID string, content string) error {
	request := m.documentRequest(document, revisionID, content)
	_, err := takodclient.RequestJSON(m.client, m.socket, "PUT", "/v1/state", request)
	return err
}

func (m *Manager) writeNodeDocument(document string, node string, content string) error {
	request := m.documentRequest(document, "", content)
	request.Node = node
	_, err := takodclient.RequestJSON(m.client, m.socket, "PUT", "/v1/state", request)
	return err
}

func (m *Manager) readDocument(document string, value any) error {
	output, err := takodclient.RequestJSON(m.client, m.socket, "GET", takodclient.StateEndpoint(m.project, m.environment, document), nil)
	if err != nil {
		return err
	}
	return decodeStateDocumentResponse(output, m.project, m.environment, document, value)
}

func (m *Manager) readNodeDocument(document string, node string, value any) error {
	output, err := takodclient.RequestJSON(m.client, m.socket, "GET", takodclient.StateNodeEndpoint(m.project, m.environment, document, node), nil)
	if err != nil {
		return err
	}
	return decodeStateDocumentResponse(output, m.project, m.environment, document, value)
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

	domains := serviceDomains(service)

	return DesiredService{
		Name:           serviceName,
		Type:           service.GetServiceType(),
		Image:          imageRef,
		Build:          service.Build,
		Dockerfile:     service.Dockerfile,
		Platform:       service.Platform,
		Command:        service.Command,
		Port:           service.Port,
		Ports:          sortedPortConfigs(service.Ports),
		Replicas:       replicas,
		Restart:        service.Restart,
		Persistent:     service.Persistent,
		Placement:      service.Placement,
		Domains:        domains,
		Volumes:        sortedCopy(service.Volumes),
		Configs:        sanitizeDesiredConfigFiles(service.Configs),
		EnvKeys:        sortedKeys(service.Env),
		EnvFile:        service.EnvFile != "",
		SecretRefs:     sortedCopy(service.Secrets),
		DependsOn:      sortedCopy(service.DependsOn),
		Export:         sanitizeDesiredExport(service.Export),
		HealthCheck:    service.HealthCheck,
		DeployStrategy: service.Deploy.Strategy,
		DeployOrder:    service.Deploy.Order,
		MaxUnavailable: service.Deploy.MaxUnavailable,
		DeployMonitor:  service.Deploy.Monitor,
		Hooks:          sanitizeDesiredHooks(service.Hooks),
	}
}

func sanitizeDesiredImports(imports map[string]config.ImportConfig) map[string]DesiredImport {
	if len(imports) == 0 {
		return nil
	}
	out := make(map[string]DesiredImport, len(imports))
	for alias, importConfig := range imports {
		out[alias] = DesiredImport{
			Project:     importConfig.Project,
			Environment: importConfig.Environment,
			Service:     importConfig.Service,
			Port:        importConfig.Port,
			Servers:     sortedCopy(importConfig.Servers),
		}
	}
	return out
}

func sanitizeDesiredExport(export *config.ServiceExportConfig) *config.ServiceExportConfig {
	if export == nil {
		return nil
	}
	clone := &config.ServiceExportConfig{}
	if len(export.Ports) > 0 {
		clone.Ports = make(map[string]int, len(export.Ports))
		for name, target := range export.Ports {
			clone.Ports[name] = target
		}
	}
	return clone
}

func buildDesiredExportRecords(project string, environment string, services map[string]config.ServiceConfig) []DesiredExportRecord {
	if len(services) == 0 {
		return nil
	}
	var records []DesiredExportRecord
	serviceNames := make([]string, 0, len(services))
	for serviceName := range services {
		serviceNames = append(serviceNames, serviceName)
	}
	sort.Strings(serviceNames)
	for _, serviceName := range serviceNames {
		service := services[serviceName]
		if service.Export == nil || len(service.Export.Ports) == 0 {
			continue
		}
		protocolByTarget := exportProtocolsByTarget(service)
		portNames := make([]string, 0, len(service.Export.Ports))
		for portName := range service.Export.Ports {
			portNames = append(portNames, portName)
		}
		sort.Strings(portNames)
		for _, portName := range portNames {
			target := service.Export.Ports[portName]
			records = append(records, DesiredExportRecord{
				Project:     project,
				Environment: environment,
				Service:     serviceName,
				Port:        portName,
				Target:      target,
				Protocol:    protocolByTarget[target],
			})
		}
	}
	return records
}

func exportProtocolsByTarget(service config.ServiceConfig) map[int]string {
	out := make(map[int]string)
	for _, port := range service.EffectivePorts() {
		if port.Target <= 0 {
			continue
		}
		if _, exists := out[port.Target]; exists {
			continue
		}
		out[port.Target] = port.Protocol
	}
	return out
}

func sanitizeDesiredConfigFiles(configs []config.ServiceConfigFileMount) []DesiredConfigFileMount {
	if len(configs) == 0 {
		return nil
	}
	out := make([]DesiredConfigFileMount, 0, len(configs))
	for _, cfg := range configs {
		out = append(out, DesiredConfigFileMount{
			Source:      cfg.Source,
			Target:      cfg.Target,
			Mode:        cfg.Mode,
			ContentHash: cfg.ContentHash,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Source == out[j].Source {
			return out[i].Target < out[j].Target
		}
		return out[i].Source < out[j].Source
	})
	return out
}

func serviceDomains(service config.ServiceConfig) []string {
	var domains []string
	for _, port := range service.EffectivePorts() {
		if port.Proxy == nil {
			continue
		}
		domains = append(domains, port.Proxy.GetAllDomains()...)
		domains = append(domains, port.Proxy.GetRedirectDomains()...)
	}
	return sortedCopy(domains)
}

func sortedPortConfigs(ports []config.PortConfig) []config.PortConfig {
	if len(ports) == 0 {
		return nil
	}
	service := config.ServiceConfig{Ports: ports}
	out := service.EffectivePorts()
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].Target < out[j].Target
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func sanitizeDesiredHooks(hooks config.HooksConfig) config.HooksConfig {
	return config.HooksConfig{
		PreDeploy:  sanitizeDesiredHook(hooks.PreDeploy),
		PostDeploy: sanitizeDesiredHook(hooks.PostDeploy),
	}
}

func sanitizeDesiredHook(hook *config.HookConfig) *config.HookConfig {
	if hook == nil {
		return nil
	}
	return &config.HookConfig{
		Command:    hook.Command,
		Timeout:    hook.Timeout,
		User:       hook.User,
		WorkingDir: hook.WorkingDir,
	}
}

func revisionID(revision *DesiredRevision) string {
	copyForHash := *revision
	copyForHash.RevisionID = ""
	data, _ := json.Marshal(copyForHash)
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%s-%s", revision.CreatedAt.Format("20060102T150405Z"), hex.EncodeToString(sum[:])[:12])
}

func sortedKeys(values map[string]config.EnvValue) []string {
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
