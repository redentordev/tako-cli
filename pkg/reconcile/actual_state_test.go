package reconcile

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

func TestAggregateActualStateByServerCombinesReplicas(t *testing.T) {
	nodeAWeb := &ActualService{
		Name:             "web",
		Image:            "demo/web:1",
		Replicas:         1,
		Containers:       []string{"a1"},
		ConfigHash:       "hash-web",
		RuntimeID:        runtimeid.ServiceIdentity("demo", "production", "web"),
		CurrentRevision:  "rev-web",
		DeployStrategy:   "recreate",
		ActiveContainers: []string{"a1"},
	}
	actualByServer := map[string]map[string]*ActualService{
		"node-a": {
			"web": nodeAWeb,
		},
		"node-b": {
			"web": {
				Name:             "web",
				Image:            "demo/web:1",
				Replicas:         2,
				Containers:       []string{"b1", "b2"},
				ConfigHash:       "hash-web",
				RuntimeID:        runtimeid.ServiceIdentity("demo", "production", "web"),
				CurrentRevision:  "rev-web",
				DeployStrategy:   "recreate",
				ActiveContainers: []string{"b1", "b2"},
			},
			"worker": {
				Name:       "worker",
				Image:      "demo/worker:1",
				Replicas:   1,
				Containers: []string{"w1"},
			},
		},
	}

	aggregate := AggregateActualStateByServer(actualByServer)

	if got := aggregate["web"].Replicas; got != 3 {
		t.Fatalf("web replicas = %d, want 3", got)
	}
	if !slices.Equal(aggregate["web"].Containers, []string{"a1", "b1", "b2"}) {
		t.Fatalf("unexpected web containers: %#v", aggregate["web"].Containers)
	}
	if got := aggregate["worker"].Replicas; got != 1 {
		t.Fatalf("worker replicas = %d, want 1", got)
	}
	if got := aggregate["web"].ConfigHash; got != "hash-web" {
		t.Fatalf("web config hash = %q, want hash-web", got)
	}
	if got := aggregate["web"].RuntimeID; got != runtimeid.ServiceIdentity("demo", "production", "web") {
		t.Fatalf("web runtime id = %q, want expected runtime id", got)
	}
	if got := aggregate["web"].CurrentRevision; got != "rev-web" {
		t.Fatalf("web revision = %q, want rev-web", got)
	}
	if !slices.Equal(aggregate["web"].ActiveContainers, []string{"a1", "b1", "b2"}) {
		t.Fatalf("active containers = %#v, want all web containers", aggregate["web"].ActiveContainers)
	}

	aggregate["web"].Containers[0] = "mutated"
	if nodeAWeb.Containers[0] != "a1" {
		t.Fatalf("aggregate aliased node state: %#v", nodeAWeb.Containers)
	}
}

func TestAggregateActualStateByServerClearsMixedConfigHashes(t *testing.T) {
	actualByServer := map[string]map[string]*ActualService{
		"node-a": {
			"web": {Name: "web", Image: "demo/web:1", Replicas: 1, ConfigHash: "hash-a"},
		},
		"node-b": {
			"web": {Name: "web", Image: "demo/web:1", Replicas: 1, ConfigHash: "hash-b"},
		},
	}

	aggregate := AggregateActualStateByServer(actualByServer)
	if got := aggregate["web"].ConfigHash; got != "" {
		t.Fatalf("mixed config hash = %q, want empty", got)
	}
}

func TestAggregateActualStateByServerClearsMixedRuntimeIDs(t *testing.T) {
	actualByServer := map[string]map[string]*ActualService{
		"node-a": {
			"web": {
				Name:      "web",
				Image:     "demo/web:1",
				Replicas:  1,
				RuntimeID: runtimeid.ServiceIdentity("demo", "production", "web"),
			},
		},
		"node-b": {
			"web": {Name: "web", Image: "demo/web:1", Replicas: 1},
		},
	}

	aggregate := AggregateActualStateByServer(actualByServer)
	if got := aggregate["web"].RuntimeID; got != "" {
		t.Fatalf("mixed runtime id = %q, want empty", got)
	}
}

