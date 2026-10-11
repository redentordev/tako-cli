package takod

// Disk hygiene (add-disk-hygiene-defaults). One manager owns:
//   - the hygiene loop (D4) that replaces the build-cache prune loop: an
//     hourly pressure check and a daily maintenance tick (image GC, then
//     build-cache prune);
//   - the soft and hard reclaim (D5), also run by POST /v1/hygiene/reclaim
//     and by scheduler admission denials;
//   - the journal and GET /v1/hygiene (D7) and the `hygiene` alert kind.
//
// Every image removal goes through cleanupOldProjectImagesWithOptions, the
// v0.14.2 selector. The reclaim never removes volumes, backup archives,
// project files, running or non-Tako containers, pulled images or container
// log files: it only prunes dangling images and build cache, runs the image
// selector over recorded scopes, and removes eligible exited Tako containers
// with `docker rm` (never -f, never -v).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/recovery"
)

// CapabilityNodeDiskHygieneV1 means the node runs the hygiene loop, records
// image GC scopes, accepts DryRun cleanup requests, serves GET /v1/hygiene
// and POST /v1/hygiene/reclaim, and applies the default log rotation.
const CapabilityNodeDiskHygieneV1 = "node.disk-hygiene-v1"

const (
	DefaultHygieneCheckInterval          = time.Hour
	DefaultImageRetention                = 3
	DefaultBuildCachePressureKeepStorage = "2GB"

	HygieneLevelSoft = "soft"
	HygieneLevelHard = "hard"

	hygieneImageGrace       = 24 * time.Hour
	hygieneProactiveBackoff = 6 * time.Hour
	// hygieneSchedulerReclaimInterval is the minimum spacing of reclaims
	// signalled by scheduler free-disk denials.
	hygieneSchedulerReclaimInterval = 15 * time.Minute
	hygieneReclaimTimeout           = 5 * time.Minute
	hygieneRunTimeout               = 30 * time.Minute
	hygieneSoftCapBytes             = int64(50) << 30
	hygieneExitedContainerAge       = time.Hour
	hygieneOneOffContainerAge       = 24 * time.Hour
	hygieneStatusRuns               = 10
	hygieneUnrotatedLimit           = 50
	hygieneJournalImageLimit        = 100
	hygieneJournalSkipLimit         = 100
	hygieneSubjectRoot              = "root"
	hygieneSubjectDocker            = "docker"
	hygieneSubjectImageGC           = "image-gc"
	hygieneReasonInsufficient       = "reclaim_insufficient"
	hygieneReasonUnreadable         = "state_unreadable"
	hygieneEventInsufficient        = "disk_reclaim_insufficient"
	hygieneEventImageGCBlocked      = "image_gc_blocked"
	hygieneEventImageGCResumed      = "image_gc_resumed"
	hygieneEventResourceNormal      = "resource_normal"
	hygieneConvergedDirName         = "heal"
)

// HygienePolicy is the effective hygiene configuration of the node.
type HygienePolicy struct {
	ImageRetention                int    `json:"imageRetention"`
	ImageGraceSeconds             int64  `json:"imageGraceSeconds"`
	MaintenanceIntervalSeconds    int64  `json:"maintenanceIntervalSeconds"`
	CheckIntervalSeconds          int64  `json:"checkIntervalSeconds"`
	BuildCacheKeepStorage         string `json:"buildCacheKeepStorage"`
	BuildCachePressureKeepStorage string `json:"buildCachePressureKeepStorage"`
	MinimumFreeDiskBytes          int64  `json:"minimumFreeDiskBytes"`
}

// HygieneThreshold is one filesystem's free space and pressure thresholds.
type HygieneThreshold struct {
	HygieneFilesystem
	SoftBytes int64 `json:"softBytes"`
	HardBytes int64 `json:"hardBytes"`
	// Level is "", "soft" or "hard".
	Level string `json:"level,omitempty"`
}

// HygieneUnrotatedContainer is a running container whose json-file log has
// no size bound.
type HygieneUnrotatedContainer struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Project     string `json:"project,omitempty"`
	Environment string `json:"environment,omitempty"`
	Service     string `json:"service,omitempty"`
	LogBytes    int64  `json:"logBytes"`
}

// HygieneStatus is the GET /v1/hygiene response.
type HygieneStatus struct {
	Node          string                      `json:"node,omitempty"`
	Policy        HygienePolicy               `json:"policy"`
	Filesystems   []HygieneThreshold          `json:"filesystems,omitempty"`
	Logging       HygieneLoggingStatus        `json:"logging"`
	Scopes        []HygieneScopeRecord        `json:"scopes"`
	ScopeProblems []string                    `json:"scopeProblems,omitempty"`
	Runs          []HygieneRun                `json:"runs"`
	Unrotated     []HygieneUnrotatedContainer `json:"unrotated,omitempty"`
	// UnrotatedError is set when the running containers could not be read.
	UnrotatedError string               `json:"unrotatedError,omitempty"`
	JournalError   string               `json:"journalError,omitempty"`
	BackoffUntil   map[string]time.Time `json:"backoffUntil,omitempty"`
	// InsufficientSubjects are filesystems whose last hard reclaim ended
	// below the hard threshold and that have not recovered above soft.
	InsufficientSubjects []string `json:"insufficientSubjects,omitempty"`
}

// HygieneReclaimRequest is the POST /v1/hygiene/reclaim body.
type HygieneReclaimRequest struct {
	DryRun bool `json:"dryRun,omitempty"`
}

// hygieneContainer is one exited or dead container candidate.
type hygieneContainer struct {
	ID         string
	Name       string
	Status     string
	FinishedAt time.Time
	Image      string
	Labels     map[string]string
}

// hygieneOps are the Docker operations the manager issues. Tests replace
// them; the production values are the only Docker mutations hygiene makes.
type hygieneOps struct {
	pruneDangling        func(ctx context.Context) (int, error)
	pruneBuildCache      func(ctx context.Context, keepStorage string) error
	imageGC              func(ctx context.Context, dataDir, project, environment string, keep int, repositories []string, options imageCleanupOptions) (CleanupHygiene, error)
	listExitedContainers func(ctx context.Context) ([]hygieneContainer, error)
	removeContainer      func(ctx context.Context, id string) error
	listUnrotated        func(ctx context.Context) ([]HygieneUnrotatedContainer, error)
}

