package cmd

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	localstate "github.com/redentordev/tako-cli/pkg/state"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodstate"
)

func TestSyncRemoteDeploymentsToLocalKeepsNewestAsCurrent(t *testing.T) {
	tempDir := t.TempDir()
	localMgr, err := localstate.NewManager(tempDir, "demo", "production")
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}

	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	oldDeployment := remoteDeployment("old", base, "demo:v1")
	newDeployment := remoteDeployment("new", base.Add(time.Hour), "demo:v2")

	synced, err := syncRemoteDeploymentsToLocal(localMgr, []*remotestate.DeploymentState{
		newDeployment,
		oldDeployment,
	}, "production")
	if err != nil {
		t.Fatalf("syncRemoteDeploymentsToLocal returned error: %v", err)
	}
	if synced != 2 {
		t.Fatalf("synced = %d, want 2", synced)
	}

	current, err := localMgr.GetCurrentDeployment()
	if err != nil {
		t.Fatalf("GetCurrentDeployment returned error: %v", err)
	}
	if current == nil {
		t.Fatal("current deployment is nil")
	}
	if current.DeploymentID != "new" {
		t.Fatalf("current deployment = %q, want newest deployment", current.DeploymentID)
	}
	if got := current.Services["web"].Image; got != "demo:v2" {
		t.Fatalf("current web image = %q, want demo:v2", got)
	}
}

func TestSyncBestDeploymentHistoryToLocalUsesFreshestHistory(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "demo"},
	}
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	older := remoteHistory(base, remoteDeployment("old", base, "demo:v1"))
	newer := remoteHistory(base.Add(time.Hour), remoteDeployment("new", base.Add(time.Hour), "demo:v2"))

	source, synced, ok, err := syncBestDeploymentHistoryToLocal(cfg, "production", []stateHistoryCandidate{
		{source: "node-a", history: older},
		{source: "node-b", history: newer},
	})
	if err != nil {
		t.Fatalf("syncBestDeploymentHistoryToLocal returned error: %v", err)
	}
	if !ok {
		t.Fatal("syncBestDeploymentHistoryToLocal returned no candidate")
	}
	if source != "node-b" {
		t.Fatalf("source = %q, want node-b", source)
	}
	if synced != 1 {
		t.Fatalf("synced = %d, want 1", synced)
	}

	localMgr, err := localstate.NewManager(".", "demo", "production")
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}
	current, err := localMgr.GetCurrentDeployment()
	if err != nil {
		t.Fatalf("GetCurrentDeployment returned error: %v", err)
	}
	if current == nil || current.DeploymentID != "new" {
		t.Fatalf("current deployment = %#v, want new", current)
	}
}

func TestLocalDeploymentStateExistsIgnoresSecretsOnlyTakoDirectory(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	if err := os.MkdirAll(filepath.Join(".tako"), 0755); err != nil {
		t.Fatalf("failed to create .tako: %v", err)
	}
	if err := os.WriteFile(filepath.Join(".tako", "secrets.production"), []byte("TOKEN=secret\n"), 0600); err != nil {
		t.Fatalf("failed to write secrets fixture: %v", err)
	}

	if localDeploymentStateExists("production") {
		t.Fatal("secrets-only .tako directory should not count as local deployment state")
	}

	localMgr, err := localstate.NewManager(".", "demo", "production")
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}
	if err := localMgr.SaveDeployment(&localstate.DeploymentState{
		DeploymentID: "current",
		Timestamp:    time.Now().UTC(),
		Environment:  "production",
		Status:       "success",
		Services:     map[string]*localstate.ServiceDeploy{},
	}); err != nil {
		t.Fatalf("SaveDeployment returned error: %v", err)
	}

	if !localDeploymentStateExists("production") {
		t.Fatal("current deployment should count as local deployment state")
	}
}

