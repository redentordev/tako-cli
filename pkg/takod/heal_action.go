package takod

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/recovery"
	"github.com/redentordev/tako-cli/pkg/selfheal"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
)

// healInspection is one fresh docker inspect of the container to act on.
type healInspection struct {
	ID            string
	Name          string
	Status        string
	Restarting    bool
	OOMKilled     bool
	ExitCode      int
	Health        string
	RestartPolicy string
	Revision      string
	Active        string
	Image         string
}

// healExecutor runs one action under the locks of D6: the restore barrier,
// the maintenance barrier, the node-upgrade exclusion and mutation lock (on
// enrolled nodes), the environment lease (holder self-heal), the worker
// fence admission lock (enrolled nodes) and the scope lock.
type healExecutor struct {
	h        *healManager
	enrolled func() bool
	// nodeLocks takes the node-wide locks; it returns a skip reason when
	// the node is busy.
	nodeLocks func(ctx context.Context) (func(), string)
	// fence takes the worker fence admission lock on enrolled nodes and
	// returns a skip reason when a worker fence is active.
	fence func() (func(), string)
	// inspect, act and backupRunning call docker; tests replace them.
	inspect func(ctx context.Context, id string) (*healInspection, error)
	// act runs docker restart/start; stopTimeout bounds the stop phase of a
	// restart (docker restart -t).
	act           func(ctx context.Context, action, id string, stopTimeout time.Duration) error
	backupRunning func(ctx context.Context, project, environment string) (bool, error)
	leaseID       func() string
}

func newHealExecutor(h *healManager) *healExecutor {
	executor := &healExecutor{
		h:             h,
		enrolled:      func() bool { return false },
		inspect:       inspectHealContainer,
		act:           dockerHealAction,
		backupRunning: dockerBackupRunning,
		leaseID:       newHealLeaseID,
	}
	executor.nodeLocks = executor.defaultNodeLocks
	executor.fence = executor.defaultFence
	return executor
}

func newHealLeaseID() string {
	buffer := make([]byte, 12)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("self-heal-%d", time.Now().UnixNano())
	}
	return "self-heal-" + hex.EncodeToString(buffer)
}

func (e *healExecutor) defaultNodeLocks(ctx context.Context) (func(), string) {
	if !backupRestoreBarrierMu.gate.TryAcquire(1) {
		return nil, selfheal.SkipRestorePending
	}
	releases := []func(){backupRestoreBarrierMu.RUnlock}
	release := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	if err := ensureNoPendingRestoreTransactionsOnNode(); err != nil {
		release()
		return nil, selfheal.SkipRestorePending
	}
	unlockBarrier, err := recovery.AcquireMaintenanceBarrier(e.h.dataDir)
	if err != nil {
		release()
		return nil, selfheal.SkipStateUnreadable
	}
	releases = append(releases, unlockBarrier)
	if e.enrolled() {
		unlockUpgrade, err := recovery.AcquireNodeUpgradeLifecycleExclusionForDataDir(e.h.dataDir)
		if err != nil {
			release()
			return nil, selfheal.SkipNodeUpgrade
		}
		releases = append(releases, unlockUpgrade)
	}
	unlockMutation, err := recovery.AcquireMutationLockContext(ctx, e.h.dataDir)
	if err != nil {
		release()
		return nil, selfheal.SkipStateUnreadable
	}
	releases = append(releases, unlockMutation)
	return release, ""
}

func (e *healExecutor) defaultFence() (func(), string) {
	if !e.enrolled() {
		return func() {}, ""
	}
	workerFenceAdmissionMu.RLock()
	active, err := activeWorkerOperation(e.h.dataDir, e.h.now())
	if err != nil || active {
		workerFenceAdmissionMu.RUnlock()
		return nil, selfheal.SkipWorkerFence
	}
	return workerFenceAdmissionMu.RUnlock, ""
}

