package takod

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

const (
	BackupStorageProviderS3           = "s3"
	BackupStorageProviderR2           = "r2"
	BackupStorageProviderS3Compatible = "s3-compatible"
)

type BackupObject struct {
	Project     string
	Environment string
	Volume      string
	BackupID    string
	ArtifactID  string
	Path        string
	Size        int64
	SHA256      string
	CreatedAt   time.Time
}

type BackupObjectRetention struct {
	Project       string
	Environment   string
	Volume        string
	RetentionDays int
}

type BackupObjectInfo struct {
	Size         int64
	LastModified time.Time
}

var (
	uploadBackupObjectS3With         = uploadBackupObjectS3
	cleanupBackupObjectsWith         = cleanupBackupObjectsS3
	downloadBackupObjectWith         = downloadBackupObjectS3
	downloadBackupObjectVerifiedWith = downloadBackupObjectVerifiedS3
	hashBackupObjectExactWith        = HashBackupObjectExact
	deleteBackupObjectWith           = deleteBackupObjectS3
	deleteBackupObjectVersionWith    = deleteBackupObjectVersionS3
)

// DownloadBackupObjectExact refuses an object whose declared or streamed size
// differs from expectedSize. This bounds disk use and makes upload readback
// prove the exact newly uploaded object rather than merely any valid bundle.
func DownloadBackupObjectExact(ctx context.Context, storage BackupStorageConfig, key, destination string, expectedSize int64) error {
	storage = normalizeBackupStorage(storage)
	if err := ValidateBackupStorage(storage); err != nil {
		return err
	}
	key = strings.TrimSpace(key)
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "..") || hasControlChars(key) {
		return fmt.Errorf("backup object key is invalid")
	}
	if strings.TrimSpace(destination) == "" {
		return fmt.Errorf("backup download destination is required")
	}
	if expectedSize <= 0 {
		return fmt.Errorf("backup download expected size is invalid")
	}
	return downloadBackupObjectWith(ctx, storage, key, destination, expectedSize)
}

// DownloadBackupObjectVerified writes only an exact-size object whose SHA-256
// matches independently supplied artifact evidence. A failed download or
// digest check leaves no destination file behind.
func DownloadBackupObjectVerified(ctx context.Context, storage BackupStorageConfig, key, destination string, expectedSize int64, expectedSHA256 string) error {
	storage = normalizeBackupStorage(storage)
	if err := ValidateBackupStorage(storage); err != nil {
		return err
	}
	key = strings.TrimSpace(key)
	expectedSHA256 = strings.TrimSpace(expectedSHA256)
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "..") || hasControlChars(key) {
		return fmt.Errorf("backup object key is invalid")
	}
	if strings.TrimSpace(destination) == "" || expectedSize <= 0 {
		return fmt.Errorf("backup download destination or expected size is invalid")
	}
	if len(expectedSHA256) != sha256.Size*2 {
		return fmt.Errorf("backup artifact SHA-256 is invalid")
	}
	if _, err := hex.DecodeString(expectedSHA256); err != nil || expectedSHA256 != strings.ToLower(expectedSHA256) {
		return fmt.Errorf("backup artifact SHA-256 is invalid")
	}
	return downloadBackupObjectVerifiedWith(ctx, storage, key, destination, expectedSize, expectedSHA256)
}

func InspectBackupObject(ctx context.Context, storage BackupStorageConfig, key string) (*BackupObjectInfo, error) {
	storage = normalizeBackupStorage(storage)
	if err := ValidateBackupStorage(storage); err != nil {
		return nil, err
	}
	key = strings.TrimSpace(key)
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "..") || hasControlChars(key) {
		return nil, fmt.Errorf("backup object key is invalid")
	}
	client, err := backupS3Client(ctx, storage)
	if err != nil {
		return nil, err
	}
	response, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(storage.Bucket), Key: aws.String(key)})
	if err != nil {
		return nil, fmt.Errorf("failed to inspect backup object: %w", err)
	}
	info := &BackupObjectInfo{}
	if response.ContentLength != nil {
		info.Size = *response.ContentLength
	}
	if response.LastModified != nil {
		info.LastModified = response.LastModified.UTC()
	}
	return info, nil
}

