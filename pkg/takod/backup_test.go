package takod

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

func TestListVolumeBackupsParsesAndSortsFiles(t *testing.T) {
	restore := useTempBackupRoot(t)
	defer restore()

	request := BackupRequest{Project: "demo", Environment: "production"}
	dir := backupDirectory(request)
	if err := os.MkdirAll(dir, 0750); err != nil {
		t.Fatalf("failed to create backup dir: %v", err)
	}
	for _, name := range []string{
		"db_data_20240101-120000.tar.gz",
		"db_data_20240102-120000.tar.gz",
		"cache_20240101-130000.tar.gz",
		"ignore.txt",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("backup"), 0600); err != nil {
			t.Fatalf("failed to write backup fixture: %v", err)
		}
	}

	response, err := ListVolumeBackups(context.Background(), request)
	if err != nil {
		t.Fatalf("ListVolumeBackups returned error: %v", err)
	}
	var got []string
	for _, backup := range response.Backups {
		got = append(got, backup.Volume+":"+backup.ID)
	}
	want := []string{
		"cache:20240101-130000",
		"db_data:20240102-120000",
		"db_data:20240101-120000",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("backups = %#v, want %#v", got, want)
	}
}

func TestListVolumeBackupsFiltersVolume(t *testing.T) {
	restore := useTempBackupRoot(t)
	defer restore()

	request := BackupRequest{Project: "demo", Environment: "production"}
	dir := backupDirectory(request)
	if err := os.MkdirAll(dir, 0750); err != nil {
		t.Fatalf("failed to create backup dir: %v", err)
	}
	for _, name := range []string{
		"db_20240101-120000.tar.gz",
		"cache_20240101-130000.tar.gz",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("backup"), 0600); err != nil {
			t.Fatalf("failed to write backup fixture: %v", err)
		}
	}

	request.Volume = "db"
	response, err := ListVolumeBackups(context.Background(), request)
	if err != nil {
		t.Fatalf("ListVolumeBackups returned error: %v", err)
	}
	if len(response.Backups) != 1 || response.Backups[0].Volume != "db" {
		t.Fatalf("unexpected filtered backups: %#v", response.Backups)
	}
}

func TestListVolumeBackupsRejectsUniqueArtifactWithoutManifest(t *testing.T) {
	restore := useTempBackupRoot(t)
	defer restore()
	request := BackupRequest{Project: "demo", Environment: "production"}
	dir := backupDirectory(request)
	if err := os.MkdirAll(dir, 0750); err != nil {
		t.Fatal(err)
	}
	name := "data_20260729-120000-0123456789abcdef0123456789abcdef.tar.gz"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ListVolumeBackups(context.Background(), request); err == nil || !strings.Contains(err.Error(), "manifest is required") {
		t.Fatalf("missing manifest error = %v", err)
	}
}

func TestDeleteVolumeBackupRemovesFile(t *testing.T) {
	restore := useTempBackupRoot(t)
	defer restore()

	request := BackupRequest{Project: "demo", Environment: "production", Volume: "data", BackupID: "20240101-120000"}
	dir := backupDirectory(request)
	path := filepath.Join(dir, backupFileName(request.Volume, request.BackupID))
	if err := os.MkdirAll(dir, 0750); err != nil {
		t.Fatalf("failed to create backup dir: %v", err)
	}
	if err := os.WriteFile(path, []byte("backup"), 0600); err != nil {
		t.Fatalf("failed to write backup fixture: %v", err)
	}

	if err := DeleteVolumeBackup(context.Background(), request); err != nil {
		t.Fatalf("DeleteVolumeBackup returned error: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected backup to be deleted, stat err=%v", err)
	}
}

