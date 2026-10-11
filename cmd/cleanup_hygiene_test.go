package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/takod"
)

// TestCleanupActionResultHygieneGolden pins the additive per-server
// `hygiene` object of the cleanup ActionResult.
func TestCleanupActionResultHygieneGolden(t *testing.T) {
	results := []cleanupNodeResult{
		{serverName: "node-a", host: "10.0.0.1", response: &takod.CleanupResponse{Hygiene: &takod.CleanupHygiene{
			Removed:    []takod.HygieneImage{{ID: "sha256:aaa", Refs: []string{"demo/web:production-r1"}, SizeBytes: 1048576}},
			Kept:       map[string]int{"current": 1, "retention": 2},
			BytesFreed: 1048576,
			DryRun:     true,
		}}},
		{serverName: "node-b", host: "10.0.0.2", response: &takod.CleanupResponse{}, notices: []string{"old-image removal skipped on node-b: node agent needs `tako upgrade servers`"}},
		{serverName: "node-c", host: "10.0.0.3", err: errors.New("dry run not supported on node-c: node agent needs `tako upgrade servers`")},
	}
	ack := cleanupActionResult("demo", "production", results, 1)
	payload, err := json.MarshalIndent(ack, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "ActionResult",
  "project": "demo",
  "environment": "production",
  "action": "cleanup",
  "outcome": "partial",
  "servers": [
    {
      "server": "node-a",
      "host": "10.0.0.1",
      "done": true,
      "hygiene": {
        "removed": [
          {
            "id": "sha256:aaa",
            "refs": [
              "demo/web:production-r1"
            ],
            "sizeBytes": 1048576
          }
        ],
        "kept": {
          "current": 1,
          "retention": 2
        },
        "bytesFreed": 1048576,
        "dryRun": true
      }
    },
    {
      "server": "node-b",
      "host": "10.0.0.2",
      "done": true,
      "warnings": [
        "old-image removal skipped on node-b: node agent needs ` + "`tako upgrade servers`" + `"
      ]
    },
    {
      "server": "node-c",
      "host": "10.0.0.3",
      "done": false,
      "error": "dry run not supported on node-c: node agent needs ` + "`tako upgrade servers`" + `"
    }
  ]
}`
	if string(payload) != want {
		t.Fatalf("cleanup action result drifted:\n%s", payload)
	}
}

func TestCleanupActionResultNoticesDoNotMakeTheRunPartial(t *testing.T) {
	results := []cleanupNodeResult{{serverName: "node-b", response: &takod.CleanupResponse{}, notices: []string{"old-image removal skipped"}}}
	if ack := cleanupActionResult("demo", "production", results, 0); ack.Outcome != engine.ActionOutcomeOK {
		t.Fatalf("outcome = %s", ack.Outcome)
	}
}

func TestPrintCleanupHygieneListsRemovedAndKeptByReason(t *testing.T) {
	var out bytes.Buffer
	printCleanupHygiene(&out, &takod.CleanupHygiene{
		Removed: []takod.HygieneImage{{ID: "sha256:aaa", Refs: []string{"demo/web:production-r1"}, SizeBytes: 2048}, {ID: "sha256:bbb"}},
		Kept:    map[string]int{"retention": 3, "in use": 1},
		DryRun:  true,
	})
	want := "  Would remove 2 old image(s):\n    - demo/web:production-r1 (2.0 KB)\n    - sha256:bbb\n  Kept: 1 in use, 3 retention\n"
	if out.String() != want {
		t.Fatalf("output:\n%q\nwant:\n%q", out.String(), want)
	}
	out.Reset()
	printCleanupHygiene(&out, &takod.CleanupHygiene{BytesFreed: 4096, Removed: []takod.HygieneImage{{ID: "sha256:ccc"}}})
	if out.String() != "  Removed 1 old image(s):\n    - sha256:ccc\n  Freed 4.0 KB\n" {
		t.Fatalf("output = %q", out.String())
	}
}
