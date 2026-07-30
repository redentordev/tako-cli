package takod

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

const (
	BackupDir                         = "/var/lib/tako/backups"
	DefaultRetention                  = 7
	backupImage                       = "alpine:3.20"
	backupArtifactFormatVolumeTarGzip = "volume-tar-gzip"
)

var errBackupDirectoryLocked = errors.New("another backup operation is active for this environment")

var backupRootDir = BackupDir
var backupRestoreBarrierMu sync.RWMutex

type backupRestoreBarrierContextKey struct{}

func acquireBackupRestoreMutationBarrier(ctx context.Context) (context.Context, func()) {
	if held, _ := ctx.Value(backupRestoreBarrierContextKey{}).(bool); held {
		return ctx, func() {}
	}
	backupRestoreBarrierMu.RLock()
	return context.WithValue(ctx, backupRestoreBarrierContextKey{}, true), backupRestoreBarrierMu.RUnlock
}

func acquireExclusiveBackupRestoreMutationBarrier(ctx context.Context) (context.Context, func()) {
	if held, _ := ctx.Value(backupRestoreBarrierContextKey{}).(bool); held {
		return ctx, func() {}
	}
	backupRestoreBarrierMu.Lock()
	return context.WithValue(ctx, backupRestoreBarrierContextKey{}, true), backupRestoreBarrierMu.Unlock
}

type BackupRequest struct {
	Project          string               `json:"project"`
	Environment      string               `json:"environment"`
	Volume           string               `json:"volume,omitempty"`
	DockerVolume     string               `json:"dockerVolume,omitempty"`
	ExternalVolume   bool                 `json:"externalVolume,omitempty"`
	BackupID         string               `json:"backupId,omitempty"`
	RetentionDays    int                  `json:"retentionDays,omitempty"`
	Storage          *BackupStorageConfig `json:"storage,omitempty"`
	Approved         bool                 `json:"approved,omitempty"`
	MinimumFreeBytes int64                `json:"-"`
	DockerDataRoot   string               `json:"-"`
	DataDir          string               `json:"-"`
}

type BackupInfo struct {
	ID           string            `json:"id"`
	ArtifactID   string            `json:"artifactId,omitempty"`
	Format       string            `json:"format,omitempty"`
	Service      string            `json:"service,omitempty"`
	Volume       string            `json:"volume"`
	Size         int64             `json:"size"`
	ExpandedSize int64             `json:"expandedSize,omitempty"`
	CreatedAt    time.Time         `json:"createdAt"`
	Path         string            `json:"path"`
	Compression  string            `json:"compression"`
	SHA256       string            `json:"sha256,omitempty"`
	Remote       *BackupRemoteInfo `json:"remote,omitempty"`
	Warnings     []string          `json:"warnings,omitempty"`
}

type BackupListResponse struct {
	Backups []BackupInfo `json:"backups"`
}

type BackupCleanupResponse struct {
	Deleted int `json:"deleted"`
}

type BackupRemoteInfo struct {
	Provider   string    `json:"provider"`
	Bucket     string    `json:"bucket"`
	Key        string    `json:"key"`
	VersionID  string    `json:"versionId,omitempty"`
	Endpoint   string    `json:"endpoint,omitempty"`
	VerifiedAt time.Time `json:"verifiedAt,omitempty"`
}

type backupArtifactManifest struct {
	SchemaVersion int        `json:"schemaVersion"`
	Backup        BackupInfo `json:"backup"`
}

type backupRestoreJournal struct {
	SchemaVersion            int    `json:"schemaVersion"`
	Phase                    string `json:"phase"`
	TargetVolume             string `json:"targetVolume"`
	TargetIdentity           string `json:"targetIdentity"`
	TargetOwnershipOperation string `json:"targetOwnershipOperation,omitempty"`
	TargetCreationOperation  string `json:"targetCreationOperation,omitempty"`
	GuardContainer           string `json:"guardContainer"`
	StageVolume              string `json:"stageVolume"`
	BackupFile               string `json:"backupFile"`
	RollbackFile             string `json:"rollbackFile,omitempty"`
	CreatedTarget            bool   `json:"createdTarget"`
}

type RestoreRecoveryResult struct {
	Action        string `json:"action"`
	Journal       string `json:"journal"`
	Phase         string `json:"phase"`
	TargetVolume  string `json:"targetVolume"`
	CreatedTarget bool   `json:"createdTarget"`
	Recovered     bool   `json:"recovered"`
}

type BackupStorageConfig struct {
	Provider        string `json:"provider,omitempty"`
	Bucket          string `json:"bucket,omitempty"`
	Region          string `json:"region,omitempty"`
	Endpoint        string `json:"endpoint,omitempty"`
	Prefix          string `json:"prefix,omitempty"`
	AccessKeyID     string `json:"accessKeyId,omitempty"`
	SecretAccessKey string `json:"secretAccessKey,omitempty"`
	SessionToken    string `json:"sessionToken,omitempty"`
	ForcePathStyle  bool   `json:"forcePathStyle,omitempty"`
	Versioned       bool   `json:"versioned,omitempty"`
}

