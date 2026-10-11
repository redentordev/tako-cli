package takod

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/selfheal"
)

const (
	healOldShortID = "0123456789ab"
	healOldFullID  = healOldShortID + "0123456789abcdef0123456789abcdef0123456789abcdef0123"
)

var healNewID = strings.Repeat("e", 64)

// newHealRecordFixture is the restore-on-failure fixture with a recorded
// previous container whose short ID docker ps reports.
func newHealRecordFixture(t *testing.T) retainFixture {
	t.Helper()
	fixture := newRetainFixture(t)
	t.Setenv("TAKO_FAKE_PS_OUTPUT", healOldShortID+"\n")
	inspect, _ := json.Marshal(map[string]string{
		healOldShortID: retainInspectLine(fixture.liveName, true, "always", serviceLabels("1", "rev-old")),
	})
	t.Setenv("TAKO_FAKE_RETAIN_INSPECT_BY_ID", string(inspect))
	t.Setenv("TAKO_FAKE_DOCKER_RUN_ID", healNewID)
	if err := selfheal.WriteRecord(fixture.dataDir, "demo", "production", "web", selfheal.Record{
		Image: "registry.example.com/demo/web:old", Revision: "rev-old", Restart: "always", RecordedAt: time.Now(),
		Containers: []selfheal.RecordContainer{{Name: fixture.liveName, ID: healOldFullID}},
	}); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func readHealRecord(t *testing.T, dataDir string) *selfheal.Record {
	t.Helper()
	record, err := selfheal.ReadRecord(dataDir, "demo", "production", "web")
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestHealRecordWrittenOnReconcileSuccess(t *testing.T) {
	fixture := newHealRecordFixture(t)
	request := fixture.request()
	request.FailurePolicy = ""
	if _, err := ReconcileService(context.Background(), request); err != nil {
		t.Fatalf("ReconcileService: %v", err)
	}
	record := readHealRecord(t, fixture.dataDir)
	if !record.Valid(request.Image) || record.Revision != "rev-new" || len(record.Containers) != 1 ||
		record.Containers[0].ID != healNewID || record.Containers[0].Name != fixture.liveName || record.Restart != "always" {
		t.Fatalf("record after success = %+v", record)
	}
}

func TestHealRecordStaysConvergingOnFailure(t *testing.T) {
	fixture := newHealRecordFixture(t)
	request := fixture.request()
	request.FailurePolicy = ""
	request.Health = unhealthyCommandHealth(t)
	if _, err := ReconcileService(context.Background(), request); err == nil {
		t.Fatal("unhealthy reconcile succeeded")
	}
	record := readHealRecord(t, fixture.dataDir)
	if record == nil || !record.Converging || record.Valid(request.Image) || record.Valid("registry.example.com/demo/web:old") {
		t.Fatalf("record after failure = %+v, want converging", record)
	}
}

func TestHealRecordStaysConvergingOnRetainFailed(t *testing.T) {
	fixture := newHealRecordFixture(t)
	request := fixture.request()
	request.FailurePolicy = FailurePolicyRetainFailed
	request.Health = unhealthyCommandHealth(t)
	if _, err := ReconcileService(context.Background(), request); err == nil {
		t.Fatal("unhealthy reconcile succeeded")
	}
	if record := readHealRecord(t, fixture.dataDir); record == nil || !record.Converging {
		t.Fatalf("record after retain-failed = %+v, want converging", record)
	}
}

func TestHealRecordRebuiltOnLocalRestore(t *testing.T) {
	fixture := newHealRecordFixture(t)
	request := fixture.request()
	request.Health = unhealthyCommandHealth(t)
	_, err := ReconcileService(context.Background(), request)
	var failure *ReconcileFailure
	if !errors.As(err, &failure) || !failure.Restored {
		t.Fatalf("ReconcileService error = %v, want restored failure", err)
	}
	record := readHealRecord(t, fixture.dataDir)
	if !record.Valid("registry.example.com/demo/web:old") || record.Revision != "rev-old" || record.Containers[0].ID != healOldFullID {
		t.Fatalf("record after local restore = %+v", record)
	}
}

func TestHealRecordFinalizeAndCrossNodeRestore(t *testing.T) {
	fixture := newHealRecordFixture(t)
	request := fixture.request()
	response, err := ReconcileService(context.Background(), request)
	if err != nil {
		t.Fatalf("ReconcileService: %v", err)
	}
	if record := readHealRecord(t, fixture.dataDir); !record.Valid(request.Image) {
		t.Fatalf("record after retained success = %+v", record)
	}
	recovery := ServiceRecoveryRequest{Project: "demo", Environment: "production", Service: "web", Revision: "rev-new", Token: response.RestoreToken}
	if _, err := RestoreRetainedService(context.Background(), fixture.dataDir, recovery); err != nil {
		t.Fatalf("RestoreRetainedService: %v", err)
	}
	record := readHealRecord(t, fixture.dataDir)
	if !record.Valid("registry.example.com/demo/web:old") || record.Containers[0].ID != healOldFullID {
		t.Fatalf("record after cross-node restore = %+v", record)
	}

	// Finalize keeps the record written at success.
	fixture2 := newHealRecordFixture(t)
	response, err = ReconcileService(context.Background(), fixture2.request())
	if err != nil {
		t.Fatalf("ReconcileService: %v", err)
	}
	recovery.Token = response.RestoreToken
	if _, err := FinalizeRetainedService(context.Background(), fixture2.dataDir, recovery); err != nil {
		t.Fatalf("FinalizeRetainedService: %v", err)
	}
	if record := readHealRecord(t, fixture2.dataDir); !record.Valid(request.Image) || record.Containers[0].ID != healNewID {
		t.Fatalf("record after finalize = %+v", record)
	}
}

func TestHealRecordClearedOnScaleToZeroAndRemove(t *testing.T) {
	fixture := newHealRecordFixture(t)
	request := fixture.request()
	request.FailurePolicy = ""
	request.Containers = nil
	if _, err := ReconcileService(context.Background(), request); err != nil {
		t.Fatalf("scale to zero: %v", err)
	}
	if record := readHealRecord(t, fixture.dataDir); record != nil {
		t.Fatalf("record after scale to zero = %+v", record)
	}

	fixture = newHealRecordFixture(t)
	if _, err := RemoveService(context.Background(), RemoveServiceRequest{Project: "demo", Environment: "production", Service: "web", KeepRevision: "rev-old", DataDir: fixture.dataDir}); err != nil {
		t.Fatalf("RemoveService keep: %v", err)
	}
	if record := readHealRecord(t, fixture.dataDir); record == nil {
		t.Fatal("rolling cleanup of older revisions cleared the kept revision's record")
	}
	if _, err := RemoveService(context.Background(), RemoveServiceRequest{Project: "demo", Environment: "production", Service: "web", DataDir: fixture.dataDir}); err != nil {
		t.Fatalf("RemoveService: %v", err)
	}
	if record := readHealRecord(t, fixture.dataDir); record != nil {
		t.Fatalf("record after removal = %+v", record)
	}
}

func TestHealRecordUntouchedByWarmOnlyReconcile(t *testing.T) {
	fixture := newHealRecordFixture(t)
	request := fixture.request()
	request.FailurePolicy = ""
	request.DeployStrategy = "blue_green"
	request.Containers[0].Labels["tako.active"] = "false"
	request.Containers[0].Name = fixture.liveName + "-green"
	before := readHealRecord(t, fixture.dataDir)
	if _, err := ReconcileService(context.Background(), request); err != nil {
		t.Fatalf("warm reconcile: %v", err)
	}
	after := readHealRecord(t, fixture.dataDir)
	if after == nil || after.Converging || after.Revision != before.Revision || after.Containers[0].ID != before.Containers[0].ID {
		t.Fatalf("warm-only reconcile changed the record: %+v", after)
	}
}

func TestHealRecordClearedOnCleanup(t *testing.T) {
	fixture := newHealRecordFixture(t)
	if _, err := CleanupProjectWithDataDir(context.Background(), fixture.dataDir, CleanupRequest{Project: "demo", Environment: "production", RemoveContainers: true}); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if record := readHealRecord(t, fixture.dataDir); record != nil {
		t.Fatalf("record after environment cleanup = %+v", record)
	}
}

func TestDockerRunContainerID(t *testing.T) {
	cases := map[string]string{
		healNewID + "\n": healNewID,
		"Unable to find image\nPulling\n" + healNewID + "\n": healNewID,
		"container-id\n": "",
		"":               "",
	}
	for output, want := range cases {
		if got := dockerRunContainerID(output); got != want {
			t.Fatalf("dockerRunContainerID(%q) = %q, want %q", output, got, want)
		}
	}
}
