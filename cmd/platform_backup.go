package cmd

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/platform"
	"github.com/redentordev/tako-cli/pkg/recovery"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodstate"
	"github.com/spf13/cobra"
)

var (
	platformBackupProvider           string
	platformBackupBucket             string
	platformBackupRegion             string
	platformBackupEndpoint           string
	platformBackupPrefix             string
	platformBackupForcePathStyle     bool
	platformBackupConfigDir          string
	platformBackupDataDir            string
	platformBackupWorkloads          string
	platformBackupNoWorkloads        bool
	platformBackupKeepLocal          bool
	platformBackupArchive            string
	platformBackupExpectedCluster    string
	platformBackupRestoreDestination string
)

var platformBackupCmd = &cobra.Command{Use: "backup", Short: "Create and verify external platform recovery bundles"}

var platformBackupCreateCmd = &cobra.Command{
	Use:          "create",
	Short:        "Create, verify, and upload a complete controller recovery bundle",
	SilenceUsage: true,
	RunE:         runPlatformBackupCreate,
}

var platformBackupVerifyCmd = &cobra.Command{
	Use:          "verify",
	Short:        "Verify an offline platform recovery bundle and every file digest",
	SilenceUsage: true,
	RunE:         runPlatformBackupVerify,
}

var platformBackupRestoreCmd = &cobra.Command{
	Use:          "restore",
	Short:        "Authenticate and extract a recovery bundle into a new staging directory",
	SilenceUsage: true,
	RunE:         runPlatformBackupRestore,
}

func init() {
	platformCmd.AddCommand(platformBackupCmd)
	platformBackupCmd.AddCommand(platformBackupCreateCmd, platformBackupVerifyCmd, platformBackupRestoreCmd)
	markHumanOnly(platformBackupCreateCmd, platformBackupVerifyCmd, platformBackupRestoreCmd)
	platformBackupCreateCmd.Flags().StringVar(&platformBackupProvider, "provider", takod.BackupStorageProviderS3, "External storage provider: s3, r2, or s3-compatible")
	platformBackupCreateCmd.Flags().StringVar(&platformBackupBucket, "bucket", "", "External recovery bucket")
	platformBackupCreateCmd.Flags().StringVar(&platformBackupRegion, "region", "", "Storage region (R2 defaults to auto)")
	platformBackupCreateCmd.Flags().StringVar(&platformBackupEndpoint, "endpoint", "", "S3-compatible HTTPS endpoint")
	platformBackupCreateCmd.Flags().StringVar(&platformBackupPrefix, "prefix", "platform-recovery", "Object key prefix")
	platformBackupCreateCmd.Flags().BoolVar(&platformBackupForcePathStyle, "force-path-style", false, "Use path-style S3 requests")
	platformBackupCreateCmd.Flags().StringVar(&platformBackupConfigDir, "config-dir", "/etc/tako", "Protected Tako configuration and identity directory")
	platformBackupCreateCmd.Flags().StringVar(&platformBackupDataDir, "data-dir", "/var/lib/tako", "Tako PaaS, operation journal, environment, and certificate data directory")
	platformBackupCreateCmd.Flags().StringVar(&platformBackupWorkloads, "workload-backup-manifest", "", "JSON evidence for externally stored persistent workload snapshots")
	platformBackupCreateCmd.Flags().BoolVar(&platformBackupNoWorkloads, "no-persistent-workloads", false, "Attest that this cluster has no persistent workload data")
	platformBackupCreateCmd.Flags().BoolVar(&platformBackupKeepLocal, "keep-local", false, "Keep the verified local bundle after external upload")
	_ = platformBackupCreateCmd.MarkFlagRequired("bucket")

	platformBackupVerifyCmd.Flags().StringVar(&platformBackupArchive, "archive", "", "Local recovery bundle to verify")
	platformBackupVerifyCmd.Flags().StringVar(&platformBackupExpectedCluster, "cluster-id", "", "Expected immutable cluster UUID")
	_ = platformBackupVerifyCmd.MarkFlagRequired("archive")
	_ = platformBackupVerifyCmd.MarkFlagRequired("cluster-id")
	platformBackupRestoreCmd.Flags().StringVar(&platformBackupArchive, "archive", "", "Encrypted local recovery bundle to authenticate and stage")
	platformBackupRestoreCmd.Flags().StringVar(&platformBackupExpectedCluster, "cluster-id", "", "Expected immutable cluster UUID")
	platformBackupRestoreCmd.Flags().StringVar(&platformBackupRestoreDestination, "destination", "", "New empty staging directory for authenticated recovery files")
	_ = platformBackupRestoreCmd.MarkFlagRequired("archive")
	_ = platformBackupRestoreCmd.MarkFlagRequired("cluster-id")
	_ = platformBackupRestoreCmd.MarkFlagRequired("destination")
}