func CreateVolumeBackup(ctx context.Context, req BackupRequest) (*BackupInfo, error) {
	ctx, releaseBarrier := acquireBackupRestoreMutationBarrier(ctx)
	defer releaseBarrier()
	if err := ensureNoPendingRestoreTransactionsOnNode(); err != nil {
		return nil, err
	}
	if err := validateBackupRequest(req, true, false); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	backupPath := backupDirectory(req)
	if err := os.MkdirAll(backupPath, 0750); err != nil {
		return nil, fmt.Errorf("failed to create backup directory: %w", err)
	}
	releaseLock, err := acquireBackupDirectoryLock(backupPath)
	if err != nil {
		return nil, err
	}
	defer releaseLock()
	if err := ensureNoPendingRestoreTransactions(backupPath); err != nil {
		return nil, err
	}
	if err := cleanupPartialBackupArtifacts(backupPath); err != nil {
		return nil, err
	}

	fullVolumeName := fullBackupVolumeName(req)
	if _, err := runDocker(ctx, "volume", "inspect", fullVolumeName); err != nil {
		return nil, fmt.Errorf("volume %s does not exist", fullVolumeName)
	}

	backupID := backupIDForRequest(req, time.Now())
	backupFile := backupFileName(req.Volume, backupID)
	tempSuffix, err := newBackupArtifactID()
	if err != nil {
		return nil, fmt.Errorf("create backup temporary identity: %w", err)
	}
	tempFile := "." + backupFile + "." + tempSuffix + ".partial"
	path := filepath.Join(backupPath, backupFile)
	tempPath := filepath.Join(backupPath, tempFile)
	defer os.Remove(tempPath)
	if _, err := runDocker(
		ctx,
		"run", "--rm",
		"-v", fullVolumeName+":/source:ro",
		"-v", backupPath+":/backup",
		backupImage,
		"tar", "-czf", "/backup/"+tempFile, "-C", "/source", ".",
	); err != nil {
		return nil, fmt.Errorf("failed to create backup: %w", err)
	}
	if err := os.Chmod(tempPath, 0600); err != nil {
		return nil, fmt.Errorf("secure backup artifact: %w", err)
	}
	if err := syncFile(tempPath); err != nil {
		return nil, fmt.Errorf("sync backup artifact: %w", err)
	}
	tempInfo, err := os.Stat(tempPath)
	if err != nil {
		return nil, fmt.Errorf("inspect backup artifact: %w", err)
	}
	createdAt, _ := backupIDTimestamp(backupID)
	artifactID, err := newBackupArtifactID()
	if err != nil {
		return nil, fmt.Errorf("create backup artifact identity: %w", err)
	}
	info := BackupInfo{
		ID: backupID, ArtifactID: artifactID, Format: backupArtifactFormatVolumeTarGzip,
		Volume: req.Volume, Size: tempInfo.Size(), CreatedAt: createdAt.UTC(), Path: path, Compression: "gzip",
	}
	info.ExpandedSize, err = backupArchiveExpandedSize(tempPath)
	if err != nil {
		return nil, err
	}
	info.SHA256, err = hashFileSHA256(tempPath)
	if err != nil {
		return nil, err
	}
	publishErr := func() error {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("backup artifact already exists: %s", backupFile)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect backup artifact destination: %w", err)
		}
		if err := writeBackupArtifactManifest(path, info); err != nil {
			return err
		}
		published := false
		defer func() {
			if !published {
				_ = os.Remove(path)
				_ = os.Remove(backupManifestPath(path))
			}
		}()
		if err := os.Link(tempPath, path); err != nil {
			return fmt.Errorf("publish backup artifact: %w", err)
		}
		if err := os.Remove(tempPath); err != nil {
			return fmt.Errorf("finalize backup artifact: %w", err)
		}
		if err := syncDirectory(backupPath); err != nil {
			return fmt.Errorf("sync backup directory: %w", err)
		}
		published = true
		return nil
	}()
	if publishErr != nil {
		return nil, publishErr
	}
	if req.Storage != nil {
		remote, err := UploadBackupObject(ctx, *req.Storage, BackupObject{
			Project:     req.Project,
			Environment: req.Environment,
			Volume:      req.Volume,
			BackupID:    backupID,
			ArtifactID:  info.ArtifactID,
			Path:        path,
			Size:        info.Size,
			SHA256:      info.SHA256,
			CreatedAt:   info.CreatedAt,
		})
		if err != nil {
			info.Warnings = append(info.Warnings, fmt.Sprintf("local backup created but upload failed: %v", err))
			return &info, nil
		}
		remoteDigest, err := hashBackupObjectExactWith(ctx, *req.Storage, remote.Key, info.Size)
		if err != nil || remoteDigest != info.SHA256 {
			if err == nil {
				err = fmt.Errorf("remote SHA-256 does not match local artifact")
			}
			info.Warnings = append(info.Warnings, fmt.Sprintf("local backup created but remote readback verification failed: %v", err))
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			deleteErr := DeleteBackupObjectVersion(cleanupCtx, *req.Storage, remote.Key, remote.VersionID)
			cancel()
			if deleteErr != nil {
				info.Warnings = append(info.Warnings, fmt.Sprintf("unverified remote object cleanup failed: %v", deleteErr))
			}
			_ = writeBackupArtifactManifest(path, info)
			return &info, nil
		}
		remote.VerifiedAt = time.Now().UTC()
		info.Remote = remote
		if err := writeBackupArtifactManifest(path, info); err != nil {
			return nil, err
		}
		if req.RetentionDays > 0 {
			if err := CleanupBackupObjects(ctx, *req.Storage, BackupObjectRetention{
				Project:       req.Project,
				Environment:   req.Environment,
				Volume:        req.Volume,
				RetentionDays: req.RetentionDays,
			}); err != nil {
				info.Warnings = append(info.Warnings, fmt.Sprintf("remote backup retention cleanup failed: %v", err))
			}
		}
	}
	return &info, nil
}

func cleanupPartialBackupArtifacts(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("inspect partial backup artifacts: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		remove := strings.HasPrefix(entry.Name(), ".") && strings.HasSuffix(entry.Name(), ".partial")
		if strings.HasSuffix(entry.Name(), ".tar.gz.manifest.json") {
			archive := strings.TrimSuffix(entry.Name(), ".manifest.json")
			if _, err := os.Lstat(filepath.Join(dir, archive)); os.IsNotExist(err) {
				remove = true
			} else if err != nil {
				return fmt.Errorf("inspect backup archive for manifest recovery: %w", err)
			}
		}
		if remove {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove abandoned backup publication artifact: %w", err)
			}
		}
	}
	return nil
}

func CleanupBackupPublicationResources(req BackupRequest) error {
	dir := backupDirectory(req)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect backup publication directory: %w", err)
	}
	releaseLock, err := acquireBackupDirectoryLock(dir)
	if err != nil {
		return err
	}
	defer releaseLock()
	if err := ensureNoPendingRestoreTransactions(dir); err != nil {
		return err
	}
	return cleanupPartialBackupArtifacts(dir)
}

func ensureNoPendingRestoreTransactions(dir string) error {
	journals, err := filepath.Glob(filepath.Join(dir, ".restore-transaction-*.json"))
	if err != nil {
		return fmt.Errorf("inspect pending restore transactions: %w", err)
	}
	if len(journals) > 0 {
		return fmt.Errorf("backup operations are blocked until the pending volume restore transaction is recovered")
	}
	return nil
}

func ensureNoPendingRestoreTransactionsOnNode() error {
	journals, err := filepath.Glob(filepath.Join(backupRootDir, "*", "*", ".restore-transaction-*.json"))
	if err != nil {
		return fmt.Errorf("inspect node restore transactions: %w", err)
	}
	if len(journals) > 0 {
		return fmt.Errorf("node workload mutations are blocked until the pending volume restore transaction is recovered")
	}
	return nil
}

func CleanupAllBackupPublicationResources() error {
	dirs, err := filepath.Glob(filepath.Join(backupRootDir, "*", "*"))
	if err != nil {
		return fmt.Errorf("list backup publication directories: %w", err)
	}
	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		releaseLock, err := acquireBackupDirectoryLock(dir)
		if err != nil {
			return err
		}
		err = cleanupPartialBackupArtifacts(dir)
		releaseLock()
		if err != nil {
			return err
		}
	}
	return nil
}

