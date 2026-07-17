package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/git"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
	"github.com/redentordev/tako-cli/pkg/scheduler"
	"github.com/redentordev/tako-cli/pkg/ssh"
	localstate "github.com/redentordev/tako-cli/pkg/state"
	"github.com/redentordev/tako-cli/pkg/takodstate"
)

// RemoteDeploymentSaver persists deployment records to remote state.
type RemoteDeploymentSaver interface {
	SaveDeployment(*remotestate.DeploymentState) error
}

// RemoteDeploymentContextSaver persists deployment records to remote state with cancellation.
type RemoteDeploymentContextSaver interface {
	SaveDeploymentContext(context.Context, *remotestate.DeploymentState) error
}

// LocalDeploymentSaver persists deployment records to local .tako state.
type LocalDeploymentSaver interface {
	SaveDeployment(*localstate.DeploymentState) error
}

const (
	// FailedDeploymentCleanupTimeout bounds best-effort failure-state writes after
	// the operation context has already been cancelled or exceeded its deadline.
	FailedDeploymentCleanupTimeout = 10 * time.Second
)

func failedDeploymentRecordContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		return context.Background(), func() {}
	}
	if ctx.Err() == nil {
		return ctx, func() {}
	}
	return context.WithTimeout(context.Background(), FailedDeploymentCleanupTimeout)
}

// RecordStartedDeploymentState marks a deployment in progress and persists it
// to remote state before any deployment mutations start.
func RecordStartedDeploymentState(
	remoteSaver RemoteDeploymentSaver,
	deployment *remotestate.DeploymentState,
) error {
	return RecordStartedDeploymentStateContext(context.Background(), remoteSaver, deployment)
}

// RecordStartedDeploymentStateContext marks a deployment in progress and
// persists it to remote state before any deployment mutations start, bounded by
// ctx. Unlike failed-state cleanup, this helper honors cancellation directly so
// a canceled operation fails before mutating remote services.
func RecordStartedDeploymentStateContext(
	ctx context.Context,
	remoteSaver RemoteDeploymentSaver,
	deployment *remotestate.DeploymentState,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if deployment == nil {
		return fmt.Errorf("deployment state is nil")
	}
	if remoteSaver == nil {
		return fmt.Errorf("remote deployment recorder is nil")
	}

	deployment.Status = remotestate.StatusInProgress
	if contextSaver, ok := remoteSaver.(RemoteDeploymentContextSaver); ok {
		if err := contextSaver.SaveDeploymentContext(ctx, deployment); err != nil {
			return fmt.Errorf("failed to save started remote deployment state: %w", err)
		}
		return nil
	}
	if err := remoteSaver.SaveDeployment(deployment); err != nil {
		return fmt.Errorf("failed to save started remote deployment state: %w", err)
	}
	return nil
}

// PersistTakodRuntimeState writes desired/actual/event state documents to
// every target node.
func PersistTakodRuntimeState(
	sshPool *ssh.Pool,
	cfg *config.Config,
	envName string,
	serverNames []string,
	source string,
	services map[string]config.ServiceConfig,
	imageRefs map[string]string,
	actualState map[string]*reconcile.ActualService,
	nodeActualState map[string]map[string]*reconcile.ActualService,
	gitInfo takodstate.GitInfo,
	eventType string,
	message string,
	details map[string]string,
	verbose bool,
) error {
	return PersistTakodRuntimeStateWithAssignments(sshPool, cfg, envName, serverNames, source, services, imageRefs, actualState, nodeActualState, nil, gitInfo, eventType, message, details, verbose)
}

// PersistTakodRuntimeStateWithAssignments writes runtime state together with
// the scheduler's authoritative replica-to-node decisions. Callers that do not
// perform placement planning may use PersistTakodRuntimeState.
func PersistTakodRuntimeStateWithAssignments(
	sshPool *ssh.Pool,
	cfg *config.Config,
	envName string,
	serverNames []string,
	source string,
	services map[string]config.ServiceConfig,
	imageRefs map[string]string,
	actualState map[string]*reconcile.ActualService,
	nodeActualState map[string]map[string]*reconcile.ActualService,
	assignments map[string][]scheduler.Assignment,
	gitInfo takodstate.GitInfo,
	eventType string,
	message string,
	details map[string]string,
	verbose bool,
) error {
	return PersistTakodRuntimeStateWithPlacementBaseline(sshPool, cfg, envName, serverNames, source, services, imageRefs, actualState, nodeActualState, assignments, nil, gitInfo, eventType, message, details, verbose)
}