// HashBackupObjectExact verifies the remote length and hashes it as a bounded
// stream, avoiding a controller-disk copy of persistent workload backups.
func HashBackupObjectExact(ctx context.Context, storage BackupStorageConfig, key string, expectedSize int64) (string, error) {
	storage = normalizeBackupStorage(storage)
	if err := ValidateBackupStorage(storage); err != nil {
		return "", err
	}
	key = strings.TrimSpace(key)
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "..") || hasControlChars(key) || expectedSize <= 0 {
		return "", fmt.Errorf("backup object key or expected size is invalid")
	}
	client, err := backupS3Client(ctx, storage)
	if err != nil {
		return "", err
	}
	response, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(storage.Bucket), Key: aws.String(key)})
	if err != nil {
		return "", fmt.Errorf("failed to download backup object for hashing: %w", err)
	}
	defer response.Body.Close()
	if response.ContentLength == nil || *response.ContentLength != expectedSize {
		return "", fmt.Errorf("backup object size does not match evidence")
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(response.Body, expectedSize+1))
	if err != nil {
		return "", err
	}
	if written != expectedSize {
		return "", fmt.Errorf("backup object body size does not match evidence")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func UploadBackupObject(ctx context.Context, storage BackupStorageConfig, object BackupObject) (*BackupRemoteInfo, error) {
	storage = normalizeBackupStorage(storage)
	if err := ValidateBackupStorage(storage); err != nil {
		return nil, err
	}
	if object.ArtifactID == "" {
		object.ArtifactID = object.BackupID
	}
	if err := validateBackupObject(object); err != nil {
		return nil, err
	}
	return uploadBackupObjectS3With(ctx, storage, object)
}

func CleanupBackupObjects(ctx context.Context, storage BackupStorageConfig, retention BackupObjectRetention) error {
	storage = normalizeBackupStorage(storage)
	if err := ValidateBackupStorage(storage); err != nil {
		return err
	}
	if retention.RetentionDays <= 0 {
		return nil
	}
	if !isSafeProjectName(retention.Project) {
		return fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(retention.Environment) {
		return fmt.Errorf("invalid environment name")
	}
	if retention.Volume != "" && !isSafeBackupVolume(retention.Volume) {
		return fmt.Errorf("invalid volume name")
	}
	return cleanupBackupObjectsWith(ctx, storage, retention)
}

func DeleteBackupObject(ctx context.Context, storage BackupStorageConfig, key string) error {
	return DeleteBackupObjectVersion(ctx, storage, key, "")
}

func DeleteBackupObjectVersion(ctx context.Context, storage BackupStorageConfig, key, versionID string) error {
	storage = normalizeBackupStorage(storage)
	if err := ValidateBackupStorage(storage); err != nil {
		return err
	}
	if err := validateBackupObjectKey(key); err != nil {
		return err
	}
	versionID = strings.TrimSpace(versionID)
	if hasControlChars(versionID) {
		return fmt.Errorf("backup object version ID is invalid")
	}
	if versionID == "" {
		return deleteBackupObjectWith(ctx, storage, key)
	}
	return deleteBackupObjectVersionWith(ctx, storage, key, versionID)
}

func ValidateBackupStorage(storage BackupStorageConfig) error {
	storage = normalizeBackupStorage(storage)
	switch storage.Provider {
	case BackupStorageProviderS3, BackupStorageProviderR2, BackupStorageProviderS3Compatible:
	default:
		return fmt.Errorf("backup storage provider must be s3, r2, or s3-compatible")
	}
	if storage.Bucket == "" {
		return fmt.Errorf("backup storage bucket is required")
	}
	if hasControlChars(storage.Bucket) || strings.Contains(storage.Bucket, "/") {
		return fmt.Errorf("backup storage bucket is invalid")
	}
	if storage.Region == "" {
		return fmt.Errorf("backup storage region is required")
	}
	if storage.Provider != BackupStorageProviderS3 && storage.Endpoint == "" {
		return fmt.Errorf("backup storage endpoint is required for %s", storage.Provider)
	}
	if storage.Provider == BackupStorageProviderR2 && storage.Versioned {
		return fmt.Errorf("R2 backup storage does not support versioned retention")
	}
	if storage.Endpoint != "" {
		parsed, err := url.Parse(storage.Endpoint)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("backup storage endpoint must be an absolute URL")
		}
		if parsed.Scheme != "https" && parsed.Scheme != "http" {
			return fmt.Errorf("backup storage endpoint must use http or https")
		}
		if parsed.User != nil {
			return fmt.Errorf("backup storage endpoint must not include credentials")
		}
		if parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("backup storage endpoint must not include query or fragment")
		}
	}
	if (storage.AccessKeyID == "") != (storage.SecretAccessKey == "") {
		return fmt.Errorf("backup storage accessKeyId and secretAccessKey must be set together; omit both to use the node's AWS credential chain")
	}
	if storage.SessionToken != "" && storage.AccessKeyID == "" {
		return fmt.Errorf("backup storage sessionToken requires accessKeyId and secretAccessKey")
	}
	for label, value := range map[string]string{
		"backup storage accessKeyId":     storage.AccessKeyID,
		"backup storage secretAccessKey": storage.SecretAccessKey,
		"backup storage sessionToken":    storage.SessionToken,
		"backup storage prefix":          storage.Prefix,
	} {
		if hasControlChars(value) {
			return fmt.Errorf("%s contains unsupported characters", label)
		}
	}
	if strings.Contains(storage.Prefix, "..") {
		return fmt.Errorf("backup storage prefix must not contain '..'")
	}
	return nil
}