func RestoreVolumeBackup(ctx context.Context, req BackupRequest) (retErr error) {
	ctx, releaseBarrier := acquireExclusiveBackupRestoreMutationBarrier(ctx)
	defer releaseBarrier()
	if err := ensureNoPendingRestoreTransactionsOnNode(); err != nil {
		return err
	}
	if err := validateBackupRequest(req, true, true); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !req.Approved {
		return fmt.Errorf("destructive volume restore requires explicit approval")
	}
	var ownershipRoot string
	if strings.TrimSpace(req.DataDir) != "" {
		ownershipRoot = filepath.Join(req.DataDir, volumeOwnershipDirName)
		if err := os.MkdirAll(ownershipRoot, 0700); err != nil {
			return fmt.Errorf("create volume ownership state directory: %w", err)
		}
		releaseOwnership, err := acquireVolumeOwnershipStateLock(ctx, ownershipRoot)
		if err != nil {
			return fmt.Errorf("lock managed volume ownership during restore: %w", err)
		}
		defer releaseOwnership()
	}
	backupPath := backupDirectory(req)
	releaseLock, err := acquireBackupDirectoryLock(backupPath)
	if err != nil {
		return err
	}
	defer releaseLock()
	if err := ensureNoPendingRestoreTransactions(backupPath); err != nil {
		return err
	}
	backupFile := backupFileName(req.Volume, req.BackupID)
	backupFullPath := filepath.Join(backupPath, backupFile)
	if _, err := os.Stat(backupFullPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("backup not found: %s", backupFullPath)
		}
		return fmt.Errorf("failed to inspect backup: %w", err)
	}
	info, err := backupInfoFromPath(backupPath, backupFullPath)
	if err != nil {
		return err
	}
	if info.SHA256 != "" {
		digest, err := hashFileSHA256(backupFullPath)
		if err != nil {
			return err
		}
		if digest != info.SHA256 {
			return fmt.Errorf("backup artifact SHA-256 does not match its manifest")
		}
	}
	actualExpandedSize, err := backupArchiveExpandedSize(backupFullPath)
	if err != nil {
		return err
	}
	if info.ExpandedSize != 0 && info.ExpandedSize != actualExpandedSize {
		return fmt.Errorf("backup artifact expanded size does not match its manifest")
	}
	info.ExpandedSize = actualExpandedSize

	fullVolumeName := fullBackupVolumeName(req)
	mounted, err := runDocker(ctx, "ps", "-q", "--filter", "volume="+fullVolumeName)
	if err != nil {
		return fmt.Errorf("inspect volume consumers before restore: %w", err)
	}
	if strings.TrimSpace(mounted) != "" {
		return fmt.Errorf("volume %s is mounted by a running container; stop its services before restore", fullVolumeName)
	}
	var ownershipJournal *volumeOwnershipJournal
	if ownershipRoot != "" {
		ownershipJournal, err = readVolumeOwnershipJournal(volumeOwnershipJournalPath(ownershipRoot, fullVolumeName))
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("inspect managed volume ownership evidence before restore: %w", err)
		}
		if os.IsNotExist(err) {
			ownershipJournal = nil
		}
	}
	createdTarget := false
	if _, err := runDocker(ctx, "volume", "inspect", fullVolumeName); err != nil {
		if req.ExternalVolume {
			return fmt.Errorf("external volume %s does not exist", fullVolumeName)
		}
		if ownershipJournal != nil {
			return fmt.Errorf("ownership-managed volume %s is missing; restore requires an explicitly recreated volume with matching durable ownership evidence", fullVolumeName)
		}
		createdTarget = true
	} else if ownershipJournal != nil && ownershipJournal.Phase != "preexisting" {
		inspect, inspectErr := inspectVolumeOwnership(ctx, fullVolumeName)
		if inspectErr != nil || inspect.Driver != "local" || inspect.OperationID != ownershipJournal.OperationID {
			return fmt.Errorf("ownership-managed volume %s does not match its durable creation identity", fullVolumeName)
		}
	}
	targetSize := int64(0)
	if !createdTarget {
		targetSize, err = dockerVolumeAllocatedSize(ctx, fullVolumeName)
		if err != nil {
			return err
		}
	}
	if req.MinimumFreeBytes > 0 {
		backupRequired, dockerRequired, err := restoreCapacityBytes(info.ExpandedSize, targetSize)
		if err != nil {
			return err
		}
		dockerRoot := req.DockerDataRoot
		if strings.TrimSpace(dockerRoot) == "" {
			dockerRoot = backupPath
		}
		for _, check := range []struct {
			path, label string
			required    int64
		}{{backupPath, "backup", backupRequired}, {dockerRoot, "Docker", dockerRequired}} {
			available, err := availableDiskBytes(check.path)
			if err != nil {
				return fmt.Errorf("inspect %s filesystem capacity for restore: %w", check.label, err)
			}
			if available < req.MinimumFreeBytes || check.required > available-req.MinimumFreeBytes {
				return fmt.Errorf("insufficient %s filesystem capacity for safe restore rollback", check.label)
			}
		}
	}

	identity, err := newBackupArtifactID()
	if err != nil {
		return fmt.Errorf("create restore transaction identity: %w", err)
	}
	stageVolume := "tako-restore-stage-" + strings.TrimPrefix(identity, "artifact-")
	guardContainer := "tako-restore-guard-" + strings.TrimPrefix(identity, "artifact-")
	rollbackFile := ".restore-rollback-" + strings.TrimPrefix(identity, "artifact-") + ".tar.gz"
	rollbackPath := filepath.Join(backupPath, rollbackFile)
	journalPath := filepath.Join(backupPath, ".restore-transaction-"+strings.TrimPrefix(identity, "artifact-")+".json")
	targetIdentity := ""
	if !createdTarget {
		targetIdentity, err = inspectDockerVolumeIdentity(ctx, fullVolumeName)
		if err != nil {
			return err
		}
	}
	targetOwnershipOperation := ""
	if ownershipJournal != nil && ownershipJournal.Phase != "preexisting" {
		targetOwnershipOperation = ownershipJournal.OperationID
	}
	journal := backupRestoreJournal{
		SchemaVersion: 2, Phase: "preparing", TargetVolume: fullVolumeName, TargetIdentity: targetIdentity,
		TargetOwnershipOperation: targetOwnershipOperation, GuardContainer: guardContainer, StageVolume: stageVolume,
		BackupFile: backupFile, RollbackFile: rollbackFile, CreatedTarget: createdTarget,
	}
	if createdTarget {
		journal.TargetCreationOperation = strings.TrimPrefix(identity, "artifact-")
	}
	if err := writeBackupRestoreJournal(journalPath, journal); err != nil {
		return err
	}
	defer func() {
		cleanupErr := cleanupVerificationDockerResource("container", guardContainer)
		if err := cleanupVerificationDockerResource("volume", stageVolume); err != nil && cleanupErr == nil {
			cleanupErr = err
		}
		if journal.Phase == "preparing" && createdTarget {
			if err := removePreparingCreatedRestoreTarget(journal); err != nil && cleanupErr == nil {
				cleanupErr = err
			}
		}
		if journal.Phase != "mutating" && cleanupErr == nil {
			if err := os.Remove(rollbackPath); err != nil && !os.IsNotExist(err) {
				cleanupErr = err
			}
			if cleanupErr == nil {
				if err := os.Remove(journalPath); err != nil && !os.IsNotExist(err) {
					cleanupErr = err
				} else if err := syncDirectory(backupPath); err != nil {
					cleanupErr = err
				}
			}
		}
		if cleanupErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("clean restore transaction resources: %w", cleanupErr))
		}
	}()
	if createdTarget {
		if output, err := runDocker(ctx,
			"volume", "create",
			"--label", "tako.project="+req.Project,
			"--label", "tako.environment="+req.Environment,
			"--label", "tako.runtime=takod",
			"--label", "tako.restore-target-operation="+journal.TargetCreationOperation,
			fullVolumeName,
		); err != nil {
			return fmt.Errorf("create restore target volume: %s: %w", strings.TrimSpace(output), err)
		}
		if err := verifyRestoreTargetCreation(ctx, journal); err != nil {
			return err
		}
		journal.TargetIdentity, err = inspectDockerVolumeIdentity(ctx, fullVolumeName)
		if err != nil {
			return err
		}
		if err := writeBackupRestoreJournal(journalPath, journal); err != nil {
			return err
		}
	}
	if err := pinRestoreTarget(ctx, journal); err != nil {
		return err
	}
	if _, err := runDocker(ctx, "volume", "create", "--label", "tako.managed=true", "--label", "tako.role=backup-restore-stage", stageVolume); err != nil {
		return fmt.Errorf("create restore staging volume: %w", err)
	}
	if _, err := runDocker(
		ctx,
		"run", "--rm",
		"-v", stageVolume+":/target",
		"-v", backupPath+":/backup:ro",
		backupImage,
		"sh", "-c", restoreVolumeScript(backupFile),
	); err != nil {
		return fmt.Errorf("failed to validate backup in staging volume: %w", err)
	}

	if !createdTarget {
		if _, err := runDocker(ctx,
			"run", "--rm", "-v", fullVolumeName+":/source:ro", "-v", backupPath+":/backup",
			backupImage, "tar", "-czf", "/backup/"+rollbackFile, "-C", "/source", ".",
		); err != nil {
			return fmt.Errorf("create pre-restore rollback archive: %w", err)
		}
		if err := os.Chmod(rollbackPath, 0600); err != nil {
			return fmt.Errorf("secure pre-restore rollback archive: %w", err)
		}
		if err := syncFile(rollbackPath); err != nil {
			return fmt.Errorf("sync pre-restore rollback archive: %w", err)
		}
		if err := syncDirectory(backupPath); err != nil {
			return fmt.Errorf("sync pre-restore rollback directory: %w", err)
		}
	}
	if mounted, err := runDocker(ctx, "ps", "-q", "--filter", "volume="+fullVolumeName); err != nil {
		return fmt.Errorf("recheck volume consumers before restore: %w", err)
	} else if strings.TrimSpace(mounted) != "" {
		return fmt.Errorf("volume %s became mounted during restore preparation", fullVolumeName)
	}
	candidateJournal := journal
	candidateJournal.Phase = "mutating"
	if err := writeBackupRestoreJournal(journalPath, candidateJournal); err != nil {
		return err
	}
	journal = candidateJournal
	if _, err := runDocker(
		ctx,
		"run", "--rm",
		"-v", fullVolumeName+":/target",
		"-v", backupPath+":/backup:ro",
		backupImage,
		"sh", "-c", restoreVolumeScript(backupFile),
	); err != nil {
		if createdTarget {
			if removeErr := removeCreatedRestoreTarget(journal); removeErr != nil {
				return errors.Join(fmt.Errorf("failed to restore backup: %w", err), fmt.Errorf("failed to remove partial target volume: %w", removeErr))
			}
			candidateJournal = journal
			candidateJournal.Phase = "rolled-back"
			if journalErr := writeBackupRestoreJournal(journalPath, candidateJournal); journalErr != nil {
				return errors.Join(fmt.Errorf("failed to restore backup: %w", err), journalErr)
			}
			journal = candidateJournal
			return fmt.Errorf("failed to restore backup: %w", err)
		}
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if _, rollbackErr := runDocker(
			rollbackCtx, "run", "--rm", "-v", fullVolumeName+":/target", "-v", backupPath+":/backup:ro",
			backupImage, "sh", "-c", restoreVolumeScript(rollbackFile),
		); rollbackErr != nil {
			return errors.Join(fmt.Errorf("failed to restore backup: %w", err), fmt.Errorf("failed to roll back original volume contents: %w", rollbackErr))
		}
		candidateJournal = journal
		candidateJournal.Phase = "rolled-back"
		if journalErr := writeBackupRestoreJournal(journalPath, candidateJournal); journalErr != nil {
			return errors.Join(fmt.Errorf("failed to restore backup; original volume contents were restored: %w", err), journalErr)
		}
		journal = candidateJournal
		return fmt.Errorf("failed to restore backup; original volume contents were restored: %w", err)
	}
	candidateJournal = journal
	candidateJournal.Phase = "committed"
	if err := writeBackupRestoreJournal(journalPath, candidateJournal); err != nil {
		return err
	}
	journal = candidateJournal
	return nil
}

