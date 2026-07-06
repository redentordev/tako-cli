package cmd

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
	"github.com/redentordev/tako-cli/pkg/ssh"
	localstate "github.com/redentordev/tako-cli/pkg/state"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodstate"
	"github.com/spf13/cobra"
)

func TestStateCommandsSilenceUsageOnExecutionErrors(t *testing.T) {
	commands := []*cobra.Command{
		stateCmd,
		statePullCmd,
		stateStatusCmd,
		stateRepairCmd,
		stateForgetNodeCmd,
		stateLeaseCmd,
		stateLeaseReleaseCmd,
	}

	for _, cmd := range commands {
		if !cmd.SilenceUsage {
			t.Fatalf("%s command should silence usage on execution errors", cmd.CommandPath())
		}
	}
}

func TestDeploymentCommitsEquivalent(t *testing.T) {
	tests := []struct {
		name         string
		localCommit  string
		remoteCommit string
		remoteShort  string
		want         bool
	}{
		{name: "empty local is equivalent to git-optional remote", remoteCommit: "abcdef", remoteShort: "abc", want: true},
		{name: "empty remote commit and short are equivalent", localCommit: "abcdef", want: true},
		{name: "whitespace is trimmed", localCommit: " abcdef ", remoteCommit: "\tabcdef\n", want: true},
		{name: "exact full commit match", localCommit: "abcdef", remoteCommit: "abcdef", want: true},
		{name: "local full commit matches remote short", localCommit: "abcdef123", remoteShort: "abcdef", want: true},
		{name: "remote full commit matches local short", localCommit: "abcdef", remoteCommit: "abcdef123", want: true},
		{name: "mismatch", localCommit: "abcdef", remoteCommit: "123456", remoteShort: "123", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deploymentCommitsEquivalent(tt.localCommit, tt.remoteCommit, tt.remoteShort)
			if got != tt.want {
				t.Fatalf("deploymentCommitsEquivalent(%q, %q, %q) = %v, want %v", tt.localCommit, tt.remoteCommit, tt.remoteShort, got, tt.want)
			}
		})
	}
}

func TestValidateStateForgetNodeNameRejectsUnsafeValues(t *testing.T) {
	for _, nodeName := range []string{"", "../node", "node/b", "node b", "..", "node..b"} {
		t.Run(nodeName, func(t *testing.T) {
			if err := validateStateForgetNodeName(nodeName); err == nil {
				t.Fatalf("validateStateForgetNodeName(%q) should reject unsafe value", nodeName)
			}
		})
	}
	if err := validateStateForgetNodeName("node-a_1.prod"); err != nil {
		t.Fatalf("validateStateForgetNodeName rejected safe name: %v", err)
	}
}

func TestActualSnapshotWithoutNodePrunesTargetAndEmbeddedNode(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	snapshot := actualSnapshot(base, "web")
	snapshot.TargetNodes = []string{"node-a", "node-b", "node-c"}
	snapshot.Nodes = map[string]takodstate.ActualNodeSnapshot{
		"node-a": {
			Node:       "node-a",
			Services:   nodeActualSnapshot("node-a", base, "web").Services,
			CapturedAt: base,
		},
		"node-b": {
			Node:       "node-b",
			Services:   nodeActualSnapshot("node-b", base, "worker").Services,
			CapturedAt: base,
		},
	}

	pruned, changed := actualSnapshotWithoutNode(snapshot, "node-b")
	if !changed {
		t.Fatal("actualSnapshotWithoutNode should report a change")
	}
	if got := strings.Join(pruned.TargetNodes, ","); got != "node-a,node-c" {
		t.Fatalf("target nodes = %q, want node-a,node-c", got)
	}
	if _, ok := pruned.Nodes["node-b"]; ok {
		t.Fatalf("embedded node-b should be removed: %#v", pruned.Nodes)
	}
	if _, ok := snapshot.Nodes["node-b"]; !ok {
		t.Fatal("original snapshot should not be mutated")
	}
	if !pruned.CapturedAt.After(base) {
		t.Fatalf("pruned capturedAt = %s, want refreshed timestamp after %s", pruned.CapturedAt, base)
	}
}

