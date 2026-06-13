package takod

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