func validateBackupObjectKey(key string) error {
	key = strings.TrimSpace(key)
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "..") || hasControlChars(key) {
		return fmt.Errorf("backup object key is invalid")
	}
	return nil
}

func normalizeBackupStorage(storage BackupStorageConfig) BackupStorageConfig {
	storage.Provider = strings.TrimSpace(storage.Provider)
	if storage.Provider == "" {
		storage.Provider = BackupStorageProviderS3
	}
	storage.Bucket = strings.TrimSpace(storage.Bucket)
	storage.Region = strings.TrimSpace(storage.Region)
	if storage.Region == "" && storage.Provider == BackupStorageProviderR2 {
		storage.Region = "auto"
	}
	storage.Endpoint = strings.TrimRight(strings.TrimSpace(storage.Endpoint), "/")
	storage.Prefix = cleanObjectKeyPrefix(storage.Prefix)
	storage.AccessKeyID = strings.TrimSpace(storage.AccessKeyID)
	storage.SecretAccessKey = strings.TrimSpace(storage.SecretAccessKey)
	storage.SessionToken = strings.TrimSpace(storage.SessionToken)
	return storage
}

func validateBackupObject(object BackupObject) error {
	if !isSafeProjectName(object.Project) {
		return fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(object.Environment) {
		return fmt.Errorf("invalid environment name")
	}
	if !isSafeBackupVolume(object.Volume) {
		return fmt.Errorf("invalid volume name")
	}
	if !isSafeBackupID(object.BackupID) {
		return fmt.Errorf("invalid backup ID")
	}
	if strings.TrimSpace(object.Path) == "" {
		return fmt.Errorf("backup object path is required")
	}
	validArtifactID := isSafeBackupID(object.ArtifactID) || isSafeBackupArtifactID(object.ArtifactID)
	if !validArtifactID || object.Size <= 0 || len(object.SHA256) != sha256.Size*2 {
		return fmt.Errorf("backup object artifact evidence is invalid")
	}
	if _, err := hex.DecodeString(object.SHA256); err != nil || object.SHA256 != strings.ToLower(object.SHA256) {
		return fmt.Errorf("backup object artifact evidence is invalid")
	}
	return nil
}

func uploadBackupObjectS3(ctx context.Context, storage BackupStorageConfig, object BackupObject) (*BackupRemoteInfo, error) {
	client, err := backupS3Client(ctx, storage)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(object.Path)
	if err != nil {
		return nil, fmt.Errorf("failed to open backup for upload: %w", err)
	}
	defer file.Close()

	key := backupObjectKey(storage.Prefix, object)
	contentType := "application/gzip"
	if strings.HasSuffix(object.Path, ".tako-recovery") {
		contentType = "application/octet-stream"
	}
	output, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(storage.Bucket),
		Key:         aws.String(key),
		Body:        file,
		IfNoneMatch: aws.String("*"),
		ContentType: aws.String(contentType),
		Metadata: map[string]string{
			"tako-project":     object.Project,
			"tako-environment": object.Environment,
			"tako-volume":      object.Volume,
			"tako-backup-id":   object.BackupID,
			"tako-artifact-id": object.ArtifactID,
			"tako-size":        fmt.Sprintf("%d", object.Size),
			"tako-sha256":      object.SHA256,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to upload backup to object storage: %w", err)
	}
	return &BackupRemoteInfo{
		Provider:  storage.Provider,
		Bucket:    storage.Bucket,
		Key:       key,
		VersionID: aws.ToString(output.VersionId),
		Endpoint:  storage.Endpoint,
	}, nil
}

func downloadBackupObjectS3(ctx context.Context, storage BackupStorageConfig, key, destination string, expectedSize int64) error {
	return downloadBackupObjectS3ToFile(ctx, storage, key, destination, expectedSize, "")
}

func downloadBackupObjectVerifiedS3(ctx context.Context, storage BackupStorageConfig, key, destination string, expectedSize int64, expectedSHA256 string) error {
	return downloadBackupObjectS3ToFile(ctx, storage, key, destination, expectedSize, expectedSHA256)
}

func downloadBackupObjectS3ToFile(ctx context.Context, storage BackupStorageConfig, key, destination string, expectedSize int64, expectedSHA256 string) error {
	client, err := backupS3Client(ctx, storage)
	if err != nil {
		return err
	}
	response, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(storage.Bucket), Key: aws.String(key)})
	if err != nil {
		return fmt.Errorf("failed to download backup object: %w", err)
	}
	defer response.Body.Close()
	if expectedSize > 0 && (response.ContentLength == nil || *response.ContentLength != expectedSize) {
		return fmt.Errorf("downloaded backup object size does not match expected upload")
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	failed := true
	defer func() {
		_ = file.Close()
		if failed {
			_ = os.Remove(destination)
		}
	}()
	hash := sha256.New()
	reader := io.Reader(response.Body)
	if expectedSize > 0 {
		reader = io.LimitReader(response.Body, expectedSize+1)
	}
	written, err := io.Copy(io.MultiWriter(file, hash), reader)
	if err != nil {
		return fmt.Errorf("download backup object body: %w", err)
	}
	if expectedSize > 0 && written != expectedSize {
		return fmt.Errorf("downloaded backup object body size does not match expected upload")
	}
	if expectedSHA256 != "" && hex.EncodeToString(hash.Sum(nil)) != expectedSHA256 {
		return fmt.Errorf("downloaded backup object SHA-256 does not match artifact evidence")
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	failed = false
	return nil
}

func cleanupBackupObjectsS3(ctx context.Context, storage BackupStorageConfig, retention BackupObjectRetention) error {
	client, err := backupS3Client(ctx, storage)
	if err != nil {
		return err
	}
	prefix := backupObjectRetentionPrefix(storage.Prefix, retention)
	cutoff := time.Now().UTC().AddDate(0, 0, -retention.RetentionDays)
	if storage.Provider == BackupStorageProviderS3 || storage.Provider == BackupStorageProviderS3Compatible {
		versioning, err := client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(storage.Bucket)})
		if err != nil {
			if storage.Provider == BackupStorageProviderS3 || storage.Versioned || !isVersioningAPIUnsupported(err) {
				return fmt.Errorf("verify backup bucket versioning mode: %w", err)
			}
		} else {
			actualVersioned := bucketVersioningHasHistory(versioning.Status)
			if actualVersioned != storage.Versioned {
				return fmt.Errorf("backup storage versioned setting does not match bucket versioning mode")
			}
		}
	}
	if !storage.Versioned {
		var continuation *string
		for {
			response, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
				Bucket: aws.String(storage.Bucket), Prefix: aws.String(prefix), ContinuationToken: continuation,
			})
			if err != nil {
				return fmt.Errorf("failed to list object backups: %w", err)
			}
			var expired []types.ObjectIdentifier
			for _, object := range response.Contents {
				if object.Key != nil && object.LastModified != nil && !object.LastModified.UTC().After(cutoff) {
					expired = append(expired, types.ObjectIdentifier{Key: object.Key})
				}
			}
			if err := deleteBackupObjectBatch(ctx, client, storage.Bucket, expired); err != nil {
				return err
			}
			if response.IsTruncated == nil || !*response.IsTruncated {
				return nil
			}
			continuation = response.NextContinuationToken
		}
	}
	var keyMarker, versionMarker *string
	for {
		response, err := client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{
			Bucket: aws.String(storage.Bucket), Prefix: aws.String(prefix),
			KeyMarker: keyMarker, VersionIdMarker: versionMarker,
		})
		if err != nil {
			return fmt.Errorf("failed to list object backups: %w", err)
		}

		var expired []types.ObjectIdentifier
		for _, object := range response.Versions {
			if object.Key == nil || object.LastModified == nil || object.LastModified.UTC().After(cutoff) {
				continue
			}
			expired = append(expired, types.ObjectIdentifier{Key: object.Key, VersionId: object.VersionId})
		}
		for _, marker := range response.DeleteMarkers {
			if marker.Key == nil || marker.LastModified == nil || marker.LastModified.UTC().After(cutoff) {
				continue
			}
			expired = append(expired, types.ObjectIdentifier{Key: marker.Key, VersionId: marker.VersionId})
		}
		if err := deleteBackupObjectBatch(ctx, client, storage.Bucket, expired); err != nil {
			return err
		}
		if response.IsTruncated == nil || !*response.IsTruncated {
			return nil
		}
		keyMarker, versionMarker = response.NextKeyMarker, response.NextVersionIdMarker
	}
}

