package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/redentordev/tako-cli/pkg/takodstate"
)

// Preview result document kinds.
const (
	KindPreviewList          = "PreviewList"
	KindPreviewDestroyResult = "PreviewDestroyResult"
	KindPreviewPruneResult   = "PreviewPruneResult"
)

// Preview statuses.
const (
	PreviewStatusActive  = "active"
	PreviewStatusExpired = "expired"

	PreviewDestroyStatusDestroyed = "destroyed"
	PreviewDestroyStatusNotFound  = "not_found"
	PreviewDestroyStatusFailed    = "failed"

	previewNodeStatusOK      = "ok"
	previewNodeStatusSkipped = "skipped"
	previewNodeStatusFailed  = "failed"
)

// DeployPreviewOptions carries the preview flags of `tako deploy --preview`.
// The preview itself is the instance the request's Config was loaded for.
type DeployPreviewOptions struct {
	// TTL overrides the template ttl for this deploy only; empty uses the
	// template value.
	TTL string
	// NoPrune skips the opportunistic prune of expired previews.
	NoPrune bool
	// CreatedBy is stored on a new record; empty derives "ci" or the user.
	CreatedBy string
	// LoadInstance loads the config for another preview of base during the
	// opportunistic prune. Nil prunes from records and replicated state only.
	LoadInstance PreviewInstanceLoader
}

// PreviewInstanceLoader loads the config of preview name created from base.
type PreviewInstanceLoader func(base string, name string) (*config.Config, error)

// DeployPreviewResult is DeployResult.preview.
type DeployPreviewResult struct {
	Name        string            `json:"name"`
	Environment string            `json:"environment"`
	Base        string            `json:"base"`
	Created     bool              `json:"created"`
	ExpiresAt   *time.Time        `json:"expiresAt,omitempty"`
	TTL         string            `json:"ttl"`
	Hosts       map[string]string `json:"hosts,omitempty"`
}

// PreviewListItem is one merged preview record.
type PreviewListItem struct {
	takod.PreviewRecord
	// Status is active or expired.
	Status string `json:"status"`
	// Nodes lists the servers holding the record.
	Nodes []string `json:"nodes"`
}

// PreviewNodeStatus reports one server queried by a preview listing.
type PreviewNodeStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// PreviewListResult is the result document of `tako preview list`.
type PreviewListResult struct {
	APIVersion string              `json:"apiVersion"`
	Kind       string              `json:"kind"`
	Project    string              `json:"project"`
	Items      []PreviewListItem   `json:"items"`
	Nodes      []PreviewNodeStatus `json:"nodes"`
	// Invalid lists environments whose record could not be parsed.
	Invalid []string `json:"invalid,omitempty"`
}

// PreviewDestroyRequest describes `tako preview destroy <name>`. Config is
// the project config loaded without a preview instance.
type PreviewDestroyRequest struct {
	Config       *config.Config
	Name         string
	LoadInstance PreviewInstanceLoader
	Verbose      bool
}

// PreviewDestroyServer reports the teardown on one server.
type PreviewDestroyServer struct {
	Name              string `json:"name"`
	Status            string `json:"status"`
	ContainersRemoved int    `json:"containersRemoved,omitempty"`
	NetworksRemoved   int    `json:"networksRemoved,omitempty"`
	VolumesRemoved    int    `json:"volumesRemoved,omitempty"`
	Error             string `json:"error,omitempty"`
}

// PreviewDestroyResult is the result document of `tako preview destroy`.
type PreviewDestroyResult struct {
	APIVersion  string                 `json:"apiVersion"`
	Kind        string                 `json:"kind"`
	Project     string                 `json:"project"`
	Name        string                 `json:"name"`
	Environment string                 `json:"environment"`
	Base        string                 `json:"base,omitempty"`
	Status      string                 `json:"status"`
	Servers     []PreviewDestroyServer `json:"servers"`
	// VolumesRemoved totals the preview volumes removed on every server.
	VolumesRemoved int       `json:"volumesRemoved"`
	StartedAt      time.Time `json:"startedAt"`
	Duration       float64   `json:"durationSeconds"`
	Error          string    `json:"error,omitempty"`
}

// PreviewPruneRequest describes `tako preview prune`.
type PreviewPruneRequest struct {
	Config       *config.Config
	DryRun       bool
	LoadInstance PreviewInstanceLoader
	// Now overrides the clock in tests.
	Now time.Time
}

// PreviewPruneEntry names one preview considered by prune.
type PreviewPruneEntry struct {
	Name        string     `json:"name"`
	Environment string     `json:"environment"`
	Base        string     `json:"base"`
	ExpiresAt   *time.Time `json:"expiresAt,omitempty"`
	Reason      string     `json:"reason,omitempty"`
}

// PreviewPruneResult is the result document of `tako preview prune`.
type PreviewPruneResult struct {
	APIVersion string              `json:"apiVersion"`
	Kind       string              `json:"kind"`
	Project    string              `json:"project"`
	DryRun     bool                `json:"dryRun,omitempty"`
	Destroyed  []PreviewPruneEntry `json:"destroyed"`
	Skipped    []PreviewPruneEntry `json:"skipped"`
	Failed     []PreviewPruneEntry `json:"failed"`
	Nodes      []PreviewNodeStatus `json:"nodes"`
	StartedAt  time.Time           `json:"startedAt"`
	Duration   float64             `json:"durationSeconds"`
	Error      string              `json:"error,omitempty"`
}

