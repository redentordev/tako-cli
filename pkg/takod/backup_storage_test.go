package takod

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

func TestDownloadBackupObjectVerifiedValidatesEvidenceBeforeDownload(t *testing.T) {
	called := false
	oldDownload := downloadBackupObjectVerifiedWith
	downloadBackupObjectVerifiedWith = func(context.Context, BackupStorageConfig, string, string, int64, string) error {
		called = true
		return nil
	}
	t.Cleanup(func() { downloadBackupObjectVerifiedWith = oldDownload })
	storage := BackupStorageConfig{Provider: BackupStorageProviderS3, Bucket: "backups", Region: "us-east-1", AccessKeyID: "access", SecretAccessKey: "secret"}
	destination := filepath.Join(t.TempDir(), "artifact.dump")
	if err := DownloadBackupObjectVerified(context.Background(), storage, "dump/object", destination, 4, strings.Repeat("A", 64)); err == nil {
		t.Fatal("uppercase artifact digest was accepted")
	}
	if called {
		t.Fatal("download started before artifact evidence validation")
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("invalid evidence left a destination file: %v", err)
	}
	if err := DownloadBackupObjectVerified(context.Background(), storage, "dump/object", destination, 4, strings.Repeat("a", 64)); err != nil {
		t.Fatalf("valid evidence rejected: %v", err)
	}
	if !called {
		t.Fatal("validated download did not reach storage implementation")
	}
}

func TestDownloadBackupObjectVerifiedStreamsExactS3Object(t *testing.T) {
	body := []byte("remote-postgres-dump")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/backups/dumps/app.dump" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		_, _ = w.Write(body)
	}))
	defer server.Close()
	storage := BackupStorageConfig{
		Provider: BackupStorageProviderS3Compatible, Bucket: "backups", Region: "us-east-1",
		Endpoint: server.URL, AccessKeyID: "access", SecretAccessKey: "secret", ForcePathStyle: true,
	}
	digest := sha256.Sum256(body)
	destination := filepath.Join(t.TempDir(), "artifact.dump")
	if err := DownloadBackupObjectVerified(context.Background(), storage, "dumps/app.dump", destination, int64(len(body)), hex.EncodeToString(digest[:])); err != nil {
		t.Fatalf("verified S3 download failed: %v", err)
	}
	if downloaded, err := os.ReadFile(destination); err != nil || string(downloaded) != string(body) {
		t.Fatalf("downloaded body = %q, err=%v", downloaded, err)
	}
	corruptDestination := filepath.Join(t.TempDir(), "corrupt.dump")
	if err := DownloadBackupObjectVerified(context.Background(), storage, "dumps/app.dump", corruptDestination, int64(len(body)), strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("corrupt evidence error = %v", err)
	}
	if _, err := os.Stat(corruptDestination); !os.IsNotExist(err) {
		t.Fatalf("digest failure left downloaded artifact: %v", err)
	}
	missingDestination := filepath.Join(t.TempDir(), "missing.dump")
	if err := DownloadBackupObjectVerified(context.Background(), storage, "dumps/missing.dump", missingDestination, int64(len(body)), hex.EncodeToString(digest[:])); err == nil {
		t.Fatal("missing remote object was accepted")
	}
	if _, err := os.Stat(missingDestination); !os.IsNotExist(err) {
		t.Fatalf("missing object left downloaded artifact: %v", err)
	}
}

