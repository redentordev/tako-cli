package takodstate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

func TestBuildDesiredRevisionSanitizesServiceState(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "demo"},
	}
	services := map[string]config.ServiceConfig{
		"web": {
			Build:       ".",
			BuildArgs:   map[string]string{"SENTRY_IMAGE": "getsentry/sentry:26.6.0", "TOKEN": "build-secret"},
			BuildTarget: "runtime",
			Port:        3000,
			Replicas:    0,
			Env: map[string]string{
				"DATABASE_URL": "postgres://user:password@example/db",
				"TOKEN":        "top-secret-token",
			},
			EnvFiles:        []string{".env.base", ".env.production"},
			User:            "1000:1000",
			WorkingDir:      "/work",
			StopGracePeriod: "60s",
			Init:            true,
			ExtraHosts:      []string{"database:10.0.0.2"},
			Ulimits:         map[string]config.UlimitConfig{"nofile": {Soft: 262144, Hard: 262144}},
			ShmSize:         "256m",
			Secrets:         []string{"TOKEN:prod/token", "DATABASE_URL"},
			Volumes:         []string{"data:/data"},
			Proxy:           &config.ProxyConfig{Domain: "example.com"},
			Deploy:          config.DeployConfig{Strategy: "recreate"},
			DependsOn:       []string{"db"},
		},
	}

	revision, err := BuildDesiredRevision(
		cfg,
		"production",
		"deploy",
		services,
		map[string]string{"web": "registry.example.com/demo/web:abc123"},
		[]string{"node-b", "node-a"},
		GitInfo{CommitShort: "abc1234"},
	)
	if err != nil {
		t.Fatalf("BuildDesiredRevision returned error: %v", err)
	}

	service := revision.Services["web"]
	if service.Replicas != 0 {
		t.Fatalf("expected scale-to-zero to be persisted as 0 replicas, got %d", service.Replicas)
	}
	if !slices.Equal(service.EnvKeys, []string{"DATABASE_URL", "TOKEN"}) {
		t.Fatalf("unexpected env keys: %#v", service.EnvKeys)
	}
	if !slices.Equal(service.SecretRefs, []string{"DATABASE_URL", "TOKEN:prod/token"}) {
		t.Fatalf("unexpected secret refs: %#v", service.SecretRefs)
	}
	if !service.EnvFile {
		t.Fatal("expected env file presence to be recorded")
	}
	if !slices.Equal(service.BuildArgKeys, []string{"SENTRY_IMAGE", "TOKEN"}) || service.BuildTarget != "runtime" {
		t.Fatalf("safe build state = %#v / %q", service.BuildArgKeys, service.BuildTarget)
	}
	if service.User != "1000:1000" || service.WorkingDir != "/work" || !service.Init || service.ShmSize != "256m" {
		t.Fatalf("runtime controls not persisted: %#v", service)
	}
	if !slices.Equal(revision.TargetNodes, []string{"node-a", "node-b"}) {
		t.Fatalf("target nodes were not sorted: %#v", revision.TargetNodes)
	}

	data, err := json.Marshal(revision)
	if err != nil {
		t.Fatalf("failed to marshal revision: %v", err)
	}
	serialized := string(data)
	for _, secretValue := range []string{"postgres://user:password@example/db", "top-secret-token", "build-secret"} {
		if strings.Contains(serialized, secretValue) {
			t.Fatalf("desired revision leaked raw env value %q: %s", secretValue, serialized)
		}
	}
}

func TestDesiredServiceCommandStateIsAdditiveAndLegacyCompatible(t *testing.T) {
	legacy := sanitizeDesiredService("worker", config.ServiceConfig{Command: config.StringValue("echo legacy")}, "busybox:latest")
	legacyJSON, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("json.Marshal legacy state: %v", err)
	}
	if !strings.Contains(string(legacyJSON), `"command":"echo legacy"`) || strings.Contains(string(legacyJSON), "commandArgs") {
		t.Fatalf("legacy desired state changed shape: %s", legacyJSON)
	}

	execForm := sanitizeDesiredService("worker", config.ServiceConfig{Command: config.ListValue("echo", "raw")}, "busybox:latest")
	execJSON, err := json.Marshal(execForm)
	if err != nil {
		t.Fatalf("json.Marshal exec state: %v", err)
	}
	if strings.Contains(string(execJSON), `"command":`) || !strings.Contains(string(execJSON), `"commandArgs":["echo","raw"]`) {
		t.Fatalf("exec desired state is not additive: %s", execJSON)
	}
}

