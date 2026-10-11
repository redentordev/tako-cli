package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

func monitorBase() engine.MonitorResult {
	return engine.MonitorResult{Kind: engine.KindMonitorResult, Project: "shop", Environment: "production", Nodes: []engine.MonitorNode{}}
}

func supportedRead(node string, subjects ...takod.SourceSubjectStatus) monitorNodeRead {
	return monitorNodeRead{
		node:   engine.MonitorNode{Node: node, Reachable: true, Supported: true, Subjects: []engine.MonitorSubject{}},
		status: &takod.AlertStatusResponse{Sources: &takod.SourceAlertStatus{Subjects: subjects, PendingDeliveries: 1, ExhaustedDeliveries: 2}},
	}
}

func TestClassifyMonitorResultExitCodes(t *testing.T) {
	since := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	okSubject := takod.SourceSubjectStatus{Kind: takod.AlertKindService, Subject: "web", State: takod.SubjectStateOK, LastObservedAt: since}
	alerting := takod.SourceSubjectStatus{Kind: takod.AlertKindHost, Subject: "@root", State: takod.SubjectStateAlerting, Severity: "critical", Reasons: []string{"disk_high"}, Since: since}
	unreachable := monitorNodeRead{node: engine.MonitorNode{Node: "b", Error: "unreachable", Subjects: []engine.MonitorSubject{}}, err: &engine.ConnectivityError{Server: "b", Err: errors.New("dial")}}
	unsupported := monitorNodeRead{node: engine.MonitorNode{Node: "c", Reachable: true, Error: "unsupported", Subjects: []engine.MonitorSubject{}}, err: &takodclient.CapabilityRequiredError{Server: "c", Capability: takod.CapabilityMonitoringHealthV1}}

	tests := []struct {
		name      string
		reads     []monitorNodeRead
		monitored []string
		status    string
		code      int
	}{
		{"all ok", []monitorNodeRead{supportedRead("a", okSubject)}, []string{"web"}, engine.MonitorStatusOK, 0},
		{"alerting", []monitorNodeRead{supportedRead("a", okSubject, alerting)}, []string{"web"}, engine.MonitorStatusAttention, 6},
		{"not deployed", []monitorNodeRead{supportedRead("a")}, []string{"web"}, engine.MonitorStatusAttention, 6},
		{"one node failed", []monitorNodeRead{supportedRead("a", okSubject), unreachable}, []string{"web"}, engine.MonitorStatusAttention, 6},
		{"all unreachable", []monitorNodeRead{unreachable}, []string{"web"}, engine.MonitorStatusUnreachable, 4},
		{"no capable node", []monitorNodeRead{unsupported}, []string{"web"}, engine.MonitorStatusUnsupported, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := classifyMonitorResult(monitorBase(), tt.reads, tt.monitored)
			if result.Status != tt.status || exitCodeForError(err) != tt.code {
				t.Fatalf("status=%q code=%d err=%v, want %q/%d", result.Status, exitCodeForError(err), err, tt.status, tt.code)
			}
		})
	}

	result, _ := classifyMonitorResult(monitorBase(), []monitorNodeRead{supportedRead("a", okSubject, alerting)}, []string{"web", "api"})
	if result.Counts.OK != 1 || result.Counts.Alerting != 1 || result.Counts.Unknown != 1 || len(result.NotDeployed) != 1 || result.NotDeployed[0].Subject != "api" {
		t.Fatalf("result = %+v", result)
	}
	if result.Nodes[0].PendingDeliveries != 1 || result.Nodes[0].ExhaustedDeliveries != 2 {
		t.Fatalf("delivery counts = %+v", result.Nodes[0])
	}
	var out bytes.Buffer
	printMonitorResult(&out, result)
	for _, want := range []string{"KIND", "LAST CHECK", "@root", "disk_high", "not_deployed", "1 pending, 2 exhausted"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("table missing %q:\n%s", want, out.String())
		}
	}
}

func TestValidateMonitorModes(t *testing.T) {
	for _, tt := range []struct {
		name                   string
		machine, local, watchd bool
		watch                  time.Duration
		code                   int
	}{
		{"status read", true, false, false, 0, 0},
		{"watch human", false, false, true, 5 * time.Second, 0},
		{"watch json", true, false, true, 5 * time.Second, 2},
		{"local json", true, true, false, 0, 2},
		{"local watch", false, true, true, 5 * time.Second, 2},
		{"watch too fast", false, false, true, time.Millisecond, 2},
		{"local human", false, true, false, 0, 0},
	} {
		if code := exitCodeForError(validateMonitorModes(tt.machine, tt.local, tt.watchd, tt.watch)); code != tt.code {
			t.Fatalf("%s: exit %d, want %d", tt.name, code, tt.code)
		}
	}
}

func TestClassifyMonitorResultServiceFilter(t *testing.T) {
	result := monitorBase()
	result.Service = "web"
	read := supportedRead("a",
		takod.SourceSubjectStatus{Kind: takod.AlertKindService, Subject: "web", State: takod.SubjectStateOK},
		takod.SourceSubjectStatus{Kind: takod.AlertKindService, Subject: "api", State: takod.SubjectStateAlerting},
	)
	result, err := classifyMonitorResult(result, []monitorNodeRead{read}, []string{"web"})
	if err != nil || len(result.Nodes[0].Subjects) != 1 || result.Nodes[0].Subjects[0].Subject != "web" {
		t.Fatalf("filtered result = %+v err=%v", result, err)
	}
}