func defaultHygieneOps() hygieneOps {
	return hygieneOps{
		pruneDangling: cleanupDanglingImages,
		pruneBuildCache: func(ctx context.Context, keepStorage string) error {
			_, err := cleanupBuildCache(ctx, keepStorage)
			return err
		},
		imageGC:              cleanupOldProjectImagesWithOptions,
		listExitedContainers: listExitedTakoContainers,
		removeContainer: func(ctx context.Context, id string) error {
			// Never -f (running containers are refused) and never -v
			// (anonymous volumes are kept).
			output, err := runDocker(ctx, "rm", id)
			if err != nil {
				return fmt.Errorf("%w: %s", err, strings.TrimSpace(output))
			}
			return nil
		},
		listUnrotated: listUnrotatedContainers,
	}
}

type hygieneCall struct {
	done chan struct{}
	run  HygieneRun
	err  error
}

type hygieneManager struct {
	dataDir        string
	dockerDataRoot string
	nodeName       func() string
	policy         HygienePolicy
	maintenance    time.Duration
	check          time.Duration
	journal        *hygieneJournal
	logging        *containerLoggingPolicy
	alerts         *SourceAlertManager
	ops            hygieneOps
	diskFree       func(string) (int64, error)
	diskTotal      func(string) (int64, error)
	diskIdentity   func(string) (string, error)
	fenceActive    func(dataDir string, now time.Time) (bool, error)
	now            func() time.Time
	// lockAutonomous takes the maintenance barrier and the mutation lock for
	// loop-originated runs, as runBuildCachePruneLoop did.
	lockAutonomous func() (func(), error)

	signal chan struct{}
	slot   chan struct{}

	mu           sync.Mutex
	backoff      map[string]time.Time
	insufficient map[string]bool
	// schedulerLast is when the last scheduler-denied reclaim started.
	schedulerLast time.Time
	inflight      map[bool]*hygieneCall
	sequence      int
}

// hygieneSettings are the takod flags of the hygiene loop.
type hygieneSettings struct {
	// Interval is --hygiene-interval; nil means "use the legacy
	// --build-cache-prune-interval".
	Interval                      *time.Duration
	LegacyInterval                time.Duration
	CheckInterval                 time.Duration
	ImageRetention                int
	BuildCacheKeepStorage         string
	BuildCachePressureKeepStorage string
	MinimumFreeDiskBytes          int64
	LoggingOff                    bool
	LogMaxSize                    string
	LogMaxFile                    int
}

// resolveHygieneMaintenanceInterval maps the legacy flag: without
// --hygiene-interval the maintenance tick runs at --build-cache-prune-interval
// (every setup-written unit sets only that flag). 0 disables autonomous runs.
func resolveHygieneMaintenanceInterval(interval *time.Duration, legacy time.Duration) time.Duration {
	if interval != nil {
		if *interval < 0 {
			return 0
		}
		return *interval
	}
	if legacy < 0 {
		return 0
	}
	return legacy
}

func newHygieneManager(dataDir, dockerDataRoot string, settings hygieneSettings, alerts *SourceAlertManager) *hygieneManager {
	maintenance := resolveHygieneMaintenanceInterval(settings.Interval, settings.LegacyInterval)
	check := settings.CheckInterval
	if check <= 0 {
		check = DefaultHygieneCheckInterval
	}
	retention := settings.ImageRetention
	if retention < 1 {
		retention = DefaultImageRetention
	}
	keep := strings.TrimSpace(settings.BuildCacheKeepStorage)
	if keep == "" {
		keep = DefaultBuildCacheKeepStorage
	}
	pressureKeep := strings.TrimSpace(settings.BuildCachePressureKeepStorage)
	if pressureKeep == "" {
		pressureKeep = DefaultBuildCachePressureKeepStorage
	}
	if strings.TrimSpace(dockerDataRoot) == "" {
		dockerDataRoot = dataDir
	}
	manager := &hygieneManager{
		dataDir:        dataDir,
		dockerDataRoot: dockerDataRoot,
		nodeName:       func() string { return "" },
		maintenance:    maintenance,
		check:          check,
		policy: HygienePolicy{
			ImageRetention:                retention,
			ImageGraceSeconds:             int64(hygieneImageGrace / time.Second),
			MaintenanceIntervalSeconds:    int64(maintenance / time.Second),
			CheckIntervalSeconds:          int64(check / time.Second),
			BuildCacheKeepStorage:         keep,
			BuildCachePressureKeepStorage: pressureKeep,
			MinimumFreeDiskBytes:          settings.MinimumFreeDiskBytes,
		},
		journal:      openHygieneJournal(dataDir),
		logging:      newContainerLoggingPolicy(settings.LoggingOff, settings.LogMaxSize, settings.LogMaxFile),
		alerts:       alerts,
		ops:          defaultHygieneOps(),
		diskFree:     availableDiskBytes,
		diskTotal:    totalDiskBytes,
		diskIdentity: diskFilesystemIdentity,
		fenceActive:  activeWorkerOperation,
		now:          func() time.Time { return time.Now().UTC() },
		signal:       make(chan struct{}, 1),
		slot:         make(chan struct{}, 1),
		backoff:      map[string]time.Time{},
		insufficient: map[string]bool{},
		inflight:     map[bool]*hygieneCall{},
	}
	manager.lockAutonomous = func() (func(), error) {
		unlockBarrier, err := recovery.AcquireMaintenanceBarrier(dataDir)
		if err != nil {
			return nil, fmt.Errorf("maintenance barrier: %w", err)
		}
		active, activeErr := manager.fenceActive(dataDir, manager.now())
		if activeErr != nil || active {
			unlockBarrier()
			if activeErr != nil {
				return nil, fmt.Errorf("%w: %v", errHygieneFenceActive, activeErr)
			}
			return nil, errHygieneFenceActive
		}
		unlockMutation, err := recovery.AcquireMutationLock(dataDir)
		if err != nil {
			unlockBarrier()
			return nil, fmt.Errorf("mutation lock: %w", err)
		}
		return func() {
			unlockMutation()
			unlockBarrier()
		}, nil
	}
	return manager
}

var errHygieneFenceActive = errors.New("worker fence active")

