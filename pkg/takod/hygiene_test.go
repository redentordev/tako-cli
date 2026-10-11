package takod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
)

const gib = int64(1) << 30

// fakeDisk serves free and total bytes per path; free can change after
// each Docker operation to model reclaimed space.
type fakeDisk struct {
	mu    sync.Mutex
	free  map[string]int64
	total map[string]int64
}

func (d *fakeDisk) Free(path string) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	value, ok := d.free[path]
	if !ok {
		return 0, fmt.Errorf("unknown path %s", path)
	}
	return value, nil
}

func (d *fakeDisk) Total(path string) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.total[path], nil
}

func (d *fakeDisk) add(path string, bytes int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.free[path] += bytes
}

type hygieneCallLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *hygieneCallLog) add(call string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, call)
}

func (l *hygieneCallLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

// newTestHygieneManager returns a manager on dataDir with one 200 GiB
// filesystem, a 10 GiB floor and Docker operations that only log.
func newTestHygieneManager(t *testing.T, dataDir string, free int64) (*hygieneManager, *fakeDisk, *hygieneCallLog) {
	t.Helper()
	disk := &fakeDisk{free: map[string]int64{dataDir: free}, total: map[string]int64{dataDir: 200 * gib}}
	calls := &hygieneCallLog{}
	manager := newHygieneManager(dataDir, dataDir, hygieneSettings{MinimumFreeDiskBytes: 10 * gib, LegacyInterval: DefaultBuildCachePruneInterval}, nil)
	manager.diskFree = disk.Free
	manager.diskTotal = disk.Total
	manager.diskIdentity = func(path string) (string, error) { return "fs:" + path, nil }
	manager.fenceActive = func(string, time.Time) (bool, error) { return false, nil }
	manager.lockAutonomous = func() (func(), error) { return func() {}, nil }
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	manager.ops = hygieneOps{
		pruneDangling: func(context.Context) (int, error) { calls.add("dangling"); return 0, nil },
		pruneBuildCache: func(_ context.Context, keep string) error {
			calls.add("buildcache " + keep)
			return nil
		},
		imageGC: func(_ context.Context, _ string, project, environment string, keep int, repositories []string, options imageCleanupOptions) (CleanupHygiene, error) {
			calls.add(fmt.Sprintf("imagegc %s/%s keep=%d repos=%s grace=%t dry=%t", project, environment, keep, strings.Join(repositories, ","), !options.CreatedAfter.IsZero(), options.DryRun))
			return CleanupHygiene{DryRun: options.DryRun}, nil
		},
		listExitedContainers: func(context.Context) ([]hygieneContainer, error) { calls.add("list containers"); return nil, nil },
		removeContainer: func(_ context.Context, id string) error {
			calls.add("rm " + id)
			return nil
		},
		listUnrotated: func(context.Context) ([]HygieneUnrotatedContainer, error) { return nil, nil },
	}
	return manager, disk, calls
}

func writeHygieneScopeFixture(t *testing.T, dataDir, project, environment string, repositories ...string) {
	t.Helper()
	writeImageCleanupDesired(t, dataDir, project, environment, map[string]string{"web": project + "/web:current"})
	if err := recordHygieneScope(dataDir, project, environment, repositories, time.Now()); err != nil {
		t.Fatalf("record scope: %v", err)
	}
}

func TestPlanOldImageRemovalsGraceAndKeptReasons(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-72 * time.Hour)
	images := []dockerImageEntry{
		{ID: "a7", Repository: "shop/web", Tag: "t7", Created: old},
		{ID: "a6", Repository: "shop/web", Tag: "t6", Created: old},
		{ID: "a5", Repository: "shop/web", Tag: "t5", Created: old},
		{ID: "a4", Repository: "shop/web", Tag: "t4", Created: now.Add(-10 * time.Minute)},
		{ID: "a3", Repository: "shop/web", Tag: "t3", Created: old},
		{ID: "a2", Repository: "shop/web", Tag: "t2", Created: old},
		{ID: "a1", Repository: "shop/web", Tag: "t1", Created: old},
		{ID: "a0", Repository: "shop/web", Tag: "t0", Created: old, Size: 100},
	}
	owners := map[string]map[string]bool{}
	for _, image := range images {
		owners[image.ref()] = map[string]bool{"production": true}
	}
	owners["shop/web:t2"]["staging"] = true
	evidence := imageCleanupEvidence{
		owners:        owners,
		environments:  map[string]bool{"production": true, "staging": true},
		protectedRefs: map[string]bool{"shop/web:t1": true},
		inUseIDs:      []string{"a3"},
		inUseRefs:     map[string]bool{},
	}

	plan := planOldImageRemovalsDetailed(images, evidence, "production", 3, []string{"shop/web"}, now.Add(-hygieneImageGrace))
	if len(plan.Removals) != 1 || plan.Removals[0].ID != "a0" || plan.Removals[0].Size != 100 {
		t.Fatalf("removals = %+v, want only a0", plan.Removals)
	}
	want := map[string]string{
		"a7": ImageKeptRetention, "a6": ImageKeptRetention, "a5": ImageKeptRetention,
		"a4": ImageKeptRecent, "a3": ImageKeptInUse, "a2": ImageKeptOtherEnvironment, "a1": ImageKeptCurrent,
	}
	for id, reason := range want {
		if plan.Kept[id] != reason {
			t.Fatalf("kept[%s] = %q, want %q (all %v)", id, plan.Kept[id], reason, plan.Kept)
		}
	}
	counts := plan.keptCounts()
	if counts[ImageKeptRetention] != 3 || counts[ImageKeptRecent] != 1 {
		t.Fatalf("kept counts = %v", counts)
	}

	// The zero cut-off is exactly v0.14.2's selection: the recent image is
	// removable there.
	legacy := planOldImageRemovals(images, evidence, "production", 3, []string{"shop/web"})
	var ids []string
	for _, removal := range legacy {
		ids = append(ids, removal.ID)
	}
	if !slices.Equal(ids, []string{"a4", "a0"}) {
		t.Fatalf("zero cut-off removals = %v, want [a4 a0]", ids)
	}

	// Unknown creation time is kept by the grace.
	images[7].Created = time.Time{}
	plan = planOldImageRemovalsDetailed(images, evidence, "production", 3, []string{"shop/web"}, now.Add(-hygieneImageGrace))
	if len(plan.Removals) != 0 || plan.Kept["a0"] != ImageKeptRecent {
		t.Fatalf("unknown creation time must be kept: %+v %v", plan.Removals, plan.Kept)
	}
}