func removeCreatedRestoreTarget(journal backupRestoreJournal) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := cleanupVerificationDockerResource("container", journal.GuardContainer); err != nil {
		return fmt.Errorf("remove restore target guard: %w", err)
	}
	if err := verifyRestoreTargetIdentity(ctx, journal); err != nil {
		return err
	}
	return fmt.Errorf("restore-created target %s was preserved for explicit recovery because automatic named-volume deletion is disabled", journal.TargetVolume)
}

func removePreparingCreatedRestoreTarget(journal backupRestoreJournal) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := cleanupVerificationDockerResource("container", journal.GuardContainer); err != nil {
		return fmt.Errorf("remove restore target guard: %w", err)
	}
	if journal.TargetIdentity != "" {
		if err := verifyRestoreTargetIdentity(ctx, journal); err != nil {
			return err
		}
	} else {
		exists, err := restoreTargetCreationExists(ctx, journal)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
	}
	return fmt.Errorf("restore-created target %s was preserved for explicit recovery because automatic named-volume deletion is disabled", journal.TargetVolume)
}

func CleanupBackupRestoreResources(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	markers, err := filepath.Glob(filepath.Join(backupRootDir, "*", "*", ".restore-transaction-*.json"))
	if err != nil {
		return fmt.Errorf("list stale backup restore markers: %w", err)
	}
	if len(markers) == 0 {
		return nil
	}
	for _, marker := range markers {
		releaseLock, err := acquireBackupDirectoryLock(filepath.Dir(marker))
		if err != nil {
			return fmt.Errorf("lock stale backup restore state: %w", err)
		}
		data, readErr := os.ReadFile(marker)
		var journal backupRestoreJournal
		if readErr != nil || json.Unmarshal(data, &journal) != nil || validateBackupRestoreJournal(journal) != nil {
			releaseLock()
			return fmt.Errorf("invalid stale backup restore marker %s", marker)
		}
		if journal.SchemaVersion == 1 && (journal.Phase == "mutating" || (journal.Phase == "preparing" && journal.CreatedTarget)) {
			releaseLock()
			return fmt.Errorf("legacy restore transaction for %s lacks identity evidence and requires manual recovery", journal.TargetVolume)
		}
		if journal.CreatedTarget && (journal.Phase == "preparing" || journal.Phase == "mutating") {
			releaseLock()
			return fmt.Errorf("restore-created target %s is preserved and requires explicit recovery; automatic named-volume deletion is disabled", journal.TargetVolume)
		}
		if journal.Phase == "preparing" && journal.CreatedTarget {
			removeCreatedTarget := true
			if journal.SchemaVersion == 2 {
				if err := cleanupVerificationDockerResource("container", journal.GuardContainer); err != nil {
					releaseLock()
					return fmt.Errorf("remove restore target guard during recovery: %w", err)
				}
				if journal.TargetIdentity == "" {
					exists, err := restoreTargetCreationExists(ctx, journal)
					if err != nil {
						releaseLock()
						return err
					}
					if !exists {
						removeCreatedTarget = false
					}
				} else if err := verifyRestoreTargetIdentity(ctx, journal); err != nil {
					releaseLock()
					return err
				}
			}
			if removeCreatedTarget {
				if err := cleanupVerificationDockerResource("volume", journal.TargetVolume); err != nil {
					releaseLock()
					return fmt.Errorf("remove uncommitted restore target during recovery: %w", err)
				}
			}
		}
		if journal.Phase == "mutating" {
			mounted, err := runDocker(ctx, "ps", "-q", "--filter", "volume="+journal.TargetVolume)
			if err != nil || strings.TrimSpace(mounted) != "" {
				releaseLock()
				return fmt.Errorf("restore transaction for %s requires recovery while its target is unmounted", journal.TargetVolume)
			}
			if journal.CreatedTarget {
				if journal.SchemaVersion == 2 {
					if err := cleanupVerificationDockerResource("container", journal.GuardContainer); err != nil {
						releaseLock()
						return fmt.Errorf("remove restore target guard during recovery: %w", err)
					}
					if err := verifyRestoreTargetIdentity(ctx, journal); err != nil {
						releaseLock()
						return err
					}
				}
				if err := cleanupVerificationDockerResource("volume", journal.TargetVolume); err != nil {
					releaseLock()
					return fmt.Errorf("remove partial restore target during recovery: %w", err)
				}
			} else {
				if journal.SchemaVersion == 2 {
					if err := pinRestoreTarget(ctx, journal); err != nil {
						releaseLock()
						return fmt.Errorf("pin interrupted restore target during recovery: %w", err)
					}
				}
				rollbackPath := filepath.Join(filepath.Dir(marker), journal.RollbackFile)
				if _, err := os.Stat(rollbackPath); err != nil {
					releaseLock()
					return fmt.Errorf("restore rollback artifact is unavailable: %w", err)
				}
				recoveryCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
				_, restoreErr := runDocker(recoveryCtx,
					"run", "--rm", "-v", journal.TargetVolume+":/target", "-v", filepath.Dir(marker)+":/backup:ro",
					backupImage, "sh", "-c", restoreVolumeScript(journal.RollbackFile),
				)
				cancel()
				if restoreErr != nil {
					releaseLock()
					return fmt.Errorf("recover interrupted volume restore: %w", restoreErr)
				}
			}
		}
		if journal.SchemaVersion == 2 {
			if err := cleanupVerificationDockerResource("container", journal.GuardContainer); err != nil {
				releaseLock()
				return fmt.Errorf("clean stale restore target guard: %w", err)
			}
		}
		if err := cleanupVerificationDockerResource("volume", journal.StageVolume); err != nil {
			releaseLock()
			return fmt.Errorf("clean stale backup restore staging volume: %w", err)
		}
		if journal.RollbackFile != "" {
			if err := os.Remove(filepath.Join(filepath.Dir(marker), journal.RollbackFile)); err != nil && !os.IsNotExist(err) {
				releaseLock()
				return fmt.Errorf("remove stale restore rollback artifact: %w", err)
			}
		}
		if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
			releaseLock()
			return fmt.Errorf("remove stale backup restore marker: %w", err)
		}
		if err := syncDirectory(filepath.Dir(marker)); err != nil {
			releaseLock()
			return fmt.Errorf("sync recovered backup restore state: %w", err)
		}
		releaseLock()
	}
	return nil
}

