package takod

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

func jsonKeys(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(document))
	for key := range document {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// TestSchedulerAlertDocumentsGolden pins the scheduler-alerts storage
// format: node-side monitoring must never add fields to it, so a downgraded
// takod keeps loading it.
func TestSchedulerAlertDocumentsGolden(t *testing.T) {
	dataDir := t.TempDir()
	manager := NewAlertManager(dataDir)
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	request := testAlertConfig(AlertPolicy{JobConsecutiveFailures: 1})
	// New fields on the request are ignored by the legacy manager.
	request.Monitors = []MonitorSpec{{Service: "web", CheckType: MonitorCheckContainer, IntervalSeconds: 60, TimeoutSeconds: 10, AlertAfter: 3, RecoverAfter: 2}}
	request.Host = &HostAlertPolicy{DiskPercent: 90}
	if err := manager.ApplyConfig(request); err != nil {
		t.Fatal(err)
	}
	if err := manager.RegisterSubject("demo", "production", "job", "worker"); err != nil {
		t.Fatal(err)
	}
	manager.deliver = func(context.Context, string, string, any) alertDeliveryResult {
		return alertDeliveryResult{Delivered: true}
	}
	observeAlertFact(t, manager, "one", JobRunStatusFailed, now.Add(time.Minute))
	manager.DispatchDue(context.Background())
	manager.mu.Lock()
	payload, _ := manager.deliveryPayloadLocked(request.legacyPart(), AlertDeliveryStatus{
		ID: "event", EpisodeID: "episode", Kind: "job", Service: "worker", Transition: "opening", Reasons: []string{"consecutive_failures"},
	}).(map[string]any)
	manager.mu.Unlock()

	root := filepath.Join(dataDir, schedulerAlertsDirName)
	if got, want := jsonKeys(t, filepath.Join(root, "config", "demo", "production.json")), []string{"credentialGeneration", "environment", "policy", "project", "providers", "schemaVersion"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("config keys = %v, want %v", got, want)
	}
	allowedState := map[string]bool{"deliveries": true, "environment": true, "evaluatedAt": true, "project": true, "retiring": true, "schemaVersion": true, "subjects": true}
	for _, key := range jsonKeys(t, filepath.Join(root, "state", "demo", "production.json")) {
		if !allowedState[key] {
			t.Fatalf("state document gained key %q", key)
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, sourceAlertsDirName)); !os.IsNotExist(err) {
		t.Fatalf("legacy manager wrote the source alert tree")
	}
	if payload == nil {
		t.Fatal("no delivery")
	}
	if payload["message"] != "Tako alert opening for demo/production job worker: consecutive_failures" {
		t.Fatalf("legacy message changed: %v", payload["message"])
	}
	for key, want := range map[string]any{"schemaVersion": AlertPayloadSchemaVersion, "kind": "job", "service": "worker", "subject": "worker", "event": "job_alert", "severity": "critical", "transition": "opening"} {
		if payload[key] != want {
			t.Errorf("payload[%s] = %v, want %v", key, payload[key], want)
		}
	}
}