func TestSyncStateOnDeployRecoversFromMeshActualBeforeRunningFallback(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	originalCollect := syncStateCollectDeploymentHistories
	originalRecoverMesh := syncStateRecoverFromMeshActual
	originalRecoverRunning := syncStateRecoverFromRunningMesh
	t.Cleanup(func() {
		syncStateCollectDeploymentHistories = originalCollect
		syncStateRecoverFromMeshActual = originalRecoverMesh
		syncStateRecoverFromRunningMesh = originalRecoverRunning
	})

	syncStateCollectDeploymentHistories = func(_ *config.Config, envName string, requestedServer string, quiet bool) ([]stateHistoryCandidate, error) {
		if envName != "production" || requestedServer != "" || !quiet {
			t.Fatalf("unexpected history collection args env=%q requested=%q quiet=%v", envName, requestedServer, quiet)
		}
		return nil, nil
	}
	meshRecovered := false
	syncStateRecoverFromMeshActual = func(_ *config.Config, envName string, requestedServer string) error {
		if envName != "production" || requestedServer != "" {
			t.Fatalf("unexpected mesh recovery args env=%q requested=%q", envName, requestedServer)
		}
		meshRecovered = true
		return nil
	}
	syncStateRecoverFromRunningMesh = func(*config.Config, string, string) error {
		t.Fatal("running mesh fallback should not run after mesh actual recovery succeeds")
		return nil
	}

	err := SyncStateOnDeploy(&config.Config{Project: config.ProjectConfig{Name: "demo"}}, "production")
	if err != nil {
		t.Fatalf("SyncStateOnDeploy returned error: %v", err)
	}
	if !meshRecovered {
		t.Fatal("mesh actual recovery was not attempted")
	}
}

func TestSyncStateOnDeployFallsBackToRunningMeshWhenMeshActualMissing(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	originalCollect := syncStateCollectDeploymentHistories
	originalRecoverMesh := syncStateRecoverFromMeshActual
	originalRecoverRunning := syncStateRecoverFromRunningMesh
	t.Cleanup(func() {
		syncStateCollectDeploymentHistories = originalCollect
		syncStateRecoverFromMeshActual = originalRecoverMesh
		syncStateRecoverFromRunningMesh = originalRecoverRunning
	})

	syncStateCollectDeploymentHistories = func(*config.Config, string, string, bool) ([]stateHistoryCandidate, error) {
		return nil, nil
	}
	syncStateRecoverFromMeshActual = func(*config.Config, string, string) error {
		return errors.New("no mesh actual state")
	}
	runningRecovered := false
	syncStateRecoverFromRunningMesh = func(_ *config.Config, envName string, requestedServer string) error {
		if envName != "production" || requestedServer != "" {
			t.Fatalf("unexpected running recovery args env=%q requested=%q", envName, requestedServer)
		}
		runningRecovered = true
		return nil
	}

	err := SyncStateOnDeploy(&config.Config{Project: config.ProjectConfig{Name: "demo"}}, "production")
	if err != nil {
		t.Fatalf("SyncStateOnDeploy returned error: %v", err)
	}
	if !runningRecovered {
		t.Fatal("running mesh fallback was not attempted")
	}
}

func TestRunStateStatusReturnsConfigurationErrors(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	configPath := filepath.Join(tempDir, "tako.yaml")
	if err := os.WriteFile(configPath, []byte(`
project:
  name: demo
  version: 1.0.0
servers:
  node-a:
    host: 10.0.0.1
    user: deploy
    password: test-password
environments:
  production:
    servers: [node-a]
    services:
      web:
        image: nginx:alpine
`), 0600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	oldCfgFile := cfgFile
	oldStateServer := stateServer
	oldEnvFlag := envFlag
	cfgFile = configPath
	stateServer = "node-b"
	envFlag = "production"
	t.Cleanup(func() {
		cfgFile = oldCfgFile
		stateServer = oldStateServer
		envFlag = oldEnvFlag
	})

	err := runStateStatus(nil, nil)
	if err == nil {
		t.Fatal("runStateStatus should return configuration errors")
	}
	if !strings.Contains(err.Error(), "server node-b not found") {
		t.Fatalf("error = %q, want server config context", err)
	}
}

func TestBestDeploymentHistoryPrefersFreshestLastUpdated(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	older := remoteHistory(base, remoteDeployment("old", base, "demo:v1"))
	newer := remoteHistory(base.Add(time.Hour), remoteDeployment("new", base.Add(time.Minute), "demo:v2"))

	best, ok := bestDeploymentHistory([]stateHistoryCandidate{
		{source: "node-a", history: older},
		{source: "node-b", history: newer},
	})
	if !ok {
		t.Fatal("bestDeploymentHistory returned no candidate")
	}
	if best.source != "node-b" {
		t.Fatalf("best source = %q, want node-b", best.source)
	}
}

func TestBestDeploymentHistoryFallsBackToDeploymentTimestamp(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	older := remoteHistory(time.Time{}, remoteDeployment("old", base, "demo:v1"))
	newer := remoteHistory(time.Time{}, remoteDeployment("new", base.Add(time.Hour), "demo:v2"))

	best, ok := bestDeploymentHistory([]stateHistoryCandidate{
		{source: "node-a", history: older},
		{source: "node-b", history: newer},
	})
	if !ok {
		t.Fatal("bestDeploymentHistory returned no candidate")
	}
	if best.source != "node-b" {
		t.Fatalf("best source = %q, want node-b", best.source)
	}
}

func TestBestDeploymentHistoryIgnoresEmptyCandidates(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)

	best, ok := bestDeploymentHistory([]stateHistoryCandidate{
		{source: "empty", history: remoteHistory(base)},
		{source: "nil", history: nil},
		{source: "good", history: remoteHistory(base, remoteDeployment("new", base, "demo:v1"))},
	})
	if !ok {
		t.Fatal("bestDeploymentHistory returned no candidate")
	}
	if best.source != "good" {
		t.Fatalf("best source = %q, want good", best.source)
	}
}