// Execute runs one candidate action. Every check is repeated under the
// locks; any change means no action.
func (e *healExecutor) Execute(ctx context.Context, candidate selfheal.Candidate) {
	h := e.h
	releaseNode, skip := e.nodeLocks(ctx)
	if skip != "" {
		return
	}
	defer releaseNode()

	lease := LeaseRequest{
		Project: candidate.Project, Environment: candidate.Environment, ID: e.leaseID(),
		Operation: healLeaseOperation, Who: "takod/" + h.node(), PID: os.Getpid(),
		TTLSeconds: int64(healLeaseTTL / time.Second),
	}
	acquired, err := AcquireLease(ctx, h.dataDir, lease)
	if err != nil || acquired == nil || !acquired.Acquired {
		return
	}
	leaseHeld := true
	releaseLease := func() {
		if leaseHeld {
			leaseHeld = false
			_, _ = ReleaseLease(context.Background(), h.dataDir, LeaseRequest{Project: lease.Project, Environment: lease.Environment, ID: lease.ID})
		}
	}
	defer releaseLease()

	releaseFence, skip := e.fence()
	if skip != "" {
		return
	}
	defer releaseFence()

	scopeLock := healScopeLock(candidate.Project, candidate.Environment)
	scopeLock.Lock()
	defer scopeLock.Unlock()

	service, eligible := e.stillEligible(ctx, candidate)
	if !eligible {
		return
	}
	inspection, err := e.inspect(ctx, candidate.Container.ID)
	if err != nil || !e.confirm(candidate, inspection) {
		return
	}
	if candidate.Health == selfheal.HealthHTTP || candidate.Health == selfheal.HealthTCP {
		if candidate.Action == selfheal.ActionRestart && !e.reprobeUnhealthy(ctx, candidate) {
			return
		}
	}

	actionCtx, cancel := context.WithTimeout(ctx, healDockerTimeout)
	actErr := e.act(actionCtx, candidate.Action, candidate.Container.ID, healRestartStopTimeout(service.Desired.StopGracePeriod))
	timedOut := actionCtx.Err() != nil || errors.Is(actErr, context.DeadlineExceeded)
	cancel()
	if timedOut && !e.waitSettled(ctx, candidate.Container.ID) {
		// The docker CLI gave up but the daemon may still be stopping or
		// starting the container. Keep the lease: it expires at its TTL, and
		// a deploy waits for it until then.
		leaseHeld = false
	}
	releaseLease()
	e.record(candidate, actErr)
}

// healRestartStopTimeout is the docker restart -t value: the service's
// stopGracePeriod (Docker's 10s default when unset), capped at
// healRestartStopMax so a restart always finishes well inside
// healDockerTimeout and the self-heal lease a deploy waits for stays short.
func healRestartStopTimeout(stopGracePeriod string) time.Duration {
	timeout := parseHealDuration(stopGracePeriod, healRestartStopDefault)
	if timeout <= 0 {
		timeout = healRestartStopDefault
	}
	if timeout > healRestartStopMax {
		timeout = healRestartStopMax
	}
	return timeout.Truncate(time.Second)
}

// waitSettled polls a fresh inspect until the container runs again (Docker
// finished the action) or healSettleTimeout passes. It reports whether the
// container settled.
func (e *healExecutor) waitSettled(ctx context.Context, id string) bool {
	settleCtx, cancel := context.WithTimeout(ctx, healSettleTimeout)
	defer cancel()
	for {
		inspection, err := e.inspect(settleCtx, id)
		if err == nil && inspection != nil && inspection.Status == "running" && !inspection.Restarting {
			return true
		}
		select {
		case <-settleCtx.Done():
			return false
		case <-time.After(healSettlePoll):
		}
	}
}

// stillEligible re-reads desired state, the record, the control state and
// the gates under the locks. It returns the service as desired now.
func (e *healExecutor) stillEligible(ctx context.Context, candidate selfheal.Candidate) (*healService, bool) {
	h := e.h
	now := h.now()
	if h.nodeGate(now) != "" || h.scopeSkipExcept(candidate.Project, candidate.Environment, now) != "" {
		return nil, false
	}
	scopes := h.loadScopes()
	var service *healService
	for _, scope := range scopes {
		if scope.Project != candidate.Project || scope.Environment != candidate.Environment {
			continue
		}
		for _, candidateService := range h.healServices(scope) {
			if candidateService.Name == candidate.Service {
				copied := candidateService
				service = &copied
			}
		}
	}
	if service == nil {
		return nil, false
	}
	samples, _ := h.samples()
	if h.serviceSkip(candidate.Project, candidate.Environment, *service, samples) != "" {
		return nil, false
	}
	record, err := selfheal.ReadRecord(h.dataDir, candidate.Project, candidate.Environment, candidate.Service)
	if err != nil || !record.Valid(service.Spec.Image) || record.Revision != candidate.Revision || len(record.Containers) != service.Spec.LocalReplicas {
		return nil, false
	}
	found := false
	for _, container := range record.Containers {
		if container.ID == candidate.Container.ID && container.Name == candidate.Container.Name {
			found = true
		}
	}
	if !found {
		return nil, false
	}
	h.controlMu.Lock()
	control, controlErr := selfheal.ReadControl(h.dataDir, candidate.Project, candidate.Environment)
	node, nodeErr := selfheal.ReadNodeState(h.dataDir)
	h.controlMu.Unlock()
	if controlErr != nil || nodeErr != nil || node.BreakerOpen(now) {
		return nil, false
	}
	if _, paused := control.PauseActive(candidate.Service, now); paused {
		return nil, false
	}
	if entry := control.Lookup(candidate.Service); entry != nil {
		if entry.GaveUp {
			return nil, false
		}
		if container := entry.Containers[candidate.Container.ID]; container != nil && now.Before(container.CooldownUntil) {
			return nil, false
		}
	}
	if busy, err := e.backupRunning(ctx, candidate.Project, candidate.Environment); err != nil || busy {
		return nil, false
	}
	return service, true
}

