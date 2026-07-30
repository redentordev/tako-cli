package takod

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	BackupArtifactFormatPostgresCustom = "postgres-custom"
	backupVerificationDirName          = "backup-verifications"
	maxBackupArtifactSize              = int64(1_000_000_000_000)
)

var postgresIdentifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)
var backupVerificationMu sync.Mutex
var postgresVerificationSlot = make(chan struct{}, 1)
var persistPostgresVerificationWith = persistPostgresVerification
var syncPostgresVerificationDirectoryWith = syncDirectory
var uncommittedPostgresVerifications sync.Map

type BackupArtifactReference struct {
	ID     string `json:"id"`
	Format string `json:"format"`
	Key    string `json:"key"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type PostgresVerificationRequest struct {
	Project       string                  `json:"project"`
	Environment   string                  `json:"environment"`
	Service       string                  `json:"service"`
	Storage       BackupStorageConfig     `json:"storage"`
	Artifact      BackupArtifactReference `json:"artifact"`
	Image         string                  `json:"image"`
	Database      string                  `json:"database"`
	Username      string                  `json:"username"`
	Query         string                  `json:"query"`
	ExpectedValue string                  `json:"expectedValue"`
	ScratchBytes  int64                   `json:"scratchBytes"`
	MemoryBytes   int64                   `json:"-"`
}

type PostgresVerificationResult struct {
	SchemaVersion     int       `json:"schemaVersion"`
	Project           string    `json:"project"`
	Environment       string    `json:"environment"`
	Service           string    `json:"service"`
	ArtifactID        string    `json:"artifactId"`
	ArtifactFormat    string    `json:"artifactFormat"`
	ObjectKey         string    `json:"objectKey"`
	StorageProvider   string    `json:"storageProvider"`
	StorageBucket     string    `json:"storageBucket"`
	StorageEndpoint   string    `json:"storageEndpoint,omitempty"`
	Size              int64     `json:"size"`
	ScratchBytes      int64     `json:"scratchBytes"`
	MemoryBytes       int64     `json:"memoryBytes"`
	SHA256            string    `json:"sha256"`
	PostgresImage     string    `json:"postgresImage"`
	Database          string    `json:"database"`
	BootstrapUsername string    `json:"bootstrapUsername"`
	RestoreUsername   string    `json:"restoreUsername"`
	QuerySHA256       string    `json:"querySha256"`
	ExpectedSHA256    string    `json:"expectedSha256"`
	Status            string    `json:"status"`
	FailureStage      string    `json:"failureStage,omitempty"`
	CleanupComplete   bool      `json:"cleanupComplete"`
	VerifiedAt        time.Time `json:"verifiedAt"`
}

func VerifyPostgresBackup(ctx context.Context, dataDir string, req PostgresVerificationRequest) (result *PostgresVerificationResult, retErr error) {
	if err := ValidatePostgresVerificationRequest(req); err != nil {
		return nil, err
	}
	req.Storage = normalizeBackupStorage(req.Storage)
	if req.MemoryBytes <= 0 {
		req.MemoryBytes = 1 << 30
	}
	req.Artifact.ID = strings.TrimSpace(req.Artifact.ID)
	req.Artifact.Key = strings.TrimSpace(req.Artifact.Key)
	select {
	case postgresVerificationSlot <- struct{}{}:
		defer func() { <-postgresVerificationSlot }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	result = &PostgresVerificationResult{
		SchemaVersion: 1, Project: req.Project, Environment: req.Environment, Service: req.Service,
		ArtifactID: req.Artifact.ID, ArtifactFormat: req.Artifact.Format, ObjectKey: req.Artifact.Key,
		StorageProvider: req.Storage.Provider, StorageBucket: req.Storage.Bucket, StorageEndpoint: req.Storage.Endpoint,
		Size: req.Artifact.Size, ScratchBytes: req.ScratchBytes, MemoryBytes: req.MemoryBytes, SHA256: req.Artifact.SHA256,
		PostgresImage: req.Image, Database: req.Database, BootstrapUsername: req.Username, RestoreUsername: "isolated-database-owner",
		QuerySHA256: digestString(req.Query), ExpectedSHA256: digestString(req.ExpectedValue), Status: "failed",
	}
	if err := validateExistingPostgresVerificationIdentity(dataDir, result); err != nil {
		return result, err
	}
	failureStage := "prepare"
	verificationRoot := filepath.Join(dataDir, backupVerificationDirName, req.Project, req.Environment, req.Service)
	if err := os.MkdirAll(verificationRoot, 0700); err != nil {
		return nil, fmt.Errorf("create backup verification directory: %w", err)
	}
	tempDir, err := os.MkdirTemp(verificationRoot, ".verify-")
	if err != nil {
		return nil, fmt.Errorf("create backup verification workspace: %w", err)
	}
	if err := os.Chmod(tempDir, 0700); err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, fmt.Errorf("secure backup verification workspace: %w", err)
	}
	operationCtx, cancelOperation := context.WithCancel(ctx)

	suffix, err := randomVerificationSuffix()
	if err != nil {
		cancelOperation()
		_ = os.RemoveAll(tempDir)
		return nil, fmt.Errorf("create backup verification identity: %w", err)
	}
	containerName := "tako-pg-verify-" + suffix
	containerCreated := false
	defer func() {
		cancelOperation()
		var cleanupErr error
		if containerCreated {
			if err := cleanupVerificationDockerResource("container", containerName); err != nil {
				cleanupErr = fmt.Errorf("remove verification container: %w", err)
			}
		}
		if err := os.RemoveAll(tempDir); err != nil && cleanupErr == nil {
			cleanupErr = fmt.Errorf("remove verification workspace: %w", err)
		}
		result.CleanupComplete = cleanupErr == nil
		result.VerifiedAt = time.Now().UTC()
		if retErr == nil && cleanupErr == nil {
			result.Status = "verified"
		} else {
			result.FailureStage = failureStage
		}
		if persistErr := persistPostgresVerificationWith(dataDir, result); persistErr != nil {
			result.Status = "failed"
			result.FailureStage = "persistence"
			retErr = errors.Join(retErr, persistErr)
		}
		if cleanupErr != nil {
			retErr = errors.Join(retErr, cleanupErr)
		}
	}()

	dumpPath := filepath.Join(tempDir, "artifact.dump")
	failureStage = "download"
	if err := DownloadBackupObjectVerified(operationCtx, req.Storage, req.Artifact.Key, dumpPath, req.Artifact.Size, req.Artifact.SHA256); err != nil {
		return result, fmt.Errorf("verify remote backup artifact: %w", err)
	}

	failureStage = "start"
	labels := []string{
		"--label", "tako.managed=true",
		"--label", "tako.role=backup-verification",
		"--label", "tako.project=" + req.Project,
		"--label", "tako.environment=" + req.Environment,
		"--label", "tako.verification-service=" + req.Service,
		"--label", "tako.backup-verification=" + req.Artifact.ID,
	}
	password, err := randomVerificationPassword()
	if err != nil {
		return result, fmt.Errorf("create PostgreSQL verification credential: %w", err)
	}
	runArgs := []string{
		"run", "-d", "--name", containerName, "--network", "none",
		"--security-opt", "no-new-privileges", "--cap-drop", "ALL",
		"--cap-add", "CHOWN", "--cap-add", "DAC_OVERRIDE", "--cap-add", "FOWNER", "--cap-add", "SETGID", "--cap-add", "SETUID",
		"--pids-limit", "256", "--cpus", "2", "--memory", strconv.FormatInt(req.MemoryBytes, 10),
		"--tmpfs", "/tmp:rw,nosuid,nodev,noexec,size=256m",
		"--tmpfs", "/run/postgresql:rw,nosuid,nodev,noexec,size=16m",
		"--tmpfs", "/tako-postgres-data:rw,nosuid,nodev,noexec,size=" + strconv.FormatInt(req.ScratchBytes, 10),
	}
	runArgs = append(runArgs, labels...)
	runArgs = append(runArgs,
		"-e", "POSTGRES_USER="+req.Username,
		"-e", "POSTGRES_PASSWORD="+password,
		"-e", "POSTGRES_DB="+req.Database,
		"-e", "PGDATA=/tako-postgres-data",
		req.Image,
	)
	containerCreated = true
	if output, err := runDocker(operationCtx, runArgs...); err != nil {
		return result, fmt.Errorf("start quota-bounded isolated PostgreSQL verifier: %w: %s", err, strings.TrimSpace(output))
	}

	failureStage = "readiness"
	if err := waitForPostgresVerification(operationCtx, containerName, req.Username, req.Database, password); err != nil {
		return result, err
	}
	versionOutput, err := runDocker(operationCtx,
		"exec", "-e", "PGPASSWORD="+password, containerName,
		"psql", "--no-psqlrc", "--tuples-only", "--no-align", "--set=ON_ERROR_STOP=1", "--host", "127.0.0.1",
		"--username", req.Username, "--dbname", req.Database, "--command", "SHOW server_version_num",
	)
	if err != nil {
		return result, fmt.Errorf("inspect isolated PostgreSQL version: %w", err)
	}
	version, err := strconv.Atoi(strings.TrimSpace(versionOutput))
	if err != nil || version < 140000 {
		return result, fmt.Errorf("PostgreSQL verification image must provide PostgreSQL 14 or newer")
	}
	restorePassword, err := randomVerificationPassword()
	if err != nil {
		return result, fmt.Errorf("create PostgreSQL restore credential: %w", err)
	}
	restoreUsername := "tako_restore_" + suffix
	restoreRoleSQL := fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s'; ALTER DATABASE %s OWNER TO %s;", restoreUsername, restorePassword, req.Database, restoreUsername)
	if _, err := runDocker(operationCtx,
		"exec", "-e", "PGPASSWORD="+password, containerName,
		"psql", "--no-psqlrc", "--set=ON_ERROR_STOP=1", "--host", "127.0.0.1",
		"--username", req.Username, "--dbname", req.Database, "--command", restoreRoleSQL,
	); err != nil {
		return result, fmt.Errorf("create non-superuser PostgreSQL restore owner: %w", err)
	}
	failureStage = "restore"
	dumpFile, err := os.Open(dumpPath)
	if err != nil {
		return result, fmt.Errorf("open verified PostgreSQL artifact: %w", err)
	}
	output, restoreErr := runDockerWithStdin(operationCtx, dumpFile,
		"exec", "-i", "-e", "PGPASSWORD="+restorePassword, containerName,
		"pg_restore", "--exit-on-error", "--no-owner", "--no-acl", "--no-tablespaces",
		"--username", restoreUsername, "--dbname", req.Database,
	)
	closeErr := dumpFile.Close()
	if restoreErr != nil {
		logsCtx, cancelLogs := context.WithTimeout(context.Background(), 30*time.Second)
		logs, _ := runDocker(logsCtx, "logs", containerName)
		cancelLogs()
		return result, fmt.Errorf("restore PostgreSQL artifact in isolation: %w: %s %s", restoreErr, strings.TrimSpace(output), strings.TrimSpace(logs))
	}
	if closeErr != nil {
		return result, fmt.Errorf("close verified PostgreSQL artifact: %w", closeErr)
	}
	failureStage = "query"
	queryPassword, err := randomVerificationPassword()
	if err != nil {
		return result, fmt.Errorf("create PostgreSQL query credential: %w", err)
	}
	queryUsername := "tako_verify_" + suffix
	roleSQL := fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s'; GRANT pg_read_all_data TO %s; ALTER ROLE %s SET default_transaction_read_only = on;", queryUsername, queryPassword, queryUsername, queryUsername)
	if _, err := runDocker(operationCtx,
		"exec", "-e", "PGPASSWORD="+password, containerName,
		"psql", "--no-psqlrc", "--set=ON_ERROR_STOP=1", "--host", "127.0.0.1",
		"--username", req.Username, "--dbname", req.Database, "--command", roleSQL,
	); err != nil {
		return result, fmt.Errorf("create read-only PostgreSQL verification role: %w", err)
	}
	output, err = runDocker(operationCtx,
		"exec", "-e", "PGPASSWORD="+queryPassword, containerName,
		"psql", "--no-psqlrc", "--csv", "--tuples-only", "--set=ON_ERROR_STOP=1", "--host", "127.0.0.1",
		"--username", queryUsername, "--dbname", req.Database, "--command", req.Query,
	)
	if err != nil {
		return result, fmt.Errorf("run PostgreSQL verification query: %w", err)
	}
	records, err := csv.NewReader(strings.NewReader(output)).ReadAll()
	if err != nil && err != io.EOF {
		return result, fmt.Errorf("parse PostgreSQL verification query result: %w", err)
	}
	if len(records) != 1 || len(records[0]) != 1 || records[0][0] != req.ExpectedValue {
		return result, fmt.Errorf("PostgreSQL verification query result did not match expected value")
	}
	failureStage = "cleanup"
	return result, nil
}

// ValidatePostgresVerificationRequest validates artifact evidence and the
// isolated restore contract before object storage or Docker is touched.
func ValidatePostgresVerificationRequest(req PostgresVerificationRequest) error {
	if !isSafeProjectName(req.Project) || !isSafeRuntimeName(req.Environment) || !isSafeServiceName(req.Service) {
		return fmt.Errorf("invalid PostgreSQL verification scope")
	}
	if !isSafeBackupVolume(req.Artifact.ID) {
		return fmt.Errorf("invalid backup artifact ID")
	}
	if req.Artifact.Format != BackupArtifactFormatPostgresCustom {
		return fmt.Errorf("backup artifact format must be postgres-custom")
	}
	if req.Artifact.Size <= 0 || req.Artifact.Size > maxBackupArtifactSize {
		return fmt.Errorf("backup artifact size must be between 1 byte and 1TB")
	}
	if req.ScratchBytes < 64<<20 || req.ScratchBytes < req.Artifact.Size || req.ScratchBytes > maxBackupArtifactSize {
		return fmt.Errorf("PostgreSQL verification scratchBytes must be at least 64MiB, at least the artifact size, and no more than 1TB")
	}
	if err := validateBackupObjectKey(req.Artifact.Key); err != nil {
		return err
	}
	if len(req.Artifact.SHA256) != sha256.Size*2 || req.Artifact.SHA256 != strings.ToLower(req.Artifact.SHA256) {
		return fmt.Errorf("backup artifact SHA-256 is invalid")
	}
	if _, err := hex.DecodeString(req.Artifact.SHA256); err != nil {
		return fmt.Errorf("backup artifact SHA-256 is invalid")
	}
	if err := ValidateBackupStorage(req.Storage); err != nil {
		return err
	}
	if err := validateImageName(req.Image); err != nil {
		return err
	}
	const digestMarker = "@sha256:"
	digestIndex := strings.LastIndex(req.Image, digestMarker)
	if digestIndex < 1 || len(req.Image[digestIndex+len(digestMarker):]) != sha256.Size*2 {
		return fmt.Errorf("PostgreSQL verification image must be pinned by sha256 digest")
	}
	imageDigest := req.Image[digestIndex+len(digestMarker):]
	if imageDigest != strings.ToLower(imageDigest) {
		return fmt.Errorf("PostgreSQL verification image must be pinned by sha256 digest")
	}
	if _, err := hex.DecodeString(imageDigest); err != nil {
		return fmt.Errorf("PostgreSQL verification image must be pinned by sha256 digest")
	}
	repository := req.Image[:digestIndex]
	if slash := strings.LastIndex(repository, "/"); slash >= 0 {
		if colon := strings.LastIndex(repository, ":"); colon > slash {
			repository = repository[:colon]
		}
	} else if colon := strings.LastIndex(repository, ":"); colon >= 0 {
		repository = repository[:colon]
	}
	if repository != "postgres" && repository != "docker.io/library/postgres" && repository != "index.docker.io/library/postgres" {
		return fmt.Errorf("PostgreSQL verification image must use the official Docker Hub postgres repository")
	}
	if !postgresIdentifierPattern.MatchString(req.Database) || !postgresIdentifierPattern.MatchString(req.Username) {
		return fmt.Errorf("PostgreSQL verification database and username must be simple identifiers")
	}
	if strings.TrimSpace(req.Query) == "" || len(req.Query) > 4096 || strings.ContainsRune(req.Query, 0) {
		return fmt.Errorf("PostgreSQL verification query is invalid")
	}
	if req.ExpectedValue == "" || len(req.ExpectedValue) > 4096 || strings.ContainsRune(req.ExpectedValue, 0) || req.ExpectedValue != strings.TrimSpace(req.ExpectedValue) {
		return fmt.Errorf("PostgreSQL verification expected value is invalid")
	}
	return nil
}

func waitForPostgresVerification(ctx context.Context, containerName, username, database, password string) error {
	readyCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		if _, err := runDocker(readyCtx, "exec", containerName, "pg_isready", "--quiet", "--host", "127.0.0.1", "--username", username, "--dbname", database); err == nil {
			if _, err := runDocker(readyCtx,
				"exec", "-e", "PGPASSWORD="+password, containerName,
				"psql", "--no-psqlrc", "--quiet", "--tuples-only", "--host", "127.0.0.1",
				"--username", username, "--dbname", database, "--command", "SELECT 1",
			); err == nil {
				return nil
			}
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-readyCtx.Done():
			timer.Stop()
			return fmt.Errorf("wait for isolated PostgreSQL readiness: %w", readyCtx.Err())
		case <-timer.C:
		}
	}
}

func cleanupVerificationDockerResource(kind, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	args := []string{"rm", "-f", "-v", name}
	if kind == "volume" {
		args = []string{"volume", "rm", name}
	}
	output, err := runDocker(ctx, args...)
	if err != nil && !strings.Contains(strings.ToLower(output), "no such") {
		return err
	}
	return nil
}

func probePostgresVerificationQuota(ctx context.Context) bool {
	output, err := runDocker(ctx,
		"run", "--rm", "--pull", "missing", "--network", "none", "--memory", "134217728",
		"--tmpfs", "/quota-probe:rw,nosuid,nodev,noexec,size=67108864", backupImage,
		"sh", "-c", `dd if=/dev/zero of=/quota-probe/probe bs=1M count=96 2>&1; code=$?; [ "$code" -ne 0 ]`,
	)
	return err == nil && strings.Contains(strings.ToLower(output), "no space left")
}

// CleanupPostgresVerificationResources removes backup scratch resources left
// by a prior crash before takod accepts new work.
func CleanupPostgresVerificationResources(ctx context.Context, dataDir string) error {
	root := filepath.Join(dataDir, backupVerificationDirName)
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect PostgreSQL verification state: %w", err)
	}
	containers, err := runDocker(ctx, "ps", "-aq", "--filter", "label=tako.role=backup-verification", "--format", "{{.ID}}")
	if err != nil {
		return fmt.Errorf("list stale PostgreSQL verification containers: %w", err)
	}
	for _, container := range strings.Fields(containers) {
		if err := cleanupVerificationDockerResource("container", container); err != nil {
			return fmt.Errorf("clean stale PostgreSQL verification container: %w", err)
		}
	}
	volumes, err := runDocker(ctx, "volume", "ls", "-q", "--filter", "label=tako.role=backup-verification")
	if err != nil {
		return fmt.Errorf("list stale PostgreSQL verification volumes: %w", err)
	}
	for _, volume := range strings.Fields(volumes) {
		if err := cleanupVerificationDockerResource("volume", volume); err != nil {
			return fmt.Errorf("clean stale PostgreSQL verification volume: %w", err)
		}
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && path != root && strings.HasPrefix(entry.Name(), ".verify-") {
			if err := os.RemoveAll(path); err != nil {
				return err
			}
			return filepath.SkipDir
		}
		return nil
	})
}

func persistPostgresVerification(dataDir string, result *PostgresVerificationResult) error {
	if result == nil {
		return fmt.Errorf("PostgreSQL verification result is required")
	}
	path := filepath.Join(dataDir, backupVerificationDirName, result.Project, result.Environment, result.Service, result.ArtifactID+".json")
	commitPath := path + ".commit"
	backupVerificationMu.Lock()
	defer backupVerificationMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create PostgreSQL verification result directory: %w", err)
	}
	var previousData []byte
	if existingData, err := os.ReadFile(path); err == nil {
		previousData = existingData
		var existing PostgresVerificationResult
		if err := json.Unmarshal(existingData, &existing); err != nil {
			return fmt.Errorf("decode existing PostgreSQL verification result: %w", err)
		}
		if !samePostgresVerificationIdentity(existing, *result) {
			return fmt.Errorf("backup artifact ID already records different verification evidence")
		}
		if existing.Status == "verified" && result.Status != "verified" {
			return nil
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read existing PostgreSQL verification result: %w", err)
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("encode PostgreSQL verification result: %w", err)
	}
	data = append(data, '\n')
	uncommittedPostgresVerifications.Store(path, struct{}{})
	committed := false
	defer func() {
		if committed {
			uncommittedPostgresVerifications.Delete(path)
		}
	}()
	if err := writeFileAtomic(path, data, 0600); err != nil {
		return fmt.Errorf("persist PostgreSQL verification result: %w", err)
	}
	if err := syncPostgresVerificationDirectoryWith(filepath.Dir(path)); err != nil {
		var rollbackErr error
		if previousData == nil {
			rollbackErr = os.Remove(path)
		} else {
			rollbackErr = writeFileAtomic(path, previousData, 0600)
		}
		if rollbackErr == nil {
			rollbackErr = syncDirectory(filepath.Dir(path))
		}
		if rollbackErr == nil {
			uncommittedPostgresVerifications.Delete(path)
		}
		return errors.Join(fmt.Errorf("sync PostgreSQL verification result directory: %w", err), rollbackErr)
	}
	marker := []byte(digestString(string(data)) + "\n")
	if err := writeFileAtomic(commitPath, marker, 0600); err != nil {
		return fmt.Errorf("persist PostgreSQL verification commit marker: %w", err)
	}
	if err := syncPostgresVerificationDirectoryWith(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync PostgreSQL verification commit marker: %w", err)
	}
	committed = true
	return nil
}

func ReadPostgresVerificationResult(dataDir, project, environment, service, artifactID string) (*PostgresVerificationResult, error) {
	if !isSafeProjectName(project) || !isSafeRuntimeName(environment) || !isSafeServiceName(service) || !isSafeBackupVolume(artifactID) {
		return nil, fmt.Errorf("invalid PostgreSQL verification result scope")
	}
	path := filepath.Join(dataDir, backupVerificationDirName, project, environment, service, artifactID+".json")
	if _, uncommitted := uncommittedPostgresVerifications.Load(path); uncommitted {
		return nil, fmt.Errorf("PostgreSQL verification result is not durably committed")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, fmt.Errorf("PostgreSQL verification result must be a regular 0600 file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	marker, err := os.ReadFile(path + ".commit")
	if err != nil || strings.TrimSpace(string(marker)) != digestString(string(data)) {
		return nil, fmt.Errorf("PostgreSQL verification result is not durably committed")
	}
	var result PostgresVerificationResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	if result.Project != project || result.Environment != environment || result.Service != service || result.ArtifactID != artifactID {
		return nil, fmt.Errorf("PostgreSQL verification result scope does not match its path")
	}
	return &result, nil
}

func validateExistingPostgresVerificationIdentity(dataDir string, result *PostgresVerificationResult) error {
	path := filepath.Join(dataDir, backupVerificationDirName, result.Project, result.Environment, result.Service, result.ArtifactID+".json")
	backupVerificationMu.Lock()
	defer backupVerificationMu.Unlock()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read existing PostgreSQL verification result: %w", err)
	}
	var existing PostgresVerificationResult
	if err := json.Unmarshal(data, &existing); err != nil {
		return fmt.Errorf("decode existing PostgreSQL verification result: %w", err)
	}
	if !samePostgresVerificationIdentity(existing, *result) {
		return fmt.Errorf("backup artifact ID already records different verification evidence")
	}
	return nil
}

func samePostgresVerificationIdentity(left, right PostgresVerificationResult) bool {
	return left.Project == right.Project && left.Environment == right.Environment && left.Service == right.Service &&
		left.ArtifactID == right.ArtifactID && left.ArtifactFormat == right.ArtifactFormat && left.ObjectKey == right.ObjectKey &&
		left.StorageProvider == right.StorageProvider && left.StorageBucket == right.StorageBucket && left.StorageEndpoint == right.StorageEndpoint &&
		left.Size == right.Size && left.ScratchBytes == right.ScratchBytes && left.MemoryBytes == right.MemoryBytes && left.SHA256 == right.SHA256 && left.PostgresImage == right.PostgresImage &&
		left.Database == right.Database && left.BootstrapUsername == right.BootstrapUsername && left.RestoreUsername == right.RestoreUsername &&
		left.QuerySHA256 == right.QuerySHA256 && left.ExpectedSHA256 == right.ExpectedSHA256
}

func randomVerificationSuffix() (string, error) {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return hex.EncodeToString(random), nil
}

func randomVerificationPassword() (string, error) {
	random := make([]byte, 24)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(random), nil
}

func digestString(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