// previewRuntime is the node access preview operations use. Tests replace
// it through Engine.previewRuntimeOverride.
type previewRuntime struct {
	clientFor func(ctx context.Context, server string) (any, error)
	// acquire takes the operation leases of env on servers and returns a
	// context cancelled on lease loss plus the release function.
	acquire func(ctx context.Context, env string, servers []string, operation string) (context.Context, func(), error)
	close   func()
}

func (e *Engine) newPreviewRuntime(cfg *config.Config) (*previewRuntime, error) {
	if e != nil && e.previewRuntimeOverride != nil {
		return e.previewRuntimeOverride(cfg)
	}
	pool := ssh.NewPool()
	factory, err := nodeclient.NewFactory(cfg, pool, TakodSocketFromConfig(cfg))
	if err != nil {
		pool.CloseAll()
		return nil, err
	}
	return &previewRuntime{
		clientFor: func(ctx context.Context, server string) (any, error) {
			client, _, err := factory.Client(ctx, server)
			if err != nil {
				return nil, &ConnectivityError{Server: server, Err: fmt.Errorf("failed to connect to %s: %w", server, err)}
			}
			return client, nil
		},
		acquire: func(ctx context.Context, env string, servers []string, operation string) (context.Context, func(), error) {
			set, err := AcquireRemoteOperationLeasesContext(ctx, pool, cfg, env, servers, operation)
			if err != nil {
				return nil, nil, err
			}
			set.SetWarnFunc(func(message string) {
				e.debug(events.TypeWarning, events.PhaseCleanup, message)
			})
			leaseCtx, cancel := set.BindContext(ctx)
			return leaseCtx, func() {
				cancel()
				set.Release()
			}, nil
		},
		close: func() {
			factory.CloseIdleConnections()
			pool.CloseAll()
		},
	}, nil
}

// takodHasCapability reports whether the node advertises capability. Only
// a failure to read the status is an error.
func takodHasCapability(ctx context.Context, client any, cfg *config.Config, capability string) (bool, error) {
	output, err := takodclient.RequestJSONWithContext(ctx, client, TakodSocketFromConfig(cfg), "GET", "/v1/status", nil)
	if err != nil {
		return false, fmt.Errorf("read takod capabilities: %w", err)
	}
	var status takod.Status
	if err := json.Unmarshal([]byte(output), &status); err != nil {
		return false, fmt.Errorf("parse takod capabilities: %w", err)
	}
	for _, advertised := range status.Capabilities {
		if advertised == capability {
			return true, nil
		}
	}
	return false, nil
}

// requirePreviewCapability fails closed when the node lacks
// preview.lifecycle-v1. The error is an invalid request (exit 2).
func requirePreviewCapability(ctx context.Context, client any, cfg *config.Config, server string) error {
	ok, err := takodHasCapability(ctx, client, cfg, takod.CapabilityPreviewLifecycleV1)
	if err != nil {
		return fmt.Errorf("%s: %w", server, err)
	}
	if !ok {
		return invalidRequestf("server %s: takod does not support preview environments (missing capability %s); upgrade the node agent", server, takod.CapabilityPreviewLifecycleV1)
	}
	return nil
}

func readPreviewListing(ctx context.Context, client any, cfg *config.Config) (*takod.PreviewListResponse, error) {
	output, err := takodclient.RequestJSONWithContext(ctx, client, TakodSocketFromConfig(cfg), "GET", "/v1/state/previews?project="+url.QueryEscape(cfg.Project.Name), nil)
	if err != nil {
		return nil, err
	}
	var response takod.PreviewListResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("parse preview listing: %w", err)
	}
	return &response, nil
}

func writePreviewRecord(ctx context.Context, client any, cfg *config.Config, record *takod.PreviewRecord) error {
	content, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = takodclient.RequestJSONWithContext(ctx, client, TakodSocketFromConfig(cfg), "PUT", "/v1/state", takod.StateDocumentRequest{
		Project:     record.Project,
		Environment: record.Environment,
		Document:    "preview",
		Content:     string(content),
	})
	return err
}

// previewListing is the merged listing of every queried server.
type previewListing struct {
	items   []PreviewListItem
	nodes   []PreviewNodeStatus
	invalid []string
	failed  int
}

func (l *previewListing) find(environment string) *PreviewListItem {
	for index := range l.items {
		if l.items[index].Environment == environment {
			return &l.items[index]
		}
	}
	return nil
}

