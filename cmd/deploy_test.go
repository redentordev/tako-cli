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
	"github.com/redentordev/tako-cli/pkg/git"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	localstate "github.com/redentordev/tako-cli/pkg/state"
)

func TestIsNonInteractiveAcceptsTruthyEnvValues(t *testing.T) {
	tests := []struct {
		name  string
		env   string
		value string
	}{
		{name: "tako one", env: "TAKO_NONINTERACTIVE", value: "1"},
		{name: "tako true", env: "TAKO_NONINTERACTIVE", value: "true"},
		{name: "ci true uppercase", env: "CI", value: "TRUE"},
		{name: "ci yes", env: "CI", value: "yes"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TAKO_NONINTERACTIVE", "")
			t.Setenv("CI", "")
			t.Setenv(tt.env, tt.value)

			if !isNonInteractive() {
				t.Fatalf("isNonInteractive() = false with %s=%q", tt.env, tt.value)
			}
		})
	}
}

func TestIsNonInteractiveRejectsFalseyEnvValues(t *testing.T) {
	t.Setenv("TAKO_NONINTERACTIVE", "0")
	t.Setenv("CI", "false")

	if isNonInteractive() {
		t.Fatal("isNonInteractive() should reject falsey values")
	}
}

func TestRequireDeployPromptAllowedRejectsNonInteractiveWithoutYes(t *testing.T) {
	t.Setenv("TAKO_NONINTERACTIVE", "true")
	t.Setenv("CI", "")

	err := requireDeployPromptAllowed("deployment plan includes destructive changes")
	if err == nil {
		t.Fatal("requireDeployPromptAllowed() error = nil, want non-interactive approval error")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("error = %q, want --yes guidance", err)
	}
}

func TestRequireDeployPromptAllowedRejectsNonTerminalWithoutYes(t *testing.T) {
	t.Setenv("TAKO_NONINTERACTIVE", "")
	t.Setenv("CI", "")

	err := requireDeployPromptAllowed("deployment plan includes destructive changes")
	if err == nil {
		t.Fatal("requireDeployPromptAllowed() error = nil, want terminal requirement error")
	}
	if !strings.Contains(err.Error(), "terminal or --yes") {
		t.Fatalf("error = %q, want terminal/--yes guidance", err)
	}
}

func TestDeployCommandSilencesUsageOnRunErrors(t *testing.T) {
	if !deployCmd.SilenceUsage {
		t.Fatal("deploy command should silence usage on execution errors")
	}
}

