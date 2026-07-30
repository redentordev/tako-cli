package takod

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

const postgresVerificationTestImage = "postgres:16-alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestVerifyPostgresBackupChecksArtifactBeforeIsolatedRestoreAndCleansUp(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()
	t.Setenv("TAKO_FAKE_PSQL_OUTPUT", "expected-row-count-value\n")

	artifact := []byte("custom-format-pg-dump")
	digest := sha256.Sum256(artifact)
	oldDownload := downloadBackupObjectVerifiedWith
	downloadBackupObjectVerifiedWith = func(_ context.Context, _ BackupStorageConfig, key, destination string, size int64, expectedSHA256 string) error {
		if key != "postgres/demo.dump" || size != int64(len(artifact)) || expectedSHA256 != hex.EncodeToString(digest[:]) {
			t.Fatalf("unexpected artifact evidence: key=%q size=%d sha=%q", key, size, expectedSHA256)
		}
		return os.WriteFile(destination, artifact, 0600)
	}
	t.Cleanup(func() { downloadBackupObjectVerifiedWith = oldDownload })

	dataDir := t.TempDir()
	request := validPostgresVerificationRequest(int64(len(artifact)), hex.EncodeToString(digest[:]))
	result, err := VerifyPostgresBackup(context.Background(), dataDir, request)
	if err != nil {
		t.Fatalf("VerifyPostgresBackup returned error: %v", err)
	}
	if result.Status != "verified" || !result.CleanupComplete || result.FailureStage != "" {
		t.Fatalf("unexpected result: %#v", result)
	}

	commands := strings.Join(readCommandLog(t, logPath), "\n")
	for _, expected := range []string{
		"tako.backup-verification=artifact-20260729",
		"run -d", "--network none", "--security-opt no-new-privileges", "--cap-drop ALL",
		"--pids-limit 256", "--cpus 2", "--memory 1073741824", "/tako-postgres-data:rw,nosuid,nodev,noexec,size=67108864", postgresVerificationTestImage,
		"PGDATA=/tako-postgres-data",
		"pg_isready --quiet --host 127.0.0.1", "exec -i", "pg_restore --exit-on-error --no-owner --no-acl --no-tablespaces",
		"GRANT pg_read_all_data", "default_transaction_read_only = on", "psql --no-psqlrc --csv",
		"rm -f -v tako-pg-verify-",
	} {
		if !strings.Contains(commands, expected) {
			t.Fatalf("Docker command log missing %q:\n%s", expected, commands)
		}
	}
	for _, forbidden := range []string{"production-db-volume", "--publish", "--network bridge", "tako.service=db"} {
		if strings.Contains(commands, forbidden) {
			t.Fatalf("Docker command log contains unsafe value %q:\n%s", forbidden, commands)
		}
	}

	recordPath := filepath.Join(dataDir, backupVerificationDirName, "demo", "production", "db", "artifact-20260729.json")
	record, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read verification record: %v", err)
	}
	if marker, err := os.Stat(recordPath + ".commit"); err != nil || marker.Mode().Perm() != 0600 {
		t.Fatalf("verification commit marker = %v, err=%v", marker, err)
	}
	for _, secret := range []string{"access-key", "secret-key", request.Query, request.ExpectedValue, "PGPASSWORD"} {
		if strings.Contains(string(record), secret) {
			t.Fatalf("verification record contains sensitive value %q: %s", secret, record)
		}
	}
}

func TestVerifyPostgresBackupRejectsDigestFailureBeforeDockerMutation(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()
	oldDownload := downloadBackupObjectVerifiedWith
	downloadBackupObjectVerifiedWith = func(context.Context, BackupStorageConfig, string, string, int64, string) error {
		return os.ErrInvalid
	}
	t.Cleanup(func() { downloadBackupObjectVerifiedWith = oldDownload })

	request := validPostgresVerificationRequest(4, strings.Repeat("a", 64))
	result, err := VerifyPostgresBackup(context.Background(), t.TempDir(), request)
	if err == nil || !strings.Contains(err.Error(), "verify remote backup artifact") {
		t.Fatalf("error = %v, want remote artifact failure", err)
	}
	if result == nil || result.Status != "failed" || result.FailureStage != "download" || !result.CleanupComplete {
		t.Fatalf("unexpected failed result: %#v", result)
	}
	if data, err := os.ReadFile(logPath); err == nil && len(strings.TrimSpace(string(data))) > 0 {
		t.Fatalf("Docker mutated before digest verification:\n%s", data)
	}
}

