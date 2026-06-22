package takod

import (
	"strings"
	"testing"
	"time"
)

func TestValidateBackupStorageSupportsR2Defaults(t *testing.T) {
	storage := normalizeBackupStorage(BackupStorageConfig{
		Provider:        BackupStorageProviderR2,
		Bucket:          "backups",
		Endpoint:        "https://account.r2.cloudflarestorage.com/",
		AccessKeyID:     "access",
		SecretAccessKey: "secret",
		Prefix:          "/apps/demo/",
	})

	if storage.Region != "auto" {
		t.Fatalf("region = %q, want auto", storage.Region)
	}
	if storage.Endpoint != "https://account.r2.cloudflarestorage.com" {
		t.Fatalf("endpoint = %q, want trimmed endpoint", storage.Endpoint)
	}
	if storage.Prefix != "apps/demo" {
		t.Fatalf("prefix = %q, want cleaned prefix", storage.Prefix)
	}
	if err := ValidateBackupStorage(storage); err != nil {
		t.Fatalf("ValidateBackupStorage returned error: %v", err)
	}
}

func TestValidateBackupStorageRejectsUnsafeValues(t *testing.T) {
	storage := BackupStorageConfig{
		Provider:        BackupStorageProviderS3Compatible,
		Bucket:          "backups",
		Region:          "us-east-1",
		Endpoint:        "ftp://storage.example.com",
		AccessKeyID:     "access",
		SecretAccessKey: "secret",
	}

	err := ValidateBackupStorage(storage)
	if err == nil {
		t.Fatal("ValidateBackupStorage should reject unsupported endpoint scheme")
	}
	if !strings.Contains(err.Error(), "http or https") {
		t.Fatalf("error = %q, want endpoint scheme guidance", err)
	}
}

func TestBackupObjectKeyUsesStableLayout(t *testing.T) {
	key := backupObjectKey("apps", BackupObject{
		Project:     "demo",
		Environment: "production",
		Volume:      "pgdata",
		BackupID:    "20260621-120000",
		CreatedAt:   time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC),
	})

	want := "apps/demo/production/pgdata/pgdata_20260621-120000.tar.gz"
	if key != want {
		t.Fatalf("object key = %q, want %q", key, want)
	}
}
