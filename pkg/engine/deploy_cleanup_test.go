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

func TestCleanupNodeAdaptsToNodeCapabilities(t *testing.T) {
	cfg := previewTestConfig("new-1", "old-1")
	request := takod.CleanupRequest{
		Project:                "demo",
		Environment:            "production",
		ImageRepositories:      []string{"demo/web"},
		KeepImages:             5,
		CleanOldImages:         true,
		CleanStoppedContainers: true,
		CleanUnusedVolumes:     true,
	}
	capable := &fakePreviewNode{capabilities: []string{takod.CapabilityNodeDiskHygieneV1}}
	old := &fakePreviewNode{capabilities: []string{takod.CapabilityPreviewLifecycleV1}}

	_, notices, err := CleanupNode(context.Background(), old, cfg, "old-1", request, false)
	if err != nil || len(notices) != 1 || !strings.Contains(notices[0], "old-image removal skipped on old-1") || !strings.Contains(notices[0], "tako upgrade servers") {
		t.Fatalf("old node: notices=%q err=%v", notices, err)
	}
	sent := old.cleanupRequests()
	if len(sent) != 1 || sent[0].CleanOldImages || !sent[0].CleanStoppedContainers || !sent[0].CleanUnusedVolumes || sent[0].Trigger != "" {
		t.Fatalf("old node request = %+v", sent)
	}

	_, notices, err = CleanupNode(context.Background(), capable, cfg, "new-1", request, false)
	sent = capable.cleanupRequests()
	if err != nil || len(notices) != 0 || len(sent) != 1 || !sent[0].CleanOldImages || sent[0].KeepImages != 5 || sent[0].Trigger != takod.HygieneTriggerCleanup {
		t.Fatalf("capable node: notices=%q err=%v request=%+v", notices, err, sent)
	}
}

func TestCleanupNodeDryRun(t *testing.T) {
	cfg := previewTestConfig("new-1", "old-1")
	request := takod.CleanupRequest{
		Project:                "demo",
		Environment:            "production",
		ImageRepositories:      []string{"demo/web"},
		KeepImages:             3,
		CleanOldImages:         true,
		CleanStoppedContainers: true,
		CleanUnusedVolumes:     true,
		SecureLogPermissions:   true,
	}
	old := &fakePreviewNode{}
	if _, _, err := CleanupNode(context.Background(), old, cfg, "old-1", request, true); err == nil || !strings.Contains(err.Error(), "dry run not supported on old-1") {
		t.Fatalf("old node dry run error = %v", err)
	}
	if len(old.cleanupRequests()) != 0 {
		t.Fatalf("an old node must receive no cleanup request in a dry run: %+v", old.cleanupRequests())
	}
	capable := &fakePreviewNode{capabilities: []string{takod.CapabilityNodeDiskHygieneV1}}
	if _, _, err := CleanupNode(context.Background(), capable, cfg, "new-1", request, true); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	sent := capable.cleanupRequests()
	if len(sent) != 1 || !sent[0].DryRun || !sent[0].CleanOldImages || sent[0].CleanStoppedContainers || sent[0].CleanUnusedVolumes || sent[0].SecureLogPermissions {
		t.Fatalf("dry run must send only an old-image dry-run request: %+v", sent)
	}
}
