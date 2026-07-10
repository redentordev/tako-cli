package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/reconcile"
)

func TestDeploymentFromHistoryFindsRequestedDeployment(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	history := &remotestate.DeploymentHistory{
		Deployments: []*remotestate.DeploymentState{
			{ID: "old", Timestamp: base},
			{ID: "target", Timestamp: base.Add(time.Hour)},
		},
	}

	deployment, err := deploymentFromHistory(history, "target")
	if err != nil {
		t.Fatalf("deploymentFromHistory returned error: %v", err)
	}
	if deployment.ID != "target" {
		t.Fatalf("deployment ID = %q, want target", deployment.ID)
	}
}

func TestDeploymentFromHistoryRejectsMissingDeployment(t *testing.T) {
	if _, err := deploymentFromHistory(&remotestate.DeploymentHistory{}, "missing"); err == nil {
		t.Fatal("deploymentFromHistory should reject missing deployment")
	}
}

func TestPreviousStableServiceDeploymentFromHistorySkipsFailedCurrent(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	history := &remotestate.DeploymentHistory{
		Deployments: []*remotestate.DeploymentState{
			rollbackHistoryDeployment("failed-newest", remotestate.StatusFailed, base.Add(2*time.Hour), "web"),
			rollbackHistoryDeployment("success-newer", remotestate.StatusSuccess, base.Add(time.Hour), "web"),
			rollbackHistoryDeployment("success-older", remotestate.StatusSuccess, base, "web"),
		},
	}

	deployment, err := previousStableServiceDeploymentFromHistory(history, "web")
	if err != nil {
		t.Fatalf("previousStableServiceDeploymentFromHistory returned error: %v", err)
	}
	if deployment.ID != "success-newer" {
		t.Fatalf("deployment ID = %q, want success-newer", deployment.ID)
	}
}

func TestPreviousStableServiceDeploymentFromHistorySelectsPreviousWhenCurrentIsStable(t *testing.T) {
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	history := &remotestate.DeploymentHistory{
		Deployments: []*remotestate.DeploymentState{
			rollbackHistoryDeployment("current", remotestate.StatusSuccess, base.Add(2*time.Hour), "web"),
			rollbackHistoryDeployment("previous-api", remotestate.StatusSuccess, base.Add(time.Hour), "api"),
			rollbackHistoryDeployment("previous-web", remotestate.StatusSuccess, base, "web"),
		},
	}

	deployment, err := previousStableServiceDeploymentFromHistory(history, "web")
	if err != nil {
		t.Fatalf("previousStableServiceDeploymentFromHistory returned error: %v", err)
	}
	if deployment.ID != "previous-web" {
		t.Fatalf("deployment ID = %q, want previous-web", deployment.ID)
	}
}

func TestPreviousStableServiceDeploymentFromHistoryRequiresPreviousStable(t *testing.T) {
	history := &remotestate.DeploymentHistory{
		Deployments: []*remotestate.DeploymentState{
			rollbackHistoryDeployment("only-current", remotestate.StatusSuccess, time.Now().UTC(), "web"),
		},
	}

	if _, err := previousStableServiceDeploymentFromHistory(history, "web"); err == nil {
		t.Fatal("previousStableServiceDeploymentFromHistory should reject history without a previous stable deployment")
	}
}

func TestSelectRollbackTargetRejectsFailedSpecificDeployment(t *testing.T) {
	history := &remotestate.DeploymentHistory{
		Deployments: []*remotestate.DeploymentState{
			rollbackHistoryDeployment("failed", remotestate.StatusFailed, time.Now().UTC(), "web"),
		},
	}

	_, err := selectRollbackTargetFromHistory(history, "failed", "web")
	if err == nil {
		t.Fatal("selectRollbackTargetFromHistory should reject failed specific targets")
	}
	if !strings.Contains(err.Error(), "not a stable rollback target") {
		t.Fatalf("error = %q, want stable target context", err)
	}
}