func TestCleanupOldBackupsUsesBackupIDTimestamp(t *testing.T) {
	restore := useTempBackupRoot(t)
	defer restore()

	request := BackupRequest{Project: "demo", Environment: "production", RetentionDays: 7}
	dir := backupDirectory(request)
	if err := os.MkdirAll(dir, 0750); err != nil {
		t.Fatalf("failed to create backup dir: %v", err)
	}
	oldPath := filepath.Join(dir, "data_20000101-120000.tar.gz")
	newPath := filepath.Join(dir, "data_29990101-120000.tar.gz")
	for _, path := range []string{oldPath, newPath} {
		if err := os.WriteFile(path, []byte("backup"), 0600); err != nil {
			t.Fatalf("failed to write backup fixture: %v", err)
		}
	}

	response, err := CleanupOldBackups(context.Background(), request)
	if err != nil {
		t.Fatalf("CleanupOldBackups returned error: %v", err)
	}
	if response.Deleted != 1 {
		t.Fatalf("deleted = %d, want 1", response.Deleted)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("expected old backup to be deleted, stat err=%v", err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("expected new backup to remain: %v", err)
	}
}

func TestCleanupOldBackupsFiltersVolume(t *testing.T) {
	restore := useTempBackupRoot(t)
	defer restore()

	request := BackupRequest{Project: "demo", Environment: "production", Volume: "data", RetentionDays: 7}
	dir := backupDirectory(request)
	if err := os.MkdirAll(dir, 0750); err != nil {
		t.Fatalf("failed to create backup dir: %v", err)
	}
	deletePath := filepath.Join(dir, "data_20000101-120000.tar.gz")
	keepPath := filepath.Join(dir, "pgdata_20000101-120000.tar.gz")
	for _, path := range []string{deletePath, keepPath} {
		if err := os.WriteFile(path, []byte("backup"), 0600); err != nil {
			t.Fatalf("failed to write backup fixture: %v", err)
		}
	}

	response, err := CleanupOldBackups(context.Background(), request)
	if err != nil {
		t.Fatalf("CleanupOldBackups returned error: %v", err)
	}
	if response.Deleted != 1 {
		t.Fatalf("deleted = %d, want 1", response.Deleted)
	}
	if _, err := os.Stat(deletePath); !os.IsNotExist(err) {
		t.Fatalf("expected data backup to be deleted, stat err=%v", err)
	}
	if _, err := os.Stat(keepPath); err != nil {
		t.Fatalf("expected other volume backup to remain: %v", err)
	}
}

func TestValidateBackupRequestRejectsUnsafeValues(t *testing.T) {
	valid := BackupRequest{
		Project:     "demo",
		Environment: "production",
		Volume:      "db_data",
		BackupID:    "20240101-120000",
	}

	invalid := valid
	invalid.Project = "../demo"
	if err := validateBackupRequest(invalid, true, true); err == nil {
		t.Fatal("expected unsafe project to be rejected")
	}

	invalid = valid
	invalid.Environment = "prod;rm"
	if err := validateBackupRequest(invalid, true, true); err == nil {
		t.Fatal("expected unsafe environment to be rejected")
	}

	invalid = valid
	invalid.Volume = "../db"
	if err := validateBackupRequest(invalid, true, true); err == nil {
		t.Fatal("expected unsafe volume to be rejected")
	}

	invalid = valid
	invalid.BackupID = "../20240101-120000"
	if err := validateBackupRequest(invalid, true, true); err == nil {
		t.Fatal("expected unsafe backup ID to be rejected")
	}
}

func TestBackupIDForRequestUsesProvidedID(t *testing.T) {
	request := BackupRequest{BackupID: "20240101-120000"}
	got := backupIDForRequest(request, time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC))
	if got != request.BackupID {
		t.Fatalf("backup ID = %q, want %q", got, request.BackupID)
	}
}

func TestBackupIDForRequestFallsBackToNow(t *testing.T) {
	now := time.Date(2026, 6, 13, 12, 34, 56, 0, time.FixedZone("test", -7*60*60))
	got := backupIDForRequest(BackupRequest{}, now)
	if !strings.HasPrefix(got, "20260613-193456-") || !isSafeBackupID(got) {
		t.Fatalf("backup ID = %q, want unique UTC identifier", got)
	}
}