// collectPreviewListing queries every server, skips reachable servers
// without preview.lifecycle-v1, and merges records by identity keeping the
// newest updatedAt.
func collectPreviewListing(ctx context.Context, cfg *config.Config, servers []string, clientFor func(context.Context, string) (any, error), now time.Time) *previewListing {
	listing := &previewListing{}
	merged := map[string]*PreviewListItem{}
	invalid := map[string]bool{}
	for _, server := range servers {
		client, err := clientFor(ctx, server)
		if err != nil {
			listing.nodes = append(listing.nodes, PreviewNodeStatus{Name: server, Status: previewNodeStatusFailed, Error: err.Error()})
			listing.failed++
			continue
		}
		capable, err := takodHasCapability(ctx, client, cfg, takod.CapabilityPreviewLifecycleV1)
		if err != nil {
			listing.nodes = append(listing.nodes, PreviewNodeStatus{Name: server, Status: previewNodeStatusFailed, Error: err.Error()})
			listing.failed++
			continue
		}
		if !capable {
			listing.nodes = append(listing.nodes, PreviewNodeStatus{Name: server, Status: previewNodeStatusSkipped, Error: fmt.Sprintf("takod lacks capability %s; it cannot hold previews", takod.CapabilityPreviewLifecycleV1)})
			continue
		}
		response, err := readPreviewListing(ctx, client, cfg)
		if err != nil {
			listing.nodes = append(listing.nodes, PreviewNodeStatus{Name: server, Status: previewNodeStatusFailed, Error: err.Error()})
			listing.failed++
			continue
		}
		listing.nodes = append(listing.nodes, PreviewNodeStatus{Name: server, Status: previewNodeStatusOK})
		for _, name := range response.Invalid {
			invalid[name] = true
		}
		for _, record := range response.Previews {
			if record.Project != cfg.Project.Name || !takod.IsPreviewEnvironment(record.Environment) {
				continue
			}
			existing := merged[record.Environment]
			if existing == nil {
				merged[record.Environment] = &PreviewListItem{PreviewRecord: record, Nodes: []string{server}}
				continue
			}
			existing.Nodes = append(existing.Nodes, server)
			if record.UpdatedAt.After(existing.UpdatedAt) {
				existing.PreviewRecord = record
			}
		}
	}
	for _, item := range merged {
		item.Status = previewStatus(item.ExpiresAt, now)
		sort.Strings(item.Nodes)
		listing.items = append(listing.items, *item)
	}
	sort.Slice(listing.items, func(i, j int) bool { return listing.items[i].Environment < listing.items[j].Environment })
	for name := range invalid {
		listing.invalid = append(listing.invalid, name)
	}
	sort.Strings(listing.invalid)
	return listing
}

func previewStatus(expiresAt *time.Time, now time.Time) string {
	if expiresAt != nil && expiresAt.Before(now) {
		return PreviewStatusExpired
	}
	return PreviewStatusActive
}

func previewListingError(listing *previewListing) error {
	if listing.failed == 0 {
		return nil
	}
	var failed []string
	for _, node := range listing.nodes {
		if node.Status == previewNodeStatusFailed {
			failed = append(failed, node.Name)
		}
	}
	return &AttentionError{Err: fmt.Errorf("preview records could not be read from %s", strings.Join(failed, ", "))}
}

func sortedServerNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ListPreviews lists the previews of the project from every server in the
// top-level servers: map. Unreachable servers make the result partial
// (attention, exit 6); servers without preview.lifecycle-v1 are skipped
// with a warning.
func (e *Engine) ListPreviews(ctx context.Context, cfg *config.Config) (*PreviewListResult, error) {
	if cfg == nil {
		return nil, invalidRequestf("preview list requires a loaded config")
	}
	runtime, err := e.newPreviewRuntime(cfg)
	if err != nil {
		return nil, err
	}
	defer runtime.close()
	listing := collectPreviewListing(ctx, cfg, sortedServerNames(cfg), runtime.clientFor, time.Now())
	e.warnPreviewNodes(listing)
	result := &PreviewListResult{
		APIVersion: takoapi.APIVersionCurrent,
		Kind:       KindPreviewList,
		Project:    cfg.Project.Name,
		Items:      append([]PreviewListItem{}, listing.items...),
		Nodes:      listing.nodes,
		Invalid:    listing.invalid,
	}
	return result, previewListingError(listing)
}

func (e *Engine) warnPreviewNodes(listing *previewListing) {
	for _, node := range listing.nodes {
		switch node.Status {
		case previewNodeStatusSkipped:
			e.warn(events.PhasePlan, fmt.Sprintf("⚠ %s skipped: %s\n", node.Name, node.Error))
		case previewNodeStatusFailed:
			e.warn(events.PhasePlan, fmt.Sprintf("⚠ %s unreachable for preview listing: %s\n", node.Name, node.Error))
		}
	}
	for _, name := range listing.invalid {
		e.warn(events.PhasePlan, fmt.Sprintf("⚠ preview record of %s is unreadable\n", name))
	}
}

// DestroyPreview tears down one preview on every server it may occupy.
func (e *Engine) DestroyPreview(ctx context.Context, req PreviewDestroyRequest) (*PreviewDestroyResult, error) {
	if req.Config == nil {
		return nil, invalidRequestf("preview destroy requires a loaded config")
	}
	cfg := req.Config
	if err := RequireTakodRuntime(cfg); err != nil {
		return nil, err
	}
	name, err := config.NormalizePreviewName(req.Name)
	if err != nil {
		return nil, &InvalidRequestError{Err: err}
	}
	identity := config.PreviewIdentity(name)
	result := &PreviewDestroyResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindPreviewDestroyResult,
		Project:     cfg.Project.Name,
		Name:        name,
		Environment: identity,
		Servers:     []PreviewDestroyServer{},
		StartedAt:   time.Now(),
	}
	finish := func(err error) (*PreviewDestroyResult, error) {
		result.Duration = time.Since(result.StartedAt).Seconds()
		if err != nil {
			result.Error = e.RedactSensitive(err.Error())
		}
		return result, err
	}
	runtime, err := e.newPreviewRuntime(cfg)
	if err != nil {
		result.Status = PreviewDestroyStatusFailed
		return finish(err)
	}
	defer runtime.close()
	listing := collectPreviewListing(ctx, cfg, sortedServerNames(cfg), runtime.clientFor, time.Now())
	e.warnPreviewNodes(listing)
	item := listing.find(identity)
	if item == nil {
		if err := previewListingError(listing); err != nil {
			result.Status = PreviewDestroyStatusFailed
			return finish(fmt.Errorf("cannot tell whether preview %s exists: %w", name, err))
		}
		result.Status = PreviewDestroyStatusNotFound
		e.info(events.TypeLogLine, events.PhaseCleanup, fmt.Sprintf("Preview %s not found; nothing to destroy.\n", name))
		return finish(nil)
	}
	result.Base = item.Base
	outcome, err := e.teardownPreview(ctx, cfg, runtime, item, req.LoadInstance, false, time.Now())
	result.Servers = outcome.servers
	result.VolumesRemoved = outcome.volumes
	if err != nil {
		result.Status = PreviewDestroyStatusFailed
		return finish(err)
	}
	result.Status = PreviewDestroyStatusDestroyed
	e.emit(events.Event{
		Type:    events.TypePreviewDestroyed,
		Phase:   events.PhaseCleanup,
		Level:   events.LevelInfo,
		Message: fmt.Sprintf("✓ Preview %s destroyed\n", name),
		Data:    map[string]any{"name": name, "environment": identity, "base": item.Base},
	})
	return finish(nil)
}

