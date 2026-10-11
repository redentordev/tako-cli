package takod

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

// CapabilityPreviewLifecycleV1 means the node stores `preview` state
// documents, serves GET /v1/state/previews, honors
// CleanupRequest.RemovePreviewData with its guard and fixed teardown order,
// and matches environment networks by exact name during cleanup.
const CapabilityPreviewLifecycleV1 = "preview.lifecycle-v1"

const (
	// PreviewEnvironmentPrefix starts the environment identity of every
	// preview. takod refuses preview records and preview data removal for any
	// other environment name.
	PreviewEnvironmentPrefix = "preview-"
	// PreviewRecordKind is the kind of a preview record document.
	PreviewRecordKind = "PreviewRecord"
	// PreviewRecordAPIVersion is the apiVersion of a preview record document.
	PreviewRecordAPIVersion = "tako.redentor.dev/v1alpha1"

	previewRecordFileName = "preview.json"
)

// PreviewRecord is the replicated record of one preview environment. It is
// written by the CLI before the first mutation of every preview deploy and
// deleted last by preview teardown.
type PreviewRecord struct {
	APIVersion  string            `json:"apiVersion"`
	Kind        string            `json:"kind"`
	Project     string            `json:"project"`
	Environment string            `json:"environment"`
	Name        string            `json:"name"`
	Base        string            `json:"base"`
	Servers     []string          `json:"servers"`
	Hosts       map[string]string `json:"hosts,omitempty"`
	CreatedAt   time.Time         `json:"createdAt"`
	UpdatedAt   time.Time         `json:"updatedAt"`
	// ExpiresAt is nil when the preview never expires (ttl none).
	ExpiresAt *time.Time     `json:"expiresAt,omitempty"`
	TTL       string         `json:"ttl"`
	Source    *PreviewSource `json:"source,omitempty"`
	CreatedBy string         `json:"createdBy,omitempty"`
}

// PreviewSource is the git source of the newest successful preview deploy.
type PreviewSource struct {
	Branch string `json:"branch,omitempty"`
	Commit string `json:"commit,omitempty"`
}

// PreviewListResponse is the body of GET /v1/state/previews.
type PreviewListResponse struct {
	Project  string          `json:"project"`
	Previews []PreviewRecord `json:"previews"`
	// Invalid lists environments whose preview.json could not be parsed.
	Invalid []string `json:"invalid,omitempty"`
}

// IsPreviewEnvironment reports whether environment is a preview identity.
func IsPreviewEnvironment(environment string) bool {
	return strings.HasPrefix(environment, PreviewEnvironmentPrefix) && len(environment) > len(PreviewEnvironmentPrefix)
}

func previewRecordPath(dataDir string, project string, environment string) string {
	return filepath.Join(dataDir, "desired", project, environment, previewRecordFileName)
}

func validatePreviewRecordContent(req StateDocumentRequest, raw json.RawMessage) error {
	if !IsPreviewEnvironment(req.Environment) {
		return fmt.Errorf("preview state documents are only accepted for %s* environments", PreviewEnvironmentPrefix)
	}
	var record PreviewRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return fmt.Errorf("invalid preview state document: %w", err)
	}
	if record.Kind != PreviewRecordKind {
		return fmt.Errorf("preview state document kind must be %s", PreviewRecordKind)
	}
	if err := validateProjectEnvIdentity(req, record.Project, record.Environment); err != nil {
		return err
	}
	if strings.TrimSpace(record.Name) == "" || strings.TrimSpace(record.Base) == "" {
		return fmt.Errorf("preview state document requires name and base")
	}
	return nil
}

// ListPreviewRecords returns every preview record of project on this node,
// sorted by environment. Unreadable records are reported in Invalid instead
// of failing the listing.
func ListPreviewRecords(ctx context.Context, dataDir string, project string) (*PreviewListResponse, error) {
	if !isSafeProjectName(project) {
		return nil, fmt.Errorf("invalid project name")
	}
	if dataDir == "" {
		return nil, fmt.Errorf("data directory is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	response := &PreviewListResponse{Project: project, Previews: []PreviewRecord{}}
	root := filepath.Join(dataDir, "desired", project)
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return response, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to list preview records: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !IsPreviewEnvironment(entry.Name()) || !isSafeRuntimeName(entry.Name()) {
			continue
		}
		record, found, err := readPreviewRecord(dataDir, project, entry.Name())
		if err != nil {
			response.Invalid = append(response.Invalid, entry.Name())
			continue
		}
		if found {
			response.Previews = append(response.Previews, *record)
		}
	}
	sort.Slice(response.Previews, func(i, j int) bool {
		return response.Previews[i].Environment < response.Previews[j].Environment
	})
	return response, nil
}

func readPreviewRecord(dataDir string, project string, environment string) (*PreviewRecord, bool, error) {
	data, err := os.ReadFile(previewRecordPath(dataDir, project, environment))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var record PreviewRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, false, err
	}
	if record.Kind != PreviewRecordKind || record.Project != project || record.Environment != environment {
		return nil, false, fmt.Errorf("preview record identity mismatch")
	}
	return &record, true, nil
}