func TestVerifyPostgresBackupQueryMismatchStillCleansScratchResources(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()
	t.Setenv("TAKO_FAKE_PSQL_OUTPUT", "wrong-value\n")
	oldDownload := downloadBackupObjectVerifiedWith
	downloadBackupObjectVerifiedWith = func(_ context.Context, _ BackupStorageConfig, _ string, destination string, _ int64, _ string) error {
		return os.WriteFile(destination, []byte("dump"), 0600)
	}
	t.Cleanup(func() { downloadBackupObjectVerifiedWith = oldDownload })

	request := validPostgresVerificationRequest(4, strings.Repeat("a", 64))
	result, err := VerifyPostgresBackup(context.Background(), t.TempDir(), request)
	if err == nil || !strings.Contains(err.Error(), "did not match") {
		t.Fatalf("error = %v, want query mismatch", err)
	}
	if result == nil || result.Status != "failed" || result.FailureStage != "query" || !result.CleanupComplete {
		t.Fatalf("unexpected result: %#v", result)
	}
	commands := strings.Join(readCommandLog(t, logPath), "\n")
	if !strings.Contains(commands, "rm -f -v tako-pg-verify-") {
		t.Fatalf("scratch resources were not removed:\n%s", commands)
	}
}

func TestVerifyPostgresBackupRejectsUnsupportedPostgresVersion(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()
	t.Setenv("TAKO_FAKE_POSTGRES_VERSION", "130021")
	oldDownload := downloadBackupObjectVerifiedWith
	downloadBackupObjectVerifiedWith = func(_ context.Context, _ BackupStorageConfig, _ string, destination string, _ int64, _ string) error {
		return os.WriteFile(destination, []byte("dump"), 0600)
	}
	t.Cleanup(func() { downloadBackupObjectVerifiedWith = oldDownload })

	result, err := VerifyPostgresBackup(context.Background(), t.TempDir(), validPostgresVerificationRequest(4, strings.Repeat("a", 64)))
	if err == nil || !strings.Contains(err.Error(), "PostgreSQL 14 or newer") {
		t.Fatalf("unsupported version error = %v", err)
	}
	if result == nil || result.Status != "failed" || result.FailureStage != "readiness" || !result.CleanupComplete {
		t.Fatalf("failure result = %#v", result)
	}
}

func TestVerifyPostgresBackupPersistenceFailureCannotReturnVerified(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()
	t.Setenv("TAKO_FAKE_PSQL_OUTPUT", "expected-row-count-value\n")
	oldDownload := downloadBackupObjectVerifiedWith
	downloadBackupObjectVerifiedWith = func(_ context.Context, _ BackupStorageConfig, _ string, destination string, _ int64, _ string) error {
		return os.WriteFile(destination, []byte("dump"), 0600)
	}
	t.Cleanup(func() { downloadBackupObjectVerifiedWith = oldDownload })
	oldPersist := persistPostgresVerificationWith
	persistPostgresVerificationWith = func(string, *PostgresVerificationResult) error { return fmt.Errorf("durability failed") }
	t.Cleanup(func() { persistPostgresVerificationWith = oldPersist })

	result, err := VerifyPostgresBackup(context.Background(), t.TempDir(), validPostgresVerificationRequest(4, strings.Repeat("a", 64)))
	if err == nil || !strings.Contains(err.Error(), "durability failed") {
		t.Fatalf("persistence error = %v", err)
	}
	if result == nil || result.Status != "failed" || result.FailureStage != "persistence" {
		t.Fatalf("contradictory persistence result = %#v", result)
	}
}

func TestPersistPostgresVerificationRemovesVisibleRecordAfterDirectorySyncFailure(t *testing.T) {
	dataDir := t.TempDir()
	result := &PostgresVerificationResult{
		SchemaVersion: 1, Project: "demo", Environment: "production", Service: "db", ArtifactID: "artifact-20260729",
		ArtifactFormat: BackupArtifactFormatPostgresCustom, ObjectKey: "postgres/demo.dump", StorageProvider: BackupStorageProviderS3,
		StorageBucket: "backups", Size: 4, ScratchBytes: 8, SHA256: strings.Repeat("a", 64), PostgresImage: postgresVerificationTestImage,
		Database: "app", RestoreUsername: "isolated-database-owner", QuerySHA256: strings.Repeat("b", 64), ExpectedSHA256: strings.Repeat("c", 64), Status: "verified",
	}
	oldSync := syncPostgresVerificationDirectoryWith
	syncPostgresVerificationDirectoryWith = func(string) error { return fmt.Errorf("directory sync failed") }
	t.Cleanup(func() { syncPostgresVerificationDirectoryWith = oldSync })
	if err := persistPostgresVerification(dataDir, result); err == nil || !strings.Contains(err.Error(), "directory sync failed") {
		t.Fatalf("persistence error = %v", err)
	}
	if _, err := ReadPostgresVerificationResult(dataDir, "demo", "production", "db", "artifact-20260729"); err == nil {
		t.Fatal("verified record remained readable after failed commit")
	}
}