type previewTeardownOutcome struct {
	servers []PreviewDestroyServer
	volumes int
}

// previewSkipError reports a prune that left a preview alone after
// re-reading its record under the lease.
type previewSkipError struct {
	reason string
}

func (e *previewSkipError) Error() string { return e.reason }

// teardownPreview removes one preview: it takes the preview's leases on
// every target server, requires preview.lifecycle-v1 on all of them before
// any mutation, re-reads the record, and sends each server one guarded
// RemovePreviewData request. now is the clock a prune judges expiry by.
func (e *Engine) teardownPreview(ctx context.Context, cfg *config.Config, runtime *previewRuntime, item *PreviewListItem, load PreviewInstanceLoader, pruning bool, now time.Time) (previewTeardownOutcome, error) {
	outcome := previewTeardownOutcome{servers: []PreviewDestroyServer{}}
	identity := item.Environment
	services := map[string]config.ServiceConfig{}
	targets := map[string]bool{}
	for _, server := range item.Servers {
		targets[server] = true
	}
	for _, server := range item.Nodes {
		targets[server] = true
	}
	if load != nil {
		instanceCfg, err := load(item.Base, item.Name)
		if err != nil {
			e.warn(events.PhasePlan, fmt.Sprintf("⚠ preview %s: template of %s did not load (%v); using the preview record and replicated state\n", item.Name, item.Base, err))
		} else if instance := instanceCfg.PreviewInstance(); instance != nil && instance.Identity == identity {
			if templateServices, err := instanceCfg.GetServices(identity); err == nil {
				for serviceName, service := range templateServices {
					services[serviceName] = service
				}
			}
			if servers, err := instanceCfg.GetEnvironmentServers(identity); err == nil {
				for _, server := range servers {
					targets[server] = true
				}
			}
		}
	}
	var serverNames []string
	var unknown []string
	for server := range targets {
		if _, ok := cfg.Servers[server]; !ok {
			unknown = append(unknown, server)
			continue
		}
		serverNames = append(serverNames, server)
	}
	sort.Strings(serverNames)
	sort.Strings(unknown)
	if len(serverNames) == 0 {
		return outcome, fmt.Errorf("preview %s: none of its servers (%s) is in the config", item.Name, strings.Join(unknown, ", "))
	}

	operation := "destroy"
	if pruning {
		operation = "prune"
	}
	leaseCtx, release, err := runtime.acquire(ctx, identity, serverNames, operation)
	if err != nil {
		return outcome, err
	}
	defer release()
	ctx = leaseCtx

	clients := map[string]any{}
	for _, server := range serverNames {
		client, err := runtime.clientFor(ctx, server)
		if err != nil {
			return outcome, err
		}
		if err := requirePreviewCapability(ctx, client, cfg, server); err != nil {
			return outcome, err
		}
		clients[server] = client
	}
	// The listing that chose this preview may be stale: re-read the record
	// under the lease. A prune skips a preview that was redeployed (and so
	// is no longer expired) or destroyed meanwhile, and every server's
	// request carries the updatedAt read here for takod to compare.
	recordUpdatedAt := map[string]time.Time{}
	var fresh *takod.PreviewRecord
	for _, server := range serverNames {
		response, err := readPreviewListing(ctx, clients[server], cfg)
		if err != nil {
			return outcome, fmt.Errorf("%s: re-read preview record: %w", server, err)
		}
		for index := range response.Previews {
			record := response.Previews[index]
			if record.Project != cfg.Project.Name || record.Environment != identity {
				continue
			}
			recordUpdatedAt[server] = record.UpdatedAt
			if fresh == nil || record.UpdatedAt.After(fresh.UpdatedAt) {
				fresh = &record
			}
		}
	}
	if pruning {
		switch {
		case fresh == nil:
			return outcome, &previewSkipError{reason: "already destroyed"}
		case previewStatus(fresh.ExpiresAt, now) != PreviewStatusExpired:
			return outcome, &previewSkipError{reason: "redeployed since the listing; no longer expired"}
		}
	}
	for _, server := range serverNames {
		desired, err := takodstate.NewManager(clients[server], cfg, identity).ReadDesired()
		if err != nil {
			continue
		}
		for serviceName, service := range desired.Services {
			if _, ok := services[serviceName]; !ok {
				services[serviceName] = config.ServiceConfig{Proxy: service.Proxy}
			}
		}
		break
	}
	request := PreviewTeardownRequest(cfg.Project.Name, identity, services)

	e.info(events.TypePhaseStarted, events.PhaseCleanup, fmt.Sprintf("\n🗑️  Removing preview %s on %d node(s)...\n", item.Name, len(serverNames)))
	failures := 0
	for _, server := range serverNames {
		row := PreviewDestroyServer{Name: server}
		if err := ctx.Err(); err != nil {
			return outcome, err
		}
		serverRequest := request
		if updatedAt, ok := recordUpdatedAt[server]; ok {
			serverRequest.PreviewUpdatedAt = &updatedAt
		}
		response, err := CleanupViaTakodContext(ctx, clients[server], cfg, serverRequest)
		if err != nil {
			row.Status = PreviewDestroyStatusFailed
			row.Error = e.RedactSensitive(err.Error())
			failures++
			e.warn(events.PhaseCleanup, fmt.Sprintf("⚠️  Errors removing preview %s on %s: %v\n", item.Name, server, err))
		} else {
			emitCleanupWarnings(e, response)
			row.Status = PreviewDestroyStatusDestroyed
			if response.PreviewNotFound {
				row.Status = PreviewDestroyStatusNotFound
			}
			row.ContainersRemoved = response.ContainersRemoved
			row.NetworksRemoved = response.NetworksRemoved
			row.VolumesRemoved = response.VolumesRemoved
			outcome.volumes += response.VolumesRemoved
			e.info(events.TypeCleanupCompleted, events.PhaseCleanup, fmt.Sprintf("✓ Preview %s removed on %s\n", item.Name, server))
		}
		outcome.servers = append(outcome.servers, row)
	}
	for _, server := range unknown {
		outcome.servers = append(outcome.servers, PreviewDestroyServer{Name: server, Status: PreviewDestroyStatusFailed, Error: "server is no longer in the config; remove the preview there by hand or restore the server entry"})
		failures++
	}
	if failures > 0 {
		return outcome, fmt.Errorf("preview %s teardown incomplete: %d server(s) failed; run the same command again to retry", item.Name, failures)
	}
	return outcome, nil
}