func RecoverPendingVolumeRestore(ctx context.Context, marker, action string) (RestoreRecoveryResult, error) {
	result := RestoreRecoveryResult{Action: strings.TrimSpace(action)}
	marker, err := validateOfflineRestoreMarkerPath(marker)
	if err != nil {
		return result, err
	}
	result.Journal = marker
	releaseLock, err := acquireBackupDirectoryLock(filepath.Dir(marker))
	if err != nil {
		return result, err
	}
	defer releaseLock()
	data, err := os.ReadFile(marker)
	if err != nil {
		return result, fmt.Errorf("read restore transaction journal: %w", err)
	}
	var journal backupRestoreJournal
	if err := json.Unmarshal(data, &journal); err != nil || validateBackupRestoreJournal(journal) != nil {
		return result, fmt.Errorf("restore transaction journal is invalid")
	}
	result.Phase = journal.Phase
	result.TargetVolume = journal.TargetVolume
	result.CreatedTarget = journal.CreatedTarget
	if journal.SchemaVersion != 2 {
		return result, fmt.Errorf("legacy restore journal lacks identity evidence; use the documented manual recovery procedure")
	}
	existingAction := map[string]string{"committed": "accept", "rolled-back": "rollback", "abandoned": "abandon"}[journal.Phase]
	resumeCleanup := result.Action != "inspect" && existingAction != ""
	if resumeCleanup && existingAction != result.Action {
		return result, fmt.Errorf("restore recovery decision is already %s and cannot be changed to %s", existingAction, result.Action)
	}
	if !resumeCleanup {
		if journal.TargetIdentity == "" {
			if err := verifyRestoreTargetCreation(ctx, journal); err != nil {
				return result, err
			}
		} else if err := verifyRestoreTargetIdentity(ctx, journal); err != nil {
			return result, err
		}
	}
	if result.Action == "inspect" {
		return result, nil
	}
	terminalPhase := ""
	switch result.Action {
	case "accept":
		terminalPhase = "committed"
	case "rollback":
		terminalPhase = "rolled-back"
	case "abandon":
		terminalPhase = "abandoned"
	default:
		return result, fmt.Errorf("recovery action must be inspect, accept, rollback, or abandon")
	}
	if !resumeCleanup {
		mounted, err := runDocker(ctx, "ps", "-q", "--filter", "volume="+journal.TargetVolume)
		if err != nil {
			return result, fmt.Errorf("inspect restore target consumers: %w", err)
		}
		if strings.TrimSpace(mounted) != "" {
			return result, fmt.Errorf("restore target %s is mounted by a running container", journal.TargetVolume)
		}
		switch result.Action {
		case "accept":
			// The operator explicitly accepts current target contents.
		case "abandon":
			if !journal.CreatedTarget {
				return result, fmt.Errorf("abandon is only valid for a transaction-created target")
			}
		case "rollback":
			if journal.CreatedTarget {
				return result, fmt.Errorf("rollback is unavailable for a transaction-created target; use accept or abandon")
			}
			rollbackPath := filepath.Join(filepath.Dir(marker), journal.RollbackFile)
			if _, err := os.Stat(rollbackPath); err != nil {
				return result, fmt.Errorf("restore rollback artifact is unavailable: %w", err)
			}
			if err := pinRestoreTarget(ctx, journal); err != nil {
				return result, err
			}
			if _, err := runDocker(ctx,
				"run", "--rm", "-v", journal.TargetVolume+":/target", "-v", filepath.Dir(marker)+":/backup:ro",
				backupImage, "sh", "-c", restoreVolumeScript(journal.RollbackFile),
			); err != nil {
				return result, fmt.Errorf("roll back interrupted volume restore: %w", err)
			}
		}
		journal.Phase = terminalPhase
		if err := writeBackupRestoreJournal(marker, journal); err != nil {
			return result, fmt.Errorf("persist restore recovery decision: %w", err)
		}
		result.Phase = terminalPhase
	}
	if err := cleanupVerificationDockerResource("container", journal.GuardContainer); err != nil {
		return result, fmt.Errorf("remove restore target guard: %w", err)
	}
	if err := cleanupVerificationDockerResource("volume", journal.StageVolume); err != nil {
		return result, fmt.Errorf("remove restore staging volume: %w", err)
	}
	if journal.RollbackFile != "" {
		if err := os.Remove(filepath.Join(filepath.Dir(marker), journal.RollbackFile)); err != nil && !os.IsNotExist(err) {
			return result, fmt.Errorf("remove restore rollback artifact: %w", err)
		}
	}
	if err := os.Remove(marker); err != nil {
		return result, fmt.Errorf("remove restore transaction journal: %w", err)
	}
	if err := syncDirectory(filepath.Dir(marker)); err != nil {
		return result, fmt.Errorf("sync recovered restore transaction: %w", err)
	}
	result.Recovered = true
	return result, nil
}

func validateOfflineRestoreMarkerPath(marker string) (string, error) {
	marker = filepath.Clean(strings.TrimSpace(marker))
	if marker == "." || !strings.HasPrefix(filepath.Base(marker), ".restore-transaction-") || !strings.HasSuffix(marker, ".json") {
		return "", fmt.Errorf("invalid restore transaction journal path")
	}
	absMarker, err := filepath.Abs(marker)
	if err != nil {
		return "", err
	}
	absRoot, err := filepath.Abs(backupRootDir)
	if err != nil {
		return "", err
	}
	resolvedRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", fmt.Errorf("resolve backup root: %w", err)
	}
	resolvedMarker, err := filepath.EvalSymlinks(absMarker)
	if err != nil {
		return "", fmt.Errorf("resolve restore transaction journal: %w", err)
	}
	relative, err := filepath.Rel(resolvedRoot, resolvedMarker)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("restore transaction journal must be under %s", resolvedRoot)
	}
	return resolvedMarker, nil
}

func writeBackupRestoreJournal(path string, journal backupRestoreJournal) error {
	if err := validateBackupRestoreJournal(journal); err != nil {
		return err
	}
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return fmt.Errorf("encode restore transaction journal: %w", err)
	}
	data = append(data, '\n')
	if err := writeFileAtomic(path, data, 0600); err != nil {
		return fmt.Errorf("write restore transaction journal: %w", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync restore transaction journal: %w", err)
	}
	return nil
}