func TestBuildDesiredRevisionNormalizesNegativeReplicaCount(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "demo"},
	}
	services := map[string]config.ServiceConfig{
		"worker": {
			Image:    "busybox:1.36.1",
			Replicas: -2,
		},
	}

	revision, err := BuildDesiredRevision(cfg, "production", "deploy", services, nil, nil, GitInfo{})
	if err != nil {
		t.Fatalf("BuildDesiredRevision returned error: %v", err)
	}

	if got := revision.Services["worker"].Replicas; got != 1 {
		t.Fatalf("expected negative replicas to normalize to 1, got %d", got)
	}
}

func TestBuildActualSnapshotWithNodesEmbedsNodeSnapshots(t *testing.T) {
	nodeActual := map[string]map[string]*reconcile.ActualService{
		"node-b": {
			"web": {
				Name:              "web",
				Image:             "demo/web:1",
				Replicas:          1,
				Containers:        []string{"b2", "b1"},
				ConfigHash:        "hash-web",
				RuntimeID:         runtimeid.ServiceIdentity("demo", "production", "web"),
				CurrentRevision:   "rev-web",
				DeployStrategy:    "recreate",
				ActiveContainers:  []string{"b2", "b1"},
				WarmingContainers: []string{"b-warm"},
			},
		},
		"node-a": {
			"web": {
				Name:       "web",
				Image:      "demo/web:1",
				Replicas:   1,
				Containers: []string{"a1"},
				ConfigHash: "hash-web",
				RuntimeID:  runtimeid.ServiceIdentity("demo", "production", "web"),
			},
			"worker": {
				Name:       "worker",
				Image:      "demo/worker:1",
				Replicas:   1,
				Containers: []string{"w1"},
			},
		},
	}
	aggregate := reconcile.AggregateActualStateByServer(nodeActual)

	snapshot := BuildActualSnapshotWithNodes("demo", "production", []string{"node-b", "node-a"}, aggregate, nodeActual)

	if !slices.Equal(snapshot.TargetNodes, []string{"node-a", "node-b"}) {
		t.Fatalf("target nodes were not sorted: %#v", snapshot.TargetNodes)
	}
	if got := snapshot.Services["web"].Replicas; got != 2 {
		t.Fatalf("aggregate web replicas = %d, want 2", got)
	}
	if !slices.Equal(snapshot.Nodes["node-b"].Services["web"].Containers, []string{"b1", "b2"}) {
		t.Fatalf("node containers were not sorted: %#v", snapshot.Nodes["node-b"].Services["web"].Containers)
	}
	if got := snapshot.Services["web"].ConfigHash; got != "hash-web" {
		t.Fatalf("aggregate config hash = %q, want hash-web", got)
	}
	if got := snapshot.Services["web"].RuntimeID; got != runtimeid.ServiceIdentity("demo", "production", "web") {
		t.Fatalf("aggregate runtime id = %q, want expected runtime id", got)
	}
	if got := snapshot.Services["web"].CurrentRevision; got != "rev-web" {
		t.Fatalf("aggregate revision = %q, want rev-web", got)
	}
	if !slices.Equal(snapshot.Nodes["node-b"].Services["web"].ActiveContainers, []string{"b1", "b2"}) {
		t.Fatalf("active containers were not sorted: %#v", snapshot.Nodes["node-b"].Services["web"].ActiveContainers)
	}
	if !slices.Equal(snapshot.Nodes["node-b"].Services["web"].WarmingContainers, []string{"b-warm"}) {
		t.Fatalf("warming containers = %#v", snapshot.Nodes["node-b"].Services["web"].WarmingContainers)
	}
	if snapshot.Nodes["node-a"].CapturedAt.IsZero() || snapshot.Nodes["node-b"].CapturedAt.IsZero() {
		t.Fatalf("expected embedded node snapshots to have capture times: %#v", snapshot.Nodes)
	}
}

func TestBuildNodeActualSnapshotRecordsNode(t *testing.T) {
	snapshot := BuildNodeActualSnapshot("demo", "production", "node-a", map[string]*reconcile.ActualService{
		"web": {
			Name:       "web",
			Image:      "demo/web:1",
			Replicas:   1,
			Containers: []string{"c2", "c1"},
			ConfigHash: "hash-web",
			RuntimeID:  runtimeid.ServiceIdentity("demo", "production", "web"),
		},
	})

	if snapshot.Node != "node-a" {
		t.Fatalf("node = %q, want node-a", snapshot.Node)
	}
	if !slices.Equal(snapshot.Services["web"].Containers, []string{"c1", "c2"}) {
		t.Fatalf("containers were not sorted: %#v", snapshot.Services["web"].Containers)
	}
	if got := snapshot.Services["web"].ConfigHash; got != "hash-web" {
		t.Fatalf("node config hash = %q, want hash-web", got)
	}
	if got := snapshot.Services["web"].RuntimeID; got != runtimeid.ServiceIdentity("demo", "production", "web") {
		t.Fatalf("node runtime id = %q, want expected runtime id", got)
	}
}