// PreviewTeardownRequest is the one guarded request each server gets when a
// preview is destroyed (D8). It never removes images, image repositories or
// deploy files, which are shared with the base environment.
func PreviewTeardownRequest(project string, identity string, services map[string]config.ServiceConfig) takod.CleanupRequest {
	return takod.CleanupRequest{
		Project:           project,
		Environment:       identity,
		RemovePreviewData: true,
		RemoveContainers:  true,
		RemoveNetworks:    true,
		RemoveTakodState:  true,
		ProxyFiles:        CleanupProxyFiles(project, identity, services),
	}
}

// PrunePreviews destroys every expired preview, one at a time under its own
// leases. A preview whose lease is held is skipped; any failure is attention
// (exit 6).
func (e *Engine) PrunePreviews(ctx context.Context, req PreviewPruneRequest) (*PreviewPruneResult, error) {
	if req.Config == nil {
		return nil, invalidRequestf("preview prune requires a loaded config")
	}
	cfg := req.Config
	if err := RequireTakodRuntime(cfg); err != nil {
		return nil, err
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	result := &PreviewPruneResult{
		APIVersion: takoapi.APIVersionCurrent,
		Kind:       KindPreviewPruneResult,
		Project:    cfg.Project.Name,
		DryRun:     req.DryRun,
		Destroyed:  []PreviewPruneEntry{},
		Skipped:    []PreviewPruneEntry{},
		Failed:     []PreviewPruneEntry{},
		StartedAt:  time.Now(),
	}
	runtime, err := e.newPreviewRuntime(cfg)
	if err != nil {
		result.Error = err.Error()
		return result, err
	}
	defer runtime.close()
	err = e.prunePreviews(ctx, cfg, runtime, req.LoadInstance, req.DryRun, now, "", result)
	result.Duration = time.Since(result.StartedAt).Seconds()
	if err != nil {
		result.Error = e.RedactSensitive(err.Error())
	}
	return result, err
}

// prunePreviews fills result with the prune of every expired preview except
// keep (the preview being deployed).
func (e *Engine) prunePreviews(ctx context.Context, cfg *config.Config, runtime *previewRuntime, load PreviewInstanceLoader, dryRun bool, now time.Time, keep string, result *PreviewPruneResult) error {
	listing := collectPreviewListing(ctx, cfg, sortedServerNames(cfg), runtime.clientFor, now)
	e.warnPreviewNodes(listing)
	result.Nodes = listing.nodes
	for index := range listing.items {
		item := &listing.items[index]
		if item.Status != PreviewStatusExpired || item.Environment == keep {
			continue
		}
		entry := PreviewPruneEntry{Name: item.Name, Environment: item.Environment, Base: item.Base, ExpiresAt: item.ExpiresAt}
		if dryRun {
			entry.Reason = "expired (dry run)"
			result.Destroyed = append(result.Destroyed, entry)
			continue
		}
		_, err := e.teardownPreview(ctx, cfg, runtime, item, load, true, now)
		var locked *LockedError
		var skipped *previewSkipError
		switch {
		case errors.As(err, &skipped):
			entry.Reason = skipped.reason
			result.Skipped = append(result.Skipped, entry)
			e.debug(events.TypeLogLine, events.PhaseCleanup, fmt.Sprintf("→ Skipped preview %s: %s\n", item.Name, skipped.reason))
		case err == nil:
			result.Destroyed = append(result.Destroyed, entry)
			e.emit(events.Event{
				Type:    events.TypePreviewPruned,
				Phase:   events.PhaseCleanup,
				Level:   events.LevelInfo,
				Message: fmt.Sprintf("✓ Pruned expired preview %s\n", item.Name),
				Data:    map[string]any{"name": item.Name, "environment": item.Environment, "base": item.Base},
			})
		case errors.As(err, &locked):
			entry.Reason = "in use: " + e.RedactSensitive(err.Error())
			result.Skipped = append(result.Skipped, entry)
			e.warn(events.PhaseCleanup, fmt.Sprintf("⚠ Skipped expired preview %s: its lease is held\n", item.Name))
		default:
			entry.Reason = e.RedactSensitive(err.Error())
			result.Failed = append(result.Failed, entry)
			e.warn(events.PhaseCleanup, fmt.Sprintf("⚠ Failed to prune preview %s: %v\n", item.Name, err))
		}
	}
	if len(result.Failed) > 0 {
		return &AttentionError{Err: fmt.Errorf("%d expired preview(s) could not be destroyed", len(result.Failed))}
	}
	return previewListingError(listing)
}

// previewDeployState carries a preview deploy between plan and apply.
type previewDeployState struct {
	info      *config.PreviewInstanceInfo
	opts      DeployPreviewOptions
	ttl       string
	duration  time.Duration
	never     bool
	existing  *takod.PreviewRecord
	record    *takod.PreviewRecord
	createdBy string
	// shared lists the servers that a declared environment targets; preview
	// deploys never clean old images there.
	shared map[string]bool
	// clientFor overrides the deploy runtime for record writes in tests.
	clientFor func(ctx context.Context, server string) (any, error)
}

func (p *previewDeployState) expiresAt(now time.Time) *time.Time {
	if p.never {
		return nil
	}
	expires := now.Add(p.duration).UTC()
	return &expires
}

// preparePreviewDeploy runs the read-only preview checks of `deploy
// --preview` during planning, before any build or mutation: capability on
// every target server, base mismatch, adoption of a declared environment's
// state, and the max limit (D6, D7, D12). The opportunistic prune and the
// record write happen in Apply (applyPreviewBeforeMutation), so a plan-only,
// cancelled or refused deploy changes nothing.
func (e *Engine) preparePreviewDeploy(ctx context.Context, session *DeploySession) error {
	cfg := session.cfg
	info := cfg.PreviewInstance()
	if info == nil || info.Identity != session.envName {
		if session.req.Preview != nil {
			return invalidRequestf("deploy --preview requires a config loaded for the preview")
		}
		if takod.IsPreviewEnvironment(session.envName) {
			return e.refuseDeclaredDeployOverPreview(ctx, cfg, session.envName)
		}
		return nil
	}
	opts := DeployPreviewOptions{}
	if session.req.Preview != nil {
		opts = *session.req.Preview
	}
	ttl := strings.TrimSpace(opts.TTL)
	if ttl == "" {
		ttl = info.Policy.EffectiveTTL()
	}
	duration, never, err := config.ParsePreviewTTL(ttl)
	if err != nil {
		return &InvalidRequestError{Err: fmt.Errorf("--ttl: %w", err)}
	}
	if never {
		ttl = config.PreviewTTLNone
	}
	state := &previewDeployState{info: info, opts: opts, ttl: ttl, duration: duration, never: never, shared: map[string]bool{}}
	state.createdBy = strings.TrimSpace(opts.CreatedBy)
	if state.createdBy == "" {
		if os.Getenv("CI") != "" {
			state.createdBy = "ci"
		} else {
			state.createdBy = remotestate.GetCurrentUser()
		}
	}
	for _, name := range cfg.DeclaredEnvironmentNames() {
		servers, err := cfg.GetEnvironmentServers(name)
		if err != nil {
			continue
		}
		for _, server := range servers {
			state.shared[server] = true
		}
	}

	targets, err := cfg.GetEnvironmentServers(session.envName)
	if err != nil {
		return fmt.Errorf("failed to get environment servers: %w", err)
	}
	runtime, err := e.newPreviewRuntime(cfg)
	if err != nil {
		return err
	}
	defer runtime.close()
	clients := map[string]any{}
	for _, server := range targets {
		client, err := runtime.clientFor(ctx, server)
		if err != nil {
			return err
		}
		if err := requirePreviewCapability(ctx, client, cfg, server); err != nil {
			return err
		}
		clients[server] = client
	}

	now := time.Now()
	listing := collectPreviewListing(ctx, cfg, sortedServerNames(cfg), runtime.clientFor, now)
	e.warnPreviewNodes(listing)
	if existing := listing.find(info.Identity); existing != nil {
		if existing.Base != info.Base {
			return invalidRequestf("preview %s was created from %s, not %s; destroy it first or deploy with -e %s", info.Name, existing.Base, info.Base, existing.Base)
		}
		record := existing.PreviewRecord
		state.existing = &record
	}
	if state.existing == nil {
		// A new preview must not adopt a declared environment of the same
		// name: its containers, volumes and state would become preview data
		// that preview destroy removes.
		for _, server := range targets {
			found, err := identityDeployState(ctx, clients[server], cfg, info.Identity)
			if err != nil {
				return fmt.Errorf("%s: %w", server, err)
			}
			if found != "" {
				return invalidRequestf("environment %s already has %s on %s but no preview record; it is a declared environment, not preview %s. Choose another preview name", info.Identity, found, server, info.Name)
			}
		}
		// Expired previews do not count when the prune in Apply will remove
		// them; Apply checks the limit again after the prune.
		if err := checkPreviewLimit(info, listing.items, !opts.NoPrune); err != nil {
			return err
		}
	}
	session.preview = state
	return nil
}

// checkPreviewLimit fails when the base already has preview.max previews
// other than the one being created. ignoreExpired leaves out expired
// previews that the opportunistic prune is about to remove. Redeploys of an
// existing preview never call it, so lowering preview.max never blocks them.
func checkPreviewLimit(info *config.PreviewInstanceInfo, items []PreviewListItem, ignoreExpired bool) error {
	limit := info.Policy.EffectiveMax()
	var others []string
	for _, item := range items {
		if item.Base != info.Base || item.Environment == info.Identity {
			continue
		}
		if ignoreExpired && item.Status == PreviewStatusExpired {
			continue
		}
		expiry := "never"
		if item.ExpiresAt != nil {
			expiry = item.ExpiresAt.UTC().Format(time.RFC3339)
		}
		others = append(others, fmt.Sprintf("%s (expires %s)", item.Name, expiry))
	}
	if len(others) >= limit {
		return invalidRequestf("preview limit reached: %s allows %d preview(s) and has %s; destroy one with tako preview destroy <name> or raise preview.max", info.Base, limit, strings.Join(others, ", "))
	}
	return nil
}

// identityDeployState names the deploy state the node holds for
// environment (its desired revision or deployment history); empty means
// neither exists.
func identityDeployState(ctx context.Context, client any, cfg *config.Config, environment string) (string, error) {
	for _, document := range []struct{ name, label string }{{"desired", "desired state"}, {"history", "deployment history"}} {
		output, err := takodclient.RequestJSONWithContext(ctx, client, TakodSocketFromConfig(cfg), "GET", takodclient.StateEndpoint(cfg.Project.Name, environment, document.name), nil)
		if err != nil {
			return "", fmt.Errorf("read %s of %s: %w", document.label, environment, err)
		}
		var response takod.StateDocumentResponse
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			return "", fmt.Errorf("parse %s of %s: %w", document.label, environment, err)
		}
		if response.Found {
			return document.label, nil
		}
	}
	return "", nil
}

