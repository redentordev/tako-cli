package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/selfheal"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

func healStatusFixture() *takod.HealStatusResponse {
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	until := at.Add(time.Hour)
	return &takod.HealStatusResponse{
		Project: "shop", Environment: "production", Node: "app-1", Enabled: true, EvaluatedAt: &at,
		Breaker: takod.HealBreakerStatus{RecentActions: 1},
		Services: []takod.HealServiceStatus{
			{Service: "api", State: selfheal.StatePaused, PausedUntil: &until},
			{Service: "web", State: selfheal.StateGaveUp, GaveUpAt: &at, GaveUpReason: "exited_nonzero", RecentActions: 3, LastAction: "started", LastActionAt: &at, LastReason: "exited_nonzero"},
		},
		Journal: []selfheal.JournalEntry{{Time: at, Node: "app-1", Service: "web", Container: "shop_production_web_1", Revision: "r1", Decision: "gave_up", Reason: "exited_nonzero"}},
	}
}

func TestHealStatusExitCodes(t *testing.T) {
	ok := &takod.HealStatusResponse{Enabled: true, Services: []takod.HealServiceStatus{{Service: "web", State: selfheal.StateOK}}}
	disabled := &takod.HealStatusResponse{Services: []takod.HealServiceStatus{}}
	breaker := &takod.HealStatusResponse{Enabled: true, Breaker: takod.HealBreakerStatus{Open: true, Reason: "mass_failure"}}
	reads := map[string]func(string) (*takod.HealStatusResponse, error){
		"ok":       func(string) (*takod.HealStatusResponse, error) { return ok, nil },
		"disabled": func(string) (*takod.HealStatusResponse, error) { return disabled, nil },
		"gave up":  func(string) (*takod.HealStatusResponse, error) { return healStatusFixture(), nil },
		"breaker":  func(string) (*takod.HealStatusResponse, error) { return breaker, nil },
		"unreachable": func(s string) (*takod.HealStatusResponse, error) {
			return nil, &engine.ConnectivityError{Server: s, Err: errors.New("dial")}
		},
		"unsupported": func(s string) (*takod.HealStatusResponse, error) {
			return nil, &takodclient.CapabilityRequiredError{Server: s, Capability: takodclient.CapabilityNodeSelfHealingV1, Feature: takodclient.HealFeature}
		},
		"one node down": func(s string) (*takod.HealStatusResponse, error) {
			if s == "b" {
				return nil, &engine.ConnectivityError{Server: s, Err: errors.New("dial")}
			}
			return ok, nil
		},
	}
	want := map[string]struct {
		status string
		code   int
	}{
		"ok": {engine.HealStatusOK, 0}, "disabled": {engine.HealStatusDisabled, 0}, "gave up": {engine.HealStatusAttention, 6},
		"breaker": {engine.HealStatusAttention, 6}, "unreachable": {engine.HealStatusUnreachable, 4},
		"unsupported": {engine.HealStatusUnsupported, 2}, "one node down": {engine.HealStatusAttention, 6},
	}
	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			result, err := engine.CollectHealStatus(context.Background(), "shop", "production", []string{"a", "b"}, "", func(_ context.Context, server string) (*takod.HealStatusResponse, error) {
				return read(server)
			})
			if result.Status != want[name].status {
				t.Fatalf("status = %s, want %s", result.Status, want[name].status)
			}
			if code := exitCodeForError(err); code != want[name].code {
				t.Fatalf("exit code = %d (%v), want %d", code, err, want[name].code)
			}
		})
	}
}

