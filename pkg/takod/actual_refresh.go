package takod

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"time"
)

const stateSchemaVersion = 1

type ActualRefreshTarget struct {
	Project     string
	Environment string
}

type persistedActualSnapshot struct {
	SchemaVersion int                                    `json:"schemaVersion"`
	Project       string                                 `json:"project"`
	Environment   string                                 `json:"environment"`
	Node          string                                 `json:"node,omitempty"`
	TargetNodes   []string                               `json:"targetNodes,omitempty"`
	Services      map[string]persistedActualService      `json:"services"`
	Nodes         map[string]persistedActualNodeSnapshot `json:"nodes,omitempty"`
	CapturedAt    time.Time                              `json:"capturedAt"`
}

type persistedActualNodeSnapshot struct {
	Node       string                            `json:"node"`
	Services   map[string]persistedActualService `json:"services"`
	CapturedAt time.Time                         `json:"capturedAt"`
}

type persistedActualService struct {
	Name       string   `json:"name"`
	Image      string   `json:"image,omitempty"`
	Replicas   int      `json:"replicas"`
	Containers []string `json:"containers,omitempty"`
	ConfigHash string   `json:"configHash,omitempty"`
	RuntimeID  string   `json:"runtimeId,omitempty"`
	Persistent bool     `json:"persistent,omitempty"`
}

func RefreshActualStateDocuments(ctx context.Context, dataDir string, node string) (int, error) {
	if dataDir == "" {
		return 0, fmt.Errorf("data directory is required")
	}
	if !isSafeRuntimeName(node) {
		return 0, fmt.Errorf("invalid node name")
	}

	targets, err := ListActualRefreshTargets(dataDir)
	if err != nil {
		return 0, err
	}

	refreshed := 0
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return refreshed, err
		}
		if locked, err := stateOperationLeaseHeld(ctx, dataDir, target.Project, target.Environment); err != nil {
			return refreshed, err
		} else if locked {
			continue
		}
		if err := refreshActualStateDocument(ctx, dataDir, node, target); err != nil {
			return refreshed, err
		}
		refreshed++
	}

	return refreshed, nil
}

func ListActualRefreshTargets(dataDir string) ([]ActualRefreshTarget, error) {
	pattern := filepath.Join(dataDir, "desired", "*", "*", "revision.json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to scan desired state documents: %w", err)
	}

	targets := make([]ActualRefreshTarget, 0, len(matches))
	for _, match := range matches {
		environment := filepath.Base(filepath.Dir(match))
		project := filepath.Base(filepath.Dir(filepath.Dir(match)))
		if !isSafeProjectName(project) || !isSafeRuntimeName(environment) {
			continue
		}
		targets = append(targets, ActualRefreshTarget{Project: project, Environment: environment})
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Project != targets[j].Project {
			return targets[i].Project < targets[j].Project
		}
		return targets[i].Environment < targets[j].Environment
	})
	return targets, nil
}

func refreshActualStateDocument(ctx context.Context, dataDir string, node string, target ActualRefreshTarget) error {
	actual, err := GatherActualState(ctx, target.Project, target.Environment)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	nodeSnapshot := persistedActualSnapshot{
		SchemaVersion: stateSchemaVersion,
		Project:       target.Project,
		Environment:   target.Environment,
		Node:          node,
		Services:      persistedServicesFromActual(actual.Services),
		CapturedAt:    now,
	}
	nodeContent, err := marshalJSONDocument(nodeSnapshot)
	if err != nil {
		return err
	}
	if _, err := WriteStateDocument(ctx, dataDir, StateDocumentRequest{
		Project:     target.Project,
		Environment: target.Environment,
		Document:    stateDocumentActualNode,
		Node:        node,
		Content:     nodeContent,
	}); err != nil {
		return err
	}

	aggregate := readPersistedActualAggregate(ctx, dataDir, target)
	if aggregate.SchemaVersion == 0 {
		aggregate.SchemaVersion = stateSchemaVersion
	}
	aggregate.Project = target.Project
	aggregate.Environment = target.Environment
	if aggregate.Nodes == nil {
		aggregate.Nodes = make(map[string]persistedActualNodeSnapshot)
	}
	aggregate.Nodes[node] = persistedActualNodeSnapshot{
		Node:       node,
		Services:   clonePersistedServices(nodeSnapshot.Services),
		CapturedAt: now,
	}
	recomputePersistedAggregate(&aggregate)
	aggregateContent, err := marshalJSONDocument(aggregate)
	if err != nil {
		return err
	}
	if _, err := WriteStateDocument(ctx, dataDir, StateDocumentRequest{
		Project:     target.Project,
		Environment: target.Environment,
		Document:    stateDocumentActual,
		Content:     aggregateContent,
	}); err != nil {
		return err
	}
	return nil
}