func TestUploadAndCleanupUseExactS3Version(t *testing.T) {
	deletedVersion := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			w.Header().Set("x-amz-version-id", "version-1")
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			deletedVersion = r.URL.Query().Get("versionId")
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	storage := BackupStorageConfig{
		Provider: BackupStorageProviderS3Compatible, Bucket: "backups", Region: "us-east-1", Endpoint: server.URL,
		AccessKeyID: "access", SecretAccessKey: "secret", ForcePathStyle: true,
	}
	content := []byte("versioned-backup")
	digest := sha256.Sum256(content)
	artifactPath := filepath.Join(t.TempDir(), "data.tar.gz")
	if err := os.WriteFile(artifactPath, content, 0600); err != nil {
		t.Fatal(err)
	}
	remote, err := UploadBackupObject(context.Background(), storage, BackupObject{
		Project: "demo", Environment: "production", Volume: "data", BackupID: "20260729-120000-0123456789abcdef0123456789abcdef",
		ArtifactID: "artifact-0123456789abcdef0123456789abcdef", Path: artifactPath, Size: int64(len(content)), SHA256: hex.EncodeToString(digest[:]), CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if remote.VersionID != "version-1" {
		t.Fatalf("uploaded version ID = %q", remote.VersionID)
	}
	if err := DeleteBackupObjectVersion(context.Background(), storage, remote.Key, remote.VersionID); err != nil {
		t.Fatal(err)
	}
	if deletedVersion != "version-1" {
		t.Fatalf("deleted version ID = %q", deletedVersion)
	}
}

func TestR2RetentionUsesUnversionedObjectListing(t *testing.T) {
	usedListObjects := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Query().Get("list-type") != "2" || r.URL.Query().Has("versions") {
			http.Error(w, "version APIs are unsupported", http.StatusNotImplemented)
			return
		}
		usedListObjects = true
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><Name>backups</Name><Prefix>demo/production/</Prefix><IsTruncated>false</IsTruncated></ListBucketResult>`))
	}))
	defer server.Close()
	storage := BackupStorageConfig{
		Provider: BackupStorageProviderR2, Bucket: "backups", Region: "auto", Endpoint: server.URL,
		AccessKeyID: "access", SecretAccessKey: "secret", ForcePathStyle: true,
	}
	if err := CleanupBackupObjects(context.Background(), storage, BackupObjectRetention{Project: "demo", Environment: "production", RetentionDays: 7}); err != nil {
		t.Fatal(err)
	}
	if !usedListObjects {
		t.Fatal("R2 retention did not use ListObjectsV2")
	}
}

func TestSuspendedS3VersioningRequiresVersionAwareRetention(t *testing.T) {
	if !bucketVersioningHasHistory(types.BucketVersioningStatusSuspended) || !bucketVersioningHasHistory(types.BucketVersioningStatusEnabled) || bucketVersioningHasHistory("") {
		t.Fatal("S3 version-history mode classification is incorrect")
	}
}

func TestVersioningFallbackRequiresDefinitiveUnsupportedResponse(t *testing.T) {
	if !isVersioningAPIUnsupported(&smithy.GenericAPIError{Code: "NotImplemented", Message: "unsupported"}) {
		t.Fatal("NotImplemented was not recognized as unsupported")
	}
	if isVersioningAPIUnsupported(&smithy.GenericAPIError{Code: "AccessDenied", Message: "denied"}) {
		t.Fatal("AccessDenied incorrectly enabled unversioned fallback")
	}
}

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

func TestValidateBackupStorageRejectsEndpointCredentials(t *testing.T) {
	storage := BackupStorageConfig{
		Provider: BackupStorageProviderS3Compatible, Bucket: "backups", Region: "us-east-1",
		Endpoint: "https://user:secret@store.example", AccessKeyID: "access", SecretAccessKey: "secret",
	}
	if err := ValidateBackupStorage(storage); err == nil || !strings.Contains(err.Error(), "must not include credentials") {
		t.Fatalf("endpoint userinfo error = %v", err)
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

func TestBackupObjectKeyIncludesNodeLocalArtifactIdentity(t *testing.T) {
	base := BackupObject{Project: "demo", Environment: "production", Volume: "pgdata", BackupID: "20260621-120000"}
	first := base
	first.ArtifactID = "artifact-0123456789abcdef0123456789abcdef"
	second := base
	second.ArtifactID = "artifact-fedcba9876543210fedcba9876543210"
	firstKey, secondKey := backupObjectKey("apps", first), backupObjectKey("apps", second)
	if firstKey == secondKey {
		t.Fatalf("distinct node-local artifacts collide at %q", firstKey)
	}
	if !strings.Contains(firstKey, first.ArtifactID) || !strings.Contains(secondKey, second.ArtifactID) {
		t.Fatalf("artifact identities missing from keys %q and %q", firstKey, secondKey)
	}
}