func TestHealStatusDocumentGolden(t *testing.T) {
	result, _ := engine.CollectHealStatus(context.Background(), "shop", "production", []string{"app-1"}, "web", func(context.Context, string) (*takod.HealStatusResponse, error) {
		return healStatusFixture(), nil
	})
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "apiVersion": "` + takoapi.APIVersionCurrent + `",
  "kind": "HealStatus",
  "project": "shop",
  "environment": "production",
  "service": "web",
  "status": "attention",
  "nodes": [
    {
      "node": "app-1",
      "reachable": true,
      "supported": true,
      "enabled": true,
      "evaluatedAt": "2026-03-01T12:00:00Z",
      "breaker": {
        "open": false,
        "recentActions": 1
      },
      "services": [
        {
          "service": "web",
          "state": "gave_up",
          "gaveUpAt": "2026-03-01T12:00:00Z",
          "gaveUpReason": "exited_nonzero",
          "recentActions": 3,
          "lastAction": "started",
          "lastActionAt": "2026-03-01T12:00:00Z",
          "lastReason": "exited_nonzero"
        }
      ],
      "journal": [
        {
          "time": "2026-03-01T12:00:00Z",
          "node": "app-1",
          "service": "web",
          "container": "shop_production_web_1",
          "revision": "r1",
          "decision": "gave_up",
          "reason": "exited_nonzero"
        }
      ]
    }
  ]
}`
	if string(payload) != want {
		t.Fatalf("HealStatus document drifted:\n%s", payload)
	}
	var out bytes.Buffer
	printHealStatus(&out, result)
	if !strings.Contains(out.String(), "web") || !strings.Contains(out.String(), "gave_up") || !strings.Contains(out.String(), "Journal:") {
		t.Fatalf("human output = %s", out.String())
	}
}

func TestHealChangeDocumentGolden(t *testing.T) {
	until := time.Date(2026, 3, 1, 13, 0, 0, 0, time.UTC)
	request := engine.HealChangeRequest{Action: "pause", Service: "web", For: time.Hour}
	result, err := engine.ApplyHealChange(context.Background(), "shop", "production", []string{"app-2", "app-1"}, request, nil, func(_ context.Context, server string, change takod.HealChangeRequest) (*takod.HealChangeResponse, error) {
		if change.Project != "shop" || change.Environment != "production" || change.Action != "pause" || change.Service != "web" || change.ForSeconds != 3600 {
			t.Fatalf("change = %+v", change)
		}
		return &takod.HealChangeResponse{Node: server, Action: "pause", Service: "web", Changed: true, PausedUntil: &until}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.MarshalIndent(result, "", "  ")
	want := `{
  "apiVersion": "` + takoapi.APIVersionCurrent + `",
  "kind": "HealChangeResult",
  "project": "shop",
  "environment": "production",
  "action": "pause",
  "service": "web",
  "pausedUntil": "2026-03-01T13:00:00Z",
  "nodes": [
    {
      "node": "app-1",
      "changed": true,
      "pausedUntil": "2026-03-01T13:00:00Z"
    },
    {
      "node": "app-2",
      "changed": true,
      "pausedUntil": "2026-03-01T13:00:00Z"
    }
  ]
}`
	if string(payload) != want {
		t.Fatalf("HealChangeResult document drifted:\n%s", payload)
	}
}

func TestHealChangeCapabilityPreflightChangesNothing(t *testing.T) {
	sent := 0
	_, err := engine.ApplyHealChange(context.Background(), "shop", "production", []string{"a", "b"}, engine.HealChangeRequest{Action: "pause"}, func(_ context.Context, server string) error {
		if server == "b" {
			return &takodclient.CapabilityRequiredError{Server: server, Capability: takodclient.CapabilityNodeSelfHealingV1, Feature: takodclient.HealFeature}
		}
		return nil
	}, func(context.Context, string, takod.HealChangeRequest) (*takod.HealChangeResponse, error) {
		sent++
		return &takod.HealChangeResponse{Changed: true}, nil
	})
	if exitCodeForError(err) != 2 || sent != 0 || !strings.Contains(err.Error(), "node.self-healing-v1") {
		t.Fatalf("err = %v (exit %d), sent = %d", err, exitCodeForError(err), sent)
	}
}

func TestHealChangePartialFailureNeedsAttention(t *testing.T) {
	_, err := engine.ApplyHealChange(context.Background(), "shop", "production", []string{"a", "b"}, engine.HealChangeRequest{Action: "reset"}, nil, func(_ context.Context, server string, _ takod.HealChangeRequest) (*takod.HealChangeResponse, error) {
		if server == "b" {
			return nil, errors.New("boom")
		}
		return &takod.HealChangeResponse{Changed: true}, nil
	})
	if exitCodeForError(err) != 6 {
		t.Fatalf("err = %v (exit %d), want 6", err, exitCodeForError(err))
	}
}

func TestHealPauseFlagValidation(t *testing.T) {
	defer func() { healFor, healUntilResume, healService = selfheal.DefaultPause, false, "" }()
	healFor, healUntilResume = selfheal.DefaultPause, false
	request, err := healChangeRequest("pause", false)
	if err != nil || request.For != time.Hour || request.Indefinite {
		t.Fatalf("default pause = %+v %v", request, err)
	}
	healUntilResume = true
	if request, err := healChangeRequest("pause", false); err != nil || !request.Indefinite || request.For != 0 {
		t.Fatalf("until-resume = %+v %v", request, err)
	}
	if _, err := healChangeRequest("pause", true); exitCodeForError(err) != 2 {
		t.Fatalf("--for with --until-resume = %v", err)
	}
	healUntilResume = false
	healFor = 8 * 24 * time.Hour
	if _, err := healChangeRequest("pause", true); exitCodeForError(err) != 2 {
		t.Fatalf("--for 8d = %v", err)
	}
	healFor = 0
	if _, err := healChangeRequest("pause", true); exitCodeForError(err) != 2 {
		t.Fatalf("--for 0 = %v", err)
	}
	if request, err := healChangeRequest("reset", false); err != nil || request.For != 0 {
		t.Fatalf("reset = %+v %v", request, err)
	}
}

// TestHealPauseSendsDurationNotEndTime: the wire request carries the
// duration, so a CLI with a skewed clock cannot shift or break the pause.
func TestHealPauseSendsDurationNotEndTime(t *testing.T) {
	defer func() { healFor, healUntilResume = selfheal.DefaultPause, false }()
	healFor, healUntilResume = 4*time.Hour+500*time.Millisecond, false
	request, err := healChangeRequest("pause", true)
	if err != nil {
		t.Fatal(err)
	}
	var sent takod.HealChangeRequest
	if _, err := engine.ApplyHealChange(context.Background(), "shop", "production", []string{"a"}, request, nil, func(_ context.Context, _ string, change takod.HealChangeRequest) (*takod.HealChangeResponse, error) {
		sent = change
		return &takod.HealChangeResponse{Changed: true}, nil
	}); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(sent)
	if sent.ForSeconds != 4*3600 || strings.Contains(string(payload), "until") {
		t.Fatalf("wire request = %s, want forSeconds and no end time", payload)
	}
}

func TestPSHealFieldAndNote(t *testing.T) {
	later := time.Date(2026, 3, 1, 12, 5, 0, 0, time.UTC)
	services := []engine.StatusService{{Name: "web"}, {Name: "api"}, {Name: "worker"}}
	nodes := []engine.HealNode{
		{Node: "a", Services: []takod.HealServiceStatus{{Service: "web", State: selfheal.StateOK, RecentActions: 1}, {Service: "api", State: selfheal.StatePaused}}},
		{Node: "b", Services: []takod.HealServiceStatus{{Service: "web", State: selfheal.StateGaveUp, RecentActions: 3, LastAction: "restarted", LastActionAt: &later}}},
	}
	engine.AttachStatusHeal(services, nodes)
	if services[0].Heal == nil || services[0].Heal.State != selfheal.StateGaveUp || services[0].Heal.RecentActions != 4 || services[0].Heal.LastAction != "restarted" {
		t.Fatalf("web heal = %+v", services[0].Heal)
	}
	if engine.HealStatusNote(services[0].Heal) != "heal:gave_up" || engine.HealStatusNote(services[1].Heal) != "heal:paused" {
		t.Fatalf("notes = %q %q", engine.HealStatusNote(services[0].Heal), engine.HealStatusNote(services[1].Heal))
	}
	// Without the capability (no node reports the service) the field is
	// absent from the document.
	if services[2].Heal != nil {
		t.Fatalf("worker heal = %+v", services[2].Heal)
	}
	payload, _ := json.Marshal(services[2])
	if strings.Contains(string(payload), `"heal"`) {
		t.Fatalf("absent heal serialized: %s", payload)
	}
	engine.AttachStatusHeal(services, nil)
	if services[0].Heal != nil || engine.HealStatusNote(nil) != "" {
		t.Fatal("old agents must leave the heal field out")
	}
}
