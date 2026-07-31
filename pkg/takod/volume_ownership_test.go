package takod

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestManagedVolumeOwnershipInitializesFreshVolumeOnce(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()
	stateFile := filepath.Join(t.TempDir(), "volume-state")
	markerFile := filepath.Join(t.TempDir(), "marker")
	t.Setenv("TAKO_FAKE_VOLUME_OWNERSHIP_STATE_FILE", stateFile)
	t.Setenv("TAKO_FAKE_VOLUME_OWNERSHIP_MARKER_FILE", markerFile)
	dataDir := t.TempDir()
	policy := ManagedVolumeOwnership{Name: "tako_demo_production_data", UID: 1000, GID: 1001}
	for range 2 {
		if err := ensureManagedVolumeOwnership(context.Background(), dataDir, "demo", "production", "web", policy); err != nil {
			t.Fatal(err)
		}
	}
	commands := strings.Join(readCommandLog(t, logPath), "\n")
	if strings.Count(commands, "volume create") != 1 || strings.Count(commands, "chown 1000:1001 /volume") != 1 {
		t.Fatalf("ownership initialization was not creation-only:\n%s", commands)
	}
	journal, err := readVolumeOwnershipJournal(volumeOwnershipJournalPath(filepath.Join(dataDir, volumeOwnershipDirName), policy.Name))
	if err != nil || journal.Phase != "initialized" {
		t.Fatalf("ownership journal = %#v, err=%v", journal, err)
	}
}

func TestManagedVolumeOwnershipNeverTouchesPreexistingVolume(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()
	t.Setenv("TAKO_FAKE_VOLUME_OWNERSHIP_INSPECT", "local|<no value>\n")
	dataDir := t.TempDir()
	policy := ManagedVolumeOwnership{Name: "shared-data", UID: 1000, GID: 1000}
	if err := ensureManagedVolumeOwnership(context.Background(), dataDir, "demo", "production", "web", policy); err != nil {
		t.Fatal(err)
	}
	commands := strings.Join(readCommandLog(t, logPath), "\n")
	if strings.Contains(commands, "volume create") || strings.Contains(commands, "chown ") {
		t.Fatalf("preexisting volume was mutated:\n%s", commands)
	}
	journal, err := readVolumeOwnershipJournal(volumeOwnershipJournalPath(filepath.Join(dataDir, volumeOwnershipDirName), policy.Name))
	if err != nil || journal.Phase != "preexisting" {
		t.Fatalf("preexisting journal = %#v, err=%v", journal, err)
	}
}

func TestManagedVolumeOwnershipFailsIfPreexistingVolumeDisappears(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()
	t.Setenv("TAKO_FAKE_VOLUME_OWNERSHIP_INSPECT", "local|<no value>\n")
	dataDir := t.TempDir()
	policy := ManagedVolumeOwnership{Name: "shared-data", UID: 1000, GID: 1000}
	if err := ensureManagedVolumeOwnership(context.Background(), dataDir, "demo", "production", "web", policy); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TAKO_FAKE_VOLUME_OWNERSHIP_STATE_FILE", filepath.Join(t.TempDir(), "missing"))
	err := ensureManagedVolumeOwnership(context.Background(), dataDir, "demo", "production", "web", policy)
	if err == nil || !strings.Contains(err.Error(), "disappeared") {
		t.Fatalf("missing preexisting volume error = %v", err)
	}
	commands := strings.Join(readCommandLog(t, logPath), "\n")
	if strings.Contains(commands, "volume create") || strings.Contains(commands, "chown ") {
		t.Fatalf("missing preexisting volume was recreated or mutated:\n%s", commands)
	}
}