func bucketVersioningHasHistory(status types.BucketVersioningStatus) bool {
	return status == types.BucketVersioningStatusEnabled || status == types.BucketVersioningStatusSuspended
}

func isVersioningAPIUnsupported(err error) bool {
	var apiError smithy.APIError
	if !errors.As(err, &apiError) {
		return false
	}
	switch strings.ToLower(apiError.ErrorCode()) {
	case "notimplemented", "not_implemented", "unsupported", "unsupportedoperation", "methodnotallowed":
		return true
	default:
		return false
	}
}

func deleteBackupObjectS3(ctx context.Context, storage BackupStorageConfig, key string) error {
	return deleteBackupObjectVersionS3(ctx, storage, key, "")
}

func deleteBackupObjectVersionS3(ctx context.Context, storage BackupStorageConfig, key, versionID string) error {
	client, err := backupS3Client(ctx, storage)
	if err != nil {
		return err
	}
	input := &s3.DeleteObjectInput{Bucket: aws.String(storage.Bucket), Key: aws.String(key)}
	if versionID != "" {
		input.VersionId = aws.String(versionID)
	}
	if _, err := client.DeleteObject(ctx, input); err != nil {
		return fmt.Errorf("failed to delete backup object: %w", err)
	}
	return nil
}

func deleteBackupObjectBatch(ctx context.Context, client *s3.Client, bucket string, objects []types.ObjectIdentifier) error {
	for len(objects) > 0 {
		batchSize := len(objects)
		if batchSize > 1000 {
			batchSize = 1000
		}
		batch := objects[:batchSize]
		objects = objects[batchSize:]
		response, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(bucket),
			Delete: &types.Delete{
				Objects: batch,
				Quiet:   aws.Bool(true),
			},
		})
		if err != nil {
			return fmt.Errorf("failed to delete old object backups: %w", err)
		}
		if response == nil {
			return fmt.Errorf("failed to delete old object backups: empty object-store response")
		}
		if len(response.Errors) > 0 {
			first := response.Errors[0]
			return fmt.Errorf("failed to delete %d old object backup(s): %s", len(response.Errors), aws.ToString(first.Code))
		}
	}
	return nil
}