func TestStateHistoryCandidatesFailClosedOnUnreadableHistory(t *testing.T) {
	_, err := stateHistoryCandidatesFromResults([]stateHistoryReadResult{
		{
			serverName:    "node-a",
			host:          "10.0.0.1",
			readAttempted: true,
			err:           errors.New("bad json"),
		},
	}, true)
	if err == nil {
		t.Fatal("stateHistoryCandidatesFromResults should fail when reachable history is unreadable")
	}
	if !strings.Contains(err.Error(), "failed to read deployment history from reachable node") {
		t.Fatalf("error = %q, want fail-closed history read context", err)
	}
}

func TestStateHistoryCandidatesFailWhenNoNodeReachable(t *testing.T) {
	_, err := stateHistoryCandidatesFromResults([]stateHistoryReadResult{
		{
			serverName: "node-a",
			host:       "10.0.0.1",
			err:        errors.New("unreachable"),
		},
	}, true)
	if err == nil {
		t.Fatal("stateHistoryCandidatesFromResults should fail when no node is reachable")
	}
	if !strings.Contains(err.Error(), "failed to reach environment node") {
		t.Fatalf("error = %q, want reachability context", err)
	}
}

func TestStateHistoryCandidatesAllowsRecoveryWhenReachableNodeHasNoHistory(t *testing.T) {
	histories, err := stateHistoryCandidatesFromResults([]stateHistoryReadResult{
		{
			serverName:    "node-a",
			host:          "10.0.0.1",
			readAttempted: true,
			err:           remotestate.ErrNotFound,
		},
		{
			serverName: "node-b",
			host:       "10.0.0.2",
			err:        errors.New("unreachable"),
		},
	}, true)
	if err != nil {
		t.Fatalf("stateHistoryCandidatesFromResults returned error: %v", err)
	}
	if len(histories) != 0 {
		t.Fatalf("histories = %#v, want none", histories)
	}
}

func TestStateHistoryCandidatesUsesValidHistoryDespiteOtherReadError(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)

	histories, err := stateHistoryCandidatesFromResults([]stateHistoryReadResult{
		{
			serverName:    "node-a",
			host:          "10.0.0.1",
			readAttempted: true,
			err:           errors.New("bad json"),
		},
		{
			serverName:    "node-b",
			host:          "10.0.0.2",
			readAttempted: true,
			history:       remoteHistory(base, remoteDeployment("deploy-1", base, "demo:v1")),
		},
	}, true)
	if err != nil {
		t.Fatalf("stateHistoryCandidatesFromResults returned error: %v", err)
	}
	if len(histories) != 1 || histories[0].source != "node-b" {
		t.Fatalf("histories = %#v, want node-b valid history", histories)
	}
}

func TestBestDesiredRevisionPrefersNewestCreatedAt(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	older := desiredRevision("rev-old", base)
	newer := desiredRevision("rev-new", base.Add(time.Hour))

	best, ok := bestDesiredRevision([]stateDesiredCandidate{
		{source: "node-a", desired: older},
		{source: "node-b", desired: newer},
	})
	if !ok {
		t.Fatal("bestDesiredRevision returned no candidate")
	}
	if best.source != "node-b" {
		t.Fatalf("best source = %q, want node-b", best.source)
	}
}

