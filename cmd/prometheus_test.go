package cmd

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takod"
)

func TestCollectPrometheusNodesWithRunsConcurrentlyAndKeepsOrder(t *testing.T) {
	serverNames := []string{"node-a", "node-b", "node-c"}
	servers := testPrometheusServers(serverNames)
	started := make(chan string, len(serverNames))
	release := make(chan struct{})

	resultsDone := make(chan []prometheusNodeResult, 1)
	go func() {
		resultsDone <- collectPrometheusNodesWith(servers, serverNames, func(serverName string, _ config.ServerConfig) (*MetricsData, []takod.ContainerStat, *takod.AlertStatusResponse, bool, error) {
			started <- serverName
			<-release
			return &MetricsData{CPUPercent: serverName}, []takod.ContainerStat{{Name: serverName + "-web"}}, &takod.AlertStatusResponse{Configured: true}, true, nil
		})
	}()

	waitForPrometheusStarts(t, started, len(serverNames))
	close(release)

	results := <-resultsDone
	if len(results) != len(serverNames) {
		t.Fatalf("results = %d, want %d", len(results), len(serverNames))
	}
	for i, serverName := range serverNames {
		if results[i].serverName != serverName {
			t.Fatalf("result %d server = %q, want %q", i, results[i].serverName, serverName)
		}
		if results[i].metrics == nil || results[i].metrics.CPUPercent != serverName {
			t.Fatalf("result %d metrics = %#v, want %s", i, results[i].metrics, serverName)
		}
		if len(results[i].stats) != 1 || results[i].stats[0].Name != serverName+"-web" {
			t.Fatalf("result %d stats = %#v", i, results[i].stats)
		}
		if !results[i].alertsRead || results[i].alertStatus == nil || !results[i].alertStatus.Configured {
			t.Fatalf("result %d alert status = %#v", i, results[i])
		}
	}
}

func TestCollectPrometheusNodesWithRecordsErrors(t *testing.T) {
	serverNames := []string{"node-a", "node-b", "missing"}
	servers := testPrometheusServers(serverNames[:2])

	results := collectPrometheusNodesWith(servers, serverNames, func(serverName string, _ config.ServerConfig) (*MetricsData, []takod.ContainerStat, *takod.AlertStatusResponse, bool, error) {
		if serverName == "node-a" {
			return nil, nil, nil, false, fmt.Errorf("connect refused")
		}
		return &MetricsData{CPUPercent: "1"}, nil, &takod.AlertStatusResponse{}, true, nil
	})

	if results[0].err == nil {
		t.Fatalf("node-a should record collector error")
	}
	if results[1].err != nil || results[1].metrics == nil {
		t.Fatalf("node-b should succeed: %#v", results[1])
	}
	if results[2].err == nil {
		t.Fatalf("missing node should record configuration error")
	}
}

func TestExportAlertMetricsIncludesExternalReachabilityAndNoCredentials(t *testing.T) {
	status := &takod.AlertStatusResponse{
		Project: "demo", Environment: "production", Configured: true,
		EvaluatedAt: time.Unix(100, 0), ActiveEpisodes: 1, PendingDelivery: 2, Exhausted: 3,
		Subjects: []takod.AlertSubjectStatus{{Kind: "job", Service: "worker", Active: true, LastSuccessAt: time.Unix(50, 0)}},
	}
	var output bytes.Buffer
	exportAlertMetrics(&output, "node-a", "node-a.example", "demo", "production", status, true, true)
	text := output.String()
	for _, expected := range []string{
		`tako_scheduler_alert_owner_reachable{server="node-a",host="node-a.example",project="demo",environment="production"} 1`,
		`tako_scheduler_alert_active_episodes{server="node-a",host="node-a.example",project="demo",environment="production"} 1`,
		`tako_scheduler_alert_subject_active{server="node-a",host="node-a.example",project="demo",environment="production",kind="job",service="worker"} 1`,
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("output missing %q:\n%s", expected, text)
		}
	}
	if strings.Contains(text, "webhook") || strings.Contains(text, "token") {
		t.Fatalf("output disclosed credentials: %s", text)
	}

	output.Reset()
	exportAlertMetrics(&output, "node-a", "node-a.example", "demo", "production", nil, false, true)
	if !strings.Contains(output.String(), `tako_scheduler_alert_owner_reachable{server="node-a",host="node-a.example",project="demo",environment="production"} 0`) || strings.Contains(output.String(), "tako_scheduler_alert_active_episodes{") {
		t.Fatalf("unreachable output = %s", output.String())
	}
}

func TestSchedulerAlertOwnersAllowsFullyCordonedEnvironment(t *testing.T) {
	servers := map[string]config.ServerConfig{
		"node-a": {Lifecycle: "cordoned"},
		"node-b": {Lifecycle: "draining"},
	}
	owners, err := schedulerAlertOwners(servers, []string{"node-a", "node-b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(owners) != 0 {
		t.Fatalf("owners = %#v", owners)
	}
	var output bytes.Buffer
	exportAlertOwnerPresence(&output, "demo", "production", false)
	if !strings.Contains(output.String(), `tako_scheduler_alert_owner_present{project="demo",environment="production"} 0`) {
		t.Fatalf("owner presence output = %s", output.String())
	}
}

func waitForPrometheusStarts(t *testing.T, started <-chan string, count int) {
	t.Helper()
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < count {
		select {
		case name := <-started:
			seen[name] = true
		case <-deadline:
			t.Fatalf("timed out waiting for prometheus fanout; saw %v", seen)
		}
	}
}

func testPrometheusServers(names []string) map[string]config.ServerConfig {
	servers := make(map[string]config.ServerConfig, len(names))
	for _, name := range names {
		servers[name] = config.ServerConfig{Host: name + ".example.test", User: "root"}
	}
	return servers
}