func TestManagedVolumeOwnershipRejectsCreationTokenMismatch(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()
	t.Setenv("TAKO_FAKE_VOLUME_OWNERSHIP_INSPECT", "local|different-token\n")
	dataDir := t.TempDir()
	policy := ManagedVolumeOwnership{Name: "managed-data", UID: 1000, GID: 1000}
	root := filepath.Join(dataDir, volumeOwnershipDirName)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeVolumeOwnershipJournal(volumeOwnershipJournalPath(root, policy.Name), volumeOwnershipJournal{
		SchemaVersion: 1, Project: "demo", Environment: "production", Service: "web", Volume: policy.Name,
		UID: policy.UID, GID: policy.GID, OperationID: "expected-token", Phase: "created",
	}); err != nil {
		t.Fatal(err)
	}
	err := ensureManagedVolumeOwnership(context.Background(), dataDir, "demo", "production", "web", policy)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("token mismatch error = %v", err)
	}
	if commands := strings.Join(readCommandLog(t, logPath), "\n"); strings.Contains(commands, "chown ") {
		t.Fatalf("token mismatch mutated volume:\n%s", commands)
	}
}

func TestManagedVolumeOwnershipRejectsReplacementBeforePinnedHelperStarts(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()
	stateFile := filepath.Join(t.TempDir(), "volume-state")
	if err := os.WriteFile(stateFile, []byte("local|expected-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TAKO_FAKE_VOLUME_OWNERSHIP_STATE_FILE", stateFile)
	t.Setenv("TAKO_FAKE_VOLUME_REPLACE_ON_HELPER_CREATE", "replacement-token")
	dataDir := t.TempDir()
	root := filepath.Join(dataDir, volumeOwnershipDirName)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeVolumeOwnershipJournal(volumeOwnershipJournalPath(root, "managed-data"), volumeOwnershipJournal{
		SchemaVersion: 1, Project: "demo", Environment: "production", Service: "web", Volume: "managed-data",
		UID: 1000, GID: 1000, OperationID: "expected-token", Phase: "created",
	}); err != nil {
		t.Fatal(err)
	}
	err := ensureManagedVolumeOwnership(context.Background(), dataDir, "demo", "production", "web", ManagedVolumeOwnership{Name: "managed-data", UID: 1000, GID: 1000})
	if err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("replacement race error = %v", err)
	}
	commands := strings.Join(readCommandLog(t, logPath), "\n")
	if strings.Contains(commands, "docker start -a") {
		t.Fatalf("ownership helper started against replacement volume:\n%s", commands)
	}
}

func TestManagedVolumeOwnershipSerializesConcurrentCreation(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()
	t.Setenv("TAKO_FAKE_VOLUME_OWNERSHIP_STATE_FILE", filepath.Join(t.TempDir(), "volume-state"))
	t.Setenv("TAKO_FAKE_VOLUME_OWNERSHIP_MARKER_FILE", filepath.Join(t.TempDir(), "marker"))
	dataDir := t.TempDir()
	policy := ManagedVolumeOwnership{Name: "managed-data", UID: 1000, GID: 1000}
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errors <- ensureManagedVolumeOwnership(context.Background(), dataDir, "demo", "production", "web", policy)
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	commands := strings.Join(readCommandLog(t, logPath), "\n")
	if strings.Count(commands, "volume create") != 1 || strings.Count(commands, "chown 1000:1000 /volume") != 1 {
		t.Fatalf("concurrent ownership initialization duplicated:\n%s", commands)
	}
}

func TestRecoverManagedVolumeOwnershipCompletesCreationIntent(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()
	t.Setenv("TAKO_FAKE_VOLUME_OWNERSHIP_STATE_FILE", filepath.Join(t.TempDir(), "volume-state"))
	t.Setenv("TAKO_FAKE_VOLUME_OWNERSHIP_MARKER_FILE", filepath.Join(t.TempDir(), "marker"))
	dataDir := t.TempDir()
	root := filepath.Join(dataDir, volumeOwnershipDirName)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	journalPath := volumeOwnershipJournalPath(root, "managed-data")
	if err := writeVolumeOwnershipJournal(journalPath, volumeOwnershipJournal{
		SchemaVersion: 1, Project: "demo", Environment: "production", Service: "web", Volume: "managed-data",
		UID: 1000, GID: 1001, OperationID: "expected-token", Phase: "intent",
	}); err != nil {
		t.Fatal(err)
	}
	if err := RecoverManagedVolumeOwnership(context.Background(), dataDir); err != nil {
		t.Fatal(err)
	}
	journal, err := readVolumeOwnershipJournal(journalPath)
	if err != nil || journal.Phase != "initialized" {
		t.Fatalf("recovered journal = %#v, err=%v", journal, err)
	}
	commands := strings.Join(readCommandLog(t, logPath), "\n")
	if strings.Count(commands, "volume create") != 1 || strings.Count(commands, "chown 1000:1001 /volume") != 1 {
		t.Fatalf("recovery did not finish ownership initialization exactly once:\n%s", commands)
	}
}

