package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/git"
	"github.com/redentordev/tako-cli/pkg/reconcile"
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
	desired, err := takodstate.BuildDesiredRevision(cfg, envName, source, services, imageRefs, serverNames, gitInfo)
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