func TestPersistPostgresVerificationRestoresReadablePreviousCommit(t *testing.T) {
	dataDir := t.TempDir()
	result := &PostgresVerificationResult{
		SchemaVersion: 1, Project: "demo", Environment: "production", Service: "db", ArtifactID: "artifact-previous",
		ArtifactFormat: BackupArtifactFormatPostgresCustom, ObjectKey: "postgres/demo.dump", StorageProvider: BackupStorageProviderS3,
		StorageBucket: "backups", Size: 4, ScratchBytes: 64 << 20, MemoryBytes: 1 << 30, SHA256: strings.Repeat("a", 64), PostgresImage: postgresVerificationTestImage,
		Database: "app", BootstrapUsername: "postgres", RestoreUsername: "isolated-database-owner", QuerySHA256: strings.Repeat("b", 64), ExpectedSHA256: strings.Repeat("c", 64), Status: "failed",
	}
	if err := persistPostgresVerification(dataDir, result); err != nil {
		t.Fatal(err)
	}
	updated := *result
	updated.Status = "verified"
	oldSync := syncPostgresVerificationDirectoryWith
	failed := false
	syncPostgresVerificationDirectoryWith = func(path string) error {
		if !failed {
			failed = true
			return fmt.Errorf("directory sync failed")
		}
		return oldSync(path)
	}
	t.Cleanup(func() { syncPostgresVerificationDirectoryWith = oldSync })
	if err := persistPostgresVerification(dataDir, &updated); err == nil {
		t.Fatal("updated verification unexpectedly committed")
	}
	preserved, err := ReadPostgresVerificationResult(dataDir, "demo", "production", "db", "artifact-previous")
	if err != nil || preserved.Status != "failed" {
		t.Fatalf("previous committed result = %#v, err=%v", preserved, err)
	}
}

func TestVerifyPostgresBackupCleansAfterAmbiguousContainerStartFailure(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()
	t.Setenv("TAKO_FAKE_DOCKER_RUN_FAIL_CONTAINS", postgresVerificationTestImage)
	oldDownload := downloadBackupObjectVerifiedWith
	downloadBackupObjectVerifiedWith = func(_ context.Context, _ BackupStorageConfig, _ string, destination string, _ int64, _ string) error {
		return os.WriteFile(destination, []byte("dump"), 0600)
	}
	t.Cleanup(func() { downloadBackupObjectVerifiedWith = oldDownload })

	result, err := VerifyPostgresBackup(context.Background(), t.TempDir(), validPostgresVerificationRequest(4, strings.Repeat("a", 64)))
	if err == nil || result == nil || result.FailureStage != "start" {
		t.Fatalf("result/error = %#v/%v", result, err)
	}
	commands := strings.Join(readCommandLog(t, logPath), "\n")
	if !strings.Contains(commands, "rm -f -v tako-pg-verify-") {
		t.Fatalf("ambiguous resources were not cleaned:\n%s", commands)
	}
}

func TestValidatePostgresVerificationRequiresDigestPinnedImage(t *testing.T) {
	request := validPostgresVerificationRequest(4, strings.Repeat("a", 64))
	request.Image = "postgres:16-alpine"
	if err := ValidatePostgresVerificationRequest(request); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("error = %v, want digest-pinned image rejection", err)
	}
}

func TestValidatePostgresVerificationRejectsUntrustedImageRepository(t *testing.T) {
	request := validPostgresVerificationRequest(4, strings.Repeat("a", 64))
	request.Image = "attacker.example/postgres:16@sha256:" + strings.Repeat("0", 64)
	if err := ValidatePostgresVerificationRequest(request); err == nil || !strings.Contains(err.Error(), "official Docker Hub postgres") {
		t.Fatalf("untrusted image error = %v", err)
	}
}

