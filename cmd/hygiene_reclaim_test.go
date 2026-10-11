package cmd

import (
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

func TestHygieneReclaimEventRetrying(t *testing.T) {
	event := hygieneReclaimEvent(takodclient.ReclaimEvent{Server: "web-1", Endpoint: "/v1/service-files", BytesFreed: 3435973837, Retrying: true})
	if event.Type != events.TypeHygieneReclaimed || event.Phase != events.PhasePreflight || event.Level != events.LevelInfo {
		t.Fatalf("event = %+v", event)
	}
	if event.Message != "Disk low on web-1: reclaimed 3.2 GB, retrying…\n" {
		t.Fatalf("message = %q", event.Message)
	}
	if event.Data["bytesFreed"] != int64(3435973837) || event.Data["retrying"] != true || event.Data["endpoint"] != "/v1/service-files" {
		t.Fatalf("data = %+v", event.Data)
	}
}

func TestHygieneReclaimEventNotRetried(t *testing.T) {
	event := hygieneReclaimEvent(takodclient.ReclaimEvent{Server: "web-1", BytesFreed: 1024})
	if event.Level != events.LevelWarn || !strings.Contains(event.Message, "re-run the command") {
		t.Fatalf("event = %+v", event)
	}
}