// refuseDeclaredDeployOverPreview fails a deploy of a declared environment
// named preview-* while a preview record exists for that name: the deploy
// would replace the preview's containers and a later preview destroy or
// prune would remove the declared environment's data.
func (e *Engine) refuseDeclaredDeployOverPreview(ctx context.Context, cfg *config.Config, environment string) error {
	servers, err := cfg.GetEnvironmentServers(environment)
	if err != nil || len(servers) == 0 {
		return nil
	}
	runtime, err := e.newPreviewRuntime(cfg)
	if err != nil {
		return err
	}
	defer runtime.close()
	for _, server := range servers {
		client, err := runtime.clientFor(ctx, server)
		if err != nil {
			return err
		}
		capable, err := takodHasCapability(ctx, client, cfg, takod.CapabilityPreviewLifecycleV1)
		if err != nil {
			return fmt.Errorf("%s: %w", server, err)
		}
		if !capable {
			continue
		}
		response, err := readPreviewListing(ctx, client, cfg)
		if err != nil {
			return fmt.Errorf("%s: %w", server, err)
		}
		for _, record := range response.Previews {
			if record.Environment == environment {
				return invalidRequestf("environment %s is preview %s (created from %s) on %s; destroy it with tako preview destroy %s before deploying a declared environment of that name", environment, record.Name, record.Base, server, record.Name)
			}
		}
		for _, invalid := range response.Invalid {
			if invalid == environment {
				return invalidRequestf("environment %s has an unreadable preview record on %s; inspect it before deploying a declared environment of that name", environment, server)
			}
		}
	}
	return nil
}