func TestForgetNodeOnRepairNodePrunesAggregateAndDeletesStandaloneSnapshot(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	actual := actualSnapshot(base, "web")
	actual.TargetNodes = []string{"node-a", "node-b"}
	actual.Nodes = map[string]takodstate.ActualNodeSnapshot{
		"node-a": {
			Node:       "node-a",
			Services:   nodeActualSnapshot("node-a", base, "web").Services,
			CapturedAt: base,
		},
		"node-b": {
			Node:       "node-b",
			Services:   nodeActualSnapshot("node-b", base, "worker").Services,
			CapturedAt: base,
		},
	}
	runtime := &recordingStateRepairRuntime{
		previousActual: actual,
		nodeActual: map[string]*takodstate.ActualSnapshot{
			"node-b": nodeActualSnapshot("node-b", base, "worker"),
		},
	}

	result := forgetNodeOnRepairNode(stateRepairNode{name: "node-a", runtime: runtime}, "demo", "production", "node-b")

	if result.err != nil {
		t.Fatalf("forgetNodeOnRepairNode returned error: %v", result.err)
	}
	if !result.nodeActualExisted {
		t.Fatal("expected node actual snapshot to be detected")
	}
	if !result.aggregatePruned {
		t.Fatal("expected aggregate actual state to be pruned")
	}
	if got := strings.Join(runtime.deleted, ","); got != "node-b" {
		t.Fatalf("deleted node actual = %q, want node-b", got)
	}
	if runtime.writtenActual == nil {
		t.Fatal("expected pruned aggregate actual state to be written")
	}
	if _, ok := runtime.writtenActual.Nodes["node-b"]; ok {
		t.Fatalf("written aggregate still contains node-b: %#v", runtime.writtenActual.Nodes)
	}
	if len(runtime.events) != 1 || runtime.events[0].Type != "state_node_forgotten" {
		t.Fatalf("events = %#v, want state_node_forgotten event", runtime.events)
	}
}

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

	syncStateCollectDeploymentHistories = func(_ *ssh.Pool, _ *config.Config, envName string, requestedServer string, quiet bool) ([]stateHistoryCandidate, error) {
		if envName != "production" || requestedServer != "" || !quiet {
			t.Fatalf("unexpected history collection args env=%q requested=%q quiet=%v", envName, requestedServer, quiet)
		}
		return nil, nil
	}
	meshRecovered := false
	syncStateRecoverFromMeshActual = func(_ *ssh.Pool, _ *config.Config, envName string, requestedServer string) error {
		if envName != "production" || requestedServer != "" {
			t.Fatalf("unexpected mesh recovery args env=%q requested=%q", envName, requestedServer)
		}
		meshRecovered = true
		return nil
	}
	syncStateRecoverFromRunningMesh = func(*ssh.Pool, *config.Config, string, string) error {
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

	syncStateCollectDeploymentHistories = func(*ssh.Pool, *config.Config, string, string, bool) ([]stateHistoryCandidate, error) {
		return nil, nil
	}
	syncStateRecoverFromMeshActual = func(*ssh.Pool, *config.Config, string, string) error {
		return errors.New("no mesh actual state")
	}
	runningRecovered := false
	syncStateRecoverFromRunningMesh = func(_ *ssh.Pool, _ *config.Config, envName string, requestedServer string) error {
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

func TestSyncStateOnDeployRefreshesExistingLocalStateFromRemoteHistory(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	localMgr, err := localstate.NewManager(".", "demo", "production")
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	if err := localMgr.SaveDeployment(&localstate.DeploymentState{
		DeploymentID: "old",
		Timestamp:    base,
		Environment:  "production",
		Status:       "success",
		Services: map[string]*localstate.ServiceDeploy{
			"web": {Image: "demo:v1", Replicas: 1},
		},
	}); err != nil {
		t.Fatalf("SaveDeployment returned error: %v", err)
	}

	originalCollect := syncStateCollectDeploymentHistories
	originalRecoverMesh := syncStateRecoverFromMeshActual
	originalRecoverRunning := syncStateRecoverFromRunningMesh
	t.Cleanup(func() {
		syncStateCollectDeploymentHistories = originalCollect
		syncStateRecoverFromMeshActual = originalRecoverMesh
		syncStateRecoverFromRunningMesh = originalRecoverRunning
	})

	syncStateCollectDeploymentHistories = func(_ *ssh.Pool, _ *config.Config, envName string, requestedServer string, quiet bool) ([]stateHistoryCandidate, error) {
		if envName != "production" || requestedServer != "" || !quiet {
			t.Fatalf("unexpected history collection args env=%q requested=%q quiet=%v", envName, requestedServer, quiet)
		}
		return []stateHistoryCandidate{
			{
				source:  "node-a",
				history: remoteHistory(base.Add(time.Hour), remoteDeployment("new", base.Add(time.Hour), "demo:v2")),
			},
		}, nil
	}
	syncStateRecoverFromMeshActual = func(*ssh.Pool, *config.Config, string, string) error {
		t.Fatal("mesh recovery should not run when remote history is available")
		return nil
	}
	syncStateRecoverFromRunningMesh = func(*ssh.Pool, *config.Config, string, string) error {
		t.Fatal("running recovery should not run when remote history is available")
		return nil
	}

	err = SyncStateOnDeploy(&config.Config{Project: config.ProjectConfig{Name: "demo"}}, "production")
	if err != nil {
		t.Fatalf("SyncStateOnDeploy returned error: %v", err)
	}

	current, err := localMgr.GetCurrentDeployment()
	if err != nil {
		t.Fatalf("GetCurrentDeployment returned error: %v", err)
	}
	if current == nil || current.DeploymentID != "new" {
		t.Fatalf("current deployment = %#v, want remote deployment new", current)
	}
	if got := current.Services["web"].Image; got != "demo:v2" {
		t.Fatalf("current web image = %q, want demo:v2", got)
	}
}

func TestSyncStateOnDeployKeepsExistingLocalStateWhenRemoteHistoryMissing(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)

	localMgr, err := localstate.NewManager(".", "demo", "production")
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	if err := localMgr.SaveDeployment(&localstate.DeploymentState{
		DeploymentID: "local",
		Timestamp:    base,
		Environment:  "production",
		Status:       "success",
		Services: map[string]*localstate.ServiceDeploy{
			"web": {Image: "demo:local", Replicas: 1},
		},
	}); err != nil {
		t.Fatalf("SaveDeployment returned error: %v", err)
	}

	originalCollect := syncStateCollectDeploymentHistories
	originalRecoverMesh := syncStateRecoverFromMeshActual
	originalRecoverRunning := syncStateRecoverFromRunningMesh
	t.Cleanup(func() {
		syncStateCollectDeploymentHistories = originalCollect
		syncStateRecoverFromMeshActual = originalRecoverMesh
		syncStateRecoverFromRunningMesh = originalRecoverRunning
	})

	syncStateCollectDeploymentHistories = func(*ssh.Pool, *config.Config, string, string, bool) ([]stateHistoryCandidate, error) {
		return nil, nil
	}
	syncStateRecoverFromMeshActual = func(*ssh.Pool, *config.Config, string, string) error {
		t.Fatal("mesh recovery should not overwrite existing local state when no remote history exists")
		return nil
	}
	syncStateRecoverFromRunningMesh = func(*ssh.Pool, *config.Config, string, string) error {
		t.Fatal("running recovery should not overwrite existing local state when no remote history exists")
		return nil
	}

	err = SyncStateOnDeploy(&config.Config{Project: config.ProjectConfig{Name: "demo"}}, "production")
	if err != nil {
		t.Fatalf("SyncStateOnDeploy returned error: %v", err)
	}

	current, err := localMgr.GetCurrentDeployment()
	if err != nil {
		t.Fatalf("GetCurrentDeployment returned error: %v", err)
	}
	if current == nil || current.DeploymentID != "local" {
		t.Fatalf("current deployment = %#v, want existing local deployment", current)
	}
	if got := current.Services["web"].Image; got != "demo:local" {
		t.Fatalf("current web image = %q, want demo:local", got)
	}
}

func TestReleaseStateLeaseByIDRequiresForceForActiveLease(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	manager := &recordingLeaseManager{}
	nodes := []stateLeaseNode{
		{
			name:    "node-a",
			manager: manager,
			lease: &remotestate.LeaseInfo{
				ID:        "lease-active",
				ExpiresAt: now.Add(time.Minute),
			},
		},
	}

	if _, err := releaseStateLeaseByID(nodes, "lease-active", false, now); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("expected active lease to require --force, got %v", err)
	}
	if released := manager.Released(); len(released) != 0 {
		t.Fatalf("released leases = %#v, want none", released)
	}

	released, err := releaseStateLeaseByID(nodes, "lease-active", true, now)
	if err != nil {
		t.Fatalf("releaseStateLeaseByID with force returned error: %v", err)
	}
	if strings.Join(released, ",") != "node-a" {
		t.Fatalf("released nodes = %#v, want node-a", released)
	}
	if got := strings.Join(manager.Released(), ","); got != "lease-active" {
		t.Fatalf("released leases = %q, want lease-active", got)
	}
}