// initHygiene creates the hygiene manager, registers the hygiene alert
// source and wraps the scheduler admission checks so that a free-disk
// denial signals a reclaim.
func (s *Server) initHygiene(opts ServerOptions) {
	manager := newHygieneManager(s.dataDir, s.dockerDataRoot, hygieneSettings{
		Interval:                      opts.HygieneInterval,
		LegacyInterval:                opts.BuildCachePruneInterval,
		CheckInterval:                 opts.HygieneCheckInterval,
		ImageRetention:                opts.ImageRetention,
		BuildCacheKeepStorage:         opts.BuildCacheKeepStorage,
		BuildCachePressureKeepStorage: opts.BuildCachePressureKeepStorage,
		MinimumFreeDiskBytes:          s.minimumFreeDiskBytes,
		LoggingOff:                    opts.DefaultContainerLoggingOff,
		LogMaxSize:                    opts.DefaultContainerLogMaxSize,
		LogMaxFile:                    opts.DefaultContainerLogMaxFile,
	}, s.sourceAlerts)
	manager.nodeName = func() string { return s.nodeName }
	manager.diskFree = s.diskAvailable
	manager.diskIdentity = s.diskIdentity
	if s.sourceAlerts != nil {
		if err := s.sourceAlerts.RegisterSource(hygieneAlertSourceSpec()); err != nil {
			panic(err)
		}
	}
	s.hygiene = manager
	if s.backupScheduler != nil && s.backupScheduler.admit != nil {
		s.backupScheduler.admit = manager.admitWithReclaim(s.backupScheduler.admit)
	}
	if s.jobScheduler != nil && s.jobScheduler.admit != nil {
		s.jobScheduler.admit = manager.admitWithReclaim(s.jobScheduler.admit)
	}
	if s.certificateScheduler != nil && s.certificateScheduler.admit != nil {
		s.certificateScheduler.admit = manager.admitWithReclaim(s.certificateScheduler.admit)
	}
}

// hygieneAlertSourceSpec registers kind hygiene (monitoring design D13).
func hygieneAlertSourceSpec() AlertSourceSpec {
	return AlertSourceSpec{
		Kind:       AlertKindHygiene,
		NodeScoped: true,
		Reasons: map[string]Severity{
			hygieneReasonInsufficient: SeverityCritical,
			hygieneReasonUnreadable:   SeverityWarning,
		},
		DetailKeys: []string{"freeBytes", "softBytes", "hardBytes", "unreadableScopes"},
		Events: map[string]string{
			hygieneReasonInsufficient: hygieneEventInsufficient,
			hygieneReasonUnreadable:   hygieneEventImageGCBlocked,
		},
		Recoveries: map[string]string{
			hygieneReasonInsufficient: hygieneEventResourceNormal,
			hygieneReasonUnreadable:   hygieneEventImageGCResumed,
		},
	}
}

// signalReclaim asks the loop for a hard reclaim without blocking; bursts
// coalesce into one pending signal.
func (h *hygieneManager) signalReclaim() {
	if h == nil {
		return
	}
	select {
	case h.signal <- struct{}{}:
	default:
	}
}

// admitWithReclaim wraps a scheduler admission check: a free-disk denial
// signals a reclaim and is returned unchanged, so scheduling is unchanged.
func (h *hygieneManager) admitWithReclaim(admit func(...string) error) func(...string) error {
	return func(paths ...string) error {
		err := admit(paths...)
		if err != nil && strings.HasPrefix(err.Error(), "operation denied") {
			h.signalReclaim()
		}
		return err
	}
}

// Run is the hygiene loop. It refreshes the logging guard on every check
// tick; with a zero maintenance interval it runs nothing autonomously.
func (h *hygieneManager) Run(ctx context.Context) {
	// A failed docker probe of the logging guard is retried every
	// loggingPolicyRetryInterval until it succeeds.
	retry := time.NewTimer(loggingPolicyRetryInterval)
	retry.Stop()
	defer retry.Stop()
	refreshLogging := func() {
		h.logging.Refresh(ctx)
		if h.logging.NeedsRetry() {
			retry.Reset(loggingPolicyRetryInterval)
		}
	}
	refreshLogging()
	checkTicker := time.NewTicker(h.check)
	defer checkTicker.Stop()
	var maintenance <-chan time.Time
	var timer *time.Timer
	if h.maintenance > 0 {
		timer = time.NewTimer(buildCachePruneInitialDelay(h.maintenance))
		defer timer.Stop()
		maintenance = timer.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-retry.C:
			refreshLogging()
		case <-checkTicker.C:
			refreshLogging()
			if h.maintenance > 0 {
				h.PressureCheck(ctx)
			}
		case <-maintenance:
			h.runAutonomous(ctx, HygieneTriggerSchedule, func(runCtx context.Context) HygieneRun {
				return h.maintenanceRun(runCtx)
			})
			timer.Reset(h.maintenance)
		case <-h.signal:
			if h.maintenance > 0 {
				h.SchedulerDeniedReclaim(ctx)
			}
		}
	}
}

// runAutonomous runs one loop-originated run under the maintenance barrier
// and mutation lock, and records a skipped run while the worker fence is
// active.
func (h *hygieneManager) runAutonomous(ctx context.Context, trigger string, fn func(context.Context) HygieneRun) (HygieneRun, bool) {
	unlock, err := h.lockAutonomous()
	if err != nil {
		started := h.now()
		run := HygieneRun{Trigger: trigger, StartedAt: started, FinishedAt: started}
		if errors.Is(err, errHygieneFenceActive) {
			run.Skipped = []string{"worker fence active"}
		} else {
			run.Skipped = []string{"locks unavailable: " + firstLine(err.Error())}
		}
		h.record(&run)
		return run, false
	}
	defer unlock()
	if !h.acquireSlot(ctx) {
		return HygieneRun{}, false
	}
	defer h.releaseSlot()
	runCtx, cancel := context.WithTimeout(ctx, hygieneRunTimeout)
	defer cancel()
	run := fn(runCtx)
	h.record(&run)
	return run, true
}

