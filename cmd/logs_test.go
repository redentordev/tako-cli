package cmd

import (
	"bytes"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
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

func resetLogsFlags(t *testing.T) {
	t.Helper()
	reset := func() {
		logsServer, logsServices, logsFollow, logsTail = "", nil, false, 100
		logsSince, logsUntil, logsGrep = "", "", ""
		logsIgnoreCase, logsTimestamps = false, false
		logsCmd.Flags().VisitAll(func(flag *pflag.Flag) {
			flag.Changed = false
			if slice, ok := flag.Value.(pflag.SliceValue); ok {
				_ = slice.Replace(nil)
			}
		})
	}
	reset()
	t.Cleanup(reset)
}

func TestLogsFlagsBuildRequest(t *testing.T) {
	resetLogsFlags(t)
	if logsCmd.Flags().Lookup("service").Annotations[cobra.BashCompOneRequiredFlag] != nil {
		t.Fatal("--service must no longer be required")
	}
	if err := logsCmd.Flags().Parse([]string{
		"--service", "web,worker", "--service", "nightly", "-f",
		"--since", "10m", "--until", "1700000000", "-g", "err(or)?", "--ignore-case", "-t", "-s", "node-a",
	}); err != nil {
		t.Fatal(err)
	}
	request := buildLogsRequest(logsCmd, []string{"api"})
	if got := strings.Join(request.Services, " "); got != "api web worker nightly" {
		t.Fatalf("services = %q", got)
	}
	if !request.Follow || request.Since != "10m" || request.Until != "1700000000" || request.Grep != "err(or)?" || !request.IgnoreCase || !request.Timestamps || request.Server != "node-a" {
		t.Fatalf("request = %#v", request)
	}
	if request.TailExplicit || request.Tail != 100 {
		t.Fatalf("tail = %d explicit = %v, want default 100 not explicit", request.Tail, request.TailExplicit)
	}
}

func TestLogsFlagsDetectExplicitTail(t *testing.T) {
	resetLogsFlags(t)
	if err := logsCmd.Flags().Parse([]string{"-n", "100", "--since", "1h"}); err != nil {
		t.Fatal(err)
	}
	request := buildLogsRequest(logsCmd, nil)
	if !request.TailExplicit || request.Tail != 100 || len(request.Services) != 0 {
		t.Fatalf("request = %#v", request)
	}
}

func TestLogsCommandAcceptsPositionalServices(t *testing.T) {
	if err := logsCmd.Args(logsCmd, []string{"web", "worker"}); err != nil {
		t.Fatalf("positional services rejected: %v", err)
	}
	for _, name := range []string{"since", "until", "grep", "ignore-case", "timestamps", "tail", "follow", "server", "service"} {
		if logsCmd.Flags().Lookup(name) == nil {
			t.Fatalf("missing --%s", name)
		}
	}
	if logsCmd.Flags().ShorthandLookup("g").Name != "grep" || logsCmd.Flags().ShorthandLookup("t").Name != "timestamps" {
		t.Fatal("expected -g/--grep and -t/--timestamps")
	}
	for _, name := range []string{"color", "no-color", "prefix", "no-prefix"} {
		if logsCmd.Flags().Lookup(name) != nil {
			t.Fatalf("unexpected --%s flag", name)
		}
	}
}

func logLineEvent(service string, replica string, node string, message string) events.Event {
	return events.Event{
		Type:    events.TypeLogLine,
		Level:   events.LevelInfo,
		Message: message,
		Data:    map[string]any{"service": service, "replica": replica, "node": node, "data": "boot", "stream": "stderr"},
	}
}

func TestHumanSinkColorsLogPrefixesStably(t *testing.T) {
	var out bytes.Buffer
	sink := humanEventSink{writer: &out, color: true}
	sink.Emit(logLineEvent("web", "1", "node-a", "web.1@node-a  | boot\n"))
	first := out.String()
	color := logsPrefixColor("web.1@node-a")
	want := "\x1b[" + color + "mweb.1@node-a\x1b[0m  | boot\n"
	if first != want {
		t.Fatalf("colored line = %q, want %q", first, want)
	}
	out.Reset()
	sink.Emit(logLineEvent("web", "1", "node-a", "2026-10-11T12:00:00Z web.1@node-a  | again\n"))
	if !strings.Contains(out.String(), "\x1b["+color+"mweb.1@node-a\x1b[0m") {
		t.Fatalf("same prefix changed color: %q", out.String())
	}
	if logsPrefixColor("web.1@node-a") != color {
		t.Fatal("prefix color is not stable")
	}
	for _, prefix := range []string{"web.1@node-a", "web.2@node-a", "worker.1@node-b", "api.1@node-c", "web.1@node-b", "db.1@node-a", "cache.1@node-a"} {
		if code := logsPrefixColor(prefix); !slices.Contains(logsPrefixPalette, code) {
			t.Fatalf("color %q outside palette", code)
		}
	}
	if len(logsPrefixPalette) != 6 {
		t.Fatalf("palette size = %d", len(logsPrefixPalette))
	}
}

func TestHumanSinkLeavesUnprefixedAndPlainOutputWithoutEscapes(t *testing.T) {
	var out bytes.Buffer
	colored := humanEventSink{writer: &out, color: true}
	colored.Emit(logLineEvent("web", "1", "node-a", "boot web.1@node-a\n"))
	plain := humanEventSink{writer: &out}
	plain.Emit(logLineEvent("web", "1", "node-a", "web.1@node-a | boot\n"))
	if strings.Contains(out.String(), "\x1b[") {
		t.Fatalf("unexpected ANSI escapes: %q", out.String())
	}
	if out.String() != "boot web.1@node-a\nweb.1@node-a | boot\n" {
		t.Fatalf("output = %q (stderr-stream log lines must stay on the log writer)", out.String())
	}
}

func TestLogsColorDisabledWhenPipedNoColorOrMachineOutput(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	if logsColorEnabled(writer) {
		t.Fatal("color enabled for a pipe")
	}
	t.Setenv("NO_COLOR", "1")
	if logsColorEnabled(os.Stdout) {
		t.Fatal("color enabled with NO_COLOR set")
	}
	os.Unsetenv("NO_COLOR")
	previous := outputFormatFlag
	outputFormatFlag = outputFormatJSON
	defer func() { outputFormatFlag = previous }()
	if logsColorEnabled(os.Stdout) {
		t.Fatal("color enabled in machine output mode")
	}
}

func TestMachineLogLineMessageHasNoEscapes(t *testing.T) {
	var out bytes.Buffer
	sink := events.NewNDJSONSink(&out)
	sink.Emit(logLineEvent("web", "1", "node-a", "web.1@node-a | boot\n"))
	if strings.Contains(out.String(), "\\u001b") || strings.Contains(out.String(), "\x1b") {
		t.Fatalf("machine event carries escapes: %s", out.String())
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
