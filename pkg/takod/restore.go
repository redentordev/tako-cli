package takod

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

// CapabilityDeployRestoreOnFailureV1 means reconcile requests honor
// FailurePolicy (restore, retain-failed): recreate keeps the previous
// containers stopped and renamed until the CLI finalizes or restores them
// through /v1/services/finalize and /v1/services/restore.
const CapabilityDeployRestoreOnFailureV1 = "deploy.restore-on-failure-v1"

// Reconcile failure policies. The empty policy is the legacy remove policy.
const (
	FailurePolicyRemove       = "remove"
	FailurePolicyRestore      = "restore"
	FailurePolicyRetainFailed = "retain-failed"
)

const (
	retainedStateDirName      = "deploy-restore"
	maxRestoreTokenLength     = 128
	retainedOperationTimeout  = maxHealthDuration + 2*time.Minute
	retainedStateMaxFileBytes = 1 << 20
)

// ServiceRecoveryRequest finalizes or restores the previous containers a
// successful recreate reconcile retained. Both operations are idempotent and
// scoped by project, environment, service, revision and the restore token.
type ServiceRecoveryRequest struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Service     string `json:"service"`
	Revision    string `json:"revision,omitempty"`
	Token       string `json:"token"`
}

// Service recovery statuses returned by finalize and restore.
const (
	ServiceRecoveryFinalized = "finalized"
	ServiceRecoveryRestored  = "restored"
	ServiceRecoveryNoop      = "noop"
)

type ServiceRecoveryResponse struct {
	Project     string   `json:"project"`
	Environment string   `json:"environment"`
	Service     string   `json:"service"`
	Revision    string   `json:"revision,omitempty"`
	Status      string   `json:"status"`
	Containers  []string `json:"containers,omitempty"`
}

// ReconcileFailureBody is the typed error document takod returns when a
// reconcile with a non-legacy failure policy fails.
type ReconcileFailureBody struct {
	Error         string   `json:"error"`
	FailurePolicy string   `json:"failurePolicy"`
	Restored      bool     `json:"restored"`
	RestoreError  string   `json:"restoreError,omitempty"`
	Restorable    bool     `json:"restorable,omitempty"`
	KeptContainers []string `json:"keptContainers,omitempty"`
}

// ReconcileFailure wraps a rollout error with the node-local recovery result.
type ReconcileFailure struct {
	Err            error
	Policy         string
	Restored       bool
	RestoreError   string
	Restorable     bool
	KeptContainers []string
}

func (e *ReconcileFailure) Error() string {
	if e == nil || e.Err == nil {
		return "service reconcile failed"
	}
	message := e.Err.Error()
	switch {
	case e.Policy == FailurePolicyRestore && e.Restored:
		message += "; previous containers restored"
	case e.Policy == FailurePolicyRestore && e.RestoreError != "":
		message += "; restoring previous containers failed: " + e.RestoreError
	case e.Policy == FailurePolicyRetainFailed && len(e.KeptContainers) > 0:
		message += "; failed containers kept stopped for debugging: " + strings.Join(e.KeptContainers, ", ")
	}
	return message
}

func (e *ReconcileFailure) Unwrap() error { return e.Err }

// Body returns the typed error document for the HTTP response.
func (e *ReconcileFailure) Body() ReconcileFailureBody {
	body := ReconcileFailureBody{
		FailurePolicy:  e.Policy,
		Restored:       e.Restored,
		RestoreError:   e.RestoreError,
		Restorable:     e.Restorable,
		KeptContainers: append([]string(nil), e.KeptContainers...),
	}
	if e.Err != nil {
		body.Error = e.Err.Error()
	}
	return body
}

func normalizeFailurePolicy(policy string) string {
	switch strings.TrimSpace(policy) {
	case "", FailurePolicyRemove:
		return FailurePolicyRemove
	case FailurePolicyRestore:
		return FailurePolicyRestore
	case FailurePolicyRetainFailed:
		return FailurePolicyRetainFailed
	default:
		return ""
	}
}

type retainedContainerRecord struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	RetainedName  string `json:"retainedName"`
	WasRunning    bool   `json:"wasRunning"`
	RestartPolicy string `json:"restartPolicy,omitempty"`
}