func (h *hygieneManager) acquireSlot(ctx context.Context) bool {
	select {
	case h.slot <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (h *hygieneManager) releaseSlot() { <-h.slot }

func (h *hygieneManager) record(run *HygieneRun) {
	h.mu.Lock()
	h.sequence++
	if run.ID == "" {
		run.ID = fmt.Sprintf("%s-%d", run.StartedAt.UTC().Format("20060102T150405Z"), h.sequence)
	}
	h.mu.Unlock()
	if run.Node == "" {
		run.Node = h.nodeName()
	}
	if len(run.Skipped) > hygieneJournalSkipLimit {
		run.Skipped = append(run.Skipped[:hygieneJournalSkipLimit], fmt.Sprintf("%d more skip reasons omitted", len(run.Skipped)-hygieneJournalSkipLimit))
	}
	h.journal.Append(*run)
	if err := h.journal.LastError(); err != nil {
		log.Printf("takod hygiene journal write failed: %v", err)
	}
}

// thresholds returns the soft and hard free-space thresholds of a
// filesystem: soft = max(1.5 x min, min(10% of total, 50 GiB)), hard =
// 1.5 x min.
func hygieneThresholds(minimumFree int64, total int64) (soft int64, hard int64) {
	if minimumFree > 0 {
		hard = minimumFree + minimumFree/2
	}
	share := int64(0)
	if total > 0 {
		share = total / 10
		if share > hygieneSoftCapBytes {
			share = hygieneSoftCapBytes
		}
	}
	soft = hard
	if share > soft {
		soft = share
	}
	return soft, hard
}

func hygieneLevel(free, soft, hard int64) string {
	switch {
	case hard > 0 && free < hard:
		return HygieneLevelHard
	case soft > 0 && free < soft:
		return HygieneLevelSoft
	}
	return ""
}

// measure returns the takod data directory ("root") and, when it is a
// separate filesystem, the Docker data root ("docker").
func (h *hygieneManager) measure() ([]HygieneThreshold, error) {
	targets := []HygieneFilesystem{{Subject: hygieneSubjectRoot, Path: h.dataDir}}
	if h.dockerDataRoot != "" && filepath.Clean(h.dockerDataRoot) != filepath.Clean(h.dataDir) {
		rootID, rootErr := h.diskIdentity(h.dataDir)
		dockerID, dockerErr := h.diskIdentity(h.dockerDataRoot)
		if rootErr != nil || dockerErr != nil || rootID != dockerID {
			targets = append(targets, HygieneFilesystem{Subject: hygieneSubjectDocker, Path: h.dockerDataRoot})
		}
	}
	var measured []HygieneThreshold
	var errs []string
	for _, target := range targets {
		free, err := h.diskFree(target.Path)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", target.Path, err))
			continue
		}
		total, err := h.diskTotal(target.Path)
		if err != nil {
			total = 0
		}
		target.FreeBytes, target.TotalBytes = free, total
		soft, hard := hygieneThresholds(h.policy.MinimumFreeDiskBytes, total)
		measured = append(measured, HygieneThreshold{HygieneFilesystem: target, SoftBytes: soft, HardBytes: hard, Level: hygieneLevel(free, soft, hard)})
	}
	if len(errs) > 0 {
		return measured, fmt.Errorf("measure free disk: %s", strings.Join(errs, "; "))
	}
	return measured, nil
}

func filesystemsOf(thresholds []HygieneThreshold) []HygieneFilesystem {
	filesystems := make([]HygieneFilesystem, 0, len(thresholds))
	for _, threshold := range thresholds {
		filesystems = append(filesystems, threshold.HygieneFilesystem)
	}
	return filesystems
}

// pressureLevel is the worst level across filesystems.
func pressureLevel(thresholds []HygieneThreshold) string {
	level := ""
	for _, threshold := range thresholds {
		switch threshold.Level {
		case HygieneLevelHard:
			return HygieneLevelHard
		case HygieneLevelSoft:
			level = HygieneLevelSoft
		}
	}
	return level
}

func allAboveSoft(thresholds []HygieneThreshold, err error) bool {
	if err != nil || len(thresholds) == 0 {
		return false
	}
	for _, threshold := range thresholds {
		if threshold.FreeBytes < threshold.SoftBytes {
			return false
		}
	}
	return true
}

func totalFree(thresholds []HygieneThreshold) int64 {
	var total int64
	for _, threshold := range thresholds {
		total += threshold.FreeBytes
	}
	return total
}

// PressureCheck runs one hourly check: it recovers insufficient-reclaim
// alerts above the soft threshold and runs a proactive reclaim at the level
// it finds, unless that level is backed off.
func (h *hygieneManager) PressureCheck(ctx context.Context) (HygieneRun, bool) {
	thresholds, err := h.measure()
	if err != nil && len(thresholds) == 0 {
		log.Printf("takod hygiene pressure check: %v", err)
		return HygieneRun{}, false
	}
	h.reportFilesystemAlerts(thresholds, false)
	level := pressureLevel(thresholds)
	if level == "" {
		return HygieneRun{}, false
	}
	now := h.now()
	h.mu.Lock()
	until := h.backoff[level]
	h.mu.Unlock()
	if now.Before(until) {
		return HygieneRun{}, false
	}
	trigger := HygieneTriggerPressureSoft
	if level == HygieneLevelHard {
		trigger = HygieneTriggerPressureHard
	}
	run, ran := h.runAutonomous(ctx, trigger, func(runCtx context.Context) HygieneRun {
		return h.reclaim(runCtx, trigger, level, false)
	})
	if ran && run.Insufficient {
		h.mu.Lock()
		h.backoff[level] = h.now().Add(hygieneProactiveBackoff)
		h.mu.Unlock()
	}
	return run, ran
}

// SchedulerDeniedReclaim runs the hard reclaim signalled by a scheduler
// free-disk denial. Scheduled jobs can be denied every minute, so these
// signals honour the hard-level backoff set by an insufficient hard run and
// are spaced by hygieneSchedulerReclaimInterval; suppressed signals are not
// journaled. POST /v1/hygiene/reclaim is never throttled.
func (h *hygieneManager) SchedulerDeniedReclaim(ctx context.Context) (HygieneRun, bool) {
	now := h.now()
	h.mu.Lock()
	if now.Before(h.backoff[HygieneLevelHard]) || (!h.schedulerLast.IsZero() && now.Sub(h.schedulerLast) < hygieneSchedulerReclaimInterval) {
		h.mu.Unlock()
		return HygieneRun{}, false
	}
	h.schedulerLast = now
	h.mu.Unlock()
	run, ran := h.runAutonomous(ctx, HygieneTriggerSchedulerDenied, func(runCtx context.Context) HygieneRun {
		return h.reclaim(runCtx, HygieneTriggerSchedulerDenied, HygieneLevelHard, false)
	})
	if ran && run.Insufficient {
		h.mu.Lock()
		h.backoff[HygieneLevelHard] = h.now().Add(hygieneProactiveBackoff)
		h.mu.Unlock()
	}
	return run, ran
}

// Reclaim runs a hard reclaim for POST /v1/hygiene/reclaim. Concurrent
// callers with the same dryRun share one run. The caller's request already
// holds the snapshot mutation lock, so no further locks are taken.
func (h *hygieneManager) Reclaim(ctx context.Context, dryRun bool) (HygieneRun, error) {
	h.mu.Lock()
	if call := h.inflight[dryRun]; call != nil {
		h.mu.Unlock()
		select {
		case <-call.done:
			return call.run, call.err
		case <-ctx.Done():
			return HygieneRun{}, ctx.Err()
		}
	}
	call := &hygieneCall{done: make(chan struct{})}
	h.inflight[dryRun] = call
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.inflight, dryRun)
		h.mu.Unlock()
		close(call.done)
	}()

	ctx, cancel := context.WithTimeout(ctx, hygieneReclaimTimeout)
	defer cancel()
	if !h.acquireSlot(ctx) {
		call.err = fmt.Errorf("hygiene reclaim did not start within %s: another hygiene run is active", hygieneReclaimTimeout)
		return HygieneRun{}, call.err
	}
	defer h.releaseSlot()
	call.run = h.reclaim(ctx, HygieneTriggerRequest, HygieneLevelHard, dryRun)
	h.record(&call.run)
	return call.run, nil
}