func stateOperationLeaseHeld(ctx context.Context, dataDir string, project string, environment string) (bool, error) {
	response, err := ReadLease(ctx, dataDir, LeaseRequest{
		Project:     project,
		Environment: environment,
	})
	if err != nil {
		return false, err
	}
	return response != nil && response.Found, nil
}

func readPersistedActualAggregate(ctx context.Context, dataDir string, target ActualRefreshTarget) persistedActualSnapshot {
	response, err := ReadStateDocument(ctx, dataDir, StateDocumentRequest{
		Project:     target.Project,
		Environment: target.Environment,
		Document:    stateDocumentActual,
	})
	if err != nil || response == nil || !response.Found || response.Content == "" {
		return persistedActualSnapshot{}
	}
	var snapshot persistedActualSnapshot
	if err := json.Unmarshal([]byte(response.Content), &snapshot); err != nil {
		return persistedActualSnapshot{}
	}
	return snapshot
}

func recomputePersistedAggregate(snapshot *persistedActualSnapshot) {
	nodes := make([]string, 0, len(snapshot.Nodes))
	for node := range snapshot.Nodes {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	snapshot.TargetNodes = nodes
	snapshot.Services = make(map[string]persistedActualService)

	var newest time.Time
	for _, node := range nodes {
		nodeSnapshot := snapshot.Nodes[node]
		if nodeSnapshot.CapturedAt.After(newest) {
			newest = nodeSnapshot.CapturedAt
		}
		for serviceName, service := range nodeSnapshot.Services {
			if existing, ok := snapshot.Services[serviceName]; ok {
				existing.Replicas += service.Replicas
				existing.Containers = append(existing.Containers, service.Containers...)
				if existing.Image == "" {
					existing.Image = service.Image
				}
				if existing.ConfigHash == "" {
					existing.ConfigHash = service.ConfigHash
				} else if service.ConfigHash != "" && existing.ConfigHash != service.ConfigHash {
					existing.ConfigHash = ""
				}
				existing.RuntimeID = mergeRuntimeID(existing.RuntimeID, service.RuntimeID)
				existing.Persistent = existing.Persistent || service.Persistent
				snapshot.Services[serviceName] = existing
				continue
			}
			service.Containers = append([]string(nil), service.Containers...)
			snapshot.Services[serviceName] = service
		}
	}
	if newest.IsZero() {
		newest = time.Now().UTC()
	}
	snapshot.CapturedAt = newest
}

func persistedServicesFromActual(services map[string]*ActualService) map[string]persistedActualService {
	out := make(map[string]persistedActualService, len(services))
	for serviceName, service := range services {
		if service == nil {
			continue
		}
		containers := append([]string(nil), service.Containers...)
		sort.Strings(containers)
		out[serviceName] = persistedActualService{
			Name:       service.Name,
			Image:      service.Image,
			Replicas:   service.Replicas,
			Containers: containers,
			ConfigHash: service.ConfigHash,
			RuntimeID:  service.RuntimeID,
			Persistent: service.Persistent,
		}
	}
	return out
}

func clonePersistedServices(services map[string]persistedActualService) map[string]persistedActualService {
	if len(services) == 0 {
		return nil
	}
	out := make(map[string]persistedActualService, len(services))
	for name, service := range services {
		service.Containers = append([]string(nil), service.Containers...)
		out[name] = service
	}
	return out
}

func marshalJSONDocument(value any) (string, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	return string(data), nil
}