func TestReleaseStateLeaseByIDAllowsExpiredLeaseWithoutForce(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	manager := &recordingLeaseManager{}
	nodes := []stateLeaseNode{
		{
			name:    "node-a",
			manager: manager,
			lease: &remotestate.LeaseInfo{
				ID:        "lease-expired",
				ExpiresAt: now.Add(-time.Second),
			},
		},
	}

	released, err := releaseStateLeaseByID(nodes, "lease-expired", false, now)
	if err != nil {
		t.Fatalf("releaseStateLeaseByID returned error: %v", err)
	}
	if strings.Join(released, ",") != "node-a" {
		t.Fatalf("released nodes = %#v, want node-a", released)
	}
}

func TestReleaseStateLeaseByIDReportsMissingReachableLease(t *testing.T) {
	nodes := []stateLeaseNode{
		{name: "node-a"},
		{name: "node-b", err: errors.New("connection refused")},
	}

	_, err := releaseStateLeaseByID(nodes, "missing", true, time.Now())
	if err == nil {
		t.Fatal("releaseStateLeaseByID should report missing lease")
	}
	for _, want := range []string{"missing", "not found", "node-b"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}

func TestRenderStateLeaseResultMachineJSONSuppressesHumanOutput(t *testing.T) {
	withMachineOutput(t, outputFormatJSON, "", func() {
		result := &engine.StateLeaseResult{
			APIVersion:  "tako.redentor.dev/v1alpha1",
			Kind:        engine.KindStateLeaseResult,
			Project:     "demo",
			Environment: "production",
			Servers:     []string{"node-a"},
			Nodes: []engine.StateLeaseNodeResult{{
				Name: "node-a",
				Host: "10.0.0.1",
				Lease: &remotestate.LeaseInfo{
					ID:        "lease-1",
					Operation: "deploy",
				},
			}},
		}
		stdout := captureConfigExportStdout(t, func() {
			if err := renderStateLeaseResult(result); err != nil {
				t.Fatalf("renderStateLeaseResult returned error: %v", err)
			}
		})
		if strings.Contains(stdout, "Project:") || strings.Contains(stdout, "Node:") || strings.Contains(stdout, "Lease:") {
			t.Fatalf("machine stdout contains human lease output: %q", stdout)
		}
		var decoded engine.StateLeaseResult
		if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
			t.Fatalf("stdout is not parseable lease JSON: %v\n%s", err, stdout)
		}
		if decoded.Kind != engine.KindStateLeaseResult || len(decoded.Nodes) != 1 || decoded.Nodes[0].Lease.ID != "lease-1" {
			t.Fatalf("decoded result = %#v", decoded)
		}
	})
}