type hygieneRunState struct {
	run          *HygieneRun
	dryRun       bool
	fenceChecked bool
	fenceActive  bool
	scopeCache   map[string]*hygieneScopeState
}

type hygieneScopeState struct {
	blocked       string
	desiredImages map[string]bool
	desiredErr    error
	converged     map[string]bool
	convergedErr  error
	// retainedFailed are containers kept by retain-failed rollouts.
	retainedFailed    map[string]bool
	retainedFailedErr error
	loaded            bool
}

func (h *hygieneManager) newRun(trigger, level string, dryRun bool) *hygieneRunState {
	return &hygieneRunState{
		run:        &HygieneRun{Trigger: trigger, Level: level, DryRun: dryRun, StartedAt: h.now(), Categories: map[string]HygieneCategory{}},
		dryRun:     dryRun,
		scopeCache: map[string]*hygieneScopeState{},
	}
}

func (s *hygieneRunState) skip(format string, args ...any) {
	s.run.Skipped = append(s.run.Skipped, fmt.Sprintf(format, args...))
}

func (s *hygieneRunState) addCategory(category string, removed int, bytes int64) {
	current := s.run.Categories[category]
	current.Removed += removed
	if bytes > 0 {
		current.BytesFreed += bytes
	}
	s.run.Categories[category] = current
}

func (h *hygieneManager) workerFenceActive(state *hygieneRunState) bool {
	if !state.fenceChecked {
		state.fenceChecked = true
		active, err := h.fenceActive(h.dataDir, h.now())
		state.fenceActive = active || err != nil
	}
	return state.fenceActive
}

// freedBy measures the free space gained by one step on every measured
// filesystem.
func (h *hygieneManager) freedBy(step func()) int64 {
	before, beforeErr := h.measure()
	step()
	after, afterErr := h.measure()
	if beforeErr != nil || afterErr != nil {
		return 0
	}
	delta := totalFree(after) - totalFree(before)
	if delta < 0 {
		return 0
	}
	return delta
}

// reclaim runs the ordered reclaim steps of D5 and stops once every
// filesystem is above its soft threshold.
func (h *hygieneManager) reclaim(ctx context.Context, trigger, level string, dryRun bool) HygieneRun {
	state := h.newRun(trigger, level, dryRun)
	before, measureErr := h.measure()
	state.run.Before = filesystemsOf(before)
	if measureErr != nil {
		state.skip("measure: %s", firstLine(measureErr.Error()))
	}
	type step struct {
		name     string
		hardOnly bool
		run      func()
	}
	steps := []step{
		{name: HygieneCategoryDanglingImages, run: func() { h.pruneDanglingStep(ctx, state) }},
		{name: HygieneCategoryBuildCache, run: func() { h.pruneBuildCacheStep(ctx, state, h.policy.BuildCacheKeepStorage) }},
		{name: HygieneCategoryImages, run: func() { h.imageGCStep(ctx, state) }},
		{name: HygieneCategoryBuildCache, hardOnly: true, run: func() { h.pruneBuildCacheStep(ctx, state, h.policy.BuildCachePressureKeepStorage) }},
		{name: HygieneCategoryContainers, hardOnly: true, run: func() { h.containersStep(ctx, state) }},
	}
	for _, step := range steps {
		if step.hardOnly && level != HygieneLevelHard {
			continue
		}
		if ctx.Err() != nil {
			state.skip("%s: %v", step.name, ctx.Err())
			break
		}
		current, err := h.measure()
		if allAboveSoft(current, err) {
			state.run.StoppedEarly = true
			break
		}
		step.run()
	}
	after, afterErr := h.measure()
	state.run.After = filesystemsOf(after)
	state.run.FinishedAt = h.now()
	for _, category := range state.run.Categories {
		state.run.BytesFreed += category.BytesFreed
	}
	if afterErr == nil && !dryRun {
		for _, threshold := range after {
			limit := threshold.SoftBytes
			if level == HygieneLevelHard {
				limit = threshold.HardBytes
			}
			if threshold.FreeBytes < limit {
				state.run.Insufficient = true
			}
		}
		if level == HygieneLevelHard {
			h.reportFilesystemAlerts(after, true)
		}
	}
	return *state.run
}

func (h *hygieneManager) pruneDanglingStep(ctx context.Context, state *hygieneRunState) {
	if state.dryRun {
		state.skip("%s: not simulated in a dry run", HygieneCategoryDanglingImages)
		return
	}
	var removed int
	var err error
	freed := h.freedBy(func() { removed, err = h.ops.pruneDangling(ctx) })
	if err != nil {
		state.skip("%s: %s", HygieneCategoryDanglingImages, firstLine(err.Error()))
	}
	state.addCategory(HygieneCategoryDanglingImages, removed, freed)
}

func (h *hygieneManager) pruneBuildCacheStep(ctx context.Context, state *hygieneRunState, keepStorage string) {
	if state.dryRun {
		state.skip("%s: not simulated in a dry run (keep %s)", HygieneCategoryBuildCache, keepStorage)
		return
	}
	var err error
	freed := h.freedBy(func() { err = h.ops.pruneBuildCache(ctx, keepStorage) })
	if err != nil {
		state.skip("%s: %s", HygieneCategoryBuildCache, firstLine(err.Error()))
	}
	state.addCategory(HygieneCategoryBuildCache, 0, freed)
}