func backupS3Client(ctx context.Context, storage BackupStorageConfig) (*s3.Client, error) {
	options := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(storage.Region)}
	// Without static keys, LoadDefaultConfig resolves the node's credential
	// chain: environment, shared config, then the EC2 instance profile.
	if storage.AccessKeyID != "" {
		options = append(options, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(storage.AccessKeyID, storage.SecretAccessKey, storage.SessionToken)))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("failed to configure object storage client: %w", err)
	}
	client := s3.NewFromConfig(cfg, func(options *s3.Options) {
		if storage.Endpoint != "" {
			options.BaseEndpoint = aws.String(storage.Endpoint)
		}
		options.UsePathStyle = storage.ForcePathStyle
	})
	return client, nil
}

func backupObjectKey(prefix string, object BackupObject) string {
	filename := backupFileName(object.Volume, object.BackupID)
	if object.ArtifactID != "" && object.ArtifactID != object.BackupID {
		filename = object.Volume + "_" + object.BackupID + "_" + object.ArtifactID + ".tar.gz"
	}
	parts := []string{
		cleanObjectKeyPrefix(prefix),
		object.Project,
		object.Environment,
		object.Volume,
		filename,
	}
	return joinObjectKey(parts...)
}

func backupObjectRetentionPrefix(prefix string, retention BackupObjectRetention) string {
	parts := []string{
		cleanObjectKeyPrefix(prefix),
		retention.Project,
		retention.Environment,
	}
	if retention.Volume != "" {
		parts = append(parts, retention.Volume)
	}
	joined := joinObjectKey(parts...)
	if joined == "" {
		return ""
	}
	return joined + "/"
}

func cleanObjectKeyPrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	prefix = strings.Trim(prefix, "/")
	if prefix == "." {
		return ""
	}
	return prefix
}

func joinObjectKey(parts ...string) string {
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.Trim(part, "/")
		if part != "" {
			cleaned = append(cleaned, part)
		}
	}
	return path.Join(cleaned...)
}