func TestBackupIDAcceptsUniqueRecoverySuffix(t *testing.T) {
	value := "20260717-120000-0123456789abcdef0123456789abcdef"
	if !isSafeBackupID(value) {
		t.Fatal("unique recovery backup ID was rejected")
	}
	createdAt, err := backupIDTimestamp(value)
	if err != nil || createdAt.UTC().Format("20060102-150405") != "20260717-120000" {
		t.Fatalf("unique recovery backup timestamp = %v, %v", createdAt, err)
	}
	if isSafeBackupID("20260717-120000-0123456789ABCDEF0123456789ABCDEF") {
		t.Fatal("non-canonical recovery suffix was accepted")
	}
}

func TestFullBackupVolumeNameUsesRuntimeVolumeIdentity(t *testing.T) {
	request := BackupRequest{Project: "demo", Environment: "production", Volume: "db_data"}
	got := fullBackupVolumeName(request)
	want := runtimeid.VolumeName("demo", "production", "db_data")
	if got != want {
		t.Fatalf("full backup volume name = %q, want %q", got, want)
	}
}

func TestFullBackupVolumeNameUsesExplicitDockerVolume(t *testing.T) {
	request := BackupRequest{
		Project:      "demo",
		Environment:  "production",
		Volume:       "n8n_data",
		DockerVolume: "captain--n8n-data",
	}
	if got := fullBackupVolumeName(request); got != "captain--n8n-data" {
		t.Fatalf("full backup volume name = %q, want explicit Docker volume", got)
	}
}

func TestCreateVolumeBackupReturnsWarningWhenRemoteUploadFails(t *testing.T) {
	restore := useTempBackupRoot(t)
	defer restore()
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()

	oldUpload := uploadBackupObjectS3With
	uploadBackupObjectS3With = func(context.Context, BackupStorageConfig, BackupObject) (*BackupRemoteInfo, error) {
		return nil, errors.New("object store unavailable")
	}
	t.Cleanup(func() { uploadBackupObjectS3With = oldUpload })

	info, err := CreateVolumeBackup(context.Background(), BackupRequest{
		Project:       "demo",
		Environment:   "production",
		Volume:        "data",
		BackupID:      "20260629-120000",
		RetentionDays: 7,
		Storage: &BackupStorageConfig{
			Provider:        BackupStorageProviderS3,
			Bucket:          "backups",
			Region:          "us-west-2",
			AccessKeyID:     "access",
			SecretAccessKey: "secret",
		},
	})
	if err != nil {
		t.Fatalf("CreateVolumeBackup returned error: %v", err)
	}
	if info == nil || info.ID != "20260629-120000" {
		t.Fatalf("backup info = %#v, want created backup", info)
	}
	if !isSafeBackupArtifactID(info.ArtifactID) || info.ArtifactID == info.ID || info.Format != backupArtifactFormatVolumeTarGzip || len(info.SHA256) != sha256.Size*2 {
		t.Fatalf("backup artifact evidence = %#v", info)
	}
	if len(info.Warnings) != 1 || !strings.Contains(info.Warnings[0], "upload failed") {
		t.Fatalf("warnings = %#v, want upload warning", info.Warnings)
	}
	if _, err := os.Stat(info.Path); err != nil {
		t.Fatalf("expected local backup archive to remain: %v", err)
	}
	manifestInfo, err := os.Stat(backupManifestPath(info.Path))
	if err != nil || manifestInfo.Mode().Perm() != 0600 {
		t.Fatalf("backup manifest mode/error = %v/%v", manifestInfo, err)
	}
}