// scopeBlocked returns why autonomous hygiene must not touch a scope's
// images or containers, or "".
func (h *hygieneManager) scopeBlocked(state *hygieneRunState, project, environment string) string {
	if h.workerFenceActive(state) {
		return "worker fence active"
	}
	path, err := leasePath(h.dataDir, project, environment)
	if err != nil {
		return "lease unreadable"
	}
	lease, err := readLeaseFile(path)
	if err != nil {
		return "lease unreadable"
	}
	if lease != nil && h.now().Before(lease.ExpiresAt) {
		return "lease held"
	}
	entries, err := os.ReadDir(filepath.Join(h.dataDir, retainedStateDirName, project, environment))
	if err != nil && !os.IsNotExist(err) {
		return "restore-on-failure retention unreadable"
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			return "restore-on-failure retention"
		}
	}
	return ""
}

// imageGCStep runs the v0.14.2 selector over every recorded scope with the
// node retention and the 24h grace.
func (h *hygieneManager) imageGCStep(ctx context.Context, state *hygieneRunState) {
	var report imageGCTotals
	var unreadable, selected int
	freed := h.freedBy(func() { report, unreadable, selected = h.imageGC(ctx, state) })
	if state.dryRun || freed == 0 {
		freed = report.bytes
	}
	state.addCategory(HygieneCategoryImages, report.removed, freed)
	if !state.dryRun {
		h.reportImageGCAlert(unreadable, selected)
	}
}

type imageGCTotals struct {
	removed int
	bytes   int64
}

func (h *hygieneManager) imageGC(ctx context.Context, state *hygieneRunState) (imageGCTotals, int, int) {
	var totals imageGCTotals
	unreadable := 0
	selected := 0
	scopes, problems, err := listHygieneScopes(h.dataDir)
	if err != nil {
		state.skip("images: unreadable state: %s", firstLine(err.Error()))
		return totals, 1, 0
	}
	for _, problem := range problems {
		state.skip("images: unreadable state: %s", firstLine(problem))
		unreadable++
	}
	cutoff := h.now().Add(-hygieneImageGrace)
	for _, scope := range scopes {
		label := scope.Project + "/" + scope.Environment
		if reason := h.scopeBlocked(state, scope.Project, scope.Environment); reason != "" {
			state.skip("images %s: %s", label, reason)
			continue
		}
		report, err := h.ops.imageGC(ctx, h.dataDir, scope.Project, scope.Environment, h.policy.ImageRetention, scope.ImageRepositories, imageCleanupOptions{CreatedAfter: cutoff, DryRun: state.dryRun})
		if err != nil {
			if errors.Is(err, errImageCleanupStateUnreadable) {
				state.skip("images %s: unreadable state: %s", label, firstLine(err.Error()))
				unreadable++
				continue
			}
			state.skip("images %s: %s", label, firstLine(err.Error()))
		}
		selected++
		for reason, count := range report.Kept {
			if state.run.Kept == nil {
				state.run.Kept = map[string]int{}
			}
			state.run.Kept[reason] += count
		}
		for _, image := range report.Removed {
			if len(state.run.Images) < hygieneJournalImageLimit {
				state.run.Images = append(state.run.Images, image)
			}
		}
		totals.removed += len(report.Removed)
		totals.bytes += report.BytesFreed
	}
	return totals, unreadable, selected
}

// containersStep removes eligible exited Tako containers (hard only).
func (h *hygieneManager) containersStep(ctx context.Context, state *hygieneRunState) {
	containers, err := h.ops.listExitedContainers(ctx)
	if err != nil {
		state.skip("containers: %s", firstLine(err.Error()))
		return
	}
	removed := 0
	freed := h.freedBy(func() {
		for _, container := range containers {
			if !h.containerEligible(state, container) {
				continue
			}
			if state.dryRun {
				state.run.Containers = append(state.run.Containers, container.Name)
				removed++
				continue
			}
			if err := h.ops.removeContainer(ctx, container.ID); err != nil {
				state.skip("container %s: %s", container.Name, firstLine(err.Error()))
				continue
			}
			state.run.Containers = append(state.run.Containers, container.Name)
			removed++
		}
	})
	state.addCategory(HygieneCategoryContainers, removed, freed)
}

// containerEligible applies D5: an exited or dead Tako container that
// finished more than 1h ago (one-offs 24h), outside held-lease and retention
// scopes, not listed in a converged record and not a replica of the current
// desired revision. Anything that cannot be proven eligible is kept.
func (h *hygieneManager) containerEligible(state *hygieneRunState, container hygieneContainer) bool {
	if container.Status != "exited" && container.Status != "dead" {
		return false
	}
	project := container.Labels["tako.project"]
	environment := container.Labels["tako.environment"]
	if !isSafeProjectName(project) || environment == "" || !isSafeRuntimeName(environment) {
		return false
	}
	if container.FinishedAt.IsZero() {
		return false
	}
	role := container.Labels["tako.role"]
	minimumAge := hygieneExitedContainerAge
	if role != "" {
		minimumAge = hygieneOneOffContainerAge
	}
	if h.now().Sub(container.FinishedAt) < minimumAge {
		return false
	}
	scope := h.loadScopeState(state, project, environment)
	if scope.blocked != "" || scope.convergedErr != nil || scope.retainedFailedErr != nil {
		return false
	}
	// Failed containers kept stopped by a retain-failed rollout stay for
	// debugging until the next reconcile of their service removes them.
	if scope.retainedFailed[strings.TrimPrefix(container.Name, "/")] {
		return false
	}
	if scope.converged[container.ID] || scope.converged[container.Name] {
		return false
	}
	for id := range scope.converged {
		if len(id) >= 12 && strings.HasPrefix(container.ID, id) {
			return false
		}
	}
	if role == "" {
		// A replica is current when its image is named by the current
		// desired revision. Without a readable desired document, keep it.
		if scope.desiredErr != nil || scope.desiredImages == nil {
			return false
		}
		if scope.desiredImages[container.Image] {
			return false
		}
	}
	return true
}