type workloadBackupEvidence struct {
	Kind                  string                    `json:"kind"`
	NoPersistentWorkloads bool                      `json:"noPersistentWorkloads,omitempty"`
	Backups               []workloadBackupReference `json:"backups,omitempty"`
}

type workloadBackupReference struct {
	Project     string    `json:"project"`
	Environment string    `json:"environment"`
	Service     string    `json:"service"`
	Volume      string    `json:"volume"`
	Provider    string    `json:"provider"`
	Bucket      string    `json:"bucket"`
	Key         string    `json:"key"`
	SHA256      string    `json:"sha256"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"createdAt"`
}

func runPlatformBackupCreate(cmd *cobra.Command, _ []string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("platform recovery backup must run as root")
	}
	if (platformBackupWorkloads == "") == !platformBackupNoWorkloads {
		return fmt.Errorf("set exactly one of --workload-backup-manifest or --no-persistent-workloads")
	}
	installation, err := nodeidentity.Read(filepath.Join(platformBackupConfigDir, filepath.Base(nodeidentity.DefaultPath)))
	if err != nil {
		return fmt.Errorf("read immutable platform identity: %w", err)
	}
	recoveryKey, err := recovery.ParseKey(os.Getenv("TAKO_RECOVERY_KEY"))
	if err != nil {
		return err
	}
	storage := takod.BackupStorageConfig{
		Provider: platformBackupProvider, Bucket: platformBackupBucket, Region: platformBackupRegion,
		Endpoint: platformBackupEndpoint, Prefix: platformBackupPrefix,
		AccessKeyID: os.Getenv("TAKO_BACKUP_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("TAKO_BACKUP_SECRET_ACCESS_KEY"),
		SessionToken: os.Getenv("TAKO_BACKUP_SESSION_TOKEN"), ForcePathStyle: platformBackupForcePathStyle,
	}
	if err := validateRecoveryStorageTLS(storage); err != nil {
		return err
	}
	unlockSnapshot, err := recovery.AcquireSnapshotLock(platformBackupDataDir)
	if err != nil {
		return fmt.Errorf("acquire exclusive controller recovery snapshot authority: %w", err)
	}
	snapshotLocked := true
	defer func() {
		if snapshotLocked {
			unlockSnapshot()
		}
	}()
	if err := recovery.EnsureNoActiveControllerOperation(platformBackupDataDir, installation.AllocationPublicKey); err != nil {
		return err
	}
	if err := platform.ValidateControllerRecoverySnapshot(
		filepath.Join(platformBackupDataDir, platform.DefaultMembershipDirName, platform.DefaultMembershipName),
		filepath.Join(platformBackupConfigDir, filepath.Base(nodeidentity.DefaultInventoryPath)),
		installation,
	); err != nil {
		return fmt.Errorf("validate authoritative controller recovery snapshot: %w", err)
	}
	evidencePath, cleanupEvidence, err := prepareWorkloadBackupEvidence(cmd.Context(), platformBackupDataDir, platformBackupWorkloads, platformBackupNoWorkloads, storage)
	if err != nil {
		return err
	}
	defer cleanupEvidence()
	temporaryDir, err := os.MkdirTemp("", "tako-platform-recovery-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporaryDir)
	backupID, err := newPlatformBackupID(time.Now().UTC())
	if err != nil {
		return err
	}
	encryptedArchive := filepath.Join(temporaryDir, "tako-platform-"+installation.ClusterID+"-"+backupID+".tako-recovery")
	result, err := recovery.CreateEncrypted(encryptedArchive, installation.ClusterID, []recovery.Source{
		{Path: platformBackupConfigDir, Archive: "etc/tako", Required: true},
		{Path: platformBackupDataDir, Archive: "var/lib/tako", Required: true},
		{Path: evidencePath, Archive: "recovery/persistent-workloads.json", Required: true},
	}, recoveryKey)
	if err != nil {
		return err
	}
	unlockSnapshot()
	snapshotLocked = false
	encryptedSHA := result.SHA256
	if _, err := recovery.VerifyEncrypted(encryptedArchive, installation.ClusterID, recoveryKey); err != nil {
		return fmt.Errorf("authenticate encrypted recovery bundle before upload: %w", err)
	}
	remote, err := takod.UploadBackupObject(cmd.Context(), storage, takod.BackupObject{
		Project: "platform", Environment: "recovery", Volume: "control-plane", BackupID: backupID,
		Path: encryptedArchive, CreatedAt: result.Manifest.CreatedAt,
	})
	if err != nil {
		return fmt.Errorf("upload recovery bundle outside node 1: %w", err)
	}
	encryptedInfo, err := os.Stat(encryptedArchive)
	if err != nil {
		return err
	}
	remoteInfo, err := takod.InspectBackupObject(cmd.Context(), storage, remote.Key)
	if err != nil || remoteInfo.Size != encryptedInfo.Size() {
		return fmt.Errorf("uploaded recovery object size does not match local encrypted bundle")
	}
	readback := filepath.Join(temporaryDir, "readback.tako-recovery")
	if err := takod.DownloadBackupObjectExact(cmd.Context(), storage, remote.Key, readback, encryptedInfo.Size()); err != nil {
		return fmt.Errorf("read back uploaded recovery object: %w", err)
	}
	readbackSHA, err := fileSHA256(readback)
	if err != nil || readbackSHA != encryptedSHA {
		return fmt.Errorf("uploaded recovery object digest does not match the exact local bundle")
	}
	if _, err := recovery.VerifyEncrypted(readback, installation.ClusterID, recoveryKey); err != nil {
		return fmt.Errorf("uploaded recovery object failed authenticated readback: %w", err)
	}
	localPath := "removed after authenticated upload readback"
	if platformBackupKeepLocal {
		destination := filepath.Join(".", filepath.Base(encryptedArchive))
		if err := copyRecoveryBundle(encryptedArchive, destination); err != nil {
			return err
		}
		localPath = destination
	}
	fmt.Fprintf(humanOut(), "Platform recovery uploaded and read back\nCluster: %s\nObject: %s/%s\nEncrypted SHA256: %s\nLocal: %s\n", installation.ClusterID, remote.Bucket, remote.Key, encryptedSHA, localPath)
	return nil
}

func newPlatformBackupID(now time.Time) (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("create unique platform backup identity: %w", err)
	}
	return now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(random), nil
}

