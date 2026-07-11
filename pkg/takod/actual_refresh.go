package takod

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
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
	Health            string            `json:"health,omitempty"`
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
				existing.RevisionImages = mergeRevisionImageMaps(existing.RevisionImages, service.RevisionImages)
				if existing.ConfigHash == "" {
					existing.ConfigHash = service.ConfigHash
				} else if service.ConfigHash != "" && existing.ConfigHash != service.ConfigHash {
					existing.ConfigHash = ""
				}
				existing.RuntimeID = mergeRuntimeID(existing.RuntimeID, service.RuntimeID)
				existing.Persistent = existing.Persistent || service.Persistent
				existing.CurrentRevision = mergeOptionalLabel(existing.CurrentRevision, service.CurrentRevision)
				existing.PreviousRevision = mergeOptionalLabel(existing.PreviousRevision, service.PreviousRevision)
				existing.WarmingRevisions = mergeRevisionLists(existing.WarmingRevisions, service.WarmingRevisions)
				existing.DeployStrategy = mergeOptionalLabel(existing.DeployStrategy, service.DeployStrategy)
				existing.ActiveContainers = append(existing.ActiveContainers, service.ActiveContainers...)
				existing.WarmingContainers = append(existing.WarmingContainers, service.WarmingContainers...)
				existing.Health = MergeHealthStates(existing.Health, service.Health)
				snapshot.Services[serviceName] = existing
				continue
			}
			service.Containers = append([]string(nil), service.Containers...)
			service.ActiveContainers = append([]string(nil), service.ActiveContainers...)
			service.WarmingContainers = append([]string(nil), service.WarmingContainers...)
			service.WarmingRevisions = append([]string(nil), service.WarmingRevisions...)
			service.RevisionImages = cloneStringMap(service.RevisionImages)
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
		containers := sortedCopy(service.Containers)
		activeContainers := sortedCopy(service.ActiveContainers)
		warmingContainers := sortedCopy(service.WarmingContainers)
		warmingRevisions := sortedCopy(service.WarmingRevisions)
		out[serviceName] = persistedActualService{
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
			Health:            service.Health,
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
		service.ActiveContainers = append([]string(nil), service.ActiveContainers...)
		service.WarmingContainers = append([]string(nil), service.WarmingContainers...)
		service.WarmingRevisions = append([]string(nil), service.WarmingRevisions...)
		service.RevisionImages = cloneStringMap(service.RevisionImages)
		out[name] = service
	}
	return out
}

func mergeRevisionLists(existing []string, incoming []string) []string {
	if len(incoming) == 0 {
		return existing
	}
	out := append([]string(nil), existing...)
	for _, revision := range incoming {
		revision = strings.TrimSpace(revision)
		if revision == "" {
			continue
		}
		found := false
		for _, current := range out {
			if current == revision {
				found = true
				break
			}
		}
		if !found {
			out = append(out, revision)
		}
	}
	sort.Strings(out)
	return out
}

func mergeRevisionImageMaps(existing map[string]string, incoming map[string]string) map[string]string {
	if len(incoming) == 0 {
		return existing
	}
	out := cloneStringMap(existing)
	if out == nil {
		out = make(map[string]string)
	}
	for revision, image := range incoming {
		revision = strings.TrimSpace(revision)
		image = strings.TrimSpace(image)
		if revision == "" || image == "" {
			continue
		}
		if current := out[revision]; current != "" && current != image {
			out[revision] = ""
			continue
		}
		out[revision] = image
	}
	return out
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

func sortedCopy(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := append([]string(nil), values...)
	sort.Strings(out)
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