func TestGatherActualStateByServerWithRunsConcurrently(t *testing.T) {
	serverNames := []string{"node-a", "node-b", "node-c"}
	servers := testActualStateServers(serverNames)
	started := make(chan string, len(serverNames))
	release := make(chan struct{})

	actualDone := make(chan map[string]map[string]*ActualService, 1)
	errDone := make(chan error, 1)
	go func() {
		actual, err := gatherActualStateByServerWith(servers, serverNames, func(serverName string, _ config.ServerConfig) (map[string]*ActualService, error) {
			started <- serverName
			<-release
			return map[string]*ActualService{
				"web": {Name: "web", Replicas: 1, Containers: []string{serverName + "-web"}},
			}, nil
		})
		actualDone <- actual
		errDone <- err
	}()

	waitForActualStateStarts(t, started, len(serverNames))
	close(release)

	if err := <-errDone; err != nil {
		t.Fatalf("gatherActualStateByServerWith returned error: %v", err)
	}
	actual := <-actualDone
	for _, serverName := range serverNames {
		if got := actual[serverName]["web"].Containers[0]; got != serverName+"-web" {
			t.Fatalf("actual state for %s = %q", serverName, got)
		}
	}
}

func TestGatherActualStateByServerWithAggregatesSortedErrors(t *testing.T) {
	serverNames := []string{"node-b", "node-a"}
	servers := testActualStateServers(serverNames)

	_, err := gatherActualStateByServerWith(servers, serverNames, func(serverName string, _ config.ServerConfig) (map[string]*ActualService, error) {
		return nil, fmt.Errorf("unavailable")
	})
	if err == nil {
		t.Fatal("expected aggregate gather error")
	}
	if got, want := err.Error(), "node-a: unavailable; node-b: unavailable"; !strings.Contains(got, want) {
		t.Fatalf("error = %q, want to contain %q", got, want)
	}
}

func TestGatherActualStateFromTakodUsesSharedClientEndpoint(t *testing.T) {
	executor := &fakeActualTakodExecutor{
		output: `{"project":"demo app","environment":"prod/us","services":{"web":{"name":"web","image":"demo/web:1","replicas":2,"containers":["a","b"],"configHash":"hash","runtimeId":"rid"}}}` + "\n__TAKO_HTTP_STATUS__:200",
	}

	actual, err := gatherActualStateFromTakodWith(executor, "/run/custom.sock", "demo app", "prod/us")
	if err != nil {
		t.Fatalf("gatherActualStateFromTakodWith returned error: %v", err)
	}

	if !strings.Contains(executor.cmd, "--write-out '\\n__TAKO_HTTP_STATUS__:%{http_code}'") {
		t.Fatalf("actual state gather should use shared takod client status handling: %s", executor.cmd)
	}
	if !strings.Contains(executor.cmd, "environment=prod%2Fus") || !strings.Contains(executor.cmd, "project=demo+app") {
		t.Fatalf("actual state endpoint was not escaped correctly: %s", executor.cmd)
	}
	web := actual["web"]
	if web == nil || web.Replicas != 2 || !slices.Equal(web.Containers, []string{"a", "b"}) || web.RuntimeID != "rid" {
		t.Fatalf("web actual state = %#v", web)
	}
}

func waitForActualStateStarts(t *testing.T, started <-chan string, count int) {
	t.Helper()
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < count {
		select {
		case name := <-started:
			seen[name] = true
		case <-deadline:
			t.Fatalf("timed out waiting for actual state fanout; saw %v", seen)
		}
	}
}

type fakeActualTakodExecutor struct {
	cmd    string
	output string
	err    error
}

func (f *fakeActualTakodExecutor) ExecuteWithContext(_ context.Context, cmd string) (string, error) {
	f.cmd = cmd
	return f.output, f.err
}

func (f *fakeActualTakodExecutor) ExecuteWithInput(_ context.Context, cmd string, _ io.Reader) (string, error) {
	f.cmd = cmd
	return f.output, f.err
}

func testActualStateServers(names []string) map[string]config.ServerConfig {
	servers := make(map[string]config.ServerConfig, len(names))
	for _, name := range names {
		servers[name] = config.ServerConfig{Host: name, User: "root"}
	}
	return servers
}