func TestRunDeployFailsInvalidYAMLBeforeGit(t *testing.T) {
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	root := t.TempDir()
	t.Cleanup(func() {
		_ = os.Chdir(oldDir)
	})
	if err := os.Chdir(root); err != nil {
		t.Fatalf("failed to switch cwd: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "tako.yaml"), []byte("project:\n  name: demo\n  version: [\n"), 0600); err != nil {
		t.Fatalf("failed to write invalid config: %v", err)
	}
	oldCfgFile := cfgFile
	cfgFile = ""
	t.Cleanup(func() {
		cfgFile = oldCfgFile
	})

	err = runDeploy(deployCmd, nil)
	if err == nil {
		t.Fatal("runDeploy should fail on invalid YAML")
	}
	for _, want := range []string{"YAML syntax error in tako.yaml", "line 3"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "Git repository") {
		t.Fatalf("deploy should fail before git checks, got %q", err)
	}
}

func TestFormatDeployConfigErrorReportsValidationFailures(t *testing.T) {
	err := formatDeployConfigError("tako.yaml", errors.New(`invalid config: service web: invalid load balancer strategy "ip_hash"; supported strategies are round_robin and sticky`))
	for _, want := range []string{"config validation failed in tako.yaml", "invalid load balancer strategy", "round_robin and sticky"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}

func TestIsAffirmative(t *testing.T) {
	tests := []struct {
		response string
		want     bool
	}{
		{response: "y", want: true},
		{response: "Y\n", want: true},
		{response: "yes", want: true},
		{response: "YES\n", want: true},
		{response: "", want: false},
		{response: "no", want: false},
	}

	for _, tt := range tests {
		if got := isAffirmative(tt.response); got != tt.want {
			t.Fatalf("isAffirmative(%q) = %v, want %v", tt.response, got, tt.want)
		}
	}
}

func TestDeployActualStateErrorRefusesUnknownRunningServices(t *testing.T) {
	err := deployActualStateError(errors.New("node-a: takod unavailable"))
	if err == nil {
		t.Fatal("deployActualStateError returned nil")
	}
	for _, want := range []string{"refusing to plan", "unknown running services", "takod unavailable"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}

func TestDeployRemoteHistoryErrorFailsSuccessfulRuntimeMutation(t *testing.T) {
	err := deployRemoteHistoryError(errors.New("disk full"))
	if err == nil {
		t.Fatal("deployRemoteHistoryError returned nil")
	}
	for _, want := range []string{"deployment succeeded", "failed to save remote deployment history", "disk full"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}

func TestResolveDeployCommitInfoRejectsDirtyWorktree(t *testing.T) {
	reader := fakeDeployGitReader{
		repository: true,
		dirty:      true,
		status:     " M main.go\n?? new.txt\n",
	}

	_, _, err := resolveDeployCommitInfo(reader, false)
	if err == nil {
		t.Fatal("resolveDeployCommitInfo should reject dirty worktrees")
	}
	for _, want := range []string{"cannot deploy with uncommitted changes", "commit, stash, or discard", "M main.go", "?? new.txt"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}

func TestResolveDeployCommitInfoReturnsCleanCommitInfo(t *testing.T) {
	want := &git.CommitInfo{
		Hash:      "abcdef",
		ShortHash: "abc",
		Branch:    "main",
		Message:   "deploy me",
		Author:    "redentor",
	}
	reader := fakeDeployGitReader{
		repository: true,
		commitInfo: want,
	}

	got, dirtyStatus, err := resolveDeployCommitInfo(reader, false)
	if err != nil {
		t.Fatalf("resolveDeployCommitInfo returned error: %v", err)
	}
	if got != want {
		t.Fatalf("commitInfo = %#v, want %#v", got, want)
	}
	if dirtyStatus != "" {
		t.Fatalf("dirtyStatus = %q, want empty", dirtyStatus)
	}
}

func TestResolveDeployCommitInfoRequiresGitRepository(t *testing.T) {
	_, _, err := resolveDeployCommitInfo(fakeDeployGitReader{}, false)
	if err == nil {
		t.Fatal("resolveDeployCommitInfo should reject non-git repositories")
	}
	if !strings.Contains(err.Error(), "not a Git repository") {
		t.Fatalf("error = %q, want git repository guidance", err)
	}
}

func TestResolveDeployCommitInfoAllowsDirtyWorktreeWhenExplicit(t *testing.T) {
	want := &git.CommitInfo{
		Hash:      "abcdef",
		ShortHash: "abc",
		Branch:    "feature",
		Message:   "deploy test",
		Author:    "redentor",
	}
	reader := fakeDeployGitReader{
		repository: true,
		dirty:      true,
		status:     " M .dockerignore\n",
		commitInfo: want,
	}

	got, dirtyStatus, err := resolveDeployCommitInfo(reader, true)
	if err != nil {
		t.Fatalf("resolveDeployCommitInfo returned error: %v", err)
	}
	if got != want {
		t.Fatalf("commitInfo = %#v, want %#v", got, want)
	}
	if dirtyStatus != "M .dockerignore" {
		t.Fatalf("dirtyStatus = %q, want dirty file list", dirtyStatus)
	}
}

func TestRecordFailedDeploymentStatePersistsRemoteAndLocalFailure(t *testing.T) {
	start := time.Now().Add(-2 * time.Second)
	remoteSaver := &fakeRemoteDeploymentSaver{}
	localSaver := &fakeLocalDeploymentSaver{}
	deployment := &remotestate.DeploymentState{
		Timestamp: start,
		Services:  map[string]remotestate.ServiceState{},
	}
	cfg := &config.Config{
		Runtime: &config.RuntimeConfig{Mode: config.RuntimeModeTakod},
	}
	commit := &git.CommitInfo{Hash: "abcdef"}
	deployErr := errors.New("web failed")

	err := recordFailedDeploymentState(remoteSaver, localSaver, deployment, cfg, "production", []string{"node-a", "node-b"}, commit, start, deployErr)
	if err != nil {
		t.Fatalf("recordFailedDeploymentState returned error: %v", err)
	}
	if remoteSaver.saved == nil {
		t.Fatal("remote deployment was not saved")
	}
	if remoteSaver.saved.Status != remotestate.StatusFailed {
		t.Fatalf("remote status = %q, want failed", remoteSaver.saved.Status)
	}
	if remoteSaver.saved.Error != "web failed" {
		t.Fatalf("remote error = %q, want deployment error", remoteSaver.saved.Error)
	}
	if remoteSaver.saved.Duration <= 0 {
		t.Fatalf("remote duration = %s, want positive duration", remoteSaver.saved.Duration)
	}
	if localSaver.saved == nil {
		t.Fatal("local deployment was not saved")
	}
	if localSaver.saved.Status != "failed" {
		t.Fatalf("local status = %q, want failed", localSaver.saved.Status)
	}
	if localSaver.saved.GitCommit != "abcdef" {
		t.Fatalf("local git commit = %q, want abcdef", localSaver.saved.GitCommit)
	}
	if got := strings.Join(localSaver.saved.Servers, ","); got != "node-a,node-b" {
		t.Fatalf("local servers = %q, want node-a,node-b", got)
	}
}

func TestRecordFailedDeploymentStateReturnsRemoteSaveError(t *testing.T) {
	remoteSaver := &fakeRemoteDeploymentSaver{err: errors.New("disk full")}
	deployment := &remotestate.DeploymentState{
		Timestamp: time.Now(),
		Services:  map[string]remotestate.ServiceState{},
	}
	cfg := &config.Config{Runtime: &config.RuntimeConfig{Mode: config.RuntimeModeTakod}}

	err := recordFailedDeploymentState(remoteSaver, nil, deployment, cfg, "production", []string{"node-a"}, nil, time.Now(), errors.New("deploy failed"))
	if err == nil {
		t.Fatal("recordFailedDeploymentState should return remote save errors")
	}
	if !strings.Contains(err.Error(), "failed to save failed remote deployment state") || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("error = %q, want remote save context", err)
	}
}

func TestFilterActualStateForServicesScopesTargetedDeployPlan(t *testing.T) {
	webActual := &reconcile.ActualService{Name: "web", Image: "demo/web:old", Replicas: 1}
	actualState := map[string]*reconcile.ActualService{
		"web": webActual,
		"api": {Name: "api", Image: "demo/api:old", Replicas: 1},
	}
	services := map[string]config.ServiceConfig{
		"web": {Build: "."},
	}

	got := filterActualStateForServices(actualState, services)
	if len(got) != 1 {
		t.Fatalf("filtered actual services = %d, want 1", len(got))
	}
	if got["web"] != webActual {
		t.Fatalf("filtered web actual = %#v, want original web actual", got["web"])
	}
	if _, ok := got["api"]; ok {
		t.Fatal("filtered actual state included unselected api service")
	}
}

func TestMergeRuntimeImageRefsPreservesNonDeployedActualImages(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "demo", Version: "1.2.3"},
	}
	services := map[string]config.ServiceConfig{
		"web":    {Build: "."},
		"api":    {Build: "./api"},
		"cache":  {Image: "redis:7"},
		"worker": {Build: "./worker"},
	}
	deployedImageRefs := map[string]string{
		"web": "demo/web:built",
	}
	actualState := map[string]*reconcile.ActualService{
		"api":   {Name: "api", Image: "demo/api:old", Replicas: 1},
		"cache": {Name: "cache", Image: "redis:6", Replicas: 1},
	}

	got := mergeRuntimeImageRefs(cfg, "production", services, deployedImageRefs, actualState)
	want := map[string]string{
		"web":    "demo/web:built",
		"api":    "demo/api:old",
		"cache":  "redis:6",
		"worker": "demo/worker:1.2.3-production",
	}
	for serviceName, wantImage := range want {
		if got[serviceName] != wantImage {
			t.Fatalf("image ref for %s = %q, want %q", serviceName, got[serviceName], wantImage)
		}
	}
}

func TestApplyDeployRemovalsCallsRemoveChangesOnly(t *testing.T) {
	remover := &fakeDeployServiceRemover{}
	plan := &reconcile.ReconciliationPlan{
		Changes: []reconcile.ServiceChange{
			{Type: reconcile.ChangeNone, ServiceName: "web"},
			{Type: reconcile.ChangeRemove, ServiceName: "old-api"},
			{Type: reconcile.ChangeUpdate, ServiceName: "worker"},
			{Type: reconcile.ChangeRemove, ServiceName: "old-worker"},
		},
	}

	if err := applyDeployRemovals(remover, plan); err != nil {
		t.Fatalf("applyDeployRemovals returned error: %v", err)
	}
	if got := strings.Join(remover.removed, ","); got != "old-api,old-worker" {
		t.Fatalf("removed services = %q, want old-api,old-worker", got)
	}
}

func TestApplyDeployRemovalsReturnsServiceContext(t *testing.T) {
	remover := &fakeDeployServiceRemover{err: errors.New("node failed")}
	plan := &reconcile.ReconciliationPlan{
		Changes: []reconcile.ServiceChange{
			{Type: reconcile.ChangeRemove, ServiceName: "old-api"},
		},
	}

	err := applyDeployRemovals(remover, plan)
	if err == nil {
		t.Fatal("applyDeployRemovals returned nil, want error")
	}
	if !strings.Contains(err.Error(), "old-api") || !strings.Contains(err.Error(), "node failed") {
		t.Fatalf("error = %q, want service and cause", err)
	}
}

func TestHasBuildServices(t *testing.T) {
	if hasBuildServices(map[string]config.ServiceConfig{
		"web": {Build: "."},
	}) != true {
		t.Fatal("hasBuildServices should detect build-backed services")
	}
	if hasBuildServices(map[string]config.ServiceConfig{
		"db": {Image: "postgres:16"},
	}) != false {
		t.Fatal("hasBuildServices should ignore image-only services")
	}
	if hasBuildServices(nil) {
		t.Fatal("hasBuildServices should reject empty service maps")
	}
}

func TestServicesToDeployForEmptyPlanIncludesOnlyBuildServices(t *testing.T) {
	services := map[string]config.ServiceConfig{
		"web": {Build: "."},
		"db":  {Image: "postgres:16"},
	}
	plan := &reconcile.ReconciliationPlan{}

	got := servicesToDeployForPlan(plan, services)
	if len(got) != 1 {
		t.Fatalf("servicesToDeployForPlan returned %d service(s), want 1: %#v", len(got), got)
	}
	if _, ok := got["web"]; !ok {
		t.Fatalf("build service missing from deploy set: %#v", got)
	}
	if _, ok := got["db"]; ok {
		t.Fatalf("image-only service should not be redeployed on empty plan: %#v", got)
	}
}

func TestServicesToDeployForPlanIncludesAddsAndUpdatesOnly(t *testing.T) {
	services := map[string]config.ServiceConfig{
		"web":    {Build: "."},
		"worker": {Image: "worker:1"},
		"old":    {Image: "old:1"},
	}
	plan := &reconcile.ReconciliationPlan{
		Summary: reconcile.ReconciliationSummary{Total: 4, Adds: 1, Updates: 1, Removes: 1, NoOps: 1},
		Changes: []reconcile.ServiceChange{
			{Type: reconcile.ChangeUpdate, ServiceName: "web"},
			{Type: reconcile.ChangeAdd, ServiceName: "worker"},
			{Type: reconcile.ChangeRemove, ServiceName: "old"},
			{Type: reconcile.ChangeNone, ServiceName: "noop"},
		},
	}

	got := servicesToDeployForPlan(plan, services)
	if len(got) != 2 {
		t.Fatalf("servicesToDeployForPlan returned %d service(s), want 2: %#v", len(got), got)
	}
	for _, want := range []string{"web", "worker"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("%s missing from deploy set: %#v", want, got)
		}
	}
	if _, ok := got["old"]; ok {
		t.Fatalf("removed service should not be in deploy set: %#v", got)
	}
}

type fakeDeployGitReader struct {
	repository bool
	dirty      bool
	status     string
	commitInfo *git.CommitInfo
}

func (f fakeDeployGitReader) IsRepository() bool {
	return f.repository
}

func (f fakeDeployGitReader) HasUncommittedChanges() (bool, error) {
	return f.dirty, nil
}

func (f fakeDeployGitReader) GetStatus() (string, error) {
	return f.status, nil
}

func (f fakeDeployGitReader) GetCommitInfo(_ string) (*git.CommitInfo, error) {
	if f.commitInfo == nil {
		return nil, errors.New("missing commit")
	}
	return f.commitInfo, nil
}

type fakeRemoteDeploymentSaver struct {
	saved *remotestate.DeploymentState
	err   error
}

func (f *fakeRemoteDeploymentSaver) SaveDeployment(deployment *remotestate.DeploymentState) error {
	if f.err != nil {
		return f.err
	}
	f.saved = deployment
	return nil
}

type fakeLocalDeploymentSaver struct {
	saved *localstate.DeploymentState
	err   error
}

func (f *fakeLocalDeploymentSaver) SaveDeployment(deployment *localstate.DeploymentState) error {
	if f.err != nil {
		return f.err
	}
	f.saved = deployment
	return nil
}

type fakeDeployServiceRemover struct {
	removed []string
	err     error
}

func (f *fakeDeployServiceRemover) RemoveServiceTakod(serviceName string) error {
	if f.err != nil {
		return f.err
	}
	f.removed = append(f.removed, serviceName)
	return nil
}