func runPlatformBackupVerify(_ *cobra.Command, _ []string) error {
	key, err := recovery.ParseKey(os.Getenv("TAKO_RECOVERY_KEY"))
	if err != nil {
		return err
	}
	manifest, err := recovery.VerifyEncrypted(platformBackupArchive, strings.TrimSpace(platformBackupExpectedCluster), key)
	if err != nil {
		return err
	}
	fmt.Fprintf(humanOut(), "Platform recovery verified\nCluster: %s\nCreated: %s\nEntries: %d\n", manifest.ClusterID, manifest.CreatedAt.Format(time.RFC3339), len(manifest.Entries))
	return nil
}

func runPlatformBackupRestore(_ *cobra.Command, _ []string) error {
	key, err := recovery.ParseKey(os.Getenv("TAKO_RECOVERY_KEY"))
	if err != nil {
		return err
	}
	manifest, err := recovery.RestoreEncrypted(platformBackupArchive, platformBackupRestoreDestination, strings.TrimSpace(platformBackupExpectedCluster), key)
	if err != nil {
		return err
	}
	fmt.Fprintf(humanOut(), "Platform recovery staged\nCluster: %s\nDestination: %s\nEntries: %d\nReview the staged files before replacing live controller state.\n", manifest.ClusterID, platformBackupRestoreDestination, len(manifest.Entries))
	return nil
}