func TestBestDesiredRevisionIgnoresIncompleteCandidates(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)

	best, ok := bestDesiredRevision([]stateDesiredCandidate{
		{source: "missing-id", desired: desiredRevision("", base.Add(time.Hour))},
		{source: "missing-time", desired: desiredRevision("rev-missing-time", time.Time{})},
		{source: "good", desired: desiredRevision("rev-good", base)},
	})
	if !ok {
		t.Fatal("bestDesiredRevision returned no candidate")
	}
	if best.source != "good" {
		t.Fatalf("best source = %q, want good", best.source)
	}
}

func TestBestActualSnapshotPrefersNewestCapturedAt(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	older := actualSnapshot(base, "web")
	newer := actualSnapshot(base.Add(time.Hour), "web", "worker")

	best, ok := bestActualSnapshot([]stateActualCandidate{
		{source: "node-a", actual: older},
		{source: "node-b", actual: newer},
	})
	if !ok {
		t.Fatal("bestActualSnapshot returned no candidate")
	}
	if best.source != "node-b" {
		t.Fatalf("best source = %q, want node-b", best.source)
	}
}

func TestBestActualSnapshotAllowsEmptyServiceSnapshot(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)

	best, ok := bestActualSnapshot([]stateActualCandidate{
		{source: "empty", actual: actualSnapshot(base)},
	})
	if !ok {
		t.Fatal("bestActualSnapshot returned no candidate")
	}
	if best.source != "empty" {
		t.Fatalf("best source = %q, want empty", best.source)
	}
}

func TestBestNodeActualSnapshotsPrefersFreshestPerNode(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)

	best := bestNodeActualSnapshots([]stateNodeActualCandidate{
		{source: "node-a", node: "node-a", actual: nodeActualSnapshot("node-a", base, "web")},
		{source: "node-b", node: "node-a", actual: nodeActualSnapshot("node-a", base.Add(time.Hour), "web", "worker")},
		{source: "node-b", node: "node-b", actual: nodeActualSnapshot("node-b", base.Add(30*time.Minute), "web")},
	})

	if len(best) != 2 {
		t.Fatalf("best node snapshots = %d, want 2", len(best))
	}
	if best["node-a"].source != "node-b" {
		t.Fatalf("node-a source = %q, want node-b", best["node-a"].source)
	}
	if best["node-b"].source != "node-b" {
		t.Fatalf("node-b source = %q, want node-b", best["node-b"].source)
	}
}

func TestAggregateActualSnapshotFromNodeSnapshots(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	best := map[string]stateNodeActualCandidate{
		"node-a": {source: "node-a", node: "node-a", actual: nodeActualSnapshot("node-a", base, "web")},
		"node-b": {source: "node-b", node: "node-b", actual: nodeActualSnapshot("node-b", base.Add(time.Hour), "web", "worker")},
	}

	aggregate := aggregateActualSnapshotFromNodeSnapshots("demo", "production", best)

	if aggregate.CapturedAt != base.Add(time.Hour) {
		t.Fatalf("aggregate freshness = %s, want newest node time", aggregate.CapturedAt)
	}
	if got := aggregate.Services["web"].Replicas; got != 2 {
		t.Fatalf("web replicas = %d, want 2", got)
	}
	if got := len(aggregate.Nodes); got != 2 {
		t.Fatalf("embedded node snapshots = %d, want 2", got)
	}
	if aggregate.TargetNodes[0] != "node-a" || aggregate.TargetNodes[1] != "node-b" {
		t.Fatalf("target nodes not sorted: %#v", aggregate.TargetNodes)
	}
}

func TestActualSnapshotFromTakodActualRecordsNodeServices(t *testing.T) {
	capturedAt := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	actual := &takod.ActualStateResponse{
		Services: map[string]*takod.ActualService{
			"web": {
				Name:       "web",
				Image:      "demo:web",
				Replicas:   2,
				Containers: []string{"web-1", "web-2"},
				ConfigHash: "abc123",
			},
			"worker": {
				Image:      "demo:worker",
				Containers: []string{"worker-1"},
			},
			"nil-service": nil,
		},
	}

	snapshot := actualSnapshotFromTakodActual("demo", "production", "node-a", actual, capturedAt)

	if snapshot.Node != "node-a" || !snapshot.CapturedAt.Equal(capturedAt) {
		t.Fatalf("unexpected snapshot metadata: %#v", snapshot)
	}
	if got := snapshot.Services["web"].Replicas; got != 2 {
		t.Fatalf("web replicas = %d, want 2", got)
	}
	if got := snapshot.Services["web"].ConfigHash; got != "abc123" {
		t.Fatalf("web config hash = %q, want abc123", got)
	}
	if got := snapshot.Services["worker"].Replicas; got != 1 {
		t.Fatalf("worker replicas = %d, want container count fallback", got)
	}
	if _, ok := snapshot.Services["nil-service"]; ok {
		t.Fatal("nil service should be skipped")
	}
}