// applyPreviewBeforeMutation runs in Apply immediately before the first
// mutation: the opportunistic prune of expired previews, the limit check
// for a new preview, and step 1 of D7 (the record write).
func (s *DeploySession) applyPreviewBeforeMutation(ctx context.Context) error {
	if s.preview == nil {
		return nil
	}
	e := s.engine
	info := s.preview.info
	if !s.preview.opts.NoPrune {
		runtime, err := e.newPreviewRuntime(s.cfg)
		if err != nil {
			return err
		}
		defer runtime.close()
		now := time.Now()
		prune := &PreviewPruneResult{}
		if err := e.prunePreviews(ctx, s.cfg, runtime, s.preview.opts.LoadInstance, false, now, info.Identity, prune); err != nil {
			e.warn(events.PhaseDeploy, fmt.Sprintf("⚠ preview prune: %v (the deploy continues)\n", err))
		}
		if s.preview.existing == nil {
			listing := collectPreviewListing(ctx, s.cfg, sortedServerNames(s.cfg), runtime.clientFor, now)
			if err := checkPreviewLimit(info, listing.items, false); err != nil {
				return err
			}
		}
	}
	return s.writePreviewRecordBeforeMutation(ctx)
}

// writePreviewRecordBeforeMutation is step 1 of D7: before the first takod
// mutation the record exists on every target server.
func (s *DeploySession) writePreviewRecordBeforeMutation(ctx context.Context) error {
	if s.preview == nil {
		return nil
	}
	now := time.Now().UTC()
	var record takod.PreviewRecord
	if s.preview.existing != nil {
		record = *s.preview.existing
		record.Servers = unionStrings(record.Servers, s.mutationServerNames)
	} else {
		record = takod.PreviewRecord{
			APIVersion:  takod.PreviewRecordAPIVersion,
			Kind:        takod.PreviewRecordKind,
			Project:     s.cfg.Project.Name,
			Environment: s.preview.info.Identity,
			Name:        s.preview.info.Name,
			Base:        s.preview.info.Base,
			Servers:     unionStrings(nil, s.mutationServerNames),
			Hosts:       copyStringMap(s.preview.info.Hosts),
			CreatedAt:   now,
			UpdatedAt:   now,
			ExpiresAt:   s.preview.expiresAt(now),
			TTL:         s.preview.ttl,
			Source:      s.previewSource(),
			CreatedBy:   s.preview.createdBy,
		}
	}
	if err := s.writePreviewRecord(ctx, &record); err != nil {
		return fmt.Errorf("write preview record: %w", err)
	}
	s.preview.record = &record
	return nil
}

