package engine

import "github.com/redentordev/tako-cli/pkg/takod"

// KindBackupResult identifies a serialized backup result document.
const KindBackupResult = "BackupResult"

// Backup actions carried in BackupResult.Action.
const (
	BackupActionList    = "list"
	BackupActionCreate  = "create"
	BackupActionRestore = "restore"
	BackupActionDelete  = "delete"
	BackupActionCleanup = "cleanup"
)

// BackupNodeOutcome reports one node's backup-operation result. Backups
// reuses the takod backup schema (`id`, `volume`, `size`, `createdAt`,
// `path`, `compression`, `remote`, `warnings`).
type BackupNodeOutcome struct {
	Server  string             `json:"server"`
	Host    string             `json:"host,omitempty"`
	Backups []takod.BackupInfo `json:"backups,omitempty"`
	// Deleted counts removed backups for delete/cleanup actions.
	Deleted int `json:"deleted,omitempty"`
	// Skipped lists volumes not present on this node during create.
	Skipped []string `json:"skipped,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// BackupResult is the serializable outcome of every `tako backup` action:
// list, create (single volume or --all), restore, delete, and cleanup.
type BackupResult struct {
	APIVersion  string              `json:"apiVersion"`
	Kind        string              `json:"kind"`
	Project     string              `json:"project"`
	Environment string              `json:"environment"`
	Action      string              `json:"action"`
	Volume      string              `json:"volume,omitempty"`
	BackupID    string              `json:"backupId,omitempty"`
	Nodes       []BackupNodeOutcome `json:"nodes"`
	Error       string              `json:"error,omitempty"`
}
