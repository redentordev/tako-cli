package takod

import (
	"context"
	"fmt"
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
	Path        string
	CreatedAt   time.Time
}

type BackupObjectRetention struct {
	Project       string
	Environment   string
	Volume        string
	RetentionDays int
}

var (
	uploadBackupObjectS3With = uploadBackupObjectS3
	cleanupBackupObjectsWith = cleanupBackupObjectsS3
)

func UploadBackupObject(ctx context.Context, storage BackupStorageConfig, object BackupObject) (*BackupRemoteInfo, error) {
	storage = normalizeBackupStorage(storage)
	if err := ValidateBackupStorage(storage); err != nil {
		return nil, err
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
	if storage.Endpoint != "" {
		parsed, err := url.Parse(storage.Endpoint)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("backup storage endpoint must be an absolute URL")
		}
		if parsed.Scheme != "https" && parsed.Scheme != "http" {
			return fmt.Errorf("backup storage endpoint must use http or https")
		}
		if parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("backup storage endpoint must not include query or fragment")
		}
	}
	if storage.AccessKeyID == "" {
		return fmt.Errorf("backup storage accessKeyId is required")
	}
	if storage.SecretAccessKey == "" {
		return fmt.Errorf("backup storage secretAccessKey is required")
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
	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(storage.Bucket),
		Key:         aws.String(key),
		Body:        file,
		ContentType: aws.String("application/gzip"),
		Metadata: map[string]string{
			"tako-project":     object.Project,
			"tako-environment": object.Environment,
			"tako-volume":      object.Volume,
			"tako-backup-id":   object.BackupID,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to upload backup to object storage: %w", err)
	}
	return &BackupRemoteInfo{
		Provider: storage.Provider,
		Bucket:   storage.Bucket,
		Key:      key,
		Endpoint: storage.Endpoint,
	}, nil
}

func cleanupBackupObjectsS3(ctx context.Context, storage BackupStorageConfig, retention BackupObjectRetention) error {
	client, err := backupS3Client(ctx, storage)
	if err != nil {
		return err
	}
	prefix := backupObjectRetentionPrefix(storage.Prefix, retention)
	cutoff := time.Now().UTC().AddDate(0, 0, -retention.RetentionDays)
	var continuation *string
	for {
		response, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(storage.Bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: continuation,
		})
		if err != nil {
			return fmt.Errorf("failed to list object backups: %w", err)
		}

		var expired []types.ObjectIdentifier
		for _, object := range response.Contents {
			if object.Key == nil || object.LastModified == nil {
				continue
			}
			if object.LastModified.UTC().After(cutoff) {
				continue
			}
			expired = append(expired, types.ObjectIdentifier{Key: object.Key})
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

func deleteBackupObjectBatch(ctx context.Context, client *s3.Client, bucket string, objects []types.ObjectIdentifier) error {
	for len(objects) > 0 {
		batchSize := len(objects)
		if batchSize > 1000 {
			batchSize = 1000
		}
		batch := objects[:batchSize]
		objects = objects[batchSize:]
		_, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(bucket),
			Delete: &types.Delete{
				Objects: batch,
				Quiet:   aws.Bool(true),
			},
		})
		if err != nil {
			return fmt.Errorf("failed to delete old object backups: %w", err)
		}
	}
	return nil
}

func backupS3Client(ctx context.Context, storage BackupStorageConfig) (*s3.Client, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(storage.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(storage.AccessKeyID, storage.SecretAccessKey, storage.SessionToken)),
	)
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
	parts := []string{
		cleanObjectKeyPrefix(prefix),
		object.Project,
		object.Environment,
		object.Volume,
		backupFileName(object.Volume, object.BackupID),
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