// PersistTakodRuntimeStateWithPlacementBaseline records final actual state
// without dropping prior desired-service records outside this workflow's
// scope. A full deploy that owns removals passes no baseline after cleanup.
func PersistTakodRuntimeStateWithPlacementBaseline(
	sshPool *ssh.Pool,
	cfg *config.Config,
	envName string,
	serverNames []string,
	source string,
	services map[string]config.ServiceConfig,
	imageRefs map[string]string,
	actualState map[string]*reconcile.ActualService,
	nodeActualState map[string]map[string]*reconcile.ActualService,
	assignments map[string][]scheduler.Assignment,
	priorDesired *takodstate.DesiredRevision,
	gitInfo takodstate.GitInfo,
	eventType string,
	message string,
	details map[string]string,
	verbose bool,
) error {
	preserved := preservedDesiredServices(priorDesired, services, nil)
	desired, err := takodstate.BuildDesiredRevisionWithPlacementSnapshot(cfg, envName, source, services, imageRefs, serverNames, assignments, nil, preserved, gitInfo)
	if err != nil {
		return fmt.Errorf("failed to build desired revision: %w", err)
	}
	actual := takodstate.BuildActualSnapshotWithNodes(cfg.Project.Name, envName, serverNames, actualState, nodeActualState)
	nodeActual := BuildNodeActualSnapshots(cfg.Project.Name, envName, nodeActualState)
	if details == nil {
		details = make(map[string]string)
	}
	details["revisionId"] = desired.RevisionID

	event := takodstate.NewEvent(cfg.Project.Name, envName, eventType, desired.RevisionID, message, details)
	return takodstate.PersistToServers(sshPool, cfg, envName, serverNames, desired, actual, nodeActual, event, verbose)
}

// PersistTakodDesiredIntent writes the exact sticky placement decision before
// images, containers, schedules, or proxy routes are mutated. A later runtime
// state write records actual state without changing the chosen assignments.
func PersistTakodDesiredIntent(
	sshPool *ssh.Pool,
	cfg *config.Config,
	envName string,
	serverNames []string,
	source string,
	services map[string]config.ServiceConfig,
	imageRefs map[string]string,
	assignments map[string][]scheduler.Assignment,
	gitInfo takodstate.GitInfo,
	message string,
	verbose bool,
) error {
	return PersistTakodDesiredIntentWithRemovals(sshPool, cfg, envName, serverNames, source, services, imageRefs, assignments, nil, gitInfo, message, verbose)
}

// PersistTakodDesiredIntentWithRemovals keeps services that are about to be
// deleted in desired state until their cleanup has completed successfully.
// This preserves their authoritative assignment on a crash and makes the next
// deploy retry the same removal instead of forgetting where the workload ran.
func PersistTakodDesiredIntentWithRemovals(
	sshPool *ssh.Pool,
	cfg *config.Config,
	envName string,
	serverNames []string,
	source string,
	services map[string]config.ServiceConfig,
	imageRefs map[string]string,
	assignments map[string][]scheduler.Assignment,
	pendingRemovals map[string]config.ServiceConfig,
	gitInfo takodstate.GitInfo,
	message string,
	verbose bool,
) error {
	return PersistTakodDesiredIntentWithPlacementBaseline(sshPool, cfg, envName, serverNames, source, services, imageRefs, assignments, pendingRemovals, nil, gitInfo, message, verbose)
}

// PersistTakodDesiredIntentWithPlacementBaseline preserves prior services that
// are absent from the current workflow while allowing an owning full deploy to
// provide explicit removal-pending records.
func PersistTakodDesiredIntentWithPlacementBaseline(
	sshPool *ssh.Pool,
	cfg *config.Config,
	envName string,
	serverNames []string,
	source string,
	services map[string]config.ServiceConfig,
	imageRefs map[string]string,
	assignments map[string][]scheduler.Assignment,
	pendingRemovals map[string]config.ServiceConfig,
	priorDesired *takodstate.DesiredRevision,
	gitInfo takodstate.GitInfo,
	message string,
	verbose bool,
) error {
	preserved := preservedDesiredServices(priorDesired, services, pendingRemovals)
	desired, err := takodstate.BuildDesiredRevisionWithPlacementSnapshot(cfg, envName, source, services, imageRefs, serverNames, assignments, pendingRemovals, preserved, gitInfo)
	if err != nil {
		return fmt.Errorf("failed to build desired placement intent: %w", err)
	}
	event := takodstate.NewEvent(cfg.Project.Name, envName, "placement.intent", desired.RevisionID, message, map[string]string{
		"revisionId": desired.RevisionID,
	})
	if err := takodstate.PersistDesiredToServers(sshPool, cfg, envName, serverNames, desired, event, verbose); err != nil {
		return fmt.Errorf("failed to persist desired placement intent before mutation: %w", err)
	}
	return nil
}