func TestRunningActualSnapshotsAggregateAcrossNodes(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	nodeA := actualSnapshotFromTakodActual("demo", "production", "node-a", &takod.ActualStateResponse{
		Services: map[string]*takod.ActualService{
			"web": {Name: "web", Image: "demo:web-a", Replicas: 2, Containers: []string{"a1", "a2"}},
		},
	}, base)
	nodeB := actualSnapshotFromTakodActual("demo", "production", "node-b", &takod.ActualStateResponse{
		Services: map[string]*takod.ActualService{
			"web":    {Name: "web", Image: "demo:web-b", Replicas: 1, Containers: []string{"b1"}},
			"worker": {Name: "worker", Image: "demo:worker", Replicas: 1, Containers: []string{"w1"}},
		},
	}, base.Add(time.Hour))

	aggregate := aggregateActualSnapshotFromNodeSnapshots("demo", "production", map[string]stateNodeActualCandidate{
		"node-a": {source: "node-a", node: "node-a", actual: nodeA},
		"node-b": {source: "node-b", node: "node-b", actual: nodeB},
	})

	if got := aggregate.Services["web"].Replicas; got != 3 {
		t.Fatalf("web replicas = %d, want 3", got)
	}
	if got := len(aggregate.Services["web"].Containers); got != 3 {
		t.Fatalf("web containers = %d, want 3", got)
	}
	if got := aggregate.Services["worker"].Replicas; got != 1 {
		t.Fatalf("worker replicas = %d, want 1", got)
	}
	if got := len(aggregate.Nodes); got != 2 {
		t.Fatalf("embedded node snapshots = %d, want 2", got)
	}
	if !aggregate.CapturedAt.Equal(base.Add(time.Hour)) {
		t.Fatalf("aggregate capturedAt = %s, want newest node time", aggregate.CapturedAt)
	}
}

func TestOrderedStateServerNamesPrefersRequestedServer(t *testing.T) {
	cfg := &config.Config{
		Servers: map[string]config.ServerConfig{
			"node-a": {Host: "10.0.0.1"},
			"node-b": {Host: "10.0.0.2"},
			"node-c": {Host: "10.0.0.3"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Servers: []string{"node-a", "node-b", "node-c"},
			},
		},
	}

	got, err := orderedStateServerNames(cfg, "production", "node-b")
	if err != nil {
		t.Fatalf("orderedStateServerNames returned error: %v", err)
	}

	want := []string{"node-b", "node-a", "node-c"}
	if len(got) != len(want) {
		t.Fatalf("ordered servers = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ordered servers = %v, want %v", got, want)
		}
	}
}

func TestStatePullServerNamesReadsAllByDefault(t *testing.T) {
	cfg := stateServerNamesConfig()

	got, err := statePullServerNames(cfg, "production", "")
	if err != nil {
		t.Fatalf("statePullServerNames returned error: %v", err)
	}
	want := []string{"node-a", "node-b", "node-c"}
	if len(got) != len(want) {
		t.Fatalf("state pull servers = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("state pull servers = %v, want %v", got, want)
		}
	}
}

func TestStatePullServerNamesUsesOnlyRequestedServer(t *testing.T) {
	cfg := stateServerNamesConfig()

	got, err := statePullServerNames(cfg, "production", "node-b")
	if err != nil {
		t.Fatalf("statePullServerNames returned error: %v", err)
	}
	if len(got) != 1 || got[0] != "node-b" {
		t.Fatalf("state pull servers = %v, want [node-b]", got)
	}
}

func TestLatestDeploymentByTimestamp(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	latest := latestDeploymentByTimestamp([]*remotestate.DeploymentState{
		remoteDeployment("old", base, "demo:v1"),
		nil,
		remoteDeployment("new", base.Add(time.Hour), "demo:v2"),
	})

	if latest == nil || latest.ID != "new" {
		t.Fatalf("latest deployment = %#v, want new", latest)
	}
}