// sevenImageFixture writes seven production deployments of shop/web.
func sevenImageFixture(t *testing.T, dataDir string, recentTag string) {
	t.Helper()
	var list, inspect strings.Builder
	created := time.Now().UTC().Add(-72 * time.Hour)
	for i := 7; i >= 1; i-- {
		tag := fmt.Sprintf("t%d", i)
		id := fmt.Sprintf("img%daaaaaaaa", i)
		writeImageCleanupDeployment(t, dataDir, "shop", "production", "d"+tag, map[string]string{"web": "shop/web:" + tag})
		fmt.Fprintf(&list, "%s\tshop/web\t%s\n", id, tag)
		when := created
		if tag == recentTag {
			when = time.Now().UTC().Add(-10 * time.Minute)
		}
		fmt.Fprintf(&inspect, "sha256:%sfull\t%s\t%d\n", id, when.Format(time.RFC3339Nano), 1000*i)
	}
	writeImageCleanupDesired(t, dataDir, "shop", "production", map[string]string{"web": "shop/web:t7"})
	t.Setenv("TAKO_FAKE_IMAGE_LIST_OUTPUT", list.String())
	t.Setenv("TAKO_FAKE_IMAGE_INSPECT_OUTPUT", inspect.String())
}

func TestImageGCKeepsThreeAndRemovesFourOldest(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	dataDir := t.TempDir()
	sevenImageFixture(t, dataDir, "")

	report, err := cleanupOldProjectImagesWithOptions(context.Background(), dataDir, "shop", "production", 3, []string{"shop/web"}, imageCleanupOptions{CreatedAfter: time.Now().UTC().Add(-hygieneImageGrace)})
	if err != nil {
		t.Fatalf("image GC: %v", err)
	}
	got := rmiEntries(readCommandLog(t, logPath))
	want := []string{"docker rmi img4aaaaaaaa", "docker rmi img3aaaaaaaa", "docker rmi img2aaaaaaaa", "docker rmi img1aaaaaaaa"}
	if !slices.Equal(got, want) {
		t.Fatalf("rmi = %v, want %v", got, want)
	}
	if len(report.Removed) != 4 || report.Kept[ImageKeptRetention] != 3 || report.BytesFreed != 10000 {
		t.Fatalf("report = %+v", report)
	}
}

func TestImageGCKeepsRecentImageBeyondRetention(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	dataDir := t.TempDir()
	sevenImageFixture(t, dataDir, "t4")

	report, err := cleanupOldProjectImagesWithOptions(context.Background(), dataDir, "shop", "production", 3, []string{"shop/web"}, imageCleanupOptions{CreatedAfter: time.Now().UTC().Add(-hygieneImageGrace)})
	if err != nil {
		t.Fatalf("image GC: %v", err)
	}
	for _, entry := range rmiEntries(readCommandLog(t, logPath)) {
		if strings.Contains(entry, "img4") {
			t.Fatalf("recent image removed: %v", entry)
		}
	}
	if report.Kept[ImageKeptRecent] != 1 || len(report.Removed) != 3 {
		t.Fatalf("report = %+v", report)
	}
}