// LoadPriorAssignments reads the last authoritative desired revision from the
// selected runtime node. A new environment has no document and therefore no
// assignments; all other read failures fail closed.
func LoadPriorAssignments(client any, cfg *config.Config, envName string) (map[string][]scheduler.Assignment, error) {
	_, assignments, err := LoadPriorPlacementState(client, cfg, envName)
	return assignments, err
}

// LoadPriorPlacementState returns the full prior desired revision together
// with validated/adopted assignments. Callers that rewrite desired state use
// the revision as a baseline so out-of-scope services survive unchanged.
func LoadPriorPlacementState(client any, cfg *config.Config, envName string) (*takodstate.DesiredRevision, map[string][]scheduler.Assignment, error) {
	desired, err := takodstate.NewManager(client, cfg, envName).ReadDesired()
	if errors.Is(err, takodstate.ErrNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read prior desired assignments: %w", err)
	}
	assignments := make(map[string][]scheduler.Assignment)
	var missing []string
	for name, service := range desired.Services {
		if len(service.Assignments) == 0 {
			missing = append(missing, name)
			continue
		}
		if err := validateAssignmentMembership(cfg, name, service.Assignments); err != nil {
			return nil, nil, err
		}
		assignments[name] = append([]scheduler.Assignment(nil), service.Assignments...)
	}
	if len(missing) > 0 {
		actual, actualErr := takodstate.NewManager(client, cfg, envName).ReadActual()
		if actualErr != nil && !errors.Is(actualErr, takodstate.ErrNotFound) {
			return nil, nil, fmt.Errorf("failed to read per-node actual state for legacy placement adoption: %w", actualErr)
		}
		for _, name := range missing {
			adopted, needed, err := adoptLegacyAssignments(cfg, envName, name, desired.Services[name], actual)
			if err != nil {
				return nil, nil, err
			}
			if needed && len(adopted) == 0 {
				return nil, nil, fmt.Errorf("legacy desired state for service %s has no replica assignments and its actual placement cannot be proven; refuse to invent placement until the workload is explicitly adopted", name)
			}
			if len(adopted) > 0 {
				assignments[name] = adopted
			}
		}
	}
	return desired, assignments, nil
}

func preservedDesiredServices(prior *takodstate.DesiredRevision, current map[string]config.ServiceConfig, pending map[string]config.ServiceConfig) map[string]takodstate.DesiredService {
	if prior == nil || len(prior.Services) == 0 {
		return nil
	}
	preserved := make(map[string]takodstate.DesiredService)
	for name, service := range prior.Services {
		if _, ok := current[name]; ok {
			continue
		}
		if _, ok := pending[name]; ok {
			continue
		}
		preserved[name] = service
	}
	if len(preserved) == 0 {
		return nil
	}
	return preserved
}

// ValidatePriorDesiredServices prevents a narrow workflow from hiding a
// persistent workload that was removed from configuration. Stateless and job
// records remain preserved until a full deploy owns their cleanup.
func ValidatePriorDesiredServices(prior *takodstate.DesiredRevision, current map[string]config.ServiceConfig) error {
	if prior == nil {
		return nil
	}
	for name, service := range prior.Services {
		if _, ok := current[name]; ok || service.RemovalPending {
			continue
		}
		if service.Persistent {
			return invalidRequestf("persistent service %s is still authoritative desired state but is absent from tako.yaml; restore it before running any workload operation", name)
		}
	}
	return nil
}

