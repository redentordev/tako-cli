package selfheal

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/takoapi"
)

func TestRecordRoundTripAndConverging(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	record := Record{Image: "web:1", Revision: "abc123", Restart: "unless-stopped", RecordedAt: now,
		Containers: []RecordContainer{{Name: "/demo-web-1", ID: "aaa"}, {Name: "demo-web-2", ID: "bbb"}}}
	if err := WriteRecord(dataDir, "demo", "production", "web", record); err != nil {
		t.Fatal(err)
	}
	path, _ := RecordPath(dataDir, "demo", "production", "web")
	if want := filepath.Join(dataDir, "heal", "demo", "production", "services", "web.json"); path != want {
		t.Fatalf("record path = %s, want %s", path, want)
	}
	got, err := ReadRecord(dataDir, "demo", "production", "web")
	if err != nil || got == nil {
		t.Fatalf("ReadRecord = %v, %v", got, err)
	}
	if got.Containers[0].Name != "demo-web-1" || !got.Valid("web:1") || got.Valid("web:2") {
		t.Fatalf("record = %+v", got)
	}
	if err := MarkConverging(dataDir, "demo", "production", "web", "web:2", "def456", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	converging, _ := ReadRecord(dataDir, "demo", "production", "web")
	if !converging.Converging || converging.Valid("web:2") || len(converging.Containers) != 2 {
		t.Fatalf("converging record must keep containers and be invalid: %+v", converging)
	}
	if err := ClearRecord(dataDir, "demo", "production", "web"); err != nil {
		t.Fatal(err)
	}
	if missing, err := ReadRecord(dataDir, "demo", "production", "web"); err != nil || missing != nil {
		t.Fatalf("cleared record = %+v, %v", missing, err)
	}
	if err := ClearRecord(dataDir, "demo", "production", "web"); err != nil {
		t.Fatalf("clearing a missing record: %v", err)
	}
}

func TestRecordValidityRules(t *testing.T) {
	record := &Record{Image: "web:1", Revision: "r1", Containers: []RecordContainer{{Name: "c", ID: "1"}}}
	if !record.Valid("web:1") {
		t.Fatal("valid record rejected")
	}
	// An older agent deployed a new image without maintaining records.
	if record.Valid("web:2") {
		t.Fatal("record with a stale image accepted")
	}
	if (&Record{Image: "web:1"}).Valid("web:1") {
		t.Fatal("empty record accepted")
	}
	var none *Record
	if none.Valid("web:1") {
		t.Fatal("nil record accepted")
	}
}

func TestRestoreRecordRebuildsPreviousContainers(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	oldID := strings.Repeat("a", 64)
	if err := WriteRecord(dataDir, "demo", "production", "web", Record{Image: "web:1", Revision: "r1", Restart: "unless-stopped", RecordedAt: now,
		Containers: []RecordContainer{{Name: "demo-web-1", ID: oldID}}}); err != nil {
		t.Fatal(err)
	}
	// Restore on failure on this node: the record is still converging.
	if err := MarkConverging(dataDir, "demo", "production", "web", "web:2", "r2", now); err != nil {
		t.Fatal(err)
	}
	if err := RestoreRecord(dataDir, "demo", "production", "web", []RecordContainer{{Name: "demo-web-1", ID: oldID[:12]}}, now); err != nil {
		t.Fatal(err)
	}
	restored, _ := ReadRecord(dataDir, "demo", "production", "web")
	if !restored.Valid("web:1") || restored.Revision != "r1" || restored.Containers[0].ID != oldID || restored.Restart != "unless-stopped" {
		t.Fatalf("restored record = %+v", restored)
	}

	// Cross-node restore: the rollout succeeded here first.
	if err := MarkConverging(dataDir, "demo", "production", "web", "web:2", "r2", now); err != nil {
		t.Fatal(err)
	}
	if err := Converged(dataDir, "demo", "production", "web", Record{Image: "web:2", Revision: "r2", Containers: []RecordContainer{{Name: "demo-web-1", ID: strings.Repeat("b", 64)}}, RecordedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := RestoreRecord(dataDir, "demo", "production", "web", []RecordContainer{{Name: "demo-web-1", ID: oldID[:12]}}, now); err != nil {
		t.Fatal(err)
	}
	restored, _ = ReadRecord(dataDir, "demo", "production", "web")
	if !restored.Valid("web:1") || restored.Containers[0].ID != oldID {
		t.Fatalf("cross-node restored record = %+v", restored)
	}

	// A restored container that was never recorded clears the record.
	if err := MarkConverging(dataDir, "demo", "production", "web", "web:2", "r2", now); err != nil {
		t.Fatal(err)
	}
	if err := RestoreRecord(dataDir, "demo", "production", "web", []RecordContainer{{Name: "demo-web-1", ID: strings.Repeat("c", 12)}}, now); err != nil {
		t.Fatal(err)
	}
	if record, _ := ReadRecord(dataDir, "demo", "production", "web"); record != nil {
		t.Fatalf("unknown restored container kept a record: %+v", record)
	}
}

func TestStaleRecordAfterRetentionRename(t *testing.T) {
	// The record names demo-web-1 with ID aaa; after a retention rename the
	// same ID carries another name, and a new container took the name. The
	// record must match neither for an action.
	record := &Record{Image: "web:1", Containers: []RecordContainer{{Name: "demo-web-1", ID: strings.Repeat("a", 64)}}}
	if record.Contains(strings.Repeat("b", 64)) {
		t.Fatal("new container under the recorded name matched by ID")
	}
	if !record.Contains(strings.Repeat("a", 64)) {
		t.Fatal("recorded ID not found")
	}
}

func TestRecordPathRejectsTraversal(t *testing.T) {
	for _, value := range []string{"", "..", "a/b", `a\b`} {
		if _, err := RecordPath(t.TempDir(), "demo", "production", value); err == nil {
			t.Fatalf("service %q accepted", value)
		}
	}
}

func TestClearScopeAndProject(t *testing.T) {
	dataDir := t.TempDir()
	for _, env := range []string{"production", "pr-1"} {
		if err := WriteRecord(dataDir, "demo", env, "web", Record{Image: "web:1"}); err != nil {
			t.Fatal(err)
		}
		if err := AppendJournal(dataDir, "demo", env, JournalEntry{Decision: DecisionRestarted}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ClearScope(dataDir, "demo", "pr-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "heal", "demo", "pr-1")); !os.IsNotExist(err) {
		t.Fatalf("preview scope not cleared: %v", err)
	}
	envs, _ := Environments(dataDir, "demo")
	if strings.Join(envs, ",") != "production" {
		t.Fatalf("environments = %v", envs)
	}
	if err := ClearProject(dataDir, "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "heal", "demo")); !os.IsNotExist(err) {
		t.Fatalf("project not cleared: %v", err)
	}
}

func TestJournalCapsAndSanitizes(t *testing.T) {
	dataDir := t.TempDir()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var entries []JournalEntry
	for i := 0; i < MaxJournalEntries+25; i++ {
		entries = append(entries, JournalEntry{Time: base.Add(time.Duration(i) * time.Second), Decision: DecisionRestarted, Service: "web", Code: i})
	}
	if err := AppendJournal(dataDir, "demo", "production", entries...); err != nil {
		t.Fatal(err)
	}
	all, err := ReadJournal(dataDir, "demo", "production", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != MaxJournalEntries || all[0].Code != 25 {
		t.Fatalf("journal kept %d entries starting at %d", len(all), all[0].Code)
	}
	last, _ := ReadJournal(dataDir, "demo", "production", 10)
	if len(last) != 10 || last[9].Code != MaxJournalEntries+24 {
		t.Fatalf("last entries = %+v", last)
	}

	// A health error that carries container log text must never reach the
	// journal: reason and result are tokens, names are runtime names.
	leak := "HTTP health request failed: last logs:\nDATABASE_URL=postgres://user:secret@db"
	if err := AppendJournal(dataDir, "demo", "staging", JournalEntry{
		Decision: DecisionFailed, Service: "web", Container: leak, Reason: leak, Result: leak, Revision: leak,
	}); err != nil {
		t.Fatal(err)
	}
	path, _ := journalPath(dataDir, "demo", "staging")
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "secret") || strings.Contains(string(data), "logs") {
		t.Fatalf("journal leaked error text: %s", data)
	}
	staged, _ := ReadJournal(dataDir, "demo", "staging", 0)
	if staged[0].Reason != "unknown" || staged[0].Container != "unknown" || staged[0].Result != "unknown" {
		t.Fatalf("sanitized entry = %+v", staged[0])
	}
}

func TestControlPersistsAcrossRestart(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	control, err := ReadControl(dataDir, "demo", "production")
	if err != nil {
		t.Fatal(err)
	}
	candidate := Candidate{Service: "web", Container: RecordContainer{ID: "aaa", Name: "web-1"}, Reason: ReasonHTTPStatus, Window: 30 * time.Minute}
	node := &NodeState{}
	RecordAction(control, node, candidate, DecisionRestarted, now)
	until := now.Add(time.Hour)
	control.AddPause("", &until, now)
	control.Service("api").GaveUp = true
	if err := WriteControl(dataDir, "demo", "production", control); err != nil {
		t.Fatal(err)
	}
	if err := WriteNodeState(dataDir, node); err != nil {
		t.Fatal(err)
	}

	// A restarted takod reads the same budget, cooldown and pause.
	reloaded, err := ReadControl(dataDir, "demo", "production")
	if err != nil {
		t.Fatal(err)
	}
	web := reloaded.Lookup("web")
	if web == nil || len(web.Actions) != 1 || web.Containers["aaa"].Actions != 1 || !web.Containers["aaa"].CooldownUntil.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("reloaded web control = %+v", web)
	}
	if _, paused := reloaded.PauseActive("web", now); !paused {
		t.Fatal("pause lost across restart")
	}
	if _, paused := reloaded.PauseActive("web", now.Add(2*time.Hour)); paused {
		t.Fatal("expired pause still active")
	}
	if !reloaded.Lookup("api").GaveUp {
		t.Fatal("gave_up lost across restart")
	}
	reloadedNode, _ := ReadNodeState(dataDir)
	if reloadedNode.ActionsWithin(BreakerWindow, now) != 1 {
		t.Fatalf("node actions = %+v", reloadedNode)
	}
	if !reloaded.Reset("api") || reloaded.Lookup("api") != nil {
		t.Fatal("reset did not clear gave_up")
	}
	if !reloaded.Resume("", now) {
		t.Fatal("resume found no pause")
	}
}

func TestPauseTargets(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	control := &Control{}
	control.AddPause("web", nil, now)
	if _, ok := control.PauseActive("web", now.Add(1000*time.Hour)); !ok {
		t.Fatal("indefinite service pause inactive")
	}
	if _, ok := control.PauseActive("api", now); ok {
		t.Fatal("service pause covered another service")
	}
	if _, ok := control.PauseActive("", now); ok {
		t.Fatal("service pause reported as environment pause")
	}
	if !control.Resume("web", now) {
		t.Fatal("resume web")
	}
	if _, ok := control.PauseActive("web", now); ok {
		t.Fatal("pause survived resume")
	}
}

func TestPolicyMatchesDesiredStateDocument(t *testing.T) {
	document := takoapi.SelfHealingPolicyDocument{Enabled: true, RestartUnhealthy: true, StartExited: true, UnhealthyThreshold: 4, MaxRestarts: 5, WindowSeconds: 600}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	var policy Policy
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		t.Fatalf("takoapi document does not decode strictly into Policy: %v", err)
	}
	back, _ := json.Marshal(policy)
	if string(back) != string(data) {
		t.Fatalf("Policy JSON %s differs from takoapi %s", back, data)
	}
}