// validatePreviewTeardownRequest rejects preview data removal combined with
// anything that reaches resources shared with other environments: image
// tags (repositories are shared with the base), /opt/<project>, and the
// project-wide maintenance passes.
func validatePreviewTeardownRequest(req CleanupRequest) error {
	if !req.RemovePreviewData {
		return nil
	}
	if !IsPreviewEnvironment(req.Environment) {
		return fmt.Errorf("removePreviewData is only accepted for %s* environments", PreviewEnvironmentPrefix)
	}
	if req.RemoveImages || len(req.ImageRepositories) > 0 {
		return fmt.Errorf("removePreviewData cannot remove images; image repositories are shared with the base environment")
	}
	if req.RemoveDeployFiles {
		return fmt.Errorf("removePreviewData cannot remove deploy files; /opt/<project> is shared by every environment")
	}
	if req.includesMaintenanceCleanup() {
		return fmt.Errorf("removePreviewData cannot be combined with maintenance cleanup")
	}
	if !req.RemoveContainers || !req.RemoveNetworks || !req.RemoveTakodState {
		return fmt.Errorf("removePreviewData requires removeContainers, removeNetworks and removeTakodState")
	}
	return nil
}

// checkPreviewTeardownGuard runs before any teardown step. It refuses a
// request for an environment that is not a preview, and a request whose
// preview record is missing while volumes labeled with exactly the
// preview's project and environment remain: the record is the proof that
// the volumes belong to a preview. With neither a record nor such volumes,
// an earlier attempt already tore the node down and noop is true.
func checkPreviewTeardownGuard(ctx context.Context, dataDir string, project string, environment string) (noop bool, err error) {
	if !IsPreviewEnvironment(environment) {
		return false, fmt.Errorf("refusing preview data removal for %s: not a preview environment", environment)
	}
	if strings.TrimSpace(dataDir) == "" {
		return false, fmt.Errorf("refusing preview data removal for %s: takod data directory is unknown", environment)
	}
	_, found, err := readPreviewRecord(dataDir, project, environment)
	if err != nil {
		return false, fmt.Errorf("refusing preview data removal for %s: unreadable preview record: %w", environment, err)
	}
	if found {
		return false, nil
	}
	volumes, err := listPreviewVolumes(ctx, project, environment)
	if err != nil {
		return false, fmt.Errorf("refusing preview data removal for %s: %w", environment, err)
	}
	if len(volumes) > 0 {
		return false, fmt.Errorf("refusing preview data removal for %s: it has no preview record but %d labeled volume(s) remain (%s); remove them by hand after verifying they are not needed", environment, len(volumes), strings.Join(volumes, ", "))
	}
	return true, nil
}

// listPreviewVolumes lists Docker volumes labeled with exactly project and
// environment. Docker label filters match the full value, so a volume of
// another environment never matches.
func listPreviewVolumes(ctx context.Context, project string, environment string) ([]string, error) {
	output, err := runDocker(ctx, "volume", "ls", "-q",
		"--filter", "label=tako.project="+project,
		"--filter", "label=tako.environment="+environment,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list preview volumes: %w", err)
	}
	return uniqueFields(output), nil
}