func TestListDeploymentsFromHistoryFiltersSortsAndLimits(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	oldSuccess := remoteDeployment("old-success", base, "demo:v1")
	newSuccess := remoteDeployment("new-success", base.Add(2*time.Hour), "demo:v3")
	failed := remoteDeployment("failed", base.Add(time.Hour), "demo:v2")
	failed.Status = remotestate.StatusFailed

	got := listDeploymentsFromHistory(remoteHistory(base, oldSuccess, failed, newSuccess), &remotestate.HistoryOptions{
		Limit:         1,
		Status:        remotestate.StatusSuccess,
		IncludeFailed: true,
	})

	if len(got) != 1 {
		t.Fatalf("deployments = %d, want 1", len(got))
	}
	if got[0].ID != "new-success" {
		t.Fatalf("deployment = %q, want newest successful deployment", got[0].ID)
	}
}

func TestStateStatusCandidatesIncludesEmbeddedAndNodeActual(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	aggregate := actualSnapshot(base.Add(time.Hour), "web")
	aggregate.Nodes = map[string]takodstate.ActualNodeSnapshot{
		"node-a": {
			Node:       "node-a",
			Services:   actualSnapshot(base, "web").Services,
			CapturedAt: base,
		},
	}

	histories, desired, actual, nodeActual := stateStatusCandidates([]stateStatusNode{
		{
			name:    "node-a",
			history: remoteHistory(base, remoteDeployment("new", base, "demo:v1")),
			desired: desiredRevision("rev-a", base),
			actual:  aggregate,
			nodeActual: []stateNodeActualCandidate{
				{source: "node-b", node: "node-b", actual: nodeActualSnapshot("node-b", base.Add(2*time.Hour), "worker")},
			},
		},
		{name: "node-c"},
	})

	if len(histories) != 1 || histories[0].source != "node-a" {
		t.Fatalf("histories = %#v, want one node-a candidate", histories)
	}
	if len(desired) != 1 || desired[0].source != "node-a" {
		t.Fatalf("desired = %#v, want one node-a candidate", desired)
	}
	if len(actual) != 1 || actual[0].source != "node-a" {
		t.Fatalf("actual = %#v, want one node-a candidate", actual)
	}
	if len(nodeActual) != 2 {
		t.Fatalf("node actual candidates = %d, want embedded plus standalone", len(nodeActual))
	}
}

func TestPrintStateStatusHistoryDistinguishesMissingFromUnreadable(t *testing.T) {
	missing := captureStdout(t, func() {
		printStateStatusHistory(nil, remotestate.ErrNotFound)
	})
	if !strings.Contains(missing, "History: not recorded") {
		t.Fatalf("missing history output = %q", missing)
	}

	unreadable := captureStdout(t, func() {
		printStateStatusHistory(nil, errors.New("bad json"))
	})
	if !strings.Contains(unreadable, "History: unavailable - bad json") {
		t.Fatalf("unreadable history output = %q", unreadable)
	}
}

func TestBestStateStatusActualBuildsAggregateFromNodeSnapshots(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)

	best, ok, nodes := bestStateStatusActual("demo", "production", nil, []stateNodeActualCandidate{
		{source: "node-a", node: "node-a", actual: nodeActualSnapshot("node-a", base, "web")},
		{source: "node-b", node: "node-b", actual: nodeActualSnapshot("node-b", base.Add(time.Hour), "worker")},
	})

	if !ok {
		t.Fatal("bestStateStatusActual returned no aggregate")
	}
	if best.source != "node actual snapshots" {
		t.Fatalf("best source = %q, want node actual snapshots", best.source)
	}
	if len(nodes) != 2 || len(best.actual.Nodes) != 2 {
		t.Fatalf("node snapshots = %d embedded = %d, want 2", len(nodes), len(best.actual.Nodes))
	}
	if got := best.actual.CapturedAt; !got.Equal(base.Add(time.Hour)) {
		t.Fatalf("aggregate capturedAt = %s, want newest node time", got)
	}
}

