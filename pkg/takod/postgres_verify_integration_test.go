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
	"time"
)

func TestPostgresBackupVerificationIntegration(t *testing.T) {
	if os.Getenv("TAKO_POSTGRES_VERIFY_INTEGRATION") != "1" {
		t.Skip("set TAKO_POSTGRES_VERIFY_INTEGRATION=1 with TAKO_POSTGRES_IMAGE to run")
	}
	image := strings.TrimSpace(os.Getenv("TAKO_POSTGRES_IMAGE"))
	if image == "" {
		t.Fatal("TAKO_POSTGRES_IMAGE must be a digest-pinned PostgreSQL image")
	}
	suffix, err := randomVerificationSuffix()
	if err != nil {
		t.Fatal(err)
	}
	containerName, volumeName := "tako-pg-source-"+suffix, "tako-pg-source-"+suffix
	password := "integration-only-password"
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, _ = runDocker(ctx, "rm", "-f", containerName)
		_, _ = runDocker(ctx, "volume", "rm", volumeName)
	}
	cleanup()
	t.Cleanup(cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if _, err := runDocker(ctx, "volume", "create", volumeName); err != nil {
		t.Fatal(err)
	}
	if _, err := runDocker(ctx,
		"run", "-d", "--name", containerName, "--network", "none",
		"-e", "POSTGRES_USER=postgres", "-e", "POSTGRES_PASSWORD="+password, "-e", "POSTGRES_DB=app",
		"-v", volumeName+":/var/lib/postgresql/data", image,
	); err != nil {
		t.Fatal(err)
	}
	if err := waitForPostgresVerification(ctx, containerName, "postgres", "app", password); err != nil {
		t.Fatal(err)
	}
	if _, err := runDocker(ctx,
		"exec", "-e", "PGPASSWORD="+password, containerName,
		"psql", "--no-psqlrc", "--set=ON_ERROR_STOP=1", "--username", "postgres", "--dbname", "app",
		"--command", "CREATE TABLE restore_probe (marker text NOT NULL); INSERT INTO restore_probe VALUES ('row-level-proof');",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := runDocker(ctx,
		"exec", "-e", "PGPASSWORD="+password, containerName,
		"pg_dump", "--format=custom", "--username", "postgres", "--dbname", "app", "--file", "/tmp/source.dump",
	); err != nil {
		t.Fatal(err)
	}
	dumpPath := filepath.Join(t.TempDir(), "source.dump")
	if _, err := runDocker(ctx, "cp", containerName+":/tmp/source.dump", dumpPath); err != nil {
		t.Fatal(err)
	}
	dump, err := os.ReadFile(dumpPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(dump)
	oldDownload := downloadBackupObjectVerifiedWith
	downloadBackupObjectVerifiedWith = func(_ context.Context, _ BackupStorageConfig, _ string, destination string, expectedSize int64, expectedSHA256 string) error {
		if expectedSize != int64(len(dump)) || expectedSHA256 != hex.EncodeToString(digest[:]) {
			t.Fatalf("verification did not use exact artifact evidence")
		}
		return os.WriteFile(destination, dump, 0600)
	}
	t.Cleanup(func() { downloadBackupObjectVerifiedWith = oldDownload })

	request := validPostgresVerificationRequest(int64(len(dump)), hex.EncodeToString(digest[:]))
	request.Image = image
	request.Query = "SELECT marker FROM restore_probe"
	request.ExpectedValue = "row-level-proof"
	result, err := VerifyPostgresBackup(ctx, t.TempDir(), request)
	if err != nil {
		t.Fatalf("isolated restore verification failed: %v", err)
	}
	if result.Status != "verified" || !result.CleanupComplete {
		t.Fatalf("verification result = %#v", result)
	}

	sourceValue, err := runDocker(ctx,
		"exec", "-e", "PGPASSWORD="+password, containerName,
		"psql", "--no-psqlrc", "--tuples-only", "--no-align", "--set=ON_ERROR_STOP=1",
		"--username", "postgres", "--dbname", "app", "--command", "SELECT marker FROM restore_probe",
	)
	if err != nil || strings.TrimSpace(sourceValue) != "row-level-proof" {
		t.Fatalf("source database changed: value=%q err=%v", sourceValue, err)
	}
	if _, err := runDocker(ctx,
		"exec", "-e", "PGPASSWORD="+password, containerName,
		"psql", "--no-psqlrc", "--set=ON_ERROR_STOP=1", "--username", "postgres", "--dbname", "app",
		"--command", "CREATE TABLE quota_probe AS SELECT value, (SELECT string_agg(md5(value::text || part::text), '') FROM generate_series(1, 64) AS part) AS payload FROM generate_series(1, 40000) AS value;",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := runDocker(ctx,
		"exec", "-e", "PGPASSWORD="+password, containerName,
		"pg_dump", "--format=custom", "--username", "postgres", "--dbname", "app", "--file", "/tmp/quota.dump",
	); err != nil {
		t.Fatal(err)
	}
	quotaDumpPath := filepath.Join(t.TempDir(), "quota.dump")
	if _, err := runDocker(ctx, "cp", containerName+":/tmp/quota.dump", quotaDumpPath); err != nil {
		t.Fatal(err)
	}
	dump, err = os.ReadFile(quotaDumpPath)
	if err != nil {
		t.Fatal(err)
	}
	digest = sha256.Sum256(dump)
	quotaRequest := validPostgresVerificationRequest(int64(len(dump)), hex.EncodeToString(digest[:]))
	quotaRequest.Artifact.ID = "quota-overflow-20260729"
	quotaRequest.Image = image
	quotaRequest.Query = "SELECT count(*) FROM quota_probe"
	quotaRequest.ExpectedValue = "40000"
	quotaResult, quotaErr := VerifyPostgresBackup(ctx, t.TempDir(), quotaRequest)
	quotaMessage := strings.ToLower(fmt.Sprint(quotaErr))
	if quotaErr == nil || quotaResult == nil || quotaResult.Status != "failed" || quotaResult.FailureStage != "restore" || !quotaResult.CleanupComplete || (!strings.Contains(quotaMessage, "no space left") && !strings.Contains(quotaMessage, "disk full")) {
		t.Fatalf("scratch quota did not fail closed: result=%#v err=%v", quotaResult, quotaErr)
	}
	containers, err := runDocker(ctx, "ps", "-a", "--filter", "label=tako.backup-verification="+request.Artifact.ID, "--format", "{{.ID}}")
	if err != nil || strings.TrimSpace(containers) != "" {
		t.Fatalf("verification containers remain: %q err=%v", containers, err)
	}
	volumes, err := runDocker(ctx, "volume", "ls", "--filter", "label=tako.backup-verification="+request.Artifact.ID, "--format", "{{.Name}}")
	if err != nil || strings.TrimSpace(volumes) != "" {
		t.Fatalf("verification volumes remain: %q err=%v", volumes, err)
	}
}