func validateBackupRestoreJournal(journal backupRestoreJournal) error {
	validPhase := journal.Phase == "preparing" || journal.Phase == "mutating" || journal.Phase == "rolled-back" || journal.Phase == "committed" || journal.Phase == "abandoned"
	baseValid := (journal.SchemaVersion == 1 || journal.SchemaVersion == 2) && validPhase && isSafeBackupVolume(journal.TargetVolume) &&
		strings.HasPrefix(journal.StageVolume, "tako-restore-stage-") && isSafeBackupVolume(journal.StageVolume) &&
		filepath.Base(journal.BackupFile) == journal.BackupFile && strings.HasSuffix(journal.BackupFile, ".tar.gz") &&
		filepath.Base(journal.RollbackFile) == journal.RollbackFile && strings.HasPrefix(journal.RollbackFile, ".restore-rollback-") && strings.HasSuffix(journal.RollbackFile, ".tar.gz")
	if !baseValid {
		return fmt.Errorf("restore transaction journal is invalid")
	}
	if journal.SchemaVersion == 1 {
		return nil
	}
	validIdentity := len(journal.TargetIdentity) == sha256.Size*2
	if journal.CreatedTarget && journal.Phase == "preparing" && journal.TargetIdentity == "" {
		validIdentity = true
	}
	if !validIdentity || (journal.TargetIdentity != "" && !isLowerHex(journal.TargetIdentity)) ||
		(journal.TargetOwnershipOperation != "" && !isSafeVolumeOwnershipOperationID(journal.TargetOwnershipOperation)) ||
		(journal.CreatedTarget != (journal.TargetCreationOperation != "")) ||
		(journal.TargetCreationOperation != "" && !isSafeVolumeOwnershipOperationID(journal.TargetCreationOperation)) ||
		!strings.HasPrefix(journal.GuardContainer, "tako-restore-guard-") || !isSafeContainerName(journal.GuardContainer) {
		return fmt.Errorf("restore transaction journal is invalid")
	}
	return nil
}

func isLowerHex(value string) bool {
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}

func inspectDockerVolumeIdentity(ctx context.Context, volume string) (string, error) {
	output, err := runDocker(ctx, "volume", "inspect", "--format", `{{.Driver}}|{{.CreatedAt}}|{{json .Labels}}|{{json .Options}}|{{.Scope}}`, volume)
	if err != nil {
		return "", fmt.Errorf("inspect Docker volume %s identity: %w", volume, err)
	}
	digest := sha256.Sum256([]byte(strings.TrimSpace(output)))
	return hex.EncodeToString(digest[:]), nil
}

func pinRestoreTarget(ctx context.Context, journal backupRestoreJournal) error {
	if err := cleanupVerificationDockerResource("container", journal.GuardContainer); err != nil {
		return fmt.Errorf("clean stale restore target guard: %w", err)
	}
	if output, err := runDocker(ctx,
		"create", "--pull", "missing", "--name", journal.GuardContainer, "--network", "none",
		"--label", "tako.restore-target-identity="+journal.TargetIdentity,
		"-v", journal.TargetVolume+":/target", backupImage, "true",
	); err != nil {
		return fmt.Errorf("pin restore target volume: %s: %w", strings.TrimSpace(output), err)
	}
	identity, err := inspectDockerVolumeIdentity(ctx, journal.TargetVolume)
	if err != nil || identity != journal.TargetIdentity {
		return fmt.Errorf("restore target volume %s changed before it was pinned", journal.TargetVolume)
	}
	if journal.TargetOwnershipOperation != "" {
		inspect, inspectErr := inspectVolumeOwnership(ctx, journal.TargetVolume)
		if inspectErr != nil || inspect.Driver != "local" || inspect.OperationID != journal.TargetOwnershipOperation {
			return fmt.Errorf("ownership-managed restore target %s changed before it was pinned", journal.TargetVolume)
		}
	}
	if journal.TargetCreationOperation != "" {
		if err := verifyRestoreTargetCreation(ctx, journal); err != nil {
			return err
		}
	}
	mount, err := runDocker(ctx, "inspect", journal.GuardContainer, "--format", `{{range .Mounts}}{{if eq .Destination "/target"}}{{.Type}}|{{.Name}}{{end}}{{end}}`)
	if err != nil || strings.TrimSpace(mount) != "volume|"+journal.TargetVolume {
		return fmt.Errorf("restore guard did not pin expected target volume %s", journal.TargetVolume)
	}
	return nil
}

func restoreTargetCreationExists(ctx context.Context, journal backupRestoreJournal) (bool, error) {
	output, err := runDocker(ctx, "volume", "inspect", "--format", `{{.Driver}}|{{index .Labels "tako.restore-target-operation"}}`, journal.TargetVolume)
	if err != nil {
		if isMissingDockerObject(output) {
			return false, nil
		}
		return false, fmt.Errorf("inspect restore target creation evidence: %w", err)
	}
	if strings.TrimSpace(output) != "local|"+journal.TargetCreationOperation {
		return false, fmt.Errorf("restore target volume %s does not match its token-bound creation intent", journal.TargetVolume)
	}
	return true, nil
}

func verifyRestoreTargetCreation(ctx context.Context, journal backupRestoreJournal) error {
	exists, err := restoreTargetCreationExists(ctx, journal)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("restore target volume %s is missing after token-bound creation", journal.TargetVolume)
	}
	return nil
}

func verifyRestoreTargetIdentity(ctx context.Context, journal backupRestoreJournal) error {
	identity, err := inspectDockerVolumeIdentity(ctx, journal.TargetVolume)
	if err != nil || identity != journal.TargetIdentity {
		return fmt.Errorf("restore target volume %s no longer matches its durable transaction identity", journal.TargetVolume)
	}
	if journal.TargetOwnershipOperation != "" {
		inspect, inspectErr := inspectVolumeOwnership(ctx, journal.TargetVolume)
		if inspectErr != nil || inspect.Driver != "local" || inspect.OperationID != journal.TargetOwnershipOperation {
			return fmt.Errorf("ownership-managed restore target %s no longer matches its durable creation identity", journal.TargetVolume)
		}
	}
	if journal.TargetCreationOperation != "" {
		if err := verifyRestoreTargetCreation(ctx, journal); err != nil {
			return err
		}
	}
	return nil
}

func dockerVolumeAllocatedSize(ctx context.Context, volume string) (int64, error) {
	output, err := runDocker(ctx, "run", "--rm", "-v", volume+":/source:ro", backupImage, "du", "-sk", "/source")
	if err != nil {
		return 0, fmt.Errorf("measure restore target volume: %w", err)
	}
	fields := strings.Fields(output)
	if len(fields) == 0 {
		return 0, fmt.Errorf("measure restore target volume: empty size response")
	}
	kilobytes, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || kilobytes < 0 || kilobytes > math.MaxInt64/1024 {
		return 0, fmt.Errorf("measure restore target volume: invalid size response")
	}
	return kilobytes * 1024, nil
}

func restoreCapacityBytes(expandedSize, targetSize int64) (backupBytes, dockerBytes int64, err error) {
	const overhead = int64(64 << 20)
	withMargin := func(value int64) (int64, error) {
		if value < 0 || value > (math.MaxInt64-overhead)/6*5 {
			return 0, fmt.Errorf("restore capacity evidence is invalid")
		}
		return value + value/5 + overhead, nil
	}
	backupBytes, err = withMargin(targetSize)
	if err != nil {
		return 0, 0, err
	}
	peakAdditional := expandedSize
	if expandedSize > targetSize {
		if expandedSize-targetSize > math.MaxInt64-peakAdditional {
			return 0, 0, fmt.Errorf("restore capacity evidence is invalid")
		}
		peakAdditional += expandedSize - targetSize
	}
	dockerBytes, err = withMargin(peakAdditional)
	return backupBytes, dockerBytes, err
}