// refreshPreviewRecord is step 2 of D7, after a successful rollout. A
// failure is a warning: the deploy succeeded and the record still exists.
func (s *DeploySession) refreshPreviewRecord(ctx context.Context) {
	if s.preview == nil || s.preview.record == nil {
		return
	}
	now := time.Now().UTC()
	record := *s.preview.record
	record.UpdatedAt = now
	record.ExpiresAt = s.preview.expiresAt(now)
	record.TTL = s.preview.ttl
	record.Hosts = copyStringMap(s.preview.info.Hosts)
	record.Source = s.previewSource()
	record.Servers = unionStrings(record.Servers, s.mutationServerNames)
	if err := s.writePreviewRecord(ctx, &record); err != nil {
		s.engine.warn(events.PhaseDeploy, fmt.Sprintf("⚠ preview record not refreshed: %v; the expiry was not extended\n", err))
		return
	}
	s.preview.record = &record
}

func (s *DeploySession) writePreviewRecord(ctx context.Context, record *takod.PreviewRecord) error {
	clientFor := s.preview.clientFor
	if clientFor == nil {
		clientFor = func(ctx context.Context, server string) (any, error) {
			client, _, err := s.runtime.Client(ctx, server)
			if err != nil {
				return nil, &ConnectivityError{Server: server, Err: fmt.Errorf("failed to connect to %s: %w", server, err)}
			}
			return client, nil
		}
	}
	for _, server := range s.mutationServerNames {
		client, err := clientFor(ctx, server)
		if err != nil {
			return err
		}
		if err := writePreviewRecord(ctx, client, s.cfg, record); err != nil {
			return fmt.Errorf("%s: %w", server, err)
		}
	}
	return nil
}

func (s *DeploySession) previewSource() *takod.PreviewSource {
	if s.gitStrings.Branch == "" && s.gitStrings.ShortHash == "" {
		return nil
	}
	return &takod.PreviewSource{Branch: s.gitStrings.Branch, Commit: s.gitStrings.ShortHash}
}

// cleanOldImagesOn reports whether the post-deploy cleanup may prune old
// images on server. A preview never prunes images on a server that a
// declared environment uses, because the repositories are shared.
func (s *DeploySession) cleanOldImagesOn(server string) bool {
	if s.preview == nil {
		return true
	}
	return !s.preview.shared[server]
}

func (s *DeploySession) previewResult() *DeployPreviewResult {
	if s.preview == nil {
		return nil
	}
	result := &DeployPreviewResult{
		Name:        s.preview.info.Name,
		Environment: s.preview.info.Identity,
		Base:        s.preview.info.Base,
		Created:     s.preview.existing == nil,
		TTL:         s.preview.ttl,
		Hosts:       copyStringMap(s.preview.info.Hosts),
	}
	if s.preview.record != nil {
		result.TTL = s.preview.record.TTL
		if s.preview.record.ExpiresAt != nil {
			expires := *s.preview.record.ExpiresAt
			result.ExpiresAt = &expires
		}
	}
	return result
}

func (s *DeploySession) summaryPreview() *SummaryPreview {
	result := s.previewResult()
	if result == nil {
		return nil
	}
	preview := &SummaryPreview{Name: result.Name, Base: result.Base}
	if result.ExpiresAt != nil {
		preview.ExpiresAt = result.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return preview
}

// FindPreviewRecord returns the merged record of preview name, or nil. It
// fails when no server could be read.
func (e *Engine) FindPreviewRecord(ctx context.Context, cfg *config.Config, name string) (*PreviewListItem, error) {
	normalized, err := config.NormalizePreviewName(name)
	if err != nil {
		return nil, &InvalidRequestError{Err: err}
	}
	runtime, err := e.newPreviewRuntime(cfg)
	if err != nil {
		return nil, err
	}
	defer runtime.close()
	listing := collectPreviewListing(ctx, cfg, sortedServerNames(cfg), runtime.clientFor, time.Now())
	if item := listing.find(config.PreviewIdentity(normalized)); item != nil {
		copied := *item
		return &copied, nil
	}
	if err := previewListingError(listing); err != nil {
		return nil, err
	}
	return nil, nil
}

func unionStrings(base []string, extra []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range [][]string{base, extra} {
		for _, value := range list {
			if value == "" || seen[value] {
				continue
			}
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func copyStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}
