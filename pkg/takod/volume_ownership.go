package takod

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const volumeOwnershipDirName = "volume-ownership"

type ManagedVolumeOwnership struct {
	Name string `json:"name"`
	UID  int    `json:"uid"`
	GID  int    `json:"gid"`
}

type volumeOwnershipJournal struct {
	SchemaVersion int       `json:"schemaVersion"`
	Project       string    `json:"project"`
	Environment   string    `json:"environment"`
	Service       string    `json:"service,omitempty"`
	Volume        string    `json:"volume"`
	UID           int       `json:"uid"`
	GID           int       `json:"gid"`
	OperationID   string    `json:"operationId,omitempty"`
	Phase         string    `json:"phase"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

func ensureManagedVolumeOwnership(ctx context.Context, dataDir, project, environment, service string, policy ManagedVolumeOwnership) error {
	if strings.TrimSpace(dataDir) == "" {
		return fmt.Errorf("takod data directory is required for managed volume ownership")
	}
	if !isSafeVolumeOwnershipName(policy.Name) || policy.UID < 0 || policy.UID > math.MaxInt32 || policy.GID < 0 || policy.GID > math.MaxInt32 {
		return fmt.Errorf("invalid managed volume ownership policy")
	}
	root := filepath.Join(dataDir, volumeOwnershipDirName)
	if err := os.MkdirAll(root, 0700); err != nil {
		return fmt.Errorf("create volume ownership state directory: %w", err)
	}
	release, err := acquireVolumeOwnershipStateLock(ctx, root)
	if err != nil {
		return fmt.Errorf("lock managed volume ownership state: %w", err)
	}
	defer release()

	journalPath := volumeOwnershipJournalPath(root, policy.Name)
	journal, err := readVolumeOwnershipJournal(journalPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	inspect, inspectErr := inspectVolumeOwnership(ctx, policy.Name)
	exists := inspectErr == nil
	if inspectErr != nil && !isMissingDockerObject(inspectErr.Error()) {
		return fmt.Errorf("inspect managed volume %s: %w", policy.Name, inspectErr)
	}
	if journal == nil {
		if exists {
			return writeVolumeOwnershipJournal(journalPath, volumeOwnershipJournal{
				SchemaVersion: 1, Project: project, Environment: environment, Service: service,
				Volume: policy.Name, UID: policy.UID, GID: policy.GID, Phase: "preexisting", UpdatedAt: time.Now().UTC(),
			})
		}
		operationID, err := newBackupArtifactID()
		if err != nil {
			return fmt.Errorf("create volume ownership operation identity: %w", err)
		}
		journal = &volumeOwnershipJournal{
			SchemaVersion: 1, Project: project, Environment: environment, Service: service,
			Volume: policy.Name, UID: policy.UID, GID: policy.GID,
			OperationID: strings.TrimPrefix(operationID, "artifact-"), Phase: "intent", UpdatedAt: time.Now().UTC(),
		}
		if err := writeVolumeOwnershipJournal(journalPath, *journal); err != nil {
			return err
		}
	} else if journal.Project != project || journal.Environment != environment || journal.Volume != policy.Name || journal.UID != policy.UID || journal.GID != policy.GID {
		return fmt.Errorf("managed volume %s has different durable ownership evidence; use an explicit migration", policy.Name)
	}

	if journal.Phase == "preexisting" {
		if !exists {
			return fmt.Errorf("preexisting managed volume %s disappeared; recreate or migrate it explicitly", policy.Name)
		}
		return nil
	}
	if journal.OperationID == "" {
		return fmt.Errorf("managed volume %s ownership evidence has no operation identity", policy.Name)
	}
	if !exists {
		if journal.Phase != "intent" {
			return fmt.Errorf("managed volume %s disappeared after ownership initialization", policy.Name)
		}
		args := []string{
			"volume", "create",
			"--label", "tako.project=" + project,
			"--label", "tako.environment=" + environment,
			"--label", "tako.runtime=takod",
			"--label", "tako.volume-ownership-operation=" + journal.OperationID,
			"--label", "tako.volume-ownership-uid=" + strconv.Itoa(policy.UID),
			"--label", "tako.volume-ownership-gid=" + strconv.Itoa(policy.GID),
		}
		if service != "" {
			args = append(args, "--label", "tako.service="+service)
		}
		args = append(args, policy.Name)
		if _, err := runDocker(ctx, args...); err != nil {
			return fmt.Errorf("create ownership-managed volume %s: %w", policy.Name, err)
		}
		inspect, err = inspectVolumeOwnership(ctx, policy.Name)
		if err != nil {
			return fmt.Errorf("inspect newly created volume %s: %w", policy.Name, err)
		}
	}
	if inspect.Driver != "local" || inspect.OperationID != journal.OperationID {
		return fmt.Errorf("managed volume %s does not match its creation ownership evidence", policy.Name)
	}
	if journal.Phase == "initialized" {
		return nil
	}
	journal.Phase = "created"
	journal.UpdatedAt = time.Now().UTC()
	if err := writeVolumeOwnershipJournal(journalPath, *journal); err != nil {
		return err
	}
	inspect, err = inspectVolumeOwnership(ctx, policy.Name)
	if err != nil || inspect.Driver != "local" || inspect.OperationID != journal.OperationID {
		return fmt.Errorf("managed volume %s changed before ownership initialization", policy.Name)
	}
	helperName := "tako-volume-owner-" + journal.OperationID
	if err := cleanupVerificationDockerResource("container", helperName); err != nil {
		return fmt.Errorf("clean stale managed volume ownership helper: %w", err)
	}
	initializeCommand := fmt.Sprintf("[ \"$(stat -c '%%u:%%g' /volume)\" = '%d:%d' ] || chown %d:%d /volume", policy.UID, policy.GID, policy.UID, policy.GID)
	if output, err := runDocker(ctx,
		"create", "--pull", "missing", "--name", helperName, "--network", "none",
		"--label", "tako.volume-ownership-operation="+journal.OperationID,
		"-v", policy.Name+":/volume", backupImage, "sh", "-c", initializeCommand,
	); err != nil {
		return fmt.Errorf("create managed volume ownership helper: %s: %w", strings.TrimSpace(output), err)
	}
	defer func() { _ = cleanupVerificationDockerResource("container", helperName) }()
	inspect, err = inspectVolumeOwnership(ctx, policy.Name)
	if err != nil || inspect.Driver != "local" || inspect.OperationID != journal.OperationID {
		return fmt.Errorf("managed volume %s changed before its pinned ownership initialization", policy.Name)
	}
	mount, err := runDocker(ctx, "inspect", helperName, "--format", `{{range .Mounts}}{{if eq .Destination "/volume"}}{{.Type}}|{{.Name}}{{end}}{{end}}`)
	if err != nil || strings.TrimSpace(mount) != "volume|"+policy.Name {
		return fmt.Errorf("managed volume %s ownership helper did not pin the expected volume", policy.Name)
	}
	if _, err := runDocker(ctx, "start", "-a", helperName); err != nil {
		return fmt.Errorf("initialize managed volume %s ownership: %w", policy.Name, err)
	}
	journal.Phase = "initialized"
	journal.UpdatedAt = time.Now().UTC()
	return writeVolumeOwnershipJournal(journalPath, *journal)
}

func acquireVolumeOwnershipStateLock(ctx context.Context, root string) (func(), error) {
	for {
		release, err := acquireBackupDirectoryLock(root)
		if err == nil {
			return release, nil
		}
		if !errors.Is(err, errBackupDirectoryLocked) {
			return nil, err
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

type volumeOwnershipInspection struct {
	Driver      string
	OperationID string
}

func inspectVolumeOwnership(ctx context.Context, volume string) (volumeOwnershipInspection, error) {
	output, err := runDocker(ctx, "volume", "inspect", "--format", `{{.Driver}}|{{index .Labels "tako.volume-ownership-operation"}}`, volume)
	if err != nil {
		return volumeOwnershipInspection{}, fmt.Errorf("%s: %w", strings.TrimSpace(output), err)
	}
	parts := strings.SplitN(strings.TrimSpace(output), "|", 2)
	if len(parts) != 2 {
		return volumeOwnershipInspection{}, fmt.Errorf("invalid Docker volume inspection response")
	}
	return volumeOwnershipInspection{Driver: parts[0], OperationID: parts[1]}, nil
}

func isMissingDockerObject(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "no such volume") || strings.Contains(message, "no such object") || strings.Contains(message, "not found")
}

func volumeOwnershipJournalPath(root, volume string) string {
	digest := sha256.Sum256([]byte(volume))
	return filepath.Join(root, hex.EncodeToString(digest[:])+".json")
}

func readVolumeOwnershipJournal(path string) (*volumeOwnershipJournal, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var journal volumeOwnershipJournal
	if err := json.Unmarshal(data, &journal); err != nil || !validVolumeOwnershipJournal(journal) {
		return nil, fmt.Errorf("invalid volume ownership journal")
	}
	return &journal, nil
}

func validVolumeOwnershipJournal(journal volumeOwnershipJournal) bool {
	if journal.SchemaVersion != 1 || journal.Project == "" || journal.Environment == "" || !isSafeVolumeOwnershipName(journal.Volume) || journal.UID < 0 || journal.UID > math.MaxInt32 || journal.GID < 0 || journal.GID > math.MaxInt32 {
		return false
	}
	switch journal.Phase {
	case "preexisting":
		return journal.OperationID == ""
	case "intent", "created", "initialized":
		return isSafeVolumeOwnershipOperationID(journal.OperationID)
	default:
		return false
	}
}

func isSafeVolumeOwnershipName(value string) bool {
	if value == "" || len(value) > maxDockerVolumeName {
		return false
	}
	for index, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') {
			continue
		}
		if index > 0 && (char == '.' || char == '_' || char == '-') {
			continue
		}
		return false
	}
	return true
}

func isSafeVolumeOwnershipOperationID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' && char != '_' {
			return false
		}
	}
	return true
}

func writeVolumeOwnershipJournal(path string, journal volumeOwnershipJournal) error {
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := writeFileAtomic(path, data, 0600); err != nil {
		return fmt.Errorf("write volume ownership journal: %w", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync volume ownership journal: %w", err)
	}
	return nil
}

func RecoverManagedVolumeOwnership(ctx context.Context, dataDir string) error {
	root := filepath.Join(dataDir, volumeOwnershipDirName)
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		journal, err := readVolumeOwnershipJournal(filepath.Join(root, entry.Name()))
		if err != nil {
			return err
		}
		if journal.Phase == "intent" || journal.Phase == "created" {
			if err := ensureManagedVolumeOwnership(ctx, dataDir, journal.Project, journal.Environment, journal.Service, ManagedVolumeOwnership{Name: journal.Volume, UID: journal.UID, GID: journal.GID}); err != nil {
				return fmt.Errorf("recover managed volume %s ownership: %w", journal.Volume, err)
			}
		}
	}
	return nil
}