func EstimateVolumeRestoreCapacity(ctx context.Context, req BackupRequest) (backupBytes, dockerBytes int64, err error) {
	if err := validateBackupRequest(req, true, true); err != nil {
		return 0, 0, err
	}
	backupPath := backupDirectory(req)
	archivePath := filepath.Join(backupPath, backupFileName(req.Volume, req.BackupID))
	info, err := backupInfoFromPath(backupPath, archivePath)
	if err != nil {
		return 0, 0, err
	}
	expandedSize, err := backupArchiveExpandedSize(archivePath)
	if err != nil {
		return 0, 0, err
	}
	if info.ExpandedSize != 0 && info.ExpandedSize != expandedSize {
		return 0, 0, fmt.Errorf("backup artifact expanded size does not match its manifest")
	}
	targetSize := int64(0)
	volume := fullBackupVolumeName(req)
	if _, err := runDocker(ctx, "volume", "inspect", volume); err == nil {
		targetSize, err = dockerVolumeAllocatedSize(ctx, volume)
		if err != nil {
			return 0, 0, err
		}
	} else if req.ExternalVolume {
		return 0, 0, fmt.Errorf("external volume %s does not exist", volume)
	}
	return restoreCapacityBytes(expandedSize, targetSize)
}

func ListVolumeBackups(ctx context.Context, req BackupRequest) (*BackupListResponse, error) {
	if err := validateBackupRequest(req, false, false); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	backupPath := backupDirectory(req)
	entries, err := os.ReadDir(backupPath)
	if os.IsNotExist(err) {
		return &BackupListResponse{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to list backups: %w", err)
	}

	var backups []BackupInfo
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".restore-") || !strings.HasSuffix(entry.Name(), ".tar.gz") {
			continue
		}
		path := filepath.Join(backupPath, entry.Name())
		info, err := backupInfoFromPath(backupPath, path)
		if err != nil {
			return nil, fmt.Errorf("invalid backup artifact %s: %w", entry.Name(), err)
		}
		if req.Volume != "" && info.Volume != req.Volume {
			continue
		}
		backups = append(backups, info)
	}
	sort.Slice(backups, func(i, j int) bool {
		if backups[i].Volume == backups[j].Volume {
			return backups[i].CreatedAt.After(backups[j].CreatedAt)
		}
		return backups[i].Volume < backups[j].Volume
	})
	return &BackupListResponse{Backups: backups}, nil
}

func DeleteVolumeBackup(ctx context.Context, req BackupRequest) error {
	ctx, releaseBarrier := acquireBackupRestoreMutationBarrier(ctx)
	defer releaseBarrier()
	if err := ensureNoPendingRestoreTransactionsOnNode(); err != nil {
		return err
	}
	if err := validateBackupRequest(req, true, true); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	backupPath := backupDirectory(req)
	releaseLock, err := acquireBackupDirectoryLock(backupPath)
	if err != nil {
		if os.IsNotExist(errors.Unwrap(err)) {
			return nil
		}
		return err
	}
	defer releaseLock()
	if err := ensureNoPendingRestoreTransactions(backupPath); err != nil {
		return err
	}
	path := filepath.Join(backupPath, backupFileName(req.Volume, req.BackupID))
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete backup: %w", err)
	}
	if err := os.Remove(backupManifestPath(path)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete backup manifest: %w", err)
	}
	return nil
}

func CleanupOldBackups(ctx context.Context, req BackupRequest) (*BackupCleanupResponse, error) {
	ctx, releaseBarrier := acquireBackupRestoreMutationBarrier(ctx)
	defer releaseBarrier()
	if err := ensureNoPendingRestoreTransactionsOnNode(); err != nil {
		return nil, err
	}
	if err := validateBackupRequest(req, false, false); err != nil {
		return nil, err
	}
	if req.RetentionDays <= 0 {
		req.RetentionDays = DefaultRetention
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	backupPath := backupDirectory(req)
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		return &BackupCleanupResponse{}, nil
	} else if err != nil {
		return nil, fmt.Errorf("inspect backup directory: %w", err)
	}
	releaseLock, err := acquireBackupDirectoryLock(backupPath)
	if err != nil {
		return nil, err
	}
	defer releaseLock()
	if err := ensureNoPendingRestoreTransactions(backupPath); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(backupPath)
	if os.IsNotExist(err) {
		return &BackupCleanupResponse{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to list backups: %w", err)
	}

	cutoff := time.Now().UTC().AddDate(0, 0, -req.RetentionDays)
	deleted := 0
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".restore-") || !strings.HasSuffix(entry.Name(), ".tar.gz") {
			continue
		}
		info, err := backupInfoFromPath(backupPath, filepath.Join(backupPath, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("invalid backup artifact %s: %w", entry.Name(), err)
		}
		if info.CreatedAt.IsZero() || info.CreatedAt.After(cutoff) {
			continue
		}
		if req.Volume != "" && info.Volume != req.Volume {
			continue
		}
		if err := os.Remove(info.Path); err != nil {
			return nil, fmt.Errorf("failed to delete old backup %s: %w", info.Path, err)
		}
		if err := os.Remove(backupManifestPath(info.Path)); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("failed to delete old backup manifest %s: %w", backupManifestPath(info.Path), err)
		}
		deleted++
	}
	return &BackupCleanupResponse{Deleted: deleted}, nil
}