// cleanupPreviewEnvironment tears down one preview on this node in a fixed
// order: proxy files, containers, networks, preview data (volumes, their
// ownership journals, history, operator files), takod state, and the
// preview record last. Proxy, container, preview-data and state failures
// return before the record is removed so the same request can be retried.
func cleanupPreviewEnvironment(ctx context.Context, req CleanupRequest) (*CleanupResponse, error) {
	response := &CleanupResponse{Project: req.Project, Environment: req.Environment}
	warn := func(format string, args ...any) {
		response.Warnings = append(response.Warnings, fmt.Sprintf(format, args...))
	}
	noop, err := checkPreviewTeardownGuard(ctx, req.DataDir, req.Project, req.Environment)
	if err != nil {
		return response, err
	}
	if noop {
		response.PreviewNotFound = true
		return response, nil
	}

	if err := RemoveProxyFiles(ctx, req.ProxyFiles); err != nil {
		return response, fmt.Errorf("failed to withdraw proxy files: %w", err)
	}
	response.ProxyFilesRemoved = len(req.ProxyFiles)
	if slices.Contains(req.ProxyFiles, runtimeid.ProxyConfigFileName(req.Project, req.Environment)) {
		if _, err := RemoveACMEDNSConfiguration(ctx, req.Project, req.Environment); err != nil {
			warn("failed to remove proxy ACME ownership: %v", err)
		}
	}

	count, err := cleanupContainers(ctx, req.Project, req.Environment)
	if err != nil {
		return response, err
	}
	response.ContainersRemoved = count

	networks, err := cleanupNetworks(ctx, req.Project, req.Environment)
	if err != nil {
		warn("%v", err)
	}
	response.NetworksRemoved = networks

	volumes, err := removePreviewData(ctx, req.DataDir, req.Project, req.Environment)
	response.VolumesRemoved = volumes
	if err != nil {
		return response, err
	}

	if err := removePreviewState(req.DataDir, req.Project, req.Environment); err != nil {
		return response, err
	}
	if err := os.Remove(previewRecordPath(req.DataDir, req.Project, req.Environment)); err != nil && !os.IsNotExist(err) {
		return response, fmt.Errorf("failed to remove preview record: %w", err)
	}
	response.PreviewRecordRemoved = true
	if err := os.Remove(filepath.Dir(previewRecordPath(req.DataDir, req.Project, req.Environment))); err != nil && !os.IsNotExist(err) {
		warn("failed to remove preview state directory: %v", err)
	}
	return response, nil
}

// removePreviewData removes the preview's labeled volumes, then their
// ownership journals, deployment history and operator files.
func removePreviewData(ctx context.Context, dataDir string, project string, environment string) (int, error) {
	volumes, err := listPreviewVolumes(ctx, project, environment)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, volume := range volumes {
		if !isSafeDockerVolumeName(volume) {
			return removed, fmt.Errorf("refusing to remove volume with unsafe name %q", volume)
		}
		if _, err := runDocker(ctx, "volume", "rm", volume); err != nil {
			return removed, fmt.Errorf("failed to remove preview volume %s: %w", volume, err)
		}
		removed++
	}
	if err := removePreviewVolumeOwnershipJournals(ctx, dataDir, project, environment); err != nil {
		return removed, err
	}
	for _, path := range []string{
		filepath.Join(dataDir, "history", project, environment),
		filepath.Join(serviceFilesRoot, project, environment),
	} {
		if err := os.RemoveAll(path); err != nil {
			return removed, fmt.Errorf("failed to remove preview data at %s: %w", path, err)
		}
	}
	return removed, nil
}

func removePreviewVolumeOwnershipJournals(ctx context.Context, dataDir string, project string, environment string) error {
	root := filepath.Join(dataDir, volumeOwnershipDirName)
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("failed to read volume ownership journals: %w", err)
	}
	release, err := acquireVolumeOwnershipStateLock(ctx, root)
	if err != nil {
		return fmt.Errorf("lock volume ownership journals: %w", err)
	}
	defer release()
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("failed to read volume ownership journals: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		journal, err := readVolumeOwnershipJournal(path)
		if err != nil || journal.Project != project || journal.Environment != environment {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove volume ownership journal for %s: %w", journal.Volume, err)
		}
	}
	return nil
}

// removePreviewState removes the preview's desired, actual and event state,
// keeping preview.json so it can be deleted last.
func removePreviewState(dataDir string, project string, environment string) error {
	desired := filepath.Join(dataDir, "desired", project, environment)
	entries, err := os.ReadDir(desired)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to read preview desired state: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() == previewRecordFileName {
			continue
		}
		if err := os.RemoveAll(filepath.Join(desired, entry.Name())); err != nil {
			return fmt.Errorf("failed to remove preview desired state: %w", err)
		}
	}
	for _, path := range []string{
		filepath.Join(dataDir, "actual", project, environment),
		filepath.Join(dataDir, "events", project, environment+".jsonl"),
	} {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("failed to remove preview state at %s: %w", path, err)
		}
	}
	return nil
}