func TestRenderStatePullResultMachineJSONSuppressesHumanOutput(t *testing.T) {
	withMachineOutput(t, outputFormatJSON, "", func() {
		result := &engine.StatePullResult{
			APIVersion:   "tako.redentor.dev/v1alpha1",
			Kind:         engine.KindStatePullResult,
			Project:      "demo",
			Environment:  "production",
			Status:       engine.StatePullStatusSyncedHistory,
			SourceServer: "node-a",
			SyncedCount:  1,
			Latest:       &engine.StatePullLatestDeployment{ID: "deploy-1", DisplayID: "deploy", Status: remotestate.StatusSuccess, Timestamp: time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC), User: "alice", Commit: "abc1234"},
		}
		stdout := captureConfigExportStdout(t, func() {
			if err := renderStatePullResult(result); err != nil {
				t.Fatalf("renderStatePullResult returned error: %v", err)
			}
		})
		for _, human := range []string{"Selected deployment history", "Synced", "Latest deployment", "Local state"} {
			if strings.Contains(stdout, human) {
				t.Fatalf("machine stdout contains human state pull output %q: %q", human, stdout)
			}
		}
		var decoded engine.StatePullResult
		if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
			t.Fatalf("stdout is not parseable state pull JSON: %v\n%s", err, stdout)
		}
		if decoded.Kind != engine.KindStatePullResult || decoded.Status != engine.StatePullStatusSyncedHistory || decoded.Latest == nil || decoded.Latest.ID != "deploy-1" {
			t.Fatalf("decoded result = %#v", decoded)
		}
	})
}