func (h *hygieneManager) loadScopeState(state *hygieneRunState, project, environment string) *hygieneScopeState {
	key := project + "/" + environment
	if scope := state.scopeCache[key]; scope != nil {
		return scope
	}
	scope := &hygieneScopeState{loaded: true}
	state.scopeCache[key] = scope
	scope.blocked = h.scopeBlocked(state, project, environment)
	var desired imageStateServices
	if err := readImageStateJSON(filepath.Join(h.dataDir, "desired", project, environment, "revision.json"), &desired); err != nil {
		scope.desiredErr = err
	} else {
		scope.desiredImages = map[string]bool{}
		for _, image := range desired.images() {
			if image = strings.TrimSpace(image); image != "" {
				scope.desiredImages[image] = true
			}
		}
	}
	scope.converged, scope.convergedErr = readConvergedContainers(h.dataDir, project, environment)
	scope.retainedFailed, scope.retainedFailedErr = readRetainedFailedContainers(h.dataDir, project, environment)
	return scope
}

// readConvergedContainers reads the container names and IDs listed in the
// self-healing converged records of a scope
// (<dataDir>/heal/<project>/<env>/services/*.json), when present.
func readConvergedContainers(dataDir, project, environment string) (map[string]bool, error) {
	names := map[string]bool{}
	dir := filepath.Join(dataDir, hygieneConvergedDirName, project, environment, "services")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return names, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var record struct {
			Containers []struct {
				Name string `json:"name"`
				ID   string `json:"id"`
			} `json:"containers"`
		}
		if err := readImageStateJSON(filepath.Join(dir, entry.Name()), &record); err != nil {
			return nil, err
		}
		for _, container := range record.Containers {
			if container.Name != "" {
				names[strings.TrimPrefix(container.Name, "/")] = true
			}
			if container.ID != "" {
				names[container.ID] = true
			}
		}
	}
	return names, nil
}

// maintenanceRun is the daily tick: image GC over recorded scopes, then the
// build-cache prune to the configured budget.
func (h *hygieneManager) maintenanceRun(ctx context.Context) HygieneRun {
	state := h.newRun(HygieneTriggerSchedule, "", false)
	before, err := h.measure()
	state.run.Before = filesystemsOf(before)
	if err != nil {
		state.skip("measure: %s", firstLine(err.Error()))
	}
	var totals imageGCTotals
	var unreadable, selected int
	freed := h.freedBy(func() { totals, unreadable, selected = h.imageGC(ctx, state) })
	if freed == 0 {
		freed = totals.bytes
	}
	state.addCategory(HygieneCategoryImages, totals.removed, freed)
	h.reportImageGCAlert(unreadable, selected)
	h.pruneBuildCacheStep(ctx, state, h.policy.BuildCacheKeepStorage)
	after, _ := h.measure()
	state.run.After = filesystemsOf(after)
	state.run.FinishedAt = h.now()
	for _, category := range state.run.Categories {
		state.run.BytesFreed += category.BytesFreed
	}
	return *state.run
}

// reportFilesystemAlerts opens reclaim_insufficient on filesystems still
// below the hard threshold after a hard reclaim, and recovers them once free
// space is above the soft threshold.
func (h *hygieneManager) reportFilesystemAlerts(thresholds []HygieneThreshold, afterHardReclaim bool) {
	for _, threshold := range thresholds {
		h.mu.Lock()
		open := h.insufficient[threshold.Subject]
		failing := false
		report := false
		switch {
		case afterHardReclaim && threshold.HardBytes > 0 && threshold.FreeBytes < threshold.HardBytes:
			failing, report = true, true
			h.insufficient[threshold.Subject] = true
		case open && threshold.FreeBytes >= threshold.SoftBytes:
			report = true
			delete(h.insufficient, threshold.Subject)
		case open:
			failing, report = true, true
		}
		h.mu.Unlock()
		if !report || h.alerts == nil {
			continue
		}
		observation := LevelObservation{
			Kind: AlertKindHygiene, Subject: threshold.Subject, Interval: h.check, ObservedAt: h.now(),
			Details: map[string]string{
				"freeBytes": fmt.Sprintf("%d", threshold.FreeBytes),
				"softBytes": fmt.Sprintf("%d", threshold.SoftBytes),
				"hardBytes": fmt.Sprintf("%d", threshold.HardBytes),
			},
		}
		if failing {
			observation.Failing = []string{hygieneReasonInsufficient}
		}
		if err := h.alerts.ReportLevel(observation); err != nil {
			log.Printf("takod hygiene alert: %v", err)
		}
	}
}

// reportImageGCAlert opens state_unreadable when a scope was skipped for
// unreadable state and recovers it after a fully readable selection.
func (h *hygieneManager) reportImageGCAlert(unreadable, selected int) {
	if h.alerts == nil || (unreadable == 0 && selected == 0) {
		return
	}
	interval := h.maintenance
	if interval <= 0 {
		interval = DefaultBuildCachePruneInterval
	}
	observation := LevelObservation{Kind: AlertKindHygiene, Subject: hygieneSubjectImageGC, Interval: interval, ObservedAt: h.now()}
	if unreadable > 0 {
		observation.Failing = []string{hygieneReasonUnreadable}
		observation.Details = map[string]string{"unreadableScopes": fmt.Sprintf("%d", unreadable)}
	}
	if err := h.alerts.ReportLevel(observation); err != nil {
		log.Printf("takod hygiene alert: %v", err)
	}
}

// journalCleanup records a scoped old-image cleanup or dry run.
func (h *hygieneManager) journalCleanup(request CleanupRequest, response *CleanupResponse) {
	if h == nil || response == nil || response.Hygiene == nil {
		return
	}
	trigger := HygieneTriggerCleanup
	if request.Trigger == HygieneTriggerDeploy {
		trigger = HygieneTriggerDeploy
	}
	now := h.now()
	report := response.Hygiene
	run := HygieneRun{
		Trigger: trigger, DryRun: report.DryRun, StartedAt: now, FinishedAt: now,
		Categories: map[string]HygieneCategory{HygieneCategoryImages: {Removed: len(report.Removed), BytesFreed: report.BytesFreed}},
		Kept:       report.Kept, BytesFreed: report.BytesFreed,
	}
	for _, image := range report.Removed {
		if len(run.Images) < hygieneJournalImageLimit {
			run.Images = append(run.Images, image)
		}
	}
	scope := request.Project + "/" + request.Environment
	for _, failure := range report.Failed {
		run.Skipped = append(run.Skipped, fmt.Sprintf("images %s: %s", scope, firstLine(failure)))
	}
	for _, warning := range response.Warnings {
		if strings.Contains(warning, errImageCleanupStateUnreadable.Error()) {
			run.Skipped = append(run.Skipped, fmt.Sprintf("images %s: unreadable state: %s", scope, firstLine(warning)))
		}
	}
	h.record(&run)
}

