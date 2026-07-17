package platform

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidatePlatformAccountFieldsRejectsPrivilegedOrInteractiveUsers(t *testing.T) {
	group := []string{"tako-platform", "x", "998", ""}
	tests := []struct {
		name   string
		passwd []string
	}{
		{name: "root uid", passwd: []string{"root", "x", "0", "998", "", DefaultStateDir, "/usr/sbin/nologin"}},
		{name: "regular uid", passwd: []string{"worker", "x", "1000", "998", "", DefaultStateDir, "/usr/sbin/nologin"}},
		{name: "wrong primary group", passwd: []string{"worker", "x", "997", "999", "", DefaultStateDir, "/usr/sbin/nologin"}},
		{name: "interactive shell", passwd: []string{"worker", "x", "997", "998", "", DefaultStateDir, "/bin/bash"}},
		{name: "wrong home", passwd: []string{"worker", "x", "997", "998", "", "/root", "/usr/sbin/nologin"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validatePlatformAccountFields("worker", "tako-platform", test.passwd, group); err == nil {
				t.Fatal("hostile existing account was accepted")
			}
		})
	}
	if err := validatePlatformAccountFields("worker", "tako-platform", []string{"worker", "x", "997", "998", "", DefaultStateDir, "/usr/sbin/nologin"}, group); err != nil {
		t.Fatalf("valid dedicated account rejected: %v", err)
	}
}

func TestValidateUniqueNumericGroupRecordsRejectsAliasesAndRoot(t *testing.T) {
	valid := []string{"root:x:0:", "tako-platform:x:998:", "tako:x:996:alice"}
	if err := validateUniqueNumericGroupRecords(valid, "tako-platform", "tako"); err != nil {
		t.Fatalf("valid groups rejected: %v", err)
	}
	aliased := append(valid, "docker:x:996:alice")
	if err := validateUniqueNumericGroupRecords(aliased, "tako-platform", "tako"); err == nil {
		t.Fatal("duplicate numeric GID alias was accepted")
	}
	if err := validateUniqueNumericGroupRecords([]string{"tako-platform:x:998:", "tako:x:0:"}, "tako-platform", "tako"); err == nil {
		t.Fatal("root GID was accepted")
	}
}

func TestValidateDedicatedWorkerGroupRejectsOtherPrincipals(t *testing.T) {
	passwd := []string{"root:x:0:0:root:/root:/bin/bash", "worker:x:997:998::/var/lib/tako/platform:/usr/sbin/nologin"}
	if err := validateDedicatedWorkerGroupRecords("tako-platform", "worker", []string{"tako-platform", "x", "998", "worker"}, passwd); err != nil {
		t.Fatalf("dedicated group rejected: %v", err)
	}
	if err := validateDedicatedWorkerGroupRecords("tako-platform", "worker", []string{"tako-platform", "x", "998", "worker,alice"}, passwd); err == nil {
		t.Fatal("shared supplementary worker group was accepted")
	}
	passwd = append(passwd, "bob:x:995:998::/nonexistent:/usr/sbin/nologin")
	if err := validateDedicatedWorkerGroupRecords("tako-platform", "worker", []string{"tako-platform", "x", "998", "worker"}, passwd); err == nil {
		t.Fatal("shared primary worker group was accepted")
	}
}

func TestValidateWorkerNumericGroupsRejectsPrivilegeInheritance(t *testing.T) {
	if err := validateWorkerNumericGroups([]int{998, 996}, 998, 996); err != nil {
		t.Fatalf("dedicated group set rejected: %v", err)
	}
	if err := validateWorkerNumericGroups([]int{998, 996, 999}, 998, 996); err == nil {
		t.Fatal("unexpected Docker group membership was accepted")
	}
	if err := validateWorkerNumericGroups([]int{998, 996, 0}, 998, 996); err == nil {
		t.Fatal("privileged group membership was accepted")
	}
}

func TestValidateWorkerNumericGroupsBeforeGrantRejectsBeforeMutation(t *testing.T) {
	if err := validateWorkerNumericGroupsBeforeGrant([]int{998}, 998, 996); err != nil {
		t.Fatalf("worker awaiting socket grant was rejected: %v", err)
	}
	if err := validateWorkerNumericGroupsBeforeGrant([]int{998, 996}, 998, 996); err != nil {
		t.Fatalf("already granted worker was rejected: %v", err)
	}
	if err := validateWorkerNumericGroupsBeforeGrant([]int{998, 999}, 998, 996); err == nil {
		t.Fatal("hostile existing membership was accepted before usermod")
	}
}

func TestValidateDockerDataRootPinsTheDaemonFilesystem(t *testing.T) {
	root, err := validateDockerDataRoot("/srv/docker", "")
	if err != nil || root != "/srv/docker" {
		t.Fatalf("Docker root = %q, %v", root, err)
	}
	if _, err := validateDockerDataRoot("/srv/docker", "/var/lib/docker"); err == nil {
		t.Fatal("changed Docker data root was accepted")
	}
	if _, err := validateDockerDataRoot("relative", ""); err == nil {
		t.Fatal("relative Docker data root was accepted")
	}
}

func TestHasWorkerReadyRecordRequiresMatchingFreshNodeRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultJournalName)
	since := time.Now().UTC()
	records := []OperationRecord{
		{OperationID: "old", Operation: "platform.worker", Phase: "ready", Status: "completed", NodeID: "node-a", Timestamp: since.Add(-time.Second)},
		{OperationID: "wrong", Operation: "platform.worker", Phase: "ready", Status: "completed", NodeID: "node-b", Timestamp: since.Add(time.Second)},
		{OperationID: "fresh", Operation: "platform.worker", Phase: "ready", Status: "completed", NodeID: "node-a", Timestamp: since.Add(time.Second)},
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if err := json.NewEncoder(file).Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	_ = file.Close()
	ready, err := hasWorkerReadyRecord(path, "node-a", since)
	if err != nil || !ready {
		t.Fatalf("ready = %v, %v", ready, err)
	}
}

func TestHasWorkerReadyRecordIgnoresTornFinalFrame(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultJournalName)
	now := time.Now().UTC()
	record := OperationRecord{OperationID: "fresh", Operation: "platform.worker", Phase: "ready", Status: "completed", NodeID: "node-a", Timestamp: now}
	data, _ := json.Marshal(record)
	data = append(append(data, '\n'), []byte(`{"operationId":"torn"`)...)
	if err := os.WriteFile(path, data, 0640); err != nil {
		t.Fatal(err)
	}
	ready, err := hasWorkerReadyRecord(path, "node-a", now.Add(-time.Second))
	if err != nil || !ready {
		t.Fatalf("ready = %v, %v", ready, err)
	}
}

func TestHasWorkerReadyRecordAcceptsLargeValidPrecedingFrame(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultJournalName)
	now := time.Now().UTC()
	journal, _ := NewJournal(path)
	if err := journal.Append(OperationRecord{OperationID: "large", Operation: "deploy", Phase: "failed", Status: "failed", Message: strings.Repeat("x", 128<<10), Timestamp: now}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Append(OperationRecord{OperationID: "ready", Operation: "platform.worker", Phase: "ready", Status: "completed", NodeID: "node-a", Timestamp: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	ready, err := hasWorkerReadyRecord(path, "node-a", now)
	if err != nil || !ready {
		t.Fatalf("large valid frame blocked readiness: ready=%v err=%v", ready, err)
	}
}