func TestReconcileStateFromActualSnapshotBuildsRecoveredDeployment(t *testing.T) {
	snapshot := actualSnapshot(time.Now().UTC(), "web", "worker")
	snapshot.Services["web"] = takodstate.ActualService{
		Name:     "web",
		Image:    "demo:web",
		Replicas: 2,
	}

	deployment, err := ReconcileStateFromActualSnapshot(&config.Config{
		Project: config.ProjectConfig{Name: "demo"},
	}, "production", snapshot, "recovered from mesh")
	if err != nil {
		t.Fatalf("ReconcileStateFromActualSnapshot returned error: %v", err)
	}

	if deployment.Environment != "production" || deployment.Mode != config.RuntimeModeTakod || deployment.Status != "recovered" {
		t.Fatalf("unexpected recovered deployment metadata: %#v", deployment)
	}
	if deployment.Notes != "recovered from mesh" {
		t.Fatalf("notes = %q, want recovered from mesh", deployment.Notes)
	}
	if got := deployment.Services["web"].Replicas; got != 2 {
		t.Fatalf("web replicas = %d, want 2", got)
	}
	if got := deployment.Services["worker"].Image; got != "demo:worker" {
		t.Fatalf("worker image = %q, want demo:worker", got)
	}
}

func TestReconcileStateFromActualSnapshotRejectsEmptySnapshot(t *testing.T) {
	_, err := ReconcileStateFromActualSnapshot(&config.Config{
		Project: config.ProjectConfig{Name: "demo"},
	}, "production", actualSnapshot(time.Now().UTC()), "empty")
	if err == nil {
		t.Fatal("ReconcileStateFromActualSnapshot should reject snapshots without services")
	}
}

func stateServerNamesConfig() *config.Config {
	return &config.Config{
		Servers: map[string]config.ServerConfig{
			"node-a": {Host: "10.0.0.1"},
			"node-b": {Host: "10.0.0.2"},
			"node-c": {Host: "10.0.0.3"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Servers: []string{"node-a", "node-b", "node-c"},
			},
		},
	}
}

func remoteDeployment(id string, timestamp time.Time, image string) *remotestate.DeploymentState {
	return &remotestate.DeploymentState{
		ID:          id,
		Timestamp:   timestamp,
		ProjectName: "demo",
		Environment: "production",
		Status:      remotestate.StatusSuccess,
		Services: map[string]remotestate.ServiceState{
			"web": {
				Name:     "web",
				Image:    image,
				Port:     3000,
				Replicas: 1,
				HealthCheck: remotestate.HealthCheckState{
					Enabled: true,
					Healthy: true,
				},
			},
		},
		User: "tester",
	}
}

func remoteHistory(lastUpdated time.Time, deployments ...*remotestate.DeploymentState) *remotestate.DeploymentHistory {
	return &remotestate.DeploymentHistory{
		ProjectName: "demo",
		Environment: "production",
		Server:      "node-a",
		Deployments: deployments,
		LastUpdated: lastUpdated,
	}
}

func desiredRevision(id string, createdAt time.Time) *takodstate.DesiredRevision {
	return &takodstate.DesiredRevision{
		SchemaVersion: takodstate.SchemaVersion,
		RevisionID:    id,
		Project:       "demo",
		Environment:   "production",
		Source:        "test",
		Services: map[string]takodstate.DesiredService{
			"web": {
				Name:     "web",
				Type:     "public",
				Image:    "demo:web",
				Replicas: 1,
			},
		},
		CreatedAt: createdAt,
	}
}

func actualSnapshot(capturedAt time.Time, services ...string) *takodstate.ActualSnapshot {
	snapshot := &takodstate.ActualSnapshot{
		SchemaVersion: takodstate.SchemaVersion,
		Project:       "demo",
		Environment:   "production",
		Services:      map[string]takodstate.ActualService{},
		CapturedAt:    capturedAt,
	}
	for _, service := range services {
		snapshot.Services[service] = takodstate.ActualService{
			Name:     service,
			Image:    "demo:" + service,
			Replicas: 1,
		}
	}
	return snapshot
}

func nodeActualSnapshot(node string, capturedAt time.Time, services ...string) *takodstate.ActualSnapshot {
	snapshot := actualSnapshot(capturedAt, services...)
	snapshot.Node = node
	return snapshot
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create stdout pipe: %v", err)
	}
	os.Stdout = writer
	defer func() {
		os.Stdout = original
	}()

	fn()

	if err := writer.Close(); err != nil {
		t.Fatalf("failed to close stdout pipe: %v", err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("failed to read stdout pipe: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("failed to close stdout reader: %v", err)
	}
	return string(output)
}