// Status builds the GET /v1/hygiene response.
func (h *hygieneManager) Status(ctx context.Context) HygieneStatus {
	status := HygieneStatus{Node: h.nodeName(), Policy: h.policy, Logging: h.logging.Status(), Runs: h.journal.Recent(hygieneStatusRuns)}
	if status.Runs == nil {
		status.Runs = []HygieneRun{}
	}
	if thresholds, err := h.measure(); err == nil || len(thresholds) > 0 {
		status.Filesystems = thresholds
	}
	scopes, problems, err := listHygieneScopes(h.dataDir)
	if err != nil {
		problems = append(problems, err.Error())
	}
	status.Scopes = scopes
	if status.Scopes == nil {
		status.Scopes = []HygieneScopeRecord{}
	}
	status.ScopeProblems = problems
	listCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	unrotated, err := h.ops.listUnrotated(listCtx)
	if err != nil {
		status.UnrotatedError = firstLine(err.Error())
	}
	status.Unrotated = unrotated
	if err := h.journal.LastError(); err != nil {
		status.JournalError = firstLine(err.Error())
	}
	now := h.now()
	h.mu.Lock()
	for level, until := range h.backoff {
		if now.Before(until) {
			if status.BackoffUntil == nil {
				status.BackoffUntil = map[string]time.Time{}
			}
			status.BackoffUntil[level] = until
		}
	}
	for subject := range h.insufficient {
		status.InsufficientSubjects = append(status.InsufficientSubjects, subject)
	}
	h.mu.Unlock()
	sort.Strings(status.InsufficientSubjects)
	return status
}

func (s *Server) handleHygiene(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.hygiene == nil {
		http.Error(w, "disk hygiene is not running", http.StatusServiceUnavailable)
		return
	}
	writeHygieneJSON(w, s.hygiene.Status(r.Context()))
}

func (s *Server) handleHygieneReclaim(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	if s.hygiene == nil {
		http.Error(w, "disk hygiene is not running", http.StatusServiceUnavailable)
		return
	}
	var request HygieneReclaimRequest
	if err := decodeJSONRequest(w, r, &request); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	run, err := s.hygiene.Reclaim(r.Context(), request.DryRun)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeHygieneJSON(w, run)
}

func writeHygieneJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}

// listExitedTakoContainers lists exited or dead containers labelled
// tako.project with their finish time, image and labels.
func listExitedTakoContainers(ctx context.Context) ([]hygieneContainer, error) {
	output, err := runDocker(ctx, "ps", "-aq", "--no-trunc", "--filter", "label=tako.project", "--filter", "status=exited", "--filter", "status=dead")
	if err != nil {
		return nil, fmt.Errorf("failed to list exited containers: %w", err)
	}
	ids := uniqueFields(output)
	if len(ids) == 0 {
		return nil, nil
	}
	args := append([]string{"inspect", "--format", "{{.Id}}\t{{.Name}}\t{{.State.Status}}\t{{.State.FinishedAt}}\t{{.Config.Image}}\t{{json .Config.Labels}}"}, ids...)
	inspectOutput, err := runDocker(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect exited containers: %w", err)
	}
	var containers []hygieneContainer
	for _, line := range strings.Split(inspectOutput, "\n") {
		fields := strings.SplitN(strings.TrimRight(line, "\r"), "\t", 6)
		if len(fields) != 6 || fields[0] == "" {
			continue
		}
		container := hygieneContainer{ID: fields[0], Name: strings.TrimPrefix(fields[1], "/"), Status: fields[2], Image: fields[4]}
		if finished, err := time.Parse(time.RFC3339Nano, fields[3]); err == nil && finished.Year() > 1 {
			container.FinishedAt = finished.UTC()
		}
		if err := json.Unmarshal([]byte(fields[5]), &container.Labels); err != nil {
			continue
		}
		containers = append(containers, container)
	}
	return containers, nil
}

// listUnrotatedContainers reports running containers whose json-file log has
// no max-size, largest log first, capped at 50.
func listUnrotatedContainers(ctx context.Context) ([]HygieneUnrotatedContainer, error) {
	output, err := runDocker(ctx, "ps", "-q", "--no-trunc")
	if err != nil {
		return nil, fmt.Errorf("failed to list running containers: %w", err)
	}
	ids := uniqueFields(output)
	if len(ids) == 0 {
		return nil, nil
	}
	args := append([]string{"inspect", "--format", "{{.Id}}\t{{.Name}}\t{{.HostConfig.LogConfig.Type}}\t{{json .HostConfig.LogConfig.Config}}\t{{.LogPath}}\t{{json .Config.Labels}}"}, ids...)
	inspectOutput, err := runDocker(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect running containers: %w", err)
	}
	return parseUnrotatedContainers(inspectOutput, func(path string) int64 {
		info, err := os.Stat(path)
		if err != nil {
			return 0
		}
		return info.Size()
	}), nil
}

func parseUnrotatedContainers(output string, logSize func(string) int64) []HygieneUnrotatedContainer {
	var unrotated []HygieneUnrotatedContainer
	for _, line := range strings.Split(output, "\n") {
		fields := strings.SplitN(strings.TrimRight(line, "\r"), "\t", 6)
		if len(fields) != 6 || fields[0] == "" || fields[2] != "json-file" {
			continue
		}
		var options map[string]string
		_ = json.Unmarshal([]byte(fields[3]), &options)
		if strings.TrimSpace(options["max-size"]) != "" {
			continue
		}
		var labels map[string]string
		_ = json.Unmarshal([]byte(fields[5]), &labels)
		entry := HygieneUnrotatedContainer{
			ID: fields[0], Name: strings.TrimPrefix(fields[1], "/"),
			Project: labels["tako.project"], Environment: labels["tako.environment"], Service: labels["tako.service"],
		}
		if path := strings.TrimSpace(fields[4]); strings.HasPrefix(path, "/") {
			entry.LogBytes = logSize(path)
		}
		unrotated = append(unrotated, entry)
	}
	sort.SliceStable(unrotated, func(i, j int) bool {
		if unrotated[i].LogBytes != unrotated[j].LogBytes {
			return unrotated[i].LogBytes > unrotated[j].LogBytes
		}
		return unrotated[i].Name < unrotated[j].Name
	})
	if len(unrotated) > hygieneUnrotatedLimit {
		unrotated = unrotated[:hygieneUnrotatedLimit]
	}
	return unrotated
}