// confirm checks one fresh inspect against the candidate: same container
// (ID, name, revision label), still active, and the condition still holds
// under the same restart policy rules.
func (e *healExecutor) confirm(candidate selfheal.Candidate, inspection *healInspection) bool {
	if inspection == nil || inspection.ID != candidate.Container.ID || strings.TrimPrefix(inspection.Name, "/") != candidate.Container.Name {
		return false
	}
	if inspection.Revision != candidate.Revision || inspection.Active == "false" || inspection.Restarting {
		return false
	}
	policy := inspection.RestartPolicy
	if strings.HasPrefix(policy, "on-failure") {
		policy = "on-failure"
	}
	switch candidate.Action {
	case selfheal.ActionRestart:
		if inspection.Status != "running" {
			return false
		}
		if candidate.Health == selfheal.HealthDocker && inspection.Health != "unhealthy" {
			return false
		}
		return true
	case selfheal.ActionStart:
		if inspection.Status != "exited" || (policy != "no" && policy != "on-failure" && policy != "") {
			return false
		}
		return inspection.ExitCode != 0 || inspection.OOMKilled
	}
	return false
}

// reprobeUnhealthy runs one more probe under the lease; only a definite
// failure lets the restart proceed.
func (e *healExecutor) reprobeUnhealthy(ctx context.Context, candidate selfheal.Candidate) bool {
	h := e.h
	h.probeMu.Lock()
	target, ok := h.targets[candidate.Container.ID]
	h.probeMu.Unlock()
	if !ok || target.Spec == nil {
		return false
	}
	result := h.probe(ctx, target.Network, candidate.Container.ID, target.Spec)
	return !result.Healthy && !result.Unknown
}

// record applies the executed action to the control and node state and
// journals it. A failed docker call counts as an action and is alerted.
func (e *healExecutor) record(candidate selfheal.Candidate, actErr error) {
	h := e.h
	now := h.now()
	decision, eventType, message := selfheal.DecisionRestarted, events.TypeHealRestarted, "self-healing restarted a container"
	if candidate.Action == selfheal.ActionStart {
		decision, eventType, message = selfheal.DecisionStarted, events.TypeHealStarted, "self-healing started a container"
	}
	result := "ok"
	if actErr != nil {
		decision, eventType, message, result = selfheal.DecisionFailed, events.TypeHealFailed, "self-healing action failed", "docker_error"
		if errors.Is(actErr, context.DeadlineExceeded) {
			result = "timeout"
		}
	}
	h.controlMu.Lock()
	control, controlErr := selfheal.ReadControl(h.dataDir, candidate.Project, candidate.Environment)
	node, nodeErr := selfheal.ReadNodeState(h.dataDir)
	if controlErr == nil && nodeErr == nil {
		selfheal.RecordAction(control, node, candidate, decision, now)
		logHealRecordError("control write", selfheal.WriteControl(h.dataDir, candidate.Project, candidate.Environment, control))
		logHealRecordError("node state write", selfheal.WriteNodeState(h.dataDir, node))
	}
	h.controlMu.Unlock()
	h.journal(candidate.Project, candidate.Environment, selfheal.JournalEntry{
		Time: now, Service: candidate.Service, Container: candidate.Container.Name, Revision: candidate.Revision,
		Decision: decision, Reason: candidate.Reason, Code: candidate.Code, Result: result,
	})
	details := map[string]string{
		"service": candidate.Service, "container": selfheal.SafeName(candidate.Container.Name),
		"action": candidate.Action, "reason": selfheal.SafeToken(candidate.Reason), "result": result,
	}
	if candidate.Code != 0 {
		details["code"] = fmt.Sprint(candidate.Code)
	}
	h.stateEvent(candidate.Project, candidate.Environment, eventType, message, details)
	if actErr != nil && h.alerts != nil {
		alertDetails := map[string]string{"action": candidate.Action, "reason": selfheal.SafeToken(candidate.Reason)}
		if token := safeContainerToken(candidate.Container.Name); token != "" {
			alertDetails["container"] = token
		}
		_ = h.alerts.ReportEvent(EventObservation{
			Project: candidate.Project, Environment: candidate.Environment, Kind: AlertKindSelfHeal,
			Subject: candidate.Service, Reason: healReasonActionFailed, Details: alertDetails, ObservedAt: now,
		})
	}
}