func TestProbePostgresVerificationQuotaRecognizesSupportedStorage(t *testing.T) {
	restoreCommands := useFakeCommands(t, filepath.Join(t.TempDir(), "commands.log"))
	defer restoreCommands()
	if !probePostgresVerificationQuota(context.Background()) {
		t.Fatal("successful hard-quota probe was not detected")
	}
	t.Setenv("TAKO_FAKE_DOCKER_RUN_FAIL_CONTAINS", "/quota-probe:rw,nosuid,nodev,noexec,size=67108864")
	if probePostgresVerificationQuota(context.Background()) {
		t.Fatal("failed hard-quota probe advertised support")
	}
}

func TestPostgresVerificationPayloadIsOperationFenceScoped(t *testing.T) {
	fence := nodeidentity.OperationFence{Project: "demo", Environment: "production"}
	request := validPostgresVerificationRequest(4, strings.Repeat("a", 64))
	if err := validateEndpointPayloadScope(&request, fence); err != nil {
		t.Fatalf("matching verification scope rejected: %v", err)
	}
	request.Environment = "staging"
	if err := validateEndpointPayloadScope(&request, fence); err == nil {
		t.Fatal("cross-environment verification scope was accepted")
	}
}

func TestPersistPostgresVerificationRejectsArtifactIdentityDrift(t *testing.T) {
	dataDir := t.TempDir()
	first := &PostgresVerificationResult{
		SchemaVersion: 1, Project: "demo", Environment: "production", Service: "db",
		ArtifactID: "artifact-1", ArtifactFormat: BackupArtifactFormatPostgresCustom,
		ObjectKey: "dumps/one.dump", Size: 4, SHA256: strings.Repeat("a", 64),
		PostgresImage: postgresVerificationTestImage, Database: "app",
		QuerySHA256: strings.Repeat("b", 64), ExpectedSHA256: strings.Repeat("c", 64), Status: "failed",
	}
	if err := persistPostgresVerification(dataDir, first); err != nil {
		t.Fatal(err)
	}
	retry := *first
	retry.Status = "verified"
	if err := persistPostgresVerification(dataDir, &retry); err != nil {
		t.Fatalf("same-evidence retry rejected: %v", err)
	}
	laterFailure := retry
	laterFailure.Status = "failed"
	laterFailure.FailureStage = "download"
	if err := persistPostgresVerification(dataDir, &laterFailure); err != nil {
		t.Fatalf("same-evidence failed retry returned persistence error: %v", err)
	}
	preserved, err := ReadPostgresVerificationResult(dataDir, "demo", "production", "db", "artifact-1")
	if err != nil || preserved.Status != "verified" {
		t.Fatalf("verified result was not preserved: %#v err=%v", preserved, err)
	}
	drift := retry
	drift.SHA256 = strings.Repeat("d", 64)
	if err := persistPostgresVerification(dataDir, &drift); err == nil || !strings.Contains(err.Error(), "different verification evidence") {
		t.Fatalf("artifact identity drift error = %v", err)
	}
}

func TestCleanupPostgresVerificationResourcesRemovesCrashLeftovers(t *testing.T) {
	dataDir := t.TempDir()
	workspace := filepath.Join(dataDir, backupVerificationDirName, "demo", "production", "db", ".verify-abandoned")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "artifact.dump"), []byte("sensitive"), 0600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restoreCommands := useFakeCommands(t, logPath)
	defer restoreCommands()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "stale-container\n")
	t.Setenv("TAKO_FAKE_VOLUME_LS_OUTPUT", "stale-volume\n")
	if err := CleanupPostgresVerificationResources(context.Background(), dataDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("abandoned workspace remains: %v", err)
	}
	commands := strings.Join(readCommandLog(t, logPath), "\n")
	if !strings.Contains(commands, "rm -f -v stale-container") || !strings.Contains(commands, "volume rm stale-volume") {
		t.Fatalf("stale Docker resources were not removed:\n%s", commands)
	}
}

func validPostgresVerificationRequest(size int64, digest string) PostgresVerificationRequest {
	return PostgresVerificationRequest{
		Project: "demo", Environment: "production", Service: "db",
		Storage: BackupStorageConfig{
			Provider: BackupStorageProviderS3, Bucket: "backups", Region: "us-east-1",
			AccessKeyID: "access-key", SecretAccessKey: "secret-key",
		},
		Artifact: BackupArtifactReference{
			ID: "artifact-20260729", Format: BackupArtifactFormatPostgresCustom,
			Key: "postgres/demo.dump", Size: size, SHA256: digest,
		},
		Image: postgresVerificationTestImage, Database: "app", Username: "postgres",
		Query: "SELECT count(*) /* private-verification-query */ FROM important_rows", ExpectedValue: "expected-row-count-value",
		ScratchBytes: 64 * 1024 * 1024,
	}
}