func TestManagerWithRequestTimeoutUsesCustomDeadline(t *testing.T) {
	client := &fakeManagerExecutor{
		output: `{"found":true,"content":"{\"schemaVersion\":1,\"revisionId\":\"rev-1\",\"project\":\"demo\",\"environment\":\"production\",\"services\":{},\"createdAt\":\"2026-06-17T12:00:00Z\"}\n"}`,
	}
	timeout := 7 * time.Second
	manager := (&Manager{
		client:      client,
		socket:      "/run/tako/takod.sock",
		project:     "demo",
		environment: "production",
	}).WithRequestTimeout(timeout)

	if _, err := manager.ReadDesired(); err != nil {
		t.Fatalf("ReadDesired returned error: %v", err)
	}
	if !client.deadlineWithin(timeout) {
		t.Fatalf("deadline = %s, want near %s", client.deadline.Sub(client.startedAt), timeout)
	}
}

func TestStatePersistErrorJoinsNodeErrors(t *testing.T) {
	nodeAErr := errors.New("node-a failed")
	nodeBErr := errors.New("node-b failed")

	err := statePersistError([]statePersistResult{
		{serverName: "node-a", err: nodeAErr},
		{serverName: "node-b"},
		{serverName: "node-c", err: nodeBErr},
	})

	if !errors.Is(err, nodeAErr) || !errors.Is(err, nodeBErr) {
		t.Fatalf("joined error did not preserve node errors: %v", err)
	}
}

type fakeManagerExecutor struct {
	output    string
	startedAt time.Time
	deadline  time.Time
}

func (f *fakeManagerExecutor) ExecuteWithContext(ctx context.Context, cmd string) (string, error) {
	f.startedAt = time.Now()
	if deadline, ok := ctx.Deadline(); ok {
		f.deadline = deadline
	}
	return f.output, nil
}

func (f *fakeManagerExecutor) ExecuteWithInput(ctx context.Context, cmd string, input io.Reader) (string, error) {
	f.startedAt = time.Now()
	if deadline, ok := ctx.Deadline(); ok {
		f.deadline = deadline
	}
	return f.output, nil
}

func (f *fakeManagerExecutor) deadlineWithin(want time.Duration) bool {
	if f.deadline.IsZero() || f.startedAt.IsZero() {
		return false
	}
	got := f.deadline.Sub(f.startedAt)
	return got > want-time.Second && got < want+time.Second
}

func TestStatePersistErrorAllowsSuccessfulResults(t *testing.T) {
	if err := statePersistError([]statePersistResult{{serverName: "node-a"}}); err != nil {
		t.Fatalf("expected successful results to return nil, got %v", err)
	}
}

func TestStaleNodeActualNamesUsesPreviousAndCurrentNodeSets(t *testing.T) {
	previous := &ActualSnapshot{
		TargetNodes: []string{"node-c", "node-a", "node-b", "node-e"},
		Nodes: map[string]ActualNodeSnapshot{
			"node-d": {Node: "node-d"},
			"node-a": {Node: "node-a"},
		},
	}
	current := &ActualSnapshot{
		TargetNodes: []string{"node-a"},
		Nodes: map[string]ActualNodeSnapshot{
			"node-e": {Node: "node-e"},
		},
	}
	currentNodeActual := map[string]*ActualSnapshot{
		"node-f": {Node: "node-f"},
	}

	stale := StaleNodeActualNames(previous, current, currentNodeActual)
	if !slices.Equal(stale, []string{"node-b", "node-c", "node-d", "node-e"}) {
		t.Fatalf("stale nodes = %#v, want node-b/node-c/node-d/node-e", stale)
	}
}

func TestStaleNodeActualNamesDoesNotDeleteWithoutActiveNodeSet(t *testing.T) {
	previous := &ActualSnapshot{TargetNodes: []string{"node-a"}}

	if stale := StaleNodeActualNames(previous, nil, nil); len(stale) != 0 {
		t.Fatalf("stale nodes = %#v, want none when current node set is unknown", stale)
	}
}