func adoptLegacyAssignments(cfg *config.Config, environment, serviceName string, desired takodstate.DesiredService, actual *takodstate.ActualSnapshot) ([]scheduler.Assignment, bool, error) {
	if actual == nil {
		return nil, desired.Replicas > 0, nil
	}
	nodeNames := make([]string, 0, len(actual.Nodes))
	for nodeName, snapshot := range actual.Nodes {
		if _, ok := snapshot.Services[serviceName]; ok {
			nodeNames = append(nodeNames, nodeName)
		}
	}
	sort.Strings(nodeNames)
	aggregate, aggregateOK := actual.Services[serviceName]
	expected := 0
	if aggregateOK {
		expected = aggregate.Replicas
	}
	if desired.WorkloadKind == config.ServiceKindJob {
		expected = 1
		if len(nodeNames) != 1 {
			return nil, true, fmt.Errorf("legacy job %s is present on %d nodes; its single owner cannot be proven", serviceName, len(nodeNames))
		}
	}
	if expected == 0 {
		for _, nodeName := range nodeNames {
			nodeService := actual.Nodes[nodeName].Services[serviceName]
			expected += nodeService.Replicas
			if nodeService.Replicas == 0 && len(nodeService.Containers) > 0 {
				expected += len(nodeService.Containers)
			}
		}
	}
	if expected == 0 {
		return nil, desired.Replicas > 0, nil
	}
	if desired.WorkloadKind == config.ServiceKindJob {
		node := nodeNames[0]
		server, ok := cfg.Servers[node]
		if !ok {
			return nil, true, fmt.Errorf("legacy job %s runs on missing node %s", serviceName, node)
		}
		return []scheduler.Assignment{{Slot: 1, Node: node, NodeID: server.NodeID}}, true, nil
	}

	assignments := make([]scheduler.Assignment, 0, expected)
	for slot := 1; slot <= expected; slot++ {
		matchedNode := ""
		for _, nodeName := range nodeNames {
			nodeService := actual.Nodes[nodeName].Services[serviceName]
			if actualServiceContainsSlot(cfg.Project.Name, environment, serviceName, slot, nodeService) {
				if matchedNode != "" {
					return nil, true, fmt.Errorf("legacy service %s slot %d appears on multiple nodes", serviceName, slot)
				}
				matchedNode = nodeName
			}
		}
		if matchedNode == "" && expected == 1 && len(nodeNames) == 1 {
			matchedNode = nodeNames[0]
		}
		if matchedNode == "" {
			return nil, true, fmt.Errorf("legacy service %s slot %d cannot be matched to an actual node; refuse implicit movement", serviceName, slot)
		}
		server, ok := cfg.Servers[matchedNode]
		if !ok {
			return nil, true, fmt.Errorf("legacy service %s slot %d runs on missing node %s", serviceName, slot, matchedNode)
		}
		assignments = append(assignments, scheduler.Assignment{Slot: slot, Node: matchedNode, NodeID: server.NodeID})
	}
	return assignments, true, nil
}

func actualServiceContainsSlot(project, environment, serviceName string, slot int, actual takodstate.ActualService) bool {
	names := append([]string(nil), actual.Containers...)
	names = append(names, actual.ActiveContainers...)
	want := map[string]struct{}{runtimeid.ContainerName(project, environment, serviceName, slot): {}}
	revisions := make(map[string]struct{})
	for revision := range actual.RevisionImages {
		revisions[revision] = struct{}{}
	}
	for _, revision := range append([]string{actual.CurrentRevision, actual.PreviousRevision}, actual.WarmingRevisions...) {
		if strings.TrimSpace(revision) != "" {
			revisions[revision] = struct{}{}
		}
	}
	for revision := range revisions {
		want[runtimeid.RevisionContainerName(project, environment, serviceName, revision, slot)] = struct{}{}
	}
	for _, name := range names {
		if _, ok := want[name]; ok {
			return true
		}
	}
	return false
}

func validateAssignmentMembership(cfg *config.Config, serviceName string, assignments []scheduler.Assignment) error {
	if err := scheduler.ValidateAssignments(assignments); err != nil {
		return fmt.Errorf("desired assignments for service %s are invalid: %w", serviceName, err)
	}
	for _, assignment := range assignments {
		server, ok := cfg.Servers[assignment.Node]
		if !ok {
			return fmt.Errorf("desired assignment for service %s references missing node %s", serviceName, assignment.Node)
		}
		if assignment.NodeID != server.NodeID && (assignment.NodeID != "" || server.NodeID != "") {
			return fmt.Errorf("desired assignment for service %s does not match immutable identity for node %s", serviceName, assignment.Node)
		}
	}
	return nil
}

// BuildNodeActualSnapshots converts per-node actual state into snapshots.
func BuildNodeActualSnapshots(project string, environment string, nodeActualState map[string]map[string]*reconcile.ActualService) map[string]*takodstate.ActualSnapshot {
	if len(nodeActualState) == 0 {
		return nil
	}
	snapshots := make(map[string]*takodstate.ActualSnapshot, len(nodeActualState))
	for node, actual := range nodeActualState {
		snapshots[node] = takodstate.BuildNodeActualSnapshot(project, environment, node, actual)
	}
	return snapshots
}

// CloneServiceMap shallow-copies a service map.
func CloneServiceMap(services map[string]config.ServiceConfig) map[string]config.ServiceConfig {
	out := make(map[string]config.ServiceConfig, len(services))
	for name, service := range services {
		out[name] = service
	}
	return out
}