func TestListVolumeBackupsRejectsIncompleteManifestEvidence(t *testing.T) {
	restore := useTempBackupRoot(t)
	defer restore()
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()
	info, err := CreateVolumeBackup(context.Background(), BackupRequest{Project: "demo", Environment: "production", Volume: "data"})
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := backupManifestPath(info.Path)
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest = []byte(strings.Replace(string(manifest), `"format": "volume-tar-gzip"`, `"format": "unknown"`, 1))
	if err := os.WriteFile(manifestPath, manifest, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ListVolumeBackups(context.Background(), BackupRequest{Project: "demo", Environment: "production"}); err == nil || !strings.Contains(err.Error(), "manifest does not match") {
		t.Fatalf("incomplete manifest error = %v", err)
	}
}

func TestCreateVolumeBackupMarksRemoteOnlyAfterDigestReadback(t *testing.T) {
	restore := useTempBackupRoot(t)
	defer restore()
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()

	var uploaded BackupObject
	oldUpload, oldHash, oldDelete := uploadBackupObjectS3With, hashBackupObjectExactWith, deleteBackupObjectWith
	uploadBackupObjectS3With = func(_ context.Context, _ BackupStorageConfig, object BackupObject) (*BackupRemoteInfo, error) {
		uploaded = object
		return &BackupRemoteInfo{Provider: BackupStorageProviderS3, Bucket: "backups", Key: backupObjectKey("", object)}, nil
	}
	hashBackupObjectExactWith = func(context.Context, BackupStorageConfig, string, int64) (string, error) {
		return uploaded.SHA256, nil
	}
	deleteBackupObjectWith = func(context.Context, BackupStorageConfig, string) error { return nil }
	t.Cleanup(func() {
		uploadBackupObjectS3With = oldUpload
		hashBackupObjectExactWith = oldHash
		deleteBackupObjectWith = oldDelete
	})

	info, err := CreateVolumeBackup(context.Background(), BackupRequest{
		Project: "demo", Environment: "production", Volume: "data", BackupID: "20260629-120000",
		Storage: &BackupStorageConfig{Provider: BackupStorageProviderS3, Bucket: "backups", Region: "us-east-1", AccessKeyID: "access", SecretAccessKey: "secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if uploaded.ArtifactID != info.ArtifactID || uploaded.Size != info.Size || uploaded.SHA256 != info.SHA256 {
		t.Fatalf("uploaded evidence = %#v, info = %#v", uploaded, info)
	}
	if info.Remote == nil || info.Remote.VerifiedAt.IsZero() {
		t.Fatalf("remote artifact was not marked verified: %#v", info.Remote)
	}

	hashBackupObjectExactWith = func(context.Context, BackupStorageConfig, string, int64) (string, error) {
		return strings.Repeat("0", 64), nil
	}
	corrupt, err := CreateVolumeBackup(context.Background(), BackupRequest{
		Project: "demo", Environment: "production", Volume: "data", BackupID: "20260629-120001",
		Storage: &BackupStorageConfig{Provider: BackupStorageProviderS3, Bucket: "backups", Region: "us-east-1", AccessKeyID: "access", SecretAccessKey: "secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if corrupt.Remote != nil || len(corrupt.Warnings) != 1 || !strings.Contains(corrupt.Warnings[0], "readback verification failed") {
		t.Fatalf("corrupt remote readback was accepted: %#v", corrupt)
	}
}

func TestRestoreVolumeBackupRejectsCorruptManifestArtifactBeforeDockerMutation(t *testing.T) {
	restore := useTempBackupRoot(t)
	defer restore()
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()

	request := BackupRequest{Project: "demo", Environment: "production", Volume: "data", BackupID: "20260629-120000"}
	info, err := CreateVolumeBackup(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(info.Path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	request.Approved = true
	err = RestoreVolumeBackup(context.Background(), request)
	if err == nil || (!strings.Contains(err.Error(), "SHA-256") && !strings.Contains(err.Error(), "manifest")) {
		t.Fatalf("restore error = %v, want artifact-integrity rejection", err)
	}
	if commands := readCommandLog(t, logPath); len(commands) != 0 {
		t.Fatalf("Docker mutated after corrupt artifact detection: %#v", commands)
	}
}

func TestRestoreExternalVolumeDoesNotCreateMissingVolume(t *testing.T) {
	restore := useTempBackupRoot(t)
	defer restore()
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()
	t.Setenv("TAKO_FAKE_MISSING_VOLUME_INSPECT", "captain--missing-data")

	request := BackupRequest{
		Project:        "demo",
		Environment:    "production",
		Volume:         "n8n_data",
		DockerVolume:   "captain--missing-data",
		ExternalVolume: true,
		BackupID:       "20240101-120000",
		Approved:       true,
	}
	dir := backupDirectory(request)
	if err := os.MkdirAll(dir, 0750); err != nil {
		t.Fatalf("failed to create backup dir: %v", err)
	}
	writeTestBackupArchive(t, filepath.Join(dir, backupFileName(request.Volume, request.BackupID)))

	err := RestoreVolumeBackup(context.Background(), request)
	if err == nil {
		t.Fatal("RestoreVolumeBackup should fail for missing external volume")
	}
	if !strings.Contains(err.Error(), "external volume captain--missing-data does not exist") {
		t.Fatalf("error = %q, want missing external volume context", err)
	}
	entries := readCommandLog(t, logPath)
	for _, entry := range entries {
		if strings.Contains(entry, "volume create") {
			t.Fatalf("restore should not create missing external volume; log %#v", entries)
		}
	}
}

func TestRestoreVolumeBackupRequiresAPIApproval(t *testing.T) {
	err := RestoreVolumeBackup(context.Background(), BackupRequest{
		Project: "demo", Environment: "production", Volume: "data", BackupID: "20240101-120000",
	})
	if err == nil || !strings.Contains(err.Error(), "explicit approval") {
		t.Fatalf("restore approval error = %v", err)
	}
}

func TestRestoreVolumeBackupRejectsMountedTarget(t *testing.T) {
	restore := useTempBackupRoot(t)
	defer restore()
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "running-container\n")
	request := BackupRequest{Project: "demo", Environment: "production", Volume: "data", BackupID: "20240101-120000", Approved: true}
	dir := backupDirectory(request)
	if err := os.MkdirAll(dir, 0750); err != nil {
		t.Fatal(err)
	}
	writeTestBackupArchive(t, filepath.Join(dir, backupFileName(request.Volume, request.BackupID)))
	if err := RestoreVolumeBackup(context.Background(), request); err == nil || !strings.Contains(err.Error(), "mounted by a running container") {
		t.Fatalf("mounted restore error = %v", err)
	}
	commands := strings.Join(readCommandLog(t, logPath), "\n")
	if strings.Contains(commands, "find \"$targetPath\"") || strings.Contains(commands, "volume create") {
		t.Fatalf("mounted restore mutated Docker state:\n%s", commands)
	}
}

func TestRestoreVolumeBackupRollsBackOriginalContentsAfterTargetFailure(t *testing.T) {
	restore := useTempBackupRoot(t)
	defer restore()
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()
	request := BackupRequest{Project: "demo", Environment: "production", Volume: "data", BackupID: "20240101-120000", Approved: true}
	dir := backupDirectory(request)
	if err := os.MkdirAll(dir, 0750); err != nil {
		t.Fatal(err)
	}
	writeTestBackupArchive(t, filepath.Join(dir, backupFileName(request.Volume, request.BackupID)))
	fullVolume := fullBackupVolumeName(request)
	t.Setenv("TAKO_FAKE_DOCKER_RUN_FAIL_CONTAINS", fullVolume+":/target")
	t.Setenv("TAKO_FAKE_DOCKER_RUN_FAIL_ONCE_FILE", filepath.Join(t.TempDir(), "failed-once"))
	err := RestoreVolumeBackup(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "original volume contents were restored") {
		t.Fatalf("transactional restore error = %v", err)
	}
	commands := strings.Join(readCommandLog(t, logPath), "\n")
	if strings.Count(commands, "targetPath='/target'") != 3 {
		t.Fatalf("expected staging, target, and rollback extraction:\n%s", commands)
	}
	partials, err := filepath.Glob(filepath.Join(dir, ".restore-rollback-*.tar.gz"))
	if err != nil || len(partials) != 0 {
		t.Fatalf("rollback artifacts remain: %#v, err=%v", partials, err)
	}
	markers, err := filepath.Glob(filepath.Join(dir, ".restore-transaction-*.json"))
	if err != nil || len(markers) != 0 {
		t.Fatalf("restore staging markers remain: %#v, err=%v", markers, err)
	}
}

func TestBackupDirectoryLockExcludesConcurrentPublisherAndCleansPartials(t *testing.T) {
	dir := t.TempDir()
	release, err := acquireBackupDirectoryLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if secondRelease, err := acquireBackupDirectoryLock(dir); err == nil {
		secondRelease()
		t.Fatal("second backup publisher acquired the same directory lock")
	}
	partial := filepath.Join(dir, ".data.tar.gz.abandoned.partial")
	if err := os.WriteFile(partial, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupPartialBackupArtifacts(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatalf("abandoned partial remains: %v", err)
	}
	release()
}

func TestCleanupBackupRestoreResourcesRemovesCrashLeftovers(t *testing.T) {
	restore := useTempBackupRoot(t)
	defer restore()
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()
	dir := filepath.Join(backupRootDir, "demo", "production")
	if err := os.MkdirAll(dir, 0750); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, ".restore-transaction-abandoned.json")
	rollbackFile := ".restore-rollback-0123456789abcdef.tar.gz"
	writeTestBackupArchive(t, filepath.Join(dir, rollbackFile))
	journal := backupRestoreJournal{
		SchemaVersion: 1, Phase: "mutating", TargetVolume: "demo-production-data", StageVolume: "tako-restore-stage-0123456789abcdef",
		BackupFile: "data_20240101-120000.tar.gz", RollbackFile: rollbackFile,
	}
	if err := writeBackupRestoreJournal(marker, journal); err != nil {
		t.Fatal(err)
	}
	if err := CleanupBackupRestoreResources(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("stale restore marker remains: %v", err)
	}
	if commands := strings.Join(readCommandLog(t, logPath), "\n"); !strings.Contains(commands, "volume rm tako-restore-stage-0123456789abcdef") || !strings.Contains(commands, "demo-production-data:/target") {
		t.Fatalf("interrupted restore was not rolled back and cleaned:\n%s", commands)
	}
}

func TestPendingRestoreJournalBlocksSubsequentMutations(t *testing.T) {
	restore := useTempBackupRoot(t)
	defer restore()
	request := BackupRequest{Project: "demo", Environment: "production", Volume: "data", BackupID: "20240101-120000"}
	dir := backupDirectory(request)
	if err := os.MkdirAll(dir, 0750); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(dir, ".restore-transaction-pending.json")
	journal := backupRestoreJournal{
		SchemaVersion: 1, Phase: "mutating", TargetVolume: "demo-production-data", StageVolume: "tako-restore-stage-0123456789abcdef",
		BackupFile: "data_20240101-120000.tar.gz", RollbackFile: ".restore-rollback-0123456789abcdef.tar.gz",
	}
	if err := writeBackupRestoreJournal(journalPath, journal); err != nil {
		t.Fatal(err)
	}
	if err := DeleteVolumeBackup(context.Background(), request); err == nil || !strings.Contains(err.Error(), "pending volume restore") {
		t.Fatalf("pending restore mutation barrier error = %v", err)
	}
	if _, err := os.Stat(journalPath); err != nil {
		t.Fatalf("pending restore journal was removed: %v", err)
	}
}

func TestBarrierMarkedSchedulerContextDoesNotReacquireRestoreLock(t *testing.T) {
	restore := useTempBackupRoot(t)
	defer restore()
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()
	ctx, release := acquireBackupRestoreMutationBarrier(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := CreateVolumeBackup(ctx, BackupRequest{Project: "demo", Environment: "production", Volume: "data"})
		result <- err
	}()
	select {
	case err := <-result:
		release()
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		release()
		<-result
		t.Fatal("barrier-marked scheduler context deadlocked on restore lock")
	}
}

func TestRestoreCapacityIncludesStagingRollbackAndFilesystemMargin(t *testing.T) {
	backupBytes, dockerBytes, err := restoreCapacityBytes(100<<20, 200<<20)
	if err != nil {
		t.Fatal(err)
	}
	if backupBytes <= 200<<20 || dockerBytes <= 100<<20 {
		t.Fatalf("restore capacity = backup %d, Docker %d", backupBytes, dockerBytes)
	}
	_, emptyTargetDockerBytes, err := restoreCapacityBytes(100<<20, 0)
	if err != nil || emptyTargetDockerBytes <= 200<<20 {
		t.Fatalf("empty-target restore capacity = %d, err=%v", emptyTargetDockerBytes, err)
	}
}

func TestRestoreVolumeScriptScopesDestructiveCleanup(t *testing.T) {
	script := restoreVolumeScript("db_20240101-120000.tar.gz")
	for _, expected := range []string{
		"backupPath='/backup/db_20240101-120000.tar.gz'",
		"targetPath='/target'",
		"[ ! -d \"$targetPath\" ] || [ -L \"$targetPath\" ]",
		"[ ! -f \"$backupPath\" ]",
		"tar -tzf \"$backupPath\" > \"$entryList\"",
		"awk '",
		"/^\\/|(^|\\/)\\.\\.(\\/|$)/",
		"find \"$targetPath\" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} \\;",
		"tar -xzf \"$backupPath\" -C \"$targetPath\"",
	} {
		if !strings.Contains(script, expected) {
			t.Fatalf("restore script missing %q:\n%s", expected, script)
		}
	}
	for _, unsafe := range []string{
		"rm -rf /target/*",
		"/target/..?*",
		"/target/.[!.]*",
	} {
		if strings.Contains(script, unsafe) {
			t.Fatalf("restore script should not use brittle glob %q:\n%s", unsafe, script)
		}
	}
}

func writeTestBackupArchive(t *testing.T, path string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	compressed := gzip.NewWriter(file)
	archive := tar.NewWriter(compressed)
	content := []byte("backup")
	if err := archive.WriteHeader(&tar.Header{Name: "data", Mode: 0600, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreVolumeScriptValidatesCorruptLegacyArchiveBeforeDeletingTarget(t *testing.T) {
	backupRoot, targetRoot := t.TempDir(), t.TempDir()
	archive := "data_20240101-120000.tar.gz"
	if err := os.WriteFile(filepath.Join(backupRoot, archive), []byte("not-a-gzip-archive"), 0600); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(targetRoot, "must-survive")
	if err := os.WriteFile(sentinel, []byte("production-data"), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("sh", "-c", restoreVolumeScriptForPaths(archive, backupRoot, targetRoot))
	if err := command.Run(); err == nil {
		t.Fatal("corrupt archive unexpectedly restored")
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "production-data" {
		t.Fatalf("target data changed before archive validation: data=%q err=%v", data, err)
	}
}

func TestRestoreVolumeScriptQuotesBackupFile(t *testing.T) {
	script := restoreVolumeScript("db_'quoted'_20240101-120000.tar.gz")
	if !strings.Contains(script, "'/backup/db_'\"'\"'quoted'\"'\"'_20240101-120000.tar.gz'") {
		t.Fatalf("restore script did not shell-quote backup file:\n%s", script)
	}
}

func useTempBackupRoot(t *testing.T) func() {
	t.Helper()
	previous := backupRootDir
	backupRootDir = t.TempDir()
	return func() {
		backupRootDir = previous
	}
}