type retainedServiceState struct {
	Token       string                    `json:"token"`
	Project     string                    `json:"project"`
	Environment string                    `json:"environment"`
	Service     string                    `json:"service"`
	Revision    string                    `json:"revision,omitempty"`
	Containers  []retainedContainerRecord `json:"containers"`
	Started     []string                  `json:"started,omitempty"`
	CreatedAt   time.Time                 `json:"createdAt"`
}

func (s *retainedServiceState) retainedNames() []string {
	names := make([]string, 0, len(s.Containers))
	for _, record := range s.Containers {
		names = append(names, record.RetainedName)
	}
	return names
}

func retainedStatePath(dataDir string, project string, environment string, service string) string {
	return filepath.Join(dataDir, retainedStateDirName, project, environment, service+".json")
}

func readRetainedState(dataDir string, project string, environment string, service string) (*retainedServiceState, error) {
	path := retainedStatePath(dataDir, project, environment, service)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if info.Size() > retainedStateMaxFileBytes {
		return nil, fmt.Errorf("retained container state exceeds size limit")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state retainedServiceState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("invalid retained container state: %w", err)
	}
	return &state, nil
}

// writeRetainedState persists the retention record before any container is
// stopped. It fsyncs the file and renames it into place; the record only
// guards one rollout, so it does not need directory durability.
func writeRetainedState(dataDir string, state *retainedServiceState) error {
	path := retainedStatePath(dataDir, state.Project, state.Environment, state.Service)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	if _, err := temp.Write(append(data, '\n')); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

func removeRetainedState(dataDir string, project string, environment string, service string) error {
	err := os.Remove(retainedStatePath(dataDir, project, environment, service))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func newRestoreToken(revision string) (string, error) {
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	prefix := strings.TrimSpace(revision)
	if prefix == "" {
		prefix = "recreate"
	}
	return prefix + "." + hex.EncodeToString(nonce), nil
}

func isSafeRestoreToken(token string) bool {
	if len(token) == 0 || len(token) > maxRestoreTokenLength {
		return false
	}
	for _, r := range token {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

type serviceContainerInfo struct {
	ID            string
	Name          string
	Running       bool
	RestartPolicy string
	Labels        map[string]string
}

// retainedNameForLabels recomputes the retained name from a container's own
// identity labels. It reports false when the container carries no usable slot.
func retainedNameForLabels(labels map[string]string) (string, bool) {
	slotText := strings.TrimSpace(labels["tako.slot"])
	if slotText == "" {
		return "", false
	}
	slot, err := strconv.Atoi(slotText)
	if err != nil || slot < 0 {
		return "", false
	}
	project := labels["tako.project"]
	environment := labels["tako.environment"]
	service := labels["tako.service"]
	if project == "" || environment == "" || service == "" {
		return "", false
	}
	return runtimeid.RetainedContainerName(project, environment, service, slot), true
}

func listServiceContainerInfo(ctx context.Context, project string, environment string, service string) ([]serviceContainerInfo, error) {
	output, err := runDocker(
		ctx,
		"ps",
		"-aq",
		"--filter", "label=tako.project="+project,
		"--filter", "label=tako.environment="+environment,
		"--filter", "label=tako.service="+service,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list old service containers: %w", err)
	}
	ids := strings.Fields(strings.TrimSpace(output))
	sort.Strings(ids)
	containers := make([]serviceContainerInfo, 0, len(ids))
	for _, id := range ids {
		raw, err := runDocker(ctx, "inspect", id, "--format", "{{.Name}}|{{.State.Running}}|{{.HostConfig.RestartPolicy.Name}}|{{json .Config.Labels}}")
		if err != nil {
			if dockerContainerNotFound(raw) {
				continue
			}
			return nil, fmt.Errorf("failed to inspect old service container %s: %w", id, err)
		}
		parts := strings.SplitN(strings.TrimSpace(raw), "|", 4)
		if len(parts) != 4 {
			return nil, fmt.Errorf("failed to inspect old service container %s: unexpected inspect output", id)
		}
		policy := strings.TrimSpace(parts[2])
		if policy == "<no value>" {
			policy = ""
		}
		containers = append(containers, serviceContainerInfo{
			ID:            id,
			Name:          strings.TrimPrefix(strings.TrimSpace(parts[0]), "/"),
			Running:       strings.TrimSpace(parts[1]) == "true",
			RestartPolicy: policy,
			Labels:        parseDockerLabels(parts[3]),
		})
	}
	return containers, nil
}

// retainServiceContainersForReconcile replaces the legacy remove step for a
// recreate rollout with restore-on-failure. It removes retained leftovers of
// an interrupted rollout (identified only by recomputing the retained name
// from each container's labels), then gracefully stops the current
// containers without removing them and renames each one to its retained name
// so the slot names are free for the new revision. Containers without a slot
// label, or extra containers competing for the same retained name, cannot be
// restored and are removed exactly as before.
func retainServiceContainersForReconcile(ctx context.Context, req ReconcileServiceRequest) (*retainedServiceState, int, error) {
	if strings.TrimSpace(req.DataDir) == "" {
		return nil, 0, fmt.Errorf("restore-on-failure requires the takod data directory")
	}
	containers, err := listServiceContainerInfo(ctx, req.Project, req.Environment, req.Service)
	if err != nil {
		return nil, 0, err
	}
	var leftovers, unretainable []string
	var keep []retainedContainerRecord
	claimed := make(map[string]bool)
	for _, container := range containers {
		retainedName, ok := retainedNameForLabels(container.Labels)
		if ok && container.Name == retainedName {
			leftovers = append(leftovers, container.ID)
			continue
		}
		if !ok || claimed[retainedName] {
			unretainable = append(unretainable, container.ID)
			continue
		}
		if !isSafeContainerName(retainedName) || !isSafeContainerName(container.Name) {
			return nil, 0, fmt.Errorf("cannot retain container %s: unsafe retained container name %q", container.Name, retainedName)
		}
		claimed[retainedName] = true
		keep = append(keep, retainedContainerRecord{
			ID:            container.ID,
			Name:          container.Name,
			RetainedName:  retainedName,
			WasRunning:    container.Running,
			RestartPolicy: container.RestartPolicy,
		})
	}
	// A previous interrupted rollout may still have a record; its containers
	// are the leftovers removed below.
	if err := removeRetainedState(req.DataDir, req.Project, req.Environment, req.Service); err != nil {
		return nil, 0, fmt.Errorf("failed to clear stale retained container state: %w", err)
	}
	if _, err := removeContainerIDs(ctx, leftovers); err != nil {
		return nil, 0, fmt.Errorf("failed to remove retained containers left by an interrupted deploy: %w", err)
	}
	removed, err := removeContainerIDs(ctx, unretainable)
	if err != nil {
		return nil, 0, err
	}

	token, err := newRestoreToken(req.Revision)
	if err != nil {
		return nil, removed, fmt.Errorf("failed to create restore token: %w", err)
	}
	state := &retainedServiceState{
		Token:       token,
		Project:     req.Project,
		Environment: req.Environment,
		Service:     req.Service,
		Revision:    req.Revision,
		Containers:  keep,
		CreatedAt:   time.Now().UTC(),
	}
	if err := writeRetainedState(req.DataDir, state); err != nil {
		return nil, removed, fmt.Errorf("failed to record retained containers: %w", err)
	}
	if len(keep) == 0 {
		return state, removed, nil
	}

	ids := make([]string, 0, len(keep))
	for _, record := range keep {
		ids = append(ids, record.ID)
	}
	stopArgs := append([]string{"stop"}, ids...)
	if output, err := runDocker(ctx, stopArgs...); err != nil && !dockerStopOnlyReportedNotFound(output, ids) {
		restoreErr := restoreRetainedContainers(ctx, state, nil)
		if restoreErr != nil {
			return nil, removed, fmt.Errorf("failed to gracefully stop service containers: %w; additionally failed to restart them: %v", err, restoreErr)
		}
		_ = removeRetainedState(req.DataDir, req.Project, req.Environment, req.Service)
		return nil, removed, fmt.Errorf("failed to gracefully stop service containers: %w", err)
	}
	for _, record := range keep {
		// A retained container must never come back on its own (daemon or
		// host restart) next to the new revision.
		if record.RestartPolicy != "" && record.RestartPolicy != "no" {
			if _, err := runDocker(ctx, "update", "--restart", "no", record.ID); err != nil {
				return nil, removed, abortRetention(ctx, state, fmt.Errorf("failed to disable restart policy of %s: %w", record.Name, err))
			}
		}
		if output, err := runDocker(ctx, "rename", record.ID, record.RetainedName); err != nil {
			return nil, removed, abortRetention(ctx, state, fmt.Errorf("failed to rename %s to %s: %w: %s", record.Name, record.RetainedName, err, strings.TrimSpace(output)))
		}
	}
	return state, removed, nil
}

// abortRetention undoes a partially applied retention: every kept container
// is stopped, and some may already be renamed or have restart disabled.
func abortRetention(ctx context.Context, state *retainedServiceState, cause error) error {
	if restoreErr := restoreRetainedContainers(ctx, state, nil); restoreErr != nil {
		return fmt.Errorf("%w; additionally failed to restore previous containers: %v", cause, restoreErr)
	}
	return cause
}

// restoreRetainedContainers removes the given new containers, renames every
// retained container back to its original name, restores its restart policy
// and starts it when it was running before the rollout. Steps already
// applied (for example a container that already carries its original name)
// are tolerated so a repeated restore converges.
func restoreRetainedContainers(ctx context.Context, state *retainedServiceState, newContainers []string) error {
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), retainedOperationTimeout)
	defer cancel()
	var problems []string
	if len(newContainers) > 0 {
		if _, err := removeContainerIDs(opCtx, newContainers); err != nil {
			problems = append(problems, fmt.Sprintf("remove new containers: %v", err))
			// The original names are still taken; renaming back cannot work.
			return errors.New(strings.Join(problems, "; "))
		}
	}
	for _, record := range state.Containers {
		if output, err := runDocker(opCtx, "rename", record.RetainedName, record.Name); err != nil {
			if !dockerContainerNotFound(output) {
				problems = append(problems, fmt.Sprintf("rename %s to %s: %v: %s", record.RetainedName, record.Name, err, strings.TrimSpace(output)))
				continue
			}
			// Already renamed back by an earlier attempt.
		}
		if record.RestartPolicy != "" && record.RestartPolicy != "no" {
			if _, err := runDocker(opCtx, "update", "--restart", record.RestartPolicy, record.Name); err != nil {
				problems = append(problems, fmt.Sprintf("restore restart policy of %s: %v", record.Name, err))
			}
		}
		if !record.WasRunning {
			continue
		}
		if output, err := runDocker(opCtx, "start", record.Name); err != nil {
			problems = append(problems, fmt.Sprintf("start %s: %v: %s", record.Name, err, strings.TrimSpace(output)))
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// stopFailedContainers implements retain-failed: failed new containers are
// stopped but never removed so their logs stay readable.
func stopFailedContainers(ctx context.Context, containerNames []string) error {
	if len(containerNames) == 0 {
		return nil
	}
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), retainedOperationTimeout)
	defer cancel()
	stopArgs := append([]string{"stop"}, containerNames...)
	if output, err := runDocker(opCtx, stopArgs...); err != nil && !dockerStopOnlyReportedNotFound(output, containerNames) {
		return fmt.Errorf("failed to stop failed containers: %w", err)
	}
	return nil
}

// failReconcile applies the request's failure policy after a rollout error
// on this node. The legacy policy keeps the historical error text.
func failReconcile(ctx context.Context, req ReconcileServiceRequest, policy string, retained *retainedServiceState, started []string, failedContainer string, cause error) error {
	switch {
	case policy == FailurePolicyRetainFailed:
		kept := append([]string(nil), started...)
		if failedContainer != "" && !containsString(kept, failedContainer) {
			kept = append(kept, failedContainer)
		}
		failure := &ReconcileFailure{Err: cause, Policy: policy, KeptContainers: kept}
		if err := stopFailedContainers(ctx, started); err != nil {
			failure.Err = fmt.Errorf("%w; additionally %v", cause, err)
		}
		return failure
	case retained != nil:
		cleanup := append([]string(nil), started...)
		if failedContainer != "" && !containsString(cleanup, failedContainer) {
			// docker run may leave a created container holding the slot name.
			cleanup = append(cleanup, failedContainer)
		}
		failure := &ReconcileFailure{Err: cause, Policy: policy}
		if err := restoreRetainedContainers(ctx, retained, cleanup); err != nil {
			failure.RestoreError = err.Error()
			return failure
		}
		failure.Restored = true
		if err := removeRetainedState(req.DataDir, req.Project, req.Environment, req.Service); err != nil {
			failure.Err = fmt.Errorf("%w; additionally failed to clear retained container state: %v", cause, err)
		}
		return failure
	default:
		if cleanupErr := cleanupStartedContainers(ctx, started); cleanupErr != nil {
			err := fmt.Errorf("%w; additionally failed to clean up started containers: %v", cause, cleanupErr)
			if policy == FailurePolicyRestore {
				return &ReconcileFailure{Err: err, Policy: policy, Restored: true}
			}
			return err
		}
		if policy == FailurePolicyRestore {
			// Revision-scoped strategies never touched the previous revision.
			return &ReconcileFailure{Err: cause, Policy: policy, Restored: true}
		}
		return cause
	}
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func validateServiceRecoveryRequest(req ServiceRecoveryRequest) error {
	for label, value := range map[string]string{
		"project":     req.Project,
		"environment": req.Environment,
		"service":     req.Service,
		"token":       req.Token,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", label)
		}
	}
	if !isSafeProjectName(req.Project) {
		return fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(req.Environment) {
		return fmt.Errorf("invalid environment name")
	}
	if !isSafeServiceName(req.Service) {
		return fmt.Errorf("invalid service name")
	}
	if req.Revision != "" && !isSafeRuntimeName(req.Revision) {
		return fmt.Errorf("invalid revision")
	}
	if !isSafeRestoreToken(req.Token) {
		return fmt.Errorf("invalid restore token")
	}
	return nil
}

func matchingRetainedState(dataDir string, req ServiceRecoveryRequest) (*retainedServiceState, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, fmt.Errorf("takod data directory is required")
	}
	state, err := readRetainedState(dataDir, req.Project, req.Environment, req.Service)
	if err != nil || state == nil {
		return nil, err
	}
	if state.Token != req.Token || state.Revision != req.Revision ||
		state.Project != req.Project || state.Environment != req.Environment || state.Service != req.Service {
		return nil, nil
	}
	return state, nil
}

// FinalizeRetainedService removes the previous containers retained by a
// successful recreate reconcile. An unknown or already finalized token is a
// no-op.
func FinalizeRetainedService(ctx context.Context, dataDir string, req ServiceRecoveryRequest) (*ServiceRecoveryResponse, error) {
	if err := validateServiceRecoveryRequest(req); err != nil {
		return nil, err
	}
	response := &ServiceRecoveryResponse{Project: req.Project, Environment: req.Environment, Service: req.Service, Revision: req.Revision, Status: ServiceRecoveryNoop}
	state, err := matchingRetainedState(dataDir, req)
	if err != nil || state == nil {
		return response, err
	}
	names := state.retainedNames()
	if _, err := removeContainerIDs(ctx, names); err != nil {
		return nil, fmt.Errorf("failed to remove retained containers: %w", err)
	}
	if err := removeRetainedState(dataDir, req.Project, req.Environment, req.Service); err != nil {
		return nil, fmt.Errorf("failed to clear retained container state: %w", err)
	}
	response.Status = ServiceRecoveryFinalized
	response.Containers = names
	return response, nil
}

// RestoreRetainedService reverts a successful recreate reconcile on this
// node because another node of the same service failed: it gracefully
// removes the new containers and restarts the retained previous ones under
// their original names. An unknown or already handled token is a no-op.
func RestoreRetainedService(ctx context.Context, dataDir string, req ServiceRecoveryRequest) (*ServiceRecoveryResponse, error) {
	if err := validateServiceRecoveryRequest(req); err != nil {
		return nil, err
	}
	response := &ServiceRecoveryResponse{Project: req.Project, Environment: req.Environment, Service: req.Service, Revision: req.Revision, Status: ServiceRecoveryNoop}
	state, err := matchingRetainedState(dataDir, req)
	if err != nil || state == nil {
		return response, err
	}
	if err := restoreRetainedContainers(ctx, state, state.Started); err != nil {
		return nil, fmt.Errorf("failed to restore previous containers: %w", err)
	}
	if err := removeRetainedState(dataDir, req.Project, req.Environment, req.Service); err != nil {
		return nil, fmt.Errorf("failed to clear retained container state: %w", err)
	}
	response.Status = ServiceRecoveryRestored
	response.Containers = make([]string, 0, len(state.Containers))
	for _, record := range state.Containers {
		response.Containers = append(response.Containers, record.Name)
	}
	return response, nil
}