func TestImageGCDryRunRemovesNothing(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	dataDir := t.TempDir()
	sevenImageFixture(t, dataDir, "")

	response, err := CleanupProjectWithDataDir(context.Background(), dataDir, CleanupRequest{Project: "shop", Environment: "production", KeepImages: 3, ImageRepositories: []string{"shop/web"}, CleanOldImages: true, DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if got := rmiEntries(readCommandLog(t, logPath)); len(got) != 0 {
		t.Fatalf("dry run removed images: %v", got)
	}
	if response.Hygiene == nil || !response.Hygiene.DryRun || len(response.Hygiene.Removed) != 4 || response.Hygiene.Kept[ImageKeptRetention] != 3 {
		t.Fatalf("dry run report = %+v", response.Hygiene)
	}
	if _, err := os.Stat(hygieneScopePath(dataDir, "shop", "production")); !os.IsNotExist(err) {
		t.Fatalf("dry run must not record a scope: %v", err)
	}
	if err := validateCleanupRequest(CleanupRequest{Project: "shop", Environment: "production", DryRun: true, CleanBuildCache: true}); err == nil {
		t.Fatalf("dry run with build cache cleanup must be rejected")
	}
}

func TestImageGCUnreadableHistoryRemovesNothing(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	dataDir := t.TempDir()
	sevenImageFixture(t, dataDir, "")
	historyPath := filepath.Join(dataDir, "history", "shop", "production", "history.json")
	if err := os.WriteFile(historyPath, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := recordHygieneScope(dataDir, "shop", "production", []string{"shop/web"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	manager, _, _ := newTestHygieneManager(t, dataDir, 100*gib)
	manager.ops.imageGC = cleanupOldProjectImagesWithOptions
	manager.now = func() time.Time { return time.Now().UTC() }
	run := manager.maintenanceRun(context.Background())
	if got := rmiEntries(readCommandLog(t, logPath)); len(got) != 0 {
		t.Fatalf("unreadable history removed images: %v", got)
	}
	joined := strings.Join(run.Skipped, "\n")
	if !strings.Contains(joined, "shop/production: unreadable state") || !strings.Contains(joined, "history.json") {
		t.Fatalf("skipped = %v, want unreadable state with the path", run.Skipped)
	}

	// Inspect failure under the grace also removes nothing.
	if err := os.Remove(historyPath); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TAKO_FAKE_IMAGE_INSPECT_ERROR", "1")
	_, err := cleanupOldProjectImagesWithOptions(context.Background(), dataDir, "shop", "production", 3, []string{"shop/web"}, imageCleanupOptions{CreatedAfter: time.Now().Add(-hygieneImageGrace)})
	if !errors.Is(err, errImageCleanupStateUnreadable) {
		t.Fatalf("inspect failure error = %v", err)
	}
	if got := rmiEntries(readCommandLog(t, logPath)); len(got) != 0 {
		t.Fatalf("inspect failure removed images: %v", got)
	}
}

func TestAutonomousImageGCStagingLeavesProductionImages(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	dataDir := t.TempDir()
	writeSharedServerState(t, dataDir)
	t.Setenv("TAKO_FAKE_IMAGE_LIST_OUTPUT", sharedServerImageList)
	var inspect strings.Builder
	for _, id := range []string{"stg-5", "stg-4", "stg-3", "stg-2", "stg-1", "prd-3", "prd-2", "prd-1", "stgapi-3", "stgapi-2", "stgapi-1"} {
		fmt.Fprintf(&inspect, "sha256:%s\t%s\t10\n", id, time.Now().UTC().Add(-72*time.Hour).Format(time.RFC3339Nano))
	}
	t.Setenv("TAKO_FAKE_IMAGE_INSPECT_OUTPUT", inspect.String())
	if err := recordHygieneScope(dataDir, "demo", "staging", []string{"demo/web", "demo/api"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	manager, _, _ := newTestHygieneManager(t, dataDir, 100*gib)
	manager.ops.imageGC = cleanupOldProjectImagesWithOptions
	manager.now = func() time.Time { return time.Now().UTC() }

	run := manager.maintenanceRun(context.Background())
	got := rmiEntries(readCommandLog(t, logPath))
	if !slices.Equal(got, []string{"docker rmi stg-2", "docker rmi stg-1"}) {
		t.Fatalf("rmi = %v, want the two oldest staging web images", got)
	}
	for _, entry := range got {
		if strings.Contains(entry, "prd") {
			t.Fatalf("staging GC removed a production image: %v", entry)
		}
	}
	if run.Categories[HygieneCategoryImages].Removed != 2 || len(run.Images) != 2 {
		t.Fatalf("run = %+v", run)
	}
}

func TestImageGCNeverSelectsUnrecordedPulledRepositories(t *testing.T) {
	dataDir := t.TempDir()
	if err := recordHygieneScope(dataDir, "shop", "production", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(hygieneScopePath(dataDir, "shop", "production")); !os.IsNotExist(err) {
		t.Fatalf("empty repository list must record nothing: %v", err)
	}
	writeHygieneScopeFixture(t, dataDir, "shop", "production", "shop/web")
	manager, _, calls := newTestHygieneManager(t, dataDir, 100*gib)
	manager.maintenanceRun(context.Background())
	for _, call := range calls.list() {
		if strings.HasPrefix(call, "imagegc") && strings.Contains(call, "postgres") {
			t.Fatalf("pulled repository passed to the selector: %s", call)
		}
	}
	if !slices.Contains(calls.list(), "imagegc shop/production keep=3 repos=shop/web grace=true dry=false") {
		t.Fatalf("calls = %v", calls.list())
	}
}

func TestHygieneScopesRecordListAndRemove(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	dataDir := t.TempDir()
	t.Setenv("TAKO_FAKE_IMAGE_LIST_OUTPUT", "")
	if _, err := CleanupProjectWithDataDir(context.Background(), dataDir, CleanupRequest{Project: "shop", Environment: "production", KeepImages: 3, ImageRepositories: []string{"shop/web", "shop/api", "shop/web"}, CleanOldImages: true}); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	record, err := readHygieneScope(hygieneScopePath(dataDir, "shop", "production"))
	if err != nil || !slices.Equal(record.ImageRepositories, []string{"shop/api", "shop/web"}) {
		t.Fatalf("record = %+v, %v", record, err)
	}
	info, err := os.Stat(hygieneScopePath(dataDir, "shop", "production"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0600 && runtime.GOOS != "windows" {
		t.Fatalf("scope mode = %o", mode)
	}
	scopes, _, err := listHygieneScopes(dataDir)
	if err != nil || len(scopes) != 0 {
		t.Fatalf("scope without desired state must not be listed: %+v %v", scopes, err)
	}
	writeImageCleanupDesired(t, dataDir, "shop", "production", map[string]string{"web": "shop/web:a"})
	scopes, _, err = listHygieneScopes(dataDir)
	if err != nil || len(scopes) != 1 {
		t.Fatalf("scopes = %+v %v", scopes, err)
	}
	if err := removeHygieneScope(dataDir, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	if scopes, _, _ := listHygieneScopes(dataDir); len(scopes) != 0 {
		t.Fatalf("scope not removed: %+v", scopes)
	}
}

func TestHygieneJournalRingAndCorruptRecovery(t *testing.T) {
	dataDir := t.TempDir()
	journal := openHygieneJournal(dataDir)
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 60; i++ {
		journal.Append(HygieneRun{ID: fmt.Sprintf("run-%d", i), Trigger: HygieneTriggerSchedule, StartedAt: start.Add(time.Duration(i) * time.Minute)})
	}
	reopened := openHygieneJournal(dataDir)
	runs := reopened.Recent(0)
	if len(runs) != hygieneJournalLimit || runs[0].ID != "run-59" || runs[len(runs)-1].ID != "run-10" {
		t.Fatalf("ring = %d runs, newest %s, oldest %s", len(runs), runs[0].ID, runs[len(runs)-1].ID)
	}
	info, err := os.Stat(hygieneJournalPath(dataDir))
	if err != nil || (info.Mode().Perm() != 0600 && runtime.GOOS != "windows") {
		t.Fatalf("journal mode: %v %v", info, err)
	}
	if err := os.WriteFile(hygieneJournalPath(dataDir), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	corrupt := openHygieneJournal(dataDir)
	if len(corrupt.Recent(0)) != 0 || corrupt.LastError() == nil {
		t.Fatalf("corrupt journal must start empty with an error")
	}
	if _, err := os.Stat(hygieneJournalPath(dataDir) + ".corrupt"); err != nil {
		t.Fatalf("corrupt copy missing: %v", err)
	}
	corrupt.Append(HygieneRun{ID: "fresh", StartedAt: start})
	if runs := openHygieneJournal(dataDir).Recent(0); len(runs) != 1 || runs[0].ID != "fresh" {
		t.Fatalf("new journal = %+v", runs)
	}
}

func TestHygieneJournalWriteFailureNeverAbortsRun(t *testing.T) {
	dataDir := t.TempDir()
	manager, _, _ := newTestHygieneManager(t, dataDir, 14*gib)
	// A file where the hygiene directory should be makes every write fail.
	if err := os.WriteFile(filepath.Join(dataDir, hygieneDirName), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	manager.journal = openHygieneJournal(dataDir)
	run, err := manager.Reclaim(context.Background(), false)
	if err != nil || run.Trigger != HygieneTriggerRequest {
		t.Fatalf("reclaim = %+v, %v", run, err)
	}
	if manager.journal.LastError() == nil || len(manager.journal.Recent(0)) != 1 {
		t.Fatalf("run must stay in memory with the write error")
	}
}

func TestHygieneThresholds(t *testing.T) {
	cases := []struct {
		name               string
		minimum, total     int64
		free               int64
		wantSoft, wantHard int64
		wantLevel          string
	}{
		{"soft pressure on 200 GiB", 10 * gib, 200 * gib, 17 * gib, 20 * gib, 15 * gib, HygieneLevelSoft},
		{"hard pressure", 10 * gib, 200 * gib, 12 * gib, 20 * gib, 15 * gib, HygieneLevelHard},
		{"2 TiB disk with 60 GiB free", 10 * gib, 2048 * gib, 60 * gib, 50 * gib, 15 * gib, ""},
		{"small disk uses the floor", 10 * gib, 50 * gib, 16 * gib, 15 * gib, 15 * gib, ""},
		{"no floor", 0, 100 * gib, 5 * gib, 10 * gib, 0, HygieneLevelSoft},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			soft, hard := hygieneThresholds(tc.minimum, tc.total)
			if soft != tc.wantSoft || hard != tc.wantHard {
				t.Fatalf("thresholds = %d/%d, want %d/%d", soft, hard, tc.wantSoft, tc.wantHard)
			}
			if level := hygieneLevel(tc.free, soft, hard); level != tc.wantLevel {
				t.Fatalf("level = %q, want %q", level, tc.wantLevel)
			}
		})
	}
}

func TestPressureCheckLargeDiskMostlyVolumesDoesNotRun(t *testing.T) {
	dataDir := t.TempDir()
	manager, disk, calls := newTestHygieneManager(t, dataDir, 60*gib)
	disk.total[dataDir] = 2048 * gib
	if _, ran := manager.PressureCheck(context.Background()); ran || len(calls.list()) != 0 {
		t.Fatalf("no reclaim expected, calls = %v", calls.list())
	}
}

func TestReclaimStopsEarlyAboveSoftThreshold(t *testing.T) {
	dataDir := t.TempDir()
	manager, disk, calls := newTestHygieneManager(t, dataDir, 12*gib)
	manager.ops.pruneDangling = func(context.Context) (int, error) {
		calls.add("dangling")
		disk.add(dataDir, 4*gib)
		return 3, nil
	}
	manager.ops.pruneBuildCache = func(_ context.Context, keep string) error {
		calls.add("buildcache " + keep)
		disk.add(dataDir, 6*gib)
		return nil
	}
	run, err := manager.Reclaim(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if got := calls.list(); !slices.Equal(got, []string{"dangling", "buildcache 20GB"}) {
		t.Fatalf("calls = %v, want only dangling and build cache", got)
	}
	if !run.StoppedEarly || run.Insufficient || run.BytesFreed != 10*gib {
		t.Fatalf("run = %+v", run)
	}
	if run.Categories[HygieneCategoryDanglingImages].Removed != 3 || run.Categories[HygieneCategoryBuildCache].BytesFreed != 6*gib {
		t.Fatalf("categories = %+v", run.Categories)
	}
}

func TestHardReclaimRunsStepsInOrder(t *testing.T) {
	dataDir := t.TempDir()
	writeHygieneScopeFixture(t, dataDir, "shop", "production", "shop/web")
	manager, _, calls := newTestHygieneManager(t, dataDir, 12*gib)
	run, err := manager.Reclaim(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"dangling",
		"buildcache 20GB",
		"imagegc shop/production keep=3 repos=shop/web grace=true dry=false",
		"buildcache 2GB",
		"list containers",
	}
	if got := calls.list(); !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if !run.Insufficient || run.Level != HygieneLevelHard {
		t.Fatalf("run = %+v", run)
	}
	soft, _ := manager.Reclaim(context.Background(), true)
	if !soft.DryRun || len(soft.Skipped) == 0 {
		t.Fatalf("dry run = %+v", soft)
	}
}

func TestMaintenanceTickOrderAndFenceSkip(t *testing.T) {
	dataDir := t.TempDir()
	writeHygieneScopeFixture(t, dataDir, "shop", "production", "shop/web")
	manager, _, calls := newTestHygieneManager(t, dataDir, 100*gib)
	run, ran := manager.runAutonomous(context.Background(), HygieneTriggerSchedule, manager.maintenanceRun)
	if !ran || run.Trigger != HygieneTriggerSchedule {
		t.Fatalf("run = %+v ran=%t", run, ran)
	}
	if got := calls.list(); !slices.Equal(got, []string{"imagegc shop/production keep=3 repos=shop/web grace=true dry=false", "buildcache 20GB"}) {
		t.Fatalf("maintenance order = %v", got)
	}
	manager.lockAutonomous = func() (func(), error) { return nil, errHygieneFenceActive }
	skipped, ran := manager.runAutonomous(context.Background(), HygieneTriggerSchedule, manager.maintenanceRun)
	if ran || !slices.Equal(skipped.Skipped, []string{"worker fence active"}) {
		t.Fatalf("fence skip = %+v ran=%t", skipped, ran)
	}
	if len(calls.list()) != 2 {
		t.Fatalf("fenced run issued docker calls: %v", calls.list())
	}
	if runs := manager.journal.Recent(0); len(runs) != 2 || runs[0].Skipped[0] != "worker fence active" {
		t.Fatalf("journal = %+v", runs)
	}
}

func TestHygieneIntervalLegacyMapping(t *testing.T) {
	twelve := 12 * time.Hour
	zero := time.Duration(0)
	if got := resolveHygieneMaintenanceInterval(nil, twelve); got != twelve {
		t.Fatalf("legacy flag = %v", got)
	}
	if got := resolveHygieneMaintenanceInterval(&zero, twelve); got != 0 {
		t.Fatalf("explicit 0 = %v", got)
	}
	six := 6 * time.Hour
	if got := resolveHygieneMaintenanceInterval(&six, twelve); got != six {
		t.Fatalf("explicit interval = %v", got)
	}
	if got := resolveHygieneMaintenanceInterval(nil, 0); got != 0 {
		t.Fatalf("legacy 0 = %v", got)
	}
}

func TestHygieneScopeSkipsLeaseAndRetention(t *testing.T) {
	dataDir := t.TempDir()
	writeHygieneScopeFixture(t, dataDir, "shop", "production", "shop/web")
	writeHygieneScopeFixture(t, dataDir, "shop", "staging", "shop/web")
	writeHygieneScopeFixture(t, dataDir, "blog", "production", "blog/web")
	manager, _, calls := newTestHygieneManager(t, dataDir, 100*gib)
	lease, _ := leasePath(dataDir, "shop", "production")
	writeImageCleanupJSON(t, lease, LeaseInfo{ID: "deploy-1", ExpiresAt: manager.now().Add(time.Hour)})
	writeImageCleanupJSON(t, retainedStatePath(dataDir, "shop", "staging", "web"), map[string]any{"service": "web"})

	run := manager.maintenanceRun(context.Background())
	var gc []string
	for _, call := range calls.list() {
		if strings.HasPrefix(call, "imagegc") {
			gc = append(gc, call)
		}
	}
	if !slices.Equal(gc, []string{"imagegc blog/production keep=3 repos=blog/web grace=true dry=false"}) {
		t.Fatalf("image GC calls = %v", gc)
	}
	joined := strings.Join(run.Skipped, "\n")
	if !strings.Contains(joined, "shop/production: lease held") || !strings.Contains(joined, "shop/staging: restore-on-failure retention") {
		t.Fatalf("skipped = %v", run.Skipped)
	}

	// An expired lease no longer blocks.
	writeImageCleanupJSON(t, lease, LeaseInfo{ID: "deploy-1", ExpiresAt: manager.now().Add(-time.Minute)})
	state := manager.newRun(HygieneTriggerSchedule, "", false)
	if reason := manager.scopeBlocked(state, "shop", "production"); reason != "" {
		t.Fatalf("expired lease blocked: %q", reason)
	}
}

func TestPressureBackoffAndDenials(t *testing.T) {
	dataDir := t.TempDir()
	manager, disk, calls := newTestHygieneManager(t, dataDir, 18*gib)
	run, ran := manager.PressureCheck(context.Background())
	if !ran || run.Trigger != HygieneTriggerPressureSoft || !run.Insufficient {
		t.Fatalf("first proactive run = %+v ran=%t", run, ran)
	}
	before := len(calls.list())
	if _, ran := manager.PressureCheck(context.Background()); ran || len(calls.list()) != before {
		t.Fatalf("proactive soft run must back off for 6h")
	}
	later := manager.now().Add(hygieneProactiveBackoff + time.Minute)
	manager.now = func() time.Time { return later }
	if _, ran := manager.PressureCheck(context.Background()); !ran {
		t.Fatalf("backoff must expire after 6h")
	}
	// A scheduler denial runs a hard reclaim; when it is insufficient,
	// further denials back off (a per-minute cron job must not cause a hard
	// reclaim and a journal entry every minute). Requests never back off.
	disk.free[dataDir] = 12 * gib
	denied, ran := manager.SchedulerDeniedReclaim(context.Background())
	if !ran || denied.Level != HygieneLevelHard || !denied.Insufficient {
		t.Fatalf("denial reclaim = %+v ran=%t", denied, ran)
	}
	journaled := len(manager.journal.Recent(1000))
	before = len(calls.list())
	for minute := 1; minute <= 30; minute++ {
		at := later.Add(time.Duration(minute) * time.Minute)
		manager.now = func() time.Time { return at }
		if _, ran := manager.SchedulerDeniedReclaim(context.Background()); ran {
			t.Fatalf("denial reclaim at +%dm must back off after an insufficient run", minute)
		}
	}
	if len(calls.list()) != before || len(manager.journal.Recent(1000)) != journaled {
		t.Fatalf("suppressed denials must not reclaim or journal")
	}
	if _, err := manager.Reclaim(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if len(manager.journal.Recent(1000)) != journaled+1 {
		t.Fatalf("explicit reclaim must run while denials back off")
	}
}

func TestSchedulerDeniedReclaimMinimumInterval(t *testing.T) {
	manager, _, _ := newTestHygieneManager(t, t.TempDir(), 100*gib)
	start := manager.now()
	if _, ran := manager.SchedulerDeniedReclaim(context.Background()); !ran {
		t.Fatalf("first denial reclaim must run")
	}
	soon := start.Add(time.Minute)
	manager.now = func() time.Time { return soon }
	if _, ran := manager.SchedulerDeniedReclaim(context.Background()); ran {
		t.Fatalf("denial reclaim within the minimum interval must be suppressed")
	}
	later := start.Add(hygieneSchedulerReclaimInterval + time.Second)
	manager.now = func() time.Time { return later }
	if _, ran := manager.SchedulerDeniedReclaim(context.Background()); !ran {
		t.Fatalf("denial reclaim after the minimum interval must run")
	}
}

func TestSchedulerDenialsCoalesceIntoOneSignal(t *testing.T) {
	manager, _, _ := newTestHygieneManager(t, t.TempDir(), 100*gib)
	admit := manager.admitWithReclaim(func(...string) error {
		return errors.New("operation denied on /var/lib/tako: 1 free bytes is below 2-byte platform minimum")
	})
	for i := 0; i < 5; i++ {
		if err := admit(); err == nil || !strings.HasPrefix(err.Error(), "operation denied") {
			t.Fatalf("denial must be returned unchanged: %v", err)
		}
	}
	if len(manager.signal) != 1 {
		t.Fatalf("pending signals = %d, want 1", len(manager.signal))
	}
	other := manager.admitWithReclaim(func(...string) error { return errors.New("failed to verify free disk on /x") })
	<-manager.signal
	_ = other()
	if len(manager.signal) != 0 {
		t.Fatalf("probe failures must not signal a reclaim")
	}
}

func TestReclaimExitedContainerSelection(t *testing.T) {
	dataDir := t.TempDir()
	writeImageCleanupDesired(t, dataDir, "shop", "production", map[string]string{"web": "shop/web:current"})
	writeImageCleanupDesired(t, dataDir, "shop", "staging", map[string]string{"web": "shop/web:current"})
	writeImageCleanupDesired(t, dataDir, "blog", "production", map[string]string{"web": "blog/web:current"})
	writeImageCleanupJSON(t, retainedStatePath(dataDir, "shop", "staging", "web"), map[string]any{"service": "web"})
	writeImageCleanupJSON(t, filepath.Join(dataDir, "heal", "blog", "production", "services", "web.json"), map[string]any{
		"image": "blog/web:old", "containers": []map[string]string{{"name": "blog_production_web_1", "id": "c-converged"}},
	})
	manager, _, calls := newTestHygieneManager(t, dataDir, 12*gib)
	now := manager.now()
	labels := func(project, environment, role string) map[string]string {
		values := map[string]string{"tako.project": project, "tako.environment": environment, "tako.service": "web"}
		if role != "" {
			values["tako.role"] = role
		}
		return values
	}
	containers := []hygieneContainer{
		{ID: "c-current", Name: "current", Status: "exited", FinishedAt: now.Add(-3 * time.Hour), Image: "shop/web:current", Labels: labels("shop", "production", "")},
		{ID: "c-old", Name: "old-replica", Status: "exited", FinishedAt: now.Add(-2 * time.Hour), Image: "shop/web:previous", Labels: labels("shop", "production", "")},
		{ID: "c-young", Name: "young", Status: "exited", FinishedAt: now.Add(-30 * time.Minute), Image: "shop/web:previous", Labels: labels("shop", "production", "")},
		{ID: "c-retained", Name: "retained", Status: "exited", FinishedAt: now.Add(-5 * time.Hour), Image: "shop/web:previous", Labels: labels("shop", "staging", "")},
		{ID: "c-converged", Name: "blog_production_web_1", Status: "exited", FinishedAt: now.Add(-5 * time.Hour), Image: "blog/web:old", Labels: labels("blog", "production", "")},
		{ID: "c-job-recent", Name: "job-recent", Status: "exited", FinishedAt: now.Add(-2 * time.Hour), Image: "shop/web:current", Labels: labels("shop", "production", "job")},
		{ID: "c-job-old", Name: "job-old", Status: "dead", FinishedAt: now.Add(-25 * time.Hour), Image: "shop/web:current", Labels: labels("shop", "production", "job")},
		{ID: "c-running", Name: "running", Status: "running", FinishedAt: now.Add(-25 * time.Hour), Image: "shop/web:previous", Labels: labels("shop", "production", "")},
		{ID: "c-foreign", Name: "foreign", Status: "exited", FinishedAt: now.Add(-25 * time.Hour), Image: "nginx:1", Labels: map[string]string{"com.example": "x"}},
		{ID: "c-nodesired", Name: "nodesired", Status: "exited", FinishedAt: now.Add(-25 * time.Hour), Image: "x/web:1", Labels: labels("other", "production", "")},
	}
	manager.ops.listExitedContainers = func(context.Context) ([]hygieneContainer, error) { return containers, nil }
	run, err := manager.Reclaim(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	var removed []string
	for _, call := range calls.list() {
		if strings.HasPrefix(call, "rm ") {
			removed = append(removed, strings.TrimPrefix(call, "rm "))
		}
	}
	if !slices.Equal(removed, []string{"c-old", "c-job-old"}) {
		t.Fatalf("removed = %v, want [c-old c-job-old]", removed)
	}
	if !slices.Equal(run.Containers, []string{"old-replica", "job-old"}) {
		t.Fatalf("run containers = %v", run.Containers)
	}
}

func TestReclaimIssuesNoVolumeBackupOrLogFileOperations(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	dataDir := t.TempDir()
	sevenImageFixture(t, dataDir, "")
	if err := recordHygieneScope(dataDir, "shop", "production", []string{"shop/web"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(dataDir, "backups")
	if err := os.MkdirAll(backupDir, 0700); err != nil {
		t.Fatal(err)
	}
	backupFile := filepath.Join(backupDir, "shop-db.tar.gz")
	if err := os.WriteFile(backupFile, []byte("archive"), 0600); err != nil {
		t.Fatal(err)
	}
	manager, _, _ := newTestHygieneManager(t, dataDir, 1*gib)
	manager.ops = defaultHygieneOps()
	manager.now = func() time.Time { return time.Now().UTC() }
	run, err := manager.Reclaim(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range readCommandLog(t, logPath) {
		lower := strings.ToLower(entry)
		for _, forbidden := range []string{" volume", "rm -f", "rm -v", "rmi -f", "--volumes", "system prune", "truncate", "backup", "--force"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("reclaim issued forbidden command %q", entry)
			}
		}
	}
	if _, err := os.Stat(backupFile); err != nil {
		t.Fatalf("backup archive touched: %v", err)
	}
	if run.Categories[HygieneCategoryImages].Removed != 4 {
		t.Fatalf("image GC under reclaim = %+v", run.Categories)
	}
}

func TestHygieneReclaimIsSingleFlight(t *testing.T) {
	dataDir := t.TempDir()
	manager, _, _ := newTestHygieneManager(t, dataDir, 12*gib)
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	var mu sync.Mutex
	prunes := 0
	manager.ops.pruneDangling = func(context.Context) (int, error) {
		mu.Lock()
		prunes++
		mu.Unlock()
		started <- struct{}{}
		<-release
		return 0, nil
	}
	results := make(chan HygieneRun, 2)
	go func() {
		run, _ := manager.Reclaim(context.Background(), false)
		results <- run
	}()
	<-started
	go func() {
		run, _ := manager.Reclaim(context.Background(), false)
		results <- run
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		manager.mu.Lock()
		call := manager.inflight[false]
		manager.mu.Unlock()
		if call != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	first, second := <-results, <-results
	if prunes != 1 || first.ID == "" || first.ID != second.ID {
		t.Fatalf("prunes=%d first=%q second=%q", prunes, first.ID, second.ID)
	}
}

func TestHygieneEndpointsAndCapability(t *testing.T) {
	dataDir := t.TempDir()
	server := NewServer(filepath.Join(dataDir, "takod.sock"), dataDir, "test")
	if !slices.Contains(server.Status().Capabilities, CapabilityNodeDiskHygieneV1) {
		t.Fatalf("capabilities = %v", server.Status().Capabilities)
	}
	manager, _, _ := newTestHygieneManager(t, dataDir, 100*gib)
	server.hygiene = manager
	writeHygieneScopeFixture(t, dataDir, "shop", "production", "shop/web")

	recorder := httptest.NewRecorder()
	server.handleHygieneReclaim(recorder, httptest.NewRequest(http.MethodPost, "/v1/hygiene/reclaim", strings.NewReader(`{"dryRun":true}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("reclaim status = %d %s", recorder.Code, recorder.Body.String())
	}
	var run HygieneRun
	if err := json.Unmarshal(recorder.Body.Bytes(), &run); err != nil || !run.DryRun || run.Trigger != HygieneTriggerRequest {
		t.Fatalf("reclaim body = %s (%v)", recorder.Body.String(), err)
	}

	recorder = httptest.NewRecorder()
	server.handleHygiene(recorder, httptest.NewRequest(http.MethodGet, "/v1/hygiene", nil))
	var status HygieneStatus
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatalf("status body: %v", err)
	}
	if status.Policy.ImageRetention != 3 || len(status.Scopes) != 1 || len(status.Runs) != 1 || len(status.Filesystems) != 1 {
		t.Fatalf("status = %+v", status)
	}
	recorder = httptest.NewRecorder()
	server.handleHygiene(recorder, httptest.NewRequest(http.MethodPost, "/v1/hygiene", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/hygiene = %d", recorder.Code)
	}
	found := 0
	for _, route := range server.registeredRoutes() {
		if route.path == "/v1/hygiene" || route.path == "/v1/hygiene/reclaim" {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("hygiene routes registered = %d", found)
	}
}

func TestParseUnrotatedContainers(t *testing.T) {
	output := strings.Join([]string{
		"id1\t/shop_production_web_1\tjson-file\t{}\t/var/lib/docker/containers/id1/id1-json.log\t{\"tako.project\":\"shop\",\"tako.environment\":\"production\",\"tako.service\":\"web\"}",
		"id2\t/rotated\tjson-file\t{\"max-file\":\"5\",\"max-size\":\"20m\"}\t/var/lib/docker/containers/id2/id2-json.log\t{}",
		"id3\t/journald\tjournald\t{}\t\t{}",
		"id4\t/small\tjson-file\t{\"max-file\":\"3\"}\t/var/lib/docker/containers/id4/id4-json.log\tnull",
	}, "\n")
	sizes := map[string]int64{"/var/lib/docker/containers/id1/id1-json.log": 3 * gib, "/var/lib/docker/containers/id4/id4-json.log": 10}
	got := parseUnrotatedContainers(output, func(path string) int64 { return sizes[path] })
	if len(got) != 2 || got[0].Name != "shop_production_web_1" || got[0].LogBytes != 3*gib || got[0].Service != "web" || got[1].Name != "small" {
		t.Fatalf("unrotated = %+v", got)
	}
}

func TestContainerLoggingPolicyCases(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name     string
		daemon   *string
		driver   string
		off      bool
		active   bool
		contains string
	}{
		{name: "stock daemon", driver: "json-file", active: true, contains: "active"},
		{name: "empty daemon.json", daemon: hygieneStringPtr(`{}`), driver: "json-file", active: true},
		{name: "log-opts max-file only", daemon: hygieneStringPtr(`{"log-opts":{"max-file":"3"}}`), driver: "json-file", contains: "log-opts"},
		{name: "log-driver set", daemon: hygieneStringPtr(`{"log-driver":"json-file"}`), driver: "json-file", contains: "log-driver"},
		{name: "journald default", driver: "journald", contains: "journald"},
		{name: "invalid JSON", daemon: hygieneStringPtr(`{nope`), driver: "json-file", contains: "not valid JSON"},
		{name: "disabled", driver: "json-file", off: true, contains: "disabled"},
	}
	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := newContainerLoggingPolicy(tc.off, "", 0)
			policy.daemonJSONPath = filepath.Join(dir, fmt.Sprintf("daemon-%d.json", index))
			if tc.daemon != nil {
				if err := os.WriteFile(policy.daemonJSONPath, []byte(*tc.daemon), 0600); err != nil {
					t.Fatal(err)
				}
			}
			policy.loggingDriver = func(context.Context) (string, error) { return tc.driver, nil }
			status := policy.Refresh(context.Background())
			published := policy.Current()
			if status.Active != tc.active || (tc.contains != "" && !strings.Contains(status.Reason, tc.contains)) {
				t.Fatalf("status = %+v", status)
			}
			if tc.active && (published == nil || published.Driver != "json-file" || published.MaxSize != "20m" || published.MaxFiles != 5) {
				t.Fatalf("published = %+v", published)
			}
			if !tc.active && published != nil {
				t.Fatalf("skipped policy published %+v", published)
			}
		})
	}
	if err := ValidateDefaultContainerLogging("on", "20m", 5); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]any{{"maybe", "20m", 5}, {"on", "20x", 5}, {"on", "20m", 0}, {"on", "20g", 5}} {
		if err := ValidateDefaultContainerLogging(bad[0].(string), bad[1].(string), bad[2].(int)); err == nil {
			t.Fatalf("expected %v to be rejected", bad)
		}
	}
}

func hygieneStringPtr(value string) *string { return &value }

func TestDefaultContainerLoggingAppliesOnlyToRunArgs(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	req := ReconcileServiceRequest{
		Project: "shop", Environment: "production", Service: "web", Image: "shop/web:1",
		Labels: map[string]string{"tako.configHash": hash},
	}
	without := strings.Join(buildServiceContainerArgs(req, ContainerSpec{Name: "tako_shop_production_web_1"}), " ")
	req.DefaultLogging = &config.ServiceLoggingConfig{Driver: "json-file", MaxSize: "20m", MaxFiles: 5}
	encoded, err := json.Marshal(req)
	if err != nil || strings.Contains(string(encoded), "20m") {
		t.Fatalf("the node default must never be serialized: %s %v", encoded, err)
	}
	with := strings.Join(buildServiceContainerArgs(req, ContainerSpec{Name: "tako_shop_production_web_1"}), " ")
	if !strings.Contains(with, "--log-driver json-file --log-opt max-size=20m --log-opt max-file=5") {
		t.Fatalf("default log options missing: %s", with)
	}
	if strings.Contains(without, "--log-") {
		t.Fatalf("no default expected: %s", without)
	}
	if req.Labels["tako.configHash"] != hash || req.Logging != nil || !strings.Contains(with, "tako.configHash="+hash) {
		t.Fatalf("config hash or request changed")
	}
	req.Logging = &config.ServiceLoggingConfig{Driver: "local", MaxSize: "50m", MaxFiles: 3}
	explicit := strings.Join(buildServiceContainerArgs(req, ContainerSpec{Name: "tako_shop_production_web_1"}), " ")
	if !strings.Contains(explicit, "--log-driver local --log-opt max-size=50m --log-opt max-file=3") || strings.Contains(explicit, "max-size=20m") {
		t.Fatalf("explicit logging must win: %s", explicit)
	}
}

func TestHygieneAlerts(t *testing.T) {
	dataDir := t.TempDir()
	h := newSourceTestHarness(t, dataDir)
	if err := h.manager.RegisterSource(hygieneAlertSourceSpec()); err != nil {
		t.Fatal(err)
	}
	if err := h.manager.ApplyConfig(testMonitorRequest("shop", "production")); err != nil {
		t.Fatal(err)
	}
	manager, disk, _ := newTestHygieneManager(t, dataDir, 12*gib)
	manager.alerts = h.manager
	manager.now = func() time.Time { return h.now }

	if _, err := manager.Reclaim(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	sent := h.dispatch()
	if len(sent) != 1 || sent[0].Payload["event"] != hygieneEventInsufficient || sent[0].Payload["severity"] != "critical" {
		t.Fatalf("insufficient opening = %+v", sent)
	}
	for _, delivery := range sent {
		if delivery.Payload["event"] == "high_disk" {
			t.Fatalf("hygiene raised high_disk")
		}
	}
	h.advance(time.Minute)
	if _, err := manager.Reclaim(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if again := h.dispatch(); len(again) != 0 {
		t.Fatalf("second insufficient reclaim delivered again: %+v", again)
	}
	// Between hard and soft: still open; above soft: recovered.
	disk.add(dataDir, 5*gib)
	h.advance(time.Minute)
	manager.PressureCheck(context.Background())
	if sent := h.dispatch(); len(sent) != 0 {
		t.Fatalf("recovered below soft: %+v", sent)
	}
	disk.add(dataDir, 10*gib)
	h.advance(time.Minute)
	manager.PressureCheck(context.Background())
	sent = h.dispatch()
	if len(sent) != 1 || sent[0].Payload["event"] != hygieneEventResourceNormal {
		t.Fatalf("recovery = %+v", sent)
	}

	// A successful reclaim delivers nothing.
	h.advance(time.Minute)
	if _, err := manager.Reclaim(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if sent := h.dispatch(); len(sent) != 0 {
		t.Fatalf("successful reclaim delivered %+v", sent)
	}

	// Unreadable state opens image_gc_blocked; a readable selection recovers it.
	writeHygieneScopeFixture(t, dataDir, "shop", "production", "shop/web")
	manager.ops.imageGC = func(context.Context, string, string, string, int, []string, imageCleanupOptions) (CleanupHygiene, error) {
		return CleanupHygiene{}, fmt.Errorf("%w: history.json", errImageCleanupStateUnreadable)
	}
	h.advance(time.Minute)
	manager.maintenanceRun(context.Background())
	sent = h.dispatch()
	if len(sent) != 1 || sent[0].Payload["event"] != hygieneEventImageGCBlocked {
		t.Fatalf("image GC blocked = %+v", sent)
	}
	manager.ops.imageGC = func(context.Context, string, string, string, int, []string, imageCleanupOptions) (CleanupHygiene, error) {
		return CleanupHygiene{}, nil
	}
	h.advance(time.Minute)
	manager.maintenanceRun(context.Background())
	sent = h.dispatch()
	if len(sent) != 1 || sent[0].Payload["event"] != hygieneEventImageGCResumed {
		t.Fatalf("image GC resumed = %+v", sent)
	}
}

func TestCleanupJournalRecordsScopedRuns(t *testing.T) {
	dataDir := t.TempDir()
	manager, _, _ := newTestHygieneManager(t, dataDir, 100*gib)
	manager.journalCleanup(CleanupRequest{Project: "shop", Environment: "production", Trigger: HygieneTriggerDeploy}, &CleanupResponse{
		Hygiene: &CleanupHygiene{Removed: []HygieneImage{{ID: "a", Refs: []string{"shop/web:1"}, SizeBytes: 10}}, BytesFreed: 10, Kept: map[string]int{ImageKeptRetention: 3}},
	})
	runs := manager.journal.Recent(0)
	if len(runs) != 1 || runs[0].Trigger != HygieneTriggerDeploy || runs[0].BytesFreed != 10 || len(runs[0].Images) != 1 {
		t.Fatalf("journal = %+v", runs)
	}
	if err := validateCleanupRequest(CleanupRequest{Project: "shop", Trigger: "nightly"}); err == nil {
		t.Fatalf("unknown trigger must be rejected")
	}
}