func TestManagedVolumeOwnershipRecoversAfterChownBeforeJournalWithoutWritingVolumeFiles(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()
	stateFile := filepath.Join(t.TempDir(), "volume-state")
	if err := os.WriteFile(stateFile, []byte("local|expected-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TAKO_FAKE_VOLUME_OWNERSHIP_STATE_FILE", stateFile)
	dataDir := t.TempDir()
	root := filepath.Join(dataDir, volumeOwnershipDirName)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	journalPath := volumeOwnershipJournalPath(root, "managed-data")
	if err := writeVolumeOwnershipJournal(journalPath, volumeOwnershipJournal{
		SchemaVersion: 1, Project: "demo", Environment: "production", Service: "web", Volume: "managed-data",
		UID: 1000, GID: 1001, OperationID: "expected-token", Phase: "created",
	}); err != nil {
		t.Fatal(err)
	}
	if err := ensureManagedVolumeOwnership(context.Background(), dataDir, "demo", "production", "web", ManagedVolumeOwnership{Name: "managed-data", UID: 1000, GID: 1001}); err != nil {
		t.Fatal(err)
	}
	commands := strings.Join(readCommandLog(t, logPath), "\n")
	if !strings.Contains(commands, `stat -c '%u:%g' /volume`) || !strings.Contains(commands, `|| chown 1000:1001 /volume`) {
		t.Fatalf("recovery helper was not conditionally idempotent:\n%s", commands)
	}
	if strings.Contains(commands, ".tako-volume-ownership") || strings.Contains(commands, "printf") {
		t.Fatalf("ownership evidence was written into the application volume:\n%s", commands)
	}
	journal, err := readVolumeOwnershipJournal(journalPath)
	if err != nil || journal.Phase != "initialized" {
		t.Fatalf("recovered journal = %#v, err=%v", journal, err)
	}
}

func TestReadVolumeOwnershipJournalRejectsUnsafeOperationID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	data := `{"schemaVersion":1,"project":"demo","environment":"production","volume":"managed-data","uid":1000,"gid":1000,"operationId":"token; chown 0:0 /","phase":"created"}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readVolumeOwnershipJournal(path); err == nil {
		t.Fatal("unsafe operation identity was accepted")
	}
}

func TestManagedVolumeOwnershipIntegrationLeavesFreshVolumeEmpty(t *testing.T) {
	if os.Getenv("TAKO_VOLUME_OWNERSHIP_INTEGRATION") != "1" {
		t.Skip("set TAKO_VOLUME_OWNERSHIP_INTEGRATION=1 to run Docker volume integration")
	}
	identity, err := newBackupArtifactID()
	if err != nil {
		t.Fatal(err)
	}
	volume := "tako-ownership-integration-" + strings.TrimPrefix(identity, "artifact-")
	defer func() { _ = cleanupVerificationDockerResource("volume", volume) }()
	if err := ensureManagedVolumeOwnership(context.Background(), t.TempDir(), "integration", "production", "postgres", ManagedVolumeOwnership{Name: volume, UID: 999, GID: 999}); err != nil {
		t.Fatal(err)
	}
	output, err := runDocker(context.Background(), "run", "--rm", "-v", volume+":/volume:ro", backupImage, "sh", "-c", "test -z \"$(ls -A /volume)\"")
	if err != nil {
		t.Fatalf("fresh ownership-managed volume was not empty: %s: %v", output, err)
	}
}
