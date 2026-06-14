package cmd

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestStreamLogNodesWithRunsConcurrentlyAndKeepsSortedOrder(t *testing.T) {
	servers := map[string]config.ServerConfig{
		"node-c": {Host: "node-c.example.test"},
		"node-a": {Host: "node-a.example.test"},
		"node-b": {Host: "node-b.example.test"},
	}
	started := make(chan string, len(servers))
	release := make(chan struct{})

	resultsDone := make(chan []logNodeResult, 1)
	go func() {
		resultsDone <- streamLogNodesWith(servers, func(serverName string, _ config.ServerConfig, prefix bool) error {
			if !prefix {
				return fmt.Errorf("expected multi-node stream to request prefixes")
			}
			started <- serverName
			<-release
			return nil
		})
	}()

	waitForLogStarts(t, started, len(servers))
	close(release)

	results := <-resultsDone
	wantNames := []string{"node-a", "node-b", "node-c"}
	for i, want := range wantNames {
		if results[i].serverName != want {
			t.Fatalf("result %d server = %q, want %q", i, results[i].serverName, want)
		}
		if results[i].err != nil {
			t.Fatalf("result %d err = %v", i, results[i].err)
		}
	}
}

func TestStreamLogNodesWithOmitsPrefixForSingleNode(t *testing.T) {
	servers := map[string]config.ServerConfig{
		"node-a": {Host: "node-a.example.test"},
	}

	results := streamLogNodesWith(servers, func(_ string, _ config.ServerConfig, prefix bool) error {
		if prefix {
			return fmt.Errorf("single-node stream should not request prefixes")
		}
		return nil
	})

	if len(results) != 1 || results[0].err != nil {
		t.Fatalf("unexpected single-node result: %#v", results)
	}
}

func TestSummarizeLogStreamResultsReportsSortedErrors(t *testing.T) {
	err := summarizeLogStreamResults([]logNodeResult{
		{serverName: "node-b", err: fmt.Errorf("second")},
		{serverName: "node-a", err: fmt.Errorf("first")},
	})
	if err == nil {
		t.Fatal("expected all-node failure")
	}
	message := err.Error()
	if !strings.Contains(message, "failed to stream logs from all target nodes") {
		t.Fatalf("unexpected error message: %s", message)
	}
	if strings.Index(message, "node-a") > strings.Index(message, "node-b") {
		t.Fatalf("expected node errors to be sorted: %s", message)
	}
}

func TestSummarizeLogStreamResultsReportsPartialErrors(t *testing.T) {
	err := summarizeLogStreamResults([]logNodeResult{
		{serverName: "node-a"},
		{serverName: "node-b", err: fmt.Errorf("failed")},
	})
	if err == nil || !strings.Contains(err.Error(), "1 node error") {
		t.Fatalf("expected partial node error, got %v", err)
	}
}

func waitForLogStarts(t *testing.T, started <-chan string, count int) {
	t.Helper()
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < count {
		select {
		case name := <-started:
			seen[name] = true
		case <-deadline:
			t.Fatalf("timed out waiting for log fanout; saw %v", seen)
		}
	}
}
