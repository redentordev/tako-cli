package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
)

func TestPostDeployCleanupSendsOldImageRemovalOnlyToCapableNodes(t *testing.T) {
	nodes := map[string]*fakePreviewNode{
		"new-1": {capabilities: []string{takod.CapabilityNodeDiskHygieneV1}},
		"old-1": {capabilities: []string{takod.CapabilityPreviewLifecycleV1}},
		"old-2": {capabilities: nil},
	}
	e, sink := newPreviewTestEngine(t)
	cfg := previewTestConfig("new-1", "old-1", "old-2")
	session := &DeploySession{
		engine:   e,
		cfg:      cfg,
		envName:  "production",
		services: map[string]config.ServiceConfig{"web": {Build: "."}},
		servers:  cfg.Servers,
	}
	session.runPostDeployCleanup(context.Background(), func(_ context.Context, server string) (any, error) {
		return nodes[server], nil
	})

	newRequests := nodes["new-1"].cleanupRequests()
	if len(newRequests) != 1 || !newRequests[0].CleanOldImages || newRequests[0].Trigger != takod.HygieneTriggerDeploy || newRequests[0].KeepImages != 3 {
		t.Fatalf("capable node cleanup = %+v", newRequests)
	}
	for _, name := range []string{"old-1", "old-2"} {
		requests := nodes[name].cleanupRequests()
		if len(requests) != 1 {
			t.Fatalf("%s: other cleanup steps must still run: %+v", name, requests)
		}
		if requests[0].CleanOldImages || requests[0].Trigger != "" {
			t.Fatalf("%s: old node was sent old-image removal: %+v", name, requests[0])
		}
		if !requests[0].CleanDanglingImages || !requests[0].CleanBuildCache || !requests[0].CleanStoppedContainers {
			t.Fatalf("%s: lost a cleanup step: %+v", name, requests[0])
		}
	}
	var warnings []string
	for _, event := range sink.ofType(events.TypeWarning) {
		if event.Level == events.LevelWarn && strings.Contains(event.Message, "old-image cleanup skipped") {
			warnings = append(warnings, event.Message)
		}
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "old-1, old-2") || !strings.Contains(warnings[0], "tako upgrade servers") {
		t.Fatalf("want one warning naming both old nodes, got %q", warnings)
	}
}

func TestPostDeployCleanupKeepsPreviewSharedServerRule(t *testing.T) {
	node := &fakePreviewNode{capabilities: []string{takod.CapabilityNodeDiskHygieneV1}}
	e, sink := newPreviewTestEngine(t)
	cfg := previewTestConfig("app-1")
	session := &DeploySession{
		engine:   e,
		cfg:      cfg,
		envName:  "preview-pr-1",
		services: map[string]config.ServiceConfig{"web": {Build: "."}},
		servers:  cfg.Servers,
		preview:  &previewDeployState{shared: map[string]bool{"app-1": true}},
	}
	session.runPostDeployCleanup(context.Background(), func(context.Context, string) (any, error) { return node, nil })
	requests := node.cleanupRequests()
	if len(requests) != 1 || requests[0].CleanOldImages {
		t.Fatalf("preview on a shared server must not prune images: %+v", requests)
	}
	if len(sink.ofType(events.TypeWarning)) != 0 {
		t.Fatalf("no upgrade warning for a preview skip: %+v", sink.ofType(events.TypeWarning))
	}
}