func prepareWorkloadBackupEvidence(ctx context.Context, dataDir, path string, noWorkloads bool, baseStorage takod.BackupStorageConfig) (string, func(), error) {
	required, err := persistentWorkloadRequirements(dataDir)
	if err != nil {
		return "", func() {}, err
	}
	if noWorkloads {
		if len(required) != 0 {
			return "", func() {}, fmt.Errorf("--no-persistent-workloads contradicts %d persistent volume(s) in authoritative desired state", len(required))
		}
		file, err := os.CreateTemp("", "tako-workload-evidence-*.json")
		if err != nil {
			return "", func() {}, err
		}
		data, _ := json.Marshal(workloadBackupEvidence{Kind: "TakoPersistentWorkloadBackupEvidence", NoPersistentWorkloads: true})
		_, writeErr := file.Write(append(data, '\n'))
		closeErr := file.Close()
		if writeErr != nil {
			os.Remove(file.Name())
			return "", func() {}, writeErr
		}
		if closeErr != nil {
			os.Remove(file.Name())
			return "", func() {}, closeErr
		}
		return file.Name(), func() { _ = os.Remove(file.Name()) }, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", func() {}, err
	}
	var evidence workloadBackupEvidence
	if err := json.Unmarshal(data, &evidence); err != nil || evidence.Kind != "TakoPersistentWorkloadBackupEvidence" || evidence.NoPersistentWorkloads || len(evidence.Backups) == 0 {
		return "", func() {}, fmt.Errorf("persistent workload backup evidence is invalid or empty")
	}
	covered := map[string]bool{}
	for _, backup := range evidence.Backups {
		if backup.Project == "" || backup.Environment == "" || backup.Service == "" || backup.Volume == "" || backup.Provider == "" || backup.Bucket == "" || backup.Key == "" || backup.SHA256 == "" || backup.Size <= 0 || backup.CreatedAt.IsZero() {
			return "", func() {}, fmt.Errorf("persistent workload backup evidence has an incomplete external object reference")
		}
		if _, err := hex.DecodeString(backup.SHA256); err != nil || len(backup.SHA256) != sha256.Size*2 {
			return "", func() {}, fmt.Errorf("persistent workload backup evidence has an invalid SHA256")
		}
		covered[workloadEvidenceKey(backup.Project, backup.Environment, backup.Service, backup.Volume)] = true
		storage := baseStorage
		storage.Provider, storage.Bucket = backup.Provider, backup.Bucket
		objectInfo, err := takod.InspectBackupObject(ctx, storage, backup.Key)
		if err != nil {
			return "", func() {}, fmt.Errorf("inspect persistent workload object %s: %w", backup.Key, err)
		}
		if objectInfo.Size != backup.Size {
			return "", func() {}, fmt.Errorf("persistent workload object %s size does not match evidence", backup.Key)
		}
		now := time.Now().UTC()
		if backup.CreatedAt.After(now.Add(time.Minute)) || now.Sub(backup.CreatedAt) > 24*time.Hour || objectInfo.LastModified.IsZero() || objectInfo.LastModified.Before(backup.CreatedAt.Add(-time.Minute)) {
			return "", func() {}, fmt.Errorf("persistent workload object %s is not a fresh backup point", backup.Key)
		}
		actual, err := takod.HashBackupObjectExact(ctx, storage, backup.Key, backup.Size)
		if err != nil {
			return "", func() {}, fmt.Errorf("verify persistent workload object %s: %w", backup.Key, err)
		}
		if actual != strings.ToLower(backup.SHA256) {
			return "", func() {}, fmt.Errorf("persistent workload object %s digest does not match evidence", backup.Key)
		}
	}
	for key := range required {
		if !covered[key] {
			return "", func() {}, fmt.Errorf("persistent workload evidence is missing %s", key)
		}
	}
	return path, func() {}, nil
}

func persistentWorkloadRequirements(dataDir string) (map[string]bool, error) {
	required := map[string]bool{}
	pattern := filepath.Join(dataDir, "desired", "*", "*", "revision.json")
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var desired takodstate.DesiredRevision
		if err := json.Unmarshal(data, &desired); err != nil {
			return nil, fmt.Errorf("decode authoritative desired state %s: %w", path, err)
		}
		for serviceName, service := range desired.Services {
			if !service.Persistent && len(service.Volumes) == 0 {
				continue
			}
			if service.Persistent && len(service.Volumes) == 0 {
				return nil, fmt.Errorf("persistent service %s/%s/%s has no named volume evidence boundary", desired.Project, desired.Environment, serviceName)
			}
			for _, volume := range service.Volumes {
				name := strings.TrimSpace(strings.SplitN(volume, ":", 2)[0])
				if name == "" {
					continue
				}
				required[workloadEvidenceKey(desired.Project, desired.Environment, serviceName, name)] = true
			}
		}
	}
	return required, nil
}

func workloadEvidenceKey(project, environment, service, volume string) string {
	return project + "/" + environment + "/" + service + "/" + volume
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func validateRecoveryStorageTLS(storage takod.BackupStorageConfig) error {
	if strings.TrimSpace(storage.Endpoint) == "" {
		return nil
	}
	parsed, err := url.Parse(storage.Endpoint)
	if err != nil || parsed.Scheme != "https" {
		return fmt.Errorf("platform recovery storage endpoint must use HTTPS")
	}
	return nil
}

func copyRecoveryBundle(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	failed := true
	defer func() {
		_ = output.Close()
		if failed {
			_ = os.Remove(destination)
		}
	}()
	if _, err := io.Copy(output, input); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	failed = false
	return nil
}
