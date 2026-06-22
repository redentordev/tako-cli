package takod

import (
	"context"
	"os"
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
	if got != "20260613-193456" {
		t.Fatalf("backup ID = %q, want UTC timestamp", got)
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
	}
	dir := backupDirectory(request)
	if err := os.MkdirAll(dir, 0750); err != nil {
		t.Fatalf("failed to create backup dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, backupFileName(request.Volume, request.BackupID)), []byte("backup"), 0600); err != nil {
		t.Fatalf("failed to write backup fixture: %v", err)
	}

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

func TestRestoreVolumeScriptScopesDestructiveCleanup(t *testing.T) {
	script := restoreVolumeScript("db_20240101-120000.tar.gz")
	for _, expected := range []string{
		"backupPath=/backup/'db_20240101-120000.tar.gz'",
		"[ ! -d /target ] || [ -L /target ]",
		"[ ! -f \"$backupPath\" ]",
		"tar -tzf \"$backupPath\" | awk",
		"/^\\/|(^|\\/)\\.\\.(\\/|$)/",
		"find /target -mindepth 1 -maxdepth 1 -exec rm -rf -- {} \\;",
		"tar -xzf \"$backupPath\" -C /target",
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

func TestRestoreVolumeScriptQuotesBackupFile(t *testing.T) {
	script := restoreVolumeScript("db_'quoted'_20240101-120000.tar.gz")
	if !strings.Contains(script, "'db_'\"'\"'quoted'\"'\"'_20240101-120000.tar.gz'") {
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