func TestRollbackRemoteHistoryErrorFailsSuccessfulRuntimeMutation(t *testing.T) {
	err := rollbackRemoteHistoryError(errors.New("disk full"))
	if err == nil {
		t.Fatal("rollbackRemoteHistoryError returned nil")
	}
	for _, want := range []string{"rollback succeeded", "failed to update remote deployment history", "disk full"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}

func TestRollbackCommandDoesNotExposeServerFlag(t *testing.T) {
	if flag := rollbackCmd.Flags().Lookup("server"); flag != nil {
		t.Fatal("rollback command should not expose a server flag")
	}
}

func TestBuildRollbackDeploymentDoesNotMutateTarget(t *testing.T) {
	start := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	target := rollbackHistoryDeployment("target", remotestate.StatusSuccess, start.Add(-time.Hour), "web")
	target.Version = "1.2.3"
	target.GitCommit = "abcdef"
	serviceState := target.Services["web"]

	rollback := buildRollbackDeployment(testRollbackConfig(), "production", "node-a", start, 3*time.Second, target, "web", serviceState)

	if target.Status != remotestate.StatusSuccess {
		t.Fatalf("target status = %q, want preserved success", target.Status)
	}
	if rollback.Status != remotestate.StatusRolledBack {
		t.Fatalf("rollback status = %q, want rolled_back", rollback.Status)
	}
	if rollback.Services["web"].Image != "web:target" {
		t.Fatalf("rollback image = %q, want web:target", rollback.Services["web"].Image)
	}
	if rollback.GitCommit != "abcdef" {
		t.Fatalf("rollback git commit = %q, want abcdef", rollback.GitCommit)
	}
}

func TestRollbackProxyInputsUseRollbackRevisionAndPreserveOtherActiveRevisions(t *testing.T) {
	cfg := testRollbackConfig()
	cfg.Project.Name = "demo"
	services := map[string]config.ServiceConfig{
		"web": {
			Build:    ".",
			Port:     3000,
			Replicas: 2,
			Deploy: config.DeployConfig{
				Strategy: config.DeployStrategyRolling,
			},
		},
		"api": {
			Image: "api:stable",
			Port:  4000,
			Deploy: config.DeployConfig{
				Strategy: config.DeployStrategyRolling,
			},
		},
		"worker": {
			Image: "worker:stable",
		},
	}
	serviceState := remotestate.ServiceState{
		Name:     "web",
		Image:    "web:target",
		Port:     8080,
		Replicas: 1,
	}
	actualState := map[string]*reconcile.ActualService{
		"web": {CurrentRevision: "rev-web-current"},
		"api": {CurrentRevision: "rev-api-current"},
	}

	desiredServices, imageRefs, activeRevisions := rollbackProxyInputs(cfg, "production", services, "web", serviceState, actualState)
	rollbackConfig := desiredServices["web"]
	if rollbackConfig.Image != "web:target" || rollbackConfig.Port != 8080 || rollbackConfig.Replicas != 1 {
		t.Fatalf("rollback service config = %#v, want target image/port/replicas", rollbackConfig)
	}
	if imageRefs["web"] != "web:target" {
		t.Fatalf("image ref = %q, want rollback image", imageRefs["web"])
	}
	wantWebRevision := deployer.ServiceRevisionID(cfg.Project.Name, "production", "web", "web:target", rollbackConfig)
	if activeRevisions["web"] != wantWebRevision {
		t.Fatalf("web active revision = %q, want rollback revision %q", activeRevisions["web"], wantWebRevision)
	}
	if activeRevisions["api"] != "rev-api-current" {
		t.Fatalf("api active revision = %q, want existing revision", activeRevisions["api"])
	}
	if _, ok := activeRevisions["worker"]; ok {
		t.Fatalf("recreate worker should not get an active revision: %#v", activeRevisions)
	}
}

func TestRollbackProxyInputsUseHistoricalSharedBuildIdentity(t *testing.T) {
	cfg := testRollbackConfig()
	cfg.Project.Name = "demo"
	current := config.ServiceConfig{ImageFrom: "application", SharedBuildHash: "current", Port: 3000, Replicas: 1, Deploy: config.DeployConfig{Strategy: config.DeployStrategyRolling}}
	services := map[string]config.ServiceConfig{"web": current}
	historical := remotestate.ServiceState{Name: "web", Image: "demo/shared/application:old", SharedBuild: "application", SharedBuildHash: "historical", Port: 3000, Replicas: 2}
	desired, refs, active := rollbackProxyInputs(cfg, "production", services, "web", historical, map[string]*reconcile.ActualService{"web": {CurrentRevision: "current"}})
	rollback := desired["web"]
	if rollback.Image != "" || rollback.ImageFrom != "application" || rollback.SharedBuildHash != "historical" || rollback.Build != "" || refs["web"] != historical.Image {
		t.Fatalf("rollback shared config/refs = %#v %#v", rollback, refs)
	}
	want := deployer.ServiceRevisionID("demo", "production", "web", historical.Image, rollback)
	if active["web"] != want {
		t.Fatalf("active revision = %q, want %q", active["web"], want)
	}
}

func TestRollbackNeedsTargetWorktreeOnlyForBuildBackedGitTargets(t *testing.T) {
	target := &remotestate.DeploymentState{GitCommit: "abcdef1234567890"}
	if !rollbackNeedsTargetWorktree(config.ServiceConfig{Build: "."}, target) {
		t.Fatal("build-backed rollback with a git commit should use a target worktree")
	}
	if rollbackNeedsTargetWorktree(config.ServiceConfig{Image: "nginx:1.27"}, target) {
		t.Fatal("image-backed rollback should not use a target worktree")
	}
	if rollbackNeedsTargetWorktree(config.ServiceConfig{Build: " \n\t "}, target) {
		t.Fatal("whitespace-only build should not use a target worktree")
	}
	if rollbackNeedsTargetWorktree(config.ServiceConfig{Build: "."}, &remotestate.DeploymentState{}) {
		t.Fatal("rollback without a target commit should not use a target worktree")
	}
}

func TestRollbackTargetCommitFallsBackToShortCommit(t *testing.T) {
	if got := rollbackTargetCommit(&remotestate.DeploymentState{GitCommit: " full ", GitCommitShort: "short"}); got != "full" {
		t.Fatalf("rollbackTargetCommit() = %q, want full", got)
	}
	if got := rollbackTargetCommit(&remotestate.DeploymentState{GitCommitShort: " short "}); got != "short" {
		t.Fatalf("rollbackTargetCommit() = %q, want short", got)
	}
	if got := rollbackTargetCommit(&remotestate.DeploymentState{GitCommit: " \n\t ", GitCommitShort: "  "}); got != "" {
		t.Fatalf("rollbackTargetCommit() = %q, want empty", got)
	}
	if got := rollbackTargetCommit(nil); got != "" {
		t.Fatalf("rollbackTargetCommit(nil) = %q, want empty", got)
	}
}

func TestWithWorkingDirectoryRestoresOriginalDirectory(t *testing.T) {
	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd returned error: %v", err)
	}
	tempDir := t.TempDir()

	err = withWorkingDirectory(tempDir, func() error {
		current, err := os.Getwd()
		if err != nil {
			return err
		}
		if canonicalPath(t, current) != canonicalPath(t, tempDir) {
			t.Fatalf("working directory = %q, want %q", current, tempDir)
		}
		return errors.New("stop")
	})
	if err == nil || err.Error() != "stop" {
		t.Fatalf("withWorkingDirectory error = %v, want stop", err)
	}
	current, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd after helper returned error: %v", err)
	}
	if canonicalPath(t, current) != canonicalPath(t, original) {
		t.Fatalf("working directory after helper = %q, want %q", current, original)
	}
}

func canonicalPath(t *testing.T, path string) string {
	t.Helper()
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("Abs(%q) returned error: %v", path, err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return resolved
	}
	return absolute
}

func rollbackHistoryDeployment(id string, status remotestate.DeploymentStatus, timestamp time.Time, services ...string) *remotestate.DeploymentState {
	serviceStates := make(map[string]remotestate.ServiceState, len(services))
	for _, serviceName := range services {
		serviceStates[serviceName] = remotestate.ServiceState{
			Name:     serviceName,
			Image:    serviceName + ":" + id,
			Port:     3000,
			Replicas: 1,
		}
	}
	return &remotestate.DeploymentState{
		ID:        id,
		Status:    status,
		Timestamp: timestamp,
		Services:  serviceStates,
	}
}

func testRollbackConfig() *config.Config {
	return &config.Config{
		Project: config.ProjectConfig{
			Name:    "demo",
			Version: "1.0.0",
		},
		Runtime: &config.RuntimeConfig{Mode: config.RuntimeModeTakod},
	}
}