// scopeSkipExcept is scopeSkip for the executor: its own lease is held, so
// only a foreign holder blocks.
func (h *healManager) scopeSkipExcept(project, environment string, now time.Time) string {
	path, err := leasePath(h.dataDir, project, environment)
	if err != nil {
		return selfheal.SkipStateUnreadable
	}
	lease, err := readLeaseFile(path)
	if err != nil {
		return selfheal.SkipStateUnreadable
	}
	if lease == nil || !now.Before(lease.ExpiresAt) || lease.Operation == healLeaseOperation {
		return ""
	}
	return selfheal.SkipLeaseHeld
}

// healNodeGate returns a node-wide exclusion (D4) or "": an active worker
// fence, a pending restore, the lifecycle deny latch, or a node that is not
// schedulable in authoritative membership.
func (s *Server) healNodeGate(now time.Time) string {
	if active, err := activeWorkerOperation(s.dataDir, now); err != nil || active {
		return selfheal.SkipWorkerFence
	}
	if err := ensureNoPendingRestoreTransactionsOnNode(); err != nil {
		return selfheal.SkipRestorePending
	}
	if s.installation == nil {
		return ""
	}
	if _, err := os.Lstat(nodeidentity.DeploymentDenyPath(s.inventoryFile)); err == nil {
		return selfheal.SkipDenyLatch
	} else if !os.IsNotExist(err) {
		return selfheal.SkipDenyLatch
	}
	inventory, err := nodeidentity.ReadInventory(s.inventoryFile)
	if err != nil {
		return selfheal.SkipMembership
	}
	if inventory.ControllerNodeID == "" || inventory.Generation == 0 {
		return ""
	}
	node, ok := inventory.Node(s.installation.NodeID)
	if inventory.ClusterID != s.installation.ClusterID || !ok || node.MeshCredentialStatus != nodeidentity.MeshCredentialActive {
		return selfheal.SkipMembership
	}
	if node.Lifecycle != nodeidentity.NodeLifecycleSchedulable || !node.Schedulable {
		return selfheal.SkipNotSchedulable
	}
	return ""
}

func inspectHealContainer(ctx context.Context, id string) (*healInspection, error) {
	inspectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	output, err := runDocker(inspectCtx, "inspect", "--type", "container", id)
	if err != nil {
		return nil, fmt.Errorf("inspect failed")
	}
	var documents []dockerInspectDocument
	if err := json.Unmarshal([]byte(output), &documents); err != nil || len(documents) != 1 {
		return nil, fmt.Errorf("inspect output invalid")
	}
	document := documents[0]
	inspection := &healInspection{
		ID: document.ID, Name: strings.TrimPrefix(document.Name, "/"), Status: document.State.Status,
		Restarting: document.State.Restarting, OOMKilled: document.State.OOMKilled, ExitCode: document.State.ExitCode,
		RestartPolicy: document.HostConfig.RestartPolicy.Name, Image: document.Config.Image,
		Revision: document.Config.Labels["tako.revision"], Active: document.Config.Labels["tako.active"],
	}
	if document.State.Health != nil {
		inspection.Health = document.State.Health.Status
	}
	return inspection, nil
}

func dockerHealAction(ctx context.Context, action, id string, stopTimeout time.Duration) error {
	switch action {
	case selfheal.ActionRestart:
		_, err := runDocker(ctx, dockerHealRestartArgs(id, stopTimeout)...)
		return err
	case selfheal.ActionStart:
		_, err := runDocker(ctx, "start", id)
		return err
	}
	return fmt.Errorf("unknown self-healing action")
}

// dockerHealRestartArgs passes an explicit -t, so the stop phase never
// waits longer than healRestartStopMax whatever the container's own stop
// timeout is.
func dockerHealRestartArgs(id string, stopTimeout time.Duration) []string {
	if stopTimeout <= 0 || stopTimeout > healRestartStopMax {
		stopTimeout = healRestartStopMax
	}
	return []string{"restart", "-t", strconv.Itoa(int(stopTimeout / time.Second)), id}
}

func dockerBackupRunning(ctx context.Context, project, environment string) (bool, error) {
	listCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, role := range []string{"scheduled-backup", "backup-verification"} {
		output, err := runDocker(listCtx, "ps", "-q",
			"--filter", "label=tako.project="+project,
			"--filter", "label=tako.environment="+environment,
			"--filter", "label=tako.role="+role)
		if err != nil {
			return false, err
		}
		if strings.TrimSpace(output) != "" {
			return true, nil
		}
	}
	return false, nil
}