func validateBackupRequest(req BackupRequest, requireVolume bool, requireBackupID bool) error {
	if !isSafeProjectName(req.Project) {
		return fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(req.Environment) {
		return fmt.Errorf("invalid environment name")
	}
	if requireVolume || req.Volume != "" {
		if !isSafeBackupVolume(req.Volume) {
			return fmt.Errorf("invalid volume name")
		}
	}
	if req.DockerVolume != "" && !isSafeDockerVolumeName(req.DockerVolume) {
		return fmt.Errorf("invalid docker volume name")
	}
	if requireBackupID || req.BackupID != "" {
		if !isSafeBackupID(req.BackupID) {
			return fmt.Errorf("invalid backup ID")
		}
	}
	if req.RetentionDays < 0 {
		return fmt.Errorf("retentionDays cannot be negative")
	}
	if req.Storage != nil {
		if err := ValidateBackupStorage(*req.Storage); err != nil {
			return err
		}
	}
	return nil
}

func backupIDForRequest(req BackupRequest, now time.Time) string {
	if req.BackupID != "" {
		return req.BackupID
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err == nil {
		return now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(random)
	}
	fallback := sha256.Sum256([]byte(fmt.Sprintf("%d", now.UnixNano())))
	return now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(fallback[:16])
}

func backupDirectory(req BackupRequest) string {
	return filepath.Join(backupRootDir, req.Project, req.Environment)
}

func fullBackupVolumeName(req BackupRequest) string {
	if req.DockerVolume != "" {
		return req.DockerVolume
	}
	return runtimeid.VolumeName(req.Project, req.Environment, req.Volume)
}

func backupFileName(volume string, backupID string) string {
	return volume + "_" + backupID + ".tar.gz"
}

func restoreVolumeScript(backupFile string) string {
	return restoreVolumeScriptForPaths(backupFile, "/backup", "/target")
}

func restoreVolumeScriptForPaths(backupFile, backupRoot, targetRoot string) string {
	quotedBackupPath := shellQuote(filepath.Join(backupRoot, backupFile))
	quotedTargetPath := shellQuote(targetRoot)
	return fmt.Sprintf(`set -eu
backupPath=%s
targetPath=%s
entryList=/tmp/tako-backup-entries
if [ ! -d "$targetPath" ] || [ -L "$targetPath" ]; then
  echo "invalid restore target" >&2
  exit 1
fi
if [ ! -f "$backupPath" ]; then
  echo "backup file not found" >&2
  exit 1
fi
tar -tzf "$backupPath" > "$entryList"
awk '
BEGIN { bad = 0 }
/^\/|(^|\/)\.\.(\/|$)/ {
  print "unsafe backup entry: " $0 > "/dev/stderr"
  bad = 1
}
END { exit bad }
' "$entryList"
find "$targetPath" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} \;
tar -xzf "$backupPath" -C "$targetPath"
`, quotedBackupPath, quotedTargetPath)
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func backupInfoFromPath(root string, path string) (BackupInfo, error) {
	filename := filepath.Base(path)
	if !strings.HasSuffix(filename, ".tar.gz") {
		return BackupInfo{}, fmt.Errorf("not a backup file")
	}
	base := strings.TrimSuffix(filename, ".tar.gz")
	separator := strings.LastIndex(base, "_")
	if separator <= 0 || separator == len(base)-1 {
		return BackupInfo{}, fmt.Errorf("invalid backup filename")
	}
	volume := base[:separator]
	backupID := base[separator+1:]
	if !isSafeBackupVolume(volume) || !isSafeBackupID(backupID) {
		return BackupInfo{}, fmt.Errorf("invalid backup filename")
	}
	info, err := os.Stat(path)
	if err != nil {
		return BackupInfo{}, fmt.Errorf("failed to stat backup: %w", err)
	}
	createdAt, _ := backupIDTimestamp(backupID)
	result := BackupInfo{
		ID:          backupID,
		ArtifactID:  backupID,
		Format:      backupArtifactFormatVolumeTarGzip,
		Volume:      volume,
		Size:        info.Size(),
		CreatedAt:   createdAt.UTC(),
		Path:        filepath.Join(root, filename),
		Compression: "gzip",
	}
	manifestData, manifestErr := os.ReadFile(backupManifestPath(path))
	if manifestErr == nil {
		var manifest backupArtifactManifest
		if err := json.Unmarshal(manifestData, &manifest); err != nil || manifest.SchemaVersion != 1 {
			return BackupInfo{}, fmt.Errorf("invalid backup artifact manifest")
		}
		if manifest.Backup.ID != backupID || manifest.Backup.Volume != volume || manifest.Backup.Size != info.Size() ||
			!isSafeBackupArtifactID(manifest.Backup.ArtifactID) || manifest.Backup.Format != backupArtifactFormatVolumeTarGzip ||
			manifest.Backup.Compression != "gzip" || manifest.Backup.ExpandedSize < 0 || manifest.Backup.Path != filename || !manifest.Backup.CreatedAt.Equal(createdAt.UTC()) ||
			len(manifest.Backup.SHA256) != sha256.Size*2 || manifest.Backup.SHA256 != strings.ToLower(manifest.Backup.SHA256) {
			return BackupInfo{}, fmt.Errorf("backup artifact manifest does not match archive")
		}
		if _, err := hex.DecodeString(manifest.Backup.SHA256); err != nil {
			return BackupInfo{}, fmt.Errorf("backup artifact manifest does not match archive")
		}
		if remote := manifest.Backup.Remote; remote != nil {
			expectedRemoteName := volume + "_" + backupID + "_" + manifest.Backup.ArtifactID + ".tar.gz"
			validProvider := remote.Provider == BackupStorageProviderS3 || remote.Provider == BackupStorageProviderR2 || remote.Provider == BackupStorageProviderS3Compatible
			if !validProvider || remote.Bucket == "" || hasControlChars(remote.Bucket) || strings.Contains(remote.Bucket, "/") ||
				validateBackupObjectKey(remote.Key) != nil || pathpkg.Base(remote.Key) != expectedRemoteName || remote.VerifiedAt.IsZero() || hasControlChars(remote.VersionID) {
				return BackupInfo{}, fmt.Errorf("backup artifact manifest has invalid remote evidence")
			}
			if remote.Endpoint != "" {
				parsed, err := url.Parse(remote.Endpoint)
				if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
					return BackupInfo{}, fmt.Errorf("backup artifact manifest has invalid remote evidence")
				}
			}
		}
		result = manifest.Backup
		result.Path = filepath.Join(root, filename)
	} else if !os.IsNotExist(manifestErr) {
		return BackupInfo{}, fmt.Errorf("read backup artifact manifest: %w", manifestErr)
	} else if len(backupID) > len("20060102-150405") {
		return BackupInfo{}, fmt.Errorf("backup artifact manifest is required for unique backup IDs")
	}
	return result, nil
}

func backupManifestPath(archivePath string) string {
	return archivePath + ".manifest.json"
}

func writeBackupArtifactManifest(archivePath string, info BackupInfo) error {
	manifestInfo := info
	manifestInfo.Path = filepath.Base(archivePath)
	manifestInfo.Warnings = nil
	data, err := json.MarshalIndent(backupArtifactManifest{SchemaVersion: 1, Backup: manifestInfo}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode backup artifact manifest: %w", err)
	}
	data = append(data, '\n')
	if err := writeFileAtomic(backupManifestPath(archivePath), data, 0600); err != nil {
		return fmt.Errorf("write backup artifact manifest: %w", err)
	}
	if err := syncDirectory(filepath.Dir(archivePath)); err != nil {
		return fmt.Errorf("sync backup artifact manifest directory: %w", err)
	}
	return nil
}

func hashFileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open backup artifact for hashing: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash backup artifact: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func backupArchiveExpandedSize(path string) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open backup artifact for size evidence: %w", err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return 0, fmt.Errorf("open backup artifact compression stream: %w", err)
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)
	var total int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("read backup artifact size evidence: %w", err)
		}
		if header.Size < 0 || header.Size > math.MaxInt64-4095 {
			return 0, fmt.Errorf("backup artifact expanded size is invalid")
		}
		allocated := (header.Size + 4095) / 4096 * 4096
		if allocated > math.MaxInt64-4096 || allocated+4096 > math.MaxInt64-total {
			return 0, fmt.Errorf("backup artifact expanded size is invalid")
		}
		total += allocated + 4096
	}
	return total, nil
}

func syncFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func newBackupArtifactID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return "artifact-" + hex.EncodeToString(random), nil
}

func isSafeBackupArtifactID(value string) bool {
	if len(value) != len("artifact-")+32 || !strings.HasPrefix(value, "artifact-") {
		return false
	}
	for _, char := range value[len("artifact-"):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func isSafeBackupVolume(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for i, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		if i > 0 && (r == '-' || r == '_' || r == '.') {
			continue
		}
		return false
	}
	return true
}

func isSafeBackupID(value string) bool {
	_, err := backupIDTimestamp(value)
	return err == nil
}

func backupIDTimestamp(value string) (time.Time, error) {
	const layout = "20060102-150405"
	if len(value) != len(layout) {
		const randomSuffixLength = 32
		if len(value) != len(layout)+1+randomSuffixLength || value[len(layout)] != '-' {
			return time.Time{}, fmt.Errorf("invalid backup ID")
		}
		for _, char := range value[len(layout)+1:] {
			if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
				return time.Time{}, fmt.Errorf("invalid backup ID")
			}
		}
	}
	return time.Parse(layout, value[:len(layout)])
}