// RedactedEnvKeys replaces env values with a redaction marker, keeping keys.
func RedactedEnvKeys(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	redacted := make(map[string]string, len(env))
	for key := range env {
		redacted[key] = "<redacted>"
	}
	return redacted
}

// RecordFailedDeploymentState marks a deployment failed and persists it to
// remote (and optionally local) state.
func RecordFailedDeploymentState(
	remoteSaver RemoteDeploymentSaver,
	localSaver LocalDeploymentSaver,
	deployment *remotestate.DeploymentState,
	cfg *config.Config,
	envName string,
	serverNames []string,
	commitInfo *git.CommitInfo,
	startTime time.Time,
	deploymentErr error,
) error {
	return RecordFailedDeploymentStateContext(context.Background(), remoteSaver, localSaver, deployment, cfg, envName, serverNames, commitInfo, startTime, deploymentErr)
}

// RecordFailedDeploymentStateContext marks a deployment failed and persists it to
// remote (and optionally local) state bounded by ctx for remote writes.
func RecordFailedDeploymentStateContext(
	ctx context.Context,
	remoteSaver RemoteDeploymentSaver,
	localSaver LocalDeploymentSaver,
	deployment *remotestate.DeploymentState,
	cfg *config.Config,
	envName string,
	serverNames []string,
	commitInfo *git.CommitInfo,
	startTime time.Time,
	deploymentErr error,
) error {
	ctx, cancel := failedDeploymentRecordContext(ctx)
	defer cancel()
	if deployment == nil {
		return fmt.Errorf("deployment state is nil")
	}
	deployment.Status = remotestate.StatusFailed
	deployment.Duration = time.Since(startTime)
	if deploymentErr != nil {
		deployment.Error = deploymentErr.Error()
	} else if deployment.Error == "" {
		deployment.Error = "deployment failed"
	}

	if remoteSaver == nil {
		return fmt.Errorf("remote deployment recorder is nil")
	}
	if contextSaver, ok := remoteSaver.(RemoteDeploymentContextSaver); ok {
		if err := contextSaver.SaveDeploymentContext(ctx, deployment); err != nil {
			return fmt.Errorf("failed to save failed remote deployment state: %w", err)
		}
	} else if err := remoteSaver.SaveDeployment(deployment); err != nil {
		return fmt.Errorf("failed to save failed remote deployment state: %w", err)
	}

	if localSaver != nil {
		localDeployment := &localstate.DeploymentState{
			DeploymentID:    fmt.Sprintf("deploy-%s", startTime.Format("20060102-150405")),
			Timestamp:       startTime,
			Environment:     envName,
			Mode:            cfg.GetRuntimeMode(),
			Servers:         append([]string(nil), serverNames...),
			Status:          "failed",
			DurationSeconds: int(time.Since(startTime).Seconds()),
			TriggeredBy:     remotestate.GetCurrentUser(),
			Notes:           deployment.Error,
		}
		if commitInfo != nil {
			localDeployment.GitCommit = commitInfo.Hash
		}
		if err := localSaver.SaveDeployment(localDeployment); err != nil {
			return fmt.Errorf("failed to save failed local deployment state: %w", err)
		}
	}
	return nil
}

// RetiredDeploymentServers lists servers present in a previous deployment but
// absent from the current target set.
func RetiredDeploymentServers(previous []string, current []string) []string {
	currentSet := make(map[string]struct{}, len(current))
	for _, server := range current {
		server = strings.TrimSpace(server)
		if server != "" {
			currentSet[server] = struct{}{}
		}
	}
	seen := make(map[string]struct{}, len(previous))
	retired := make([]string, 0)
	for _, server := range previous {
		server = strings.TrimSpace(server)
		if server == "" {
			continue
		}
		if _, ok := currentSet[server]; ok {
			continue
		}
		if _, ok := seen[server]; ok {
			continue
		}
		seen[server] = struct{}{}
		retired = append(retired, server)
	}
	sort.Strings(retired)
	return retired
}

// DeploymentSuccessStatus resolves the final status for a successful apply.
func DeploymentSuccessStatus(manualPending []string) remotestate.DeploymentStatus {
	if len(manualPending) > 0 {
		return remotestate.StatusWarmed
	}
	return remotestate.StatusSuccess
}

// ActualStateError wraps failures to read running state before planning.
func ActualStateError(err error) error {
	return fmt.Errorf("failed to gather actual state from takod; refusing to plan against unknown running services: %w", err)
}

// RemoteHistoryError wraps failures to persist history after a successful deploy.
func RemoteHistoryError(err error) error {
	return fmt.Errorf("deployment succeeded but failed to save remote deployment history: %w", err)
}