func TestRenderStatePullResultNDJSONSuppressesHumanOutput(t *testing.T) {
	withMachineOutput(t, outputFormatText, eventsFormatNDJSON, func() {
		result := &engine.StatePullResult{
			APIVersion:  "tako.redentor.dev/v1alpha1",
			Kind:        engine.KindStatePullResult,
			Project:     "demo",
			Environment: "production",
			Status:      engine.StatePullStatusNoneFound,
		}
		stdout := captureConfigExportStdout(t, func() {
			if err := renderStatePullResult(result); err != nil {
				t.Fatalf("renderStatePullResult returned error: %v", err)
			}
		})
		if strings.Contains(stdout, "No remote deployment history found") || strings.Contains(stdout, "Run 'tako deploy'") {
			t.Fatalf("NDJSON stdout contains human state pull output: %q", stdout)
		}
		lines := strings.Split(strings.TrimSpace(stdout), "\n")
		if len(lines) != 1 {
			t.Fatalf("NDJSON stdout lines = %d, want 1: %q", len(lines), stdout)
		}
		var event struct {
			Type string `json:"type"`
			Data struct {
				Result engine.StatePullResult `json:"result"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(lines[0]), &event); err != nil {
			t.Fatalf("stdout is not parseable NDJSON: %v\n%s", err, stdout)
		}
		if event.Type != "result" || event.Data.Result.Kind != engine.KindStatePullResult || event.Data.Result.Status != engine.StatePullStatusNoneFound {
			t.Fatalf("decoded event = %#v", event)
		}
	})
}

func TestRunStateLeaseReleaseMachineJSONSuppressesReleaseProse(t *testing.T) {
	withMachineOutput(t, outputFormatJSON, "", func() {
		result := &engine.StateLeaseReleaseResult{
			APIVersion:    "tako.redentor.dev/v1alpha1",
			Kind:          engine.KindStateLeaseReleaseResult,
			Project:       "demo",
			Environment:   "production",
			Servers:       []string{"node-a"},
			LeaseID:       "lease-1",
			Force:         true,
			Released:      []string{"node-a"},
			ReleasedCount: 1,
		}
		stdout := captureConfigExportStdout(t, func() {
			if err := renderStateLeaseReleaseResult(result); err != nil {
				t.Fatalf("renderStateLeaseReleaseResult returned error: %v", err)
			}
		})
		if strings.Contains(stdout, "Released lease") {
			t.Fatalf("machine stdout contains human release prose: %q", stdout)
		}
		var decoded engine.StateLeaseReleaseResult
		if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
			t.Fatalf("stdout is not parseable release JSON: %v\n%s", err, stdout)
		}
		if decoded.Kind != engine.KindStateLeaseReleaseResult || decoded.LeaseID != "lease-1" || decoded.ReleasedCount != 1 {
			t.Fatalf("decoded result = %#v", decoded)
		}
	})
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

func TestStateSyncRecommendationReportsSyncedState(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	lines := stateSyncRecommendation(
		true,
		&localstate.DeploymentState{
			DeploymentID: "deploy-new",
			Timestamp:    base,
		},
		stateHistoryCandidate{
			source:  "node-a",
			history: remoteHistory(base, remoteDeployment("deploy-new", base, "demo:v1")),
		},
		true,
		0,
	)

	output := strings.Join(lines, "\n")
	if !strings.Contains(output, "No state pull needed.") {
		t.Fatalf("recommendation = %q, want no-pull guidance", output)
	}
	if strings.Contains(output, "Run 'tako state pull'") {
		t.Fatalf("recommendation = %q, should not suggest pull when local matches remote", output)
	}
}

func TestStateSyncRecommendationTreatsEquivalentDeploymentWithDifferentIDAsSynced(t *testing.T) {
	base := time.Date(2026, 6, 17, 19, 31, 29, 0, time.UTC)
	remote := remoteDeployment("1781724699", base, "demo:v1")
	remote.GitCommitShort = "c08d8fc"

	lines := stateSyncRecommendation(
		true,
		&localstate.DeploymentState{
			DeploymentID: "deploy-20260617-123144",
			Timestamp:    base,
			Status:       "success",
			GitCommit:    "c08d8fc1234567890",
			Services: map[string]*localstate.ServiceDeploy{
				"web": {
					Image:    "demo:v1",
					Replicas: 1,
				},
			},
		},
		stateHistoryCandidate{
			source:  "node-a",
			history: remoteHistory(base, remote),
		},
		true,
		0,
	)

	output := strings.Join(lines, "\n")
	if !strings.Contains(output, "different ID formats") {
		t.Fatalf("recommendation = %q, want ID-format explanation", output)
	}
	if !strings.Contains(output, "No state pull needed.") {
		t.Fatalf("recommendation = %q, want no-pull guidance", output)
	}
	if strings.Contains(output, "Run 'tako state pull'") {
		t.Fatalf("recommendation = %q, should not suggest pull for equivalent deployment", output)
	}
}

func TestStateSyncRecommendationTreatsMatchingCommitWithMissingServicesAsSynced(t *testing.T) {
	base := time.Date(2026, 6, 17, 19, 31, 29, 0, time.UTC)
	remote := remoteDeployment("1781724699", base, "demo:v1")
	remote.GitCommit = "c08d8fcda4353183b3ecf7f23a5b0d6f0f8ee302"

	lines := stateSyncRecommendation(
		true,
		&localstate.DeploymentState{
			DeploymentID: "deploy-20260617-123144",
			Timestamp:    base.Add(341 * time.Millisecond),
			Status:       "success",
			GitCommit:    "c08d8fcda4353183b3ecf7f23a5b0d6f0f8ee302",
		},
		stateHistoryCandidate{
			source:  "node-a",
			history: remoteHistory(base, remote),
		},
		true,
		0,
	)

	output := strings.Join(lines, "\n")
	if !strings.Contains(output, "No state pull needed.") {
		t.Fatalf("recommendation = %q, want no-pull guidance", output)
	}
	if strings.Contains(output, "Run 'tako state pull'") {
		t.Fatalf("recommendation = %q, should not suggest pull for matching commit with missing service details", output)
	}
}

func TestStateSyncRecommendationReportsStaleLocalState(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	lines := stateSyncRecommendation(
		true,
		&localstate.DeploymentState{
			DeploymentID: "deploy-old",
			Timestamp:    base,
		},
		stateHistoryCandidate{
			source:  "node-b",
			history: remoteHistory(base.Add(time.Hour), remoteDeployment("deploy-new", base.Add(time.Hour), "demo:v2")),
		},
		true,
		0,
	)

	output := strings.Join(lines, "\n")
	for _, want := range []string{"Remote deployment history from node-b is newer", "Run 'tako state pull'"} {
		if !strings.Contains(output, want) {
			t.Fatalf("recommendation = %q, want %q", output, want)
		}
	}
}

func TestStateSyncRecommendationReportsLocalStateNewerThanRemote(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	lines := stateSyncRecommendation(
		true,
		&localstate.DeploymentState{
			DeploymentID: "deploy-local",
			Timestamp:    base.Add(time.Hour),
		},
		stateHistoryCandidate{
			source:  "node-a",
			history: remoteHistory(base, remoteDeployment("deploy-remote", base, "demo:v1")),
		},
		true,
		0,
	)

	output := strings.Join(lines, "\n")
	for _, want := range []string{"Local deployment records are newer", "All checked nodes are reachable", "Run 'tako deploy --yes'", "avoid 'tako state pull'"} {
		if !strings.Contains(output, want) {
			t.Fatalf("recommendation = %q, want %q", output, want)
		}
	}
}

func TestStateSyncRecommendationReportsLocalStateNewerThanRemoteWithUnreachableNodes(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	lines := stateSyncRecommendation(
		true,
		&localstate.DeploymentState{
			DeploymentID: "deploy-local",
			Timestamp:    base.Add(time.Hour),
		},
		stateHistoryCandidate{
			source:  "node-a",
			history: remoteHistory(base, remoteDeployment("deploy-remote", base, "demo:v1")),
		},
		true,
		1,
	)

	output := strings.Join(lines, "\n")
	for _, want := range []string{"Local deployment records are newer", "Some checked nodes are unreachable", "remove destroyed nodes from config"} {
		if !strings.Contains(output, want) {
			t.Fatalf("recommendation = %q, want %q", output, want)
		}
	}
	if strings.Contains(output, "Run 'tako deploy --yes'") {
		t.Fatalf("recommendation = %q, should not suggest deploy before unreachable nodes are handled", output)
	}
}

func TestStateSyncRecommendationHandlesMissingLocalState(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	lines := stateSyncRecommendation(
		false,
		nil,
		stateHistoryCandidate{
			source:  "node-a",
			history: remoteHistory(base, remoteDeployment("deploy-new", base, "demo:v1")),
		},
		true,
		0,
	)

	output := strings.Join(lines, "\n")
	for _, want := range []string{"Local state is missing.", "Remote deployment history is available from node-a.", "Run 'tako state pull'"} {
		if !strings.Contains(output, want) {
			t.Fatalf("recommendation = %q, want %q", output, want)
		}
	}
}

func TestStateSyncRecommendationHandlesExistingLocalStateWithoutRemoteHistory(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	lines := stateSyncRecommendation(
		true,
		&localstate.DeploymentState{
			DeploymentID: "deploy-local",
			Timestamp:    base,
		},
		stateHistoryCandidate{},
		false,
		0,
	)

	output := strings.Join(lines, "\n")
	if !strings.Contains(output, "local deployment records are the best known copy") {
		t.Fatalf("recommendation = %q, want best-known local guidance", output)
	}
	if !strings.Contains(output, "Run 'tako deploy --yes'") {
		t.Fatalf("recommendation = %q, want deploy guidance when all checked nodes are reachable", output)
	}
	if strings.Contains(output, "Run 'tako state pull'") {
		t.Fatalf("recommendation = %q, should not suggest pull without remote history", output)
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

func TestStateStatusReachableCount(t *testing.T) {
	nodes := []stateStatusNode{
		{name: "node-a"},
		{name: "node-b", connectErr: errors.New("timeout")},
		{name: "node-c"},
	}
	if got := stateStatusReachableCount(nodes); got != 2 {
		t.Fatalf("stateStatusReachableCount = %d, want 2", got)
	}
}

func TestStateStatusUnreachableGuidanceNamesRecoveryPaths(t *testing.T) {
	lines := stateStatusUnreachableGuidance([]stateStatusNode{
		{name: "node-a"},
		{name: "node-b", connectErr: errors.New("timeout")},
	})
	output := strings.Join(lines, "\n")
	for _, want := range []string{
		"Unreachable node: node-b",
		"Destroyed node: remove node-b from tako.yaml",
		"tako state forget-node node-b --yes",
		"Rebuilt same-name node: keep node-b in tako.yaml",
		"tako setup --server node-b",
		"tako upgrade servers --server node-b",
		"tako state repair",
		"tako deploy --yes",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("guidance = %q, want %q", output, want)
		}
	}
}

func TestStateStatusUnreachableGuidanceHandlesMultipleNodes(t *testing.T) {
	lines := stateStatusUnreachableGuidance([]stateStatusNode{
		{name: "node-c", connectErr: errors.New("timeout")},
		{name: "node-a"},
		{name: "node-b", connectErr: errors.New("connection refused")},
	})
	output := strings.Join(lines, "\n")
	for _, want := range []string{
		"Unreachable nodes: node-b, node-c",
		"Destroyed nodes: remove them from tako.yaml",
		"tako state forget-node <node> --yes",
		"Rebuilt same-name nodes: keep them in tako.yaml",
		"tako setup --server <node>",
		"tako upgrade servers --server <node>",
		"tako state repair",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("guidance = %q, want %q", output, want)
		}
	}
}

func TestStateStatusUnreachableGuidanceEmptyWhenAllReachable(t *testing.T) {
	if lines := stateStatusUnreachableGuidance([]stateStatusNode{{name: "node-a"}}); len(lines) != 0 {
		t.Fatalf("guidance = %#v, want none", lines)
	}
}

func TestStateStatusNoReachableErrorIncludesFailClosedGuidance(t *testing.T) {
	err := stateStatusNoReachableError("production", []stateStatusNode{
		{name: "node-a", connectErr: errors.New("timeout")},
		{name: "node-b", connectErr: errors.New("connection refused")},
	})
	if err == nil {
		t.Fatal("stateStatusNoReachableError returned nil")
	}
	for _, want := range []string{"no reachable environment nodes", "deploy will fail closed", "node-a", "node-b"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
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
	if got := aggregate.Services["web"].RuntimeID; got != runtimeid.ServiceIdentity("demo", "production", "web") {
		t.Fatalf("web runtime id = %q, want expected runtime id", got)
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
				RuntimeID:  runtimeid.ServiceIdentity("demo", "production", "web"),
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
	if got := snapshot.Services["web"].RuntimeID; got != runtimeid.ServiceIdentity("demo", "production", "web") {
		t.Fatalf("web runtime id = %q, want expected runtime id", got)
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

func TestStateStatusCandidatesIgnoresEmbeddedNodeActualOutsideEnvironment(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	aggregate := actualSnapshot(base.Add(time.Hour), "web")
	aggregate.Nodes = map[string]takodstate.ActualNodeSnapshot{
		"node-a": {
			Node:       "node-a",
			Services:   nodeActualSnapshot("node-a", base, "web").Services,
			CapturedAt: base,
		},
		"removed-node": {
			Node:       "removed-node",
			Services:   nodeActualSnapshot("removed-node", base, "worker").Services,
			CapturedAt: base,
		},
	}

	_, _, _, nodeActual := stateStatusCandidates([]stateStatusNode{
		{
			name:     "node-a",
			envNodes: []string{"node-a", "node-b"},
			actual:   aggregate,
		},
	})

	if len(nodeActual) != 1 {
		t.Fatalf("node actual candidates = %d, want only configured embedded node", len(nodeActual))
	}
	if nodeActual[0].node != "node-a" {
		t.Fatalf("node actual candidate = %q, want node-a", nodeActual[0].node)
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

func TestBestStateStatusActualRebuildsAggregateFromNodeSnapshots(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	aggregate := actualSnapshot(base.Add(2*time.Hour), "web")
	aggregate.Services["web"] = takodstate.ActualService{
		Name:     "web",
		Image:    "demo:web",
		Replicas: 2,
	}

	best, ok, nodes := bestStateStatusActual("demo", "production", []stateActualCandidate{
		{source: "node-a", actual: aggregate},
	}, []stateNodeActualCandidate{
		{source: "node-a aggregate", node: "node-a", actual: nodeActualSnapshot("node-a", base, "web")},
	})

	if !ok {
		t.Fatal("bestStateStatusActual returned no aggregate")
	}
	if got := best.actual.Services["web"].Replicas; got != 1 {
		t.Fatalf("web replicas = %d, want filtered node actual count", got)
	}
	if len(nodes) != 1 || len(best.actual.Nodes) != 1 {
		t.Fatalf("node snapshots = %d embedded = %d, want 1", len(nodes), len(best.actual.Nodes))
	}
	if got := best.actual.TargetNodes; len(got) != 1 || got[0] != "node-a" {
		t.Fatalf("target nodes = %#v, want node-a only", got)
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
			Name:      service,
			Image:     "demo:" + service,
			Replicas:  1,
			RuntimeID: runtimeid.ServiceIdentity("demo", "production", service),
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
