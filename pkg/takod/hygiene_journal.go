package takod

// Hygiene journal (design D7 of add-disk-hygiene-defaults): a ring of the
// last 50 hygiene, reclaim and image GC runs under
// <dataDir>/hygiene/journal.json. Writing it never aborts a run: under disk
// pressure a failed write keeps the run in memory and the next successful
// write persists it.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	hygieneJournalFileName      = "journal.json"
	hygieneJournalLimit         = 50
	hygieneJournalSchemaVersion = 1
	hygieneJournalMaxFileBytes  = 8 << 20
)

// Hygiene run triggers.
const (
	HygieneTriggerSchedule        = "schedule"
	HygieneTriggerPressureSoft    = "pressure-soft"
	HygieneTriggerPressureHard    = "pressure-hard"
	HygieneTriggerRequest         = "request"
	HygieneTriggerDeploy          = "deploy"
	HygieneTriggerCleanup         = "cleanup"
	HygieneTriggerSchedulerDenied = "scheduler-denied"
)

// Hygiene categories.
const (
	HygieneCategoryDanglingImages = "danglingImages"
	HygieneCategoryBuildCache     = "buildCache"
	HygieneCategoryImages         = "images"
	HygieneCategoryContainers     = "containers"
)

// HygieneFilesystem is one measured filesystem.
type HygieneFilesystem struct {
	// Subject is "root" (the takod data directory) or "docker" (the Docker
	// data root when it is a separate filesystem).
	Subject    string `json:"subject"`
	Path       string `json:"path"`
	FreeBytes  int64  `json:"freeBytes"`
	TotalBytes int64  `json:"totalBytes,omitempty"`
}

// HygieneCategory counts what one category removed.
type HygieneCategory struct {
	Removed    int   `json:"removed"`
	BytesFreed int64 `json:"bytesFreed"`
}

// HygieneRun is one journaled hygiene, reclaim or image GC run.
type HygieneRun struct {
	ID         string                     `json:"id"`
	Node       string                     `json:"node,omitempty"`
	Trigger    string                     `json:"trigger"`
	Level      string                     `json:"level,omitempty"`
	DryRun     bool                       `json:"dryRun,omitempty"`
	StartedAt  time.Time                  `json:"startedAt"`
	FinishedAt time.Time                  `json:"finishedAt"`
	Before     []HygieneFilesystem        `json:"before,omitempty"`
	After      []HygieneFilesystem        `json:"after,omitempty"`
	Categories map[string]HygieneCategory `json:"categories,omitempty"`
	// Images and Containers name what was removed (or, in a dry run, what
	// would be removed); Images is capped at 100 entries.
	Images     []HygieneImage `json:"images,omitempty"`
	Containers []string       `json:"containers,omitempty"`
	Kept       map[string]int `json:"kept,omitempty"`
	Skipped    []string       `json:"skipped,omitempty"`
	BytesFreed int64          `json:"bytesFreed"`
	// Insufficient reports a reclaim that ended still below its level's
	// threshold.
	Insufficient bool `json:"insufficient,omitempty"`
	// StoppedEarly reports a reclaim that stopped once free space was above
	// the soft threshold.
	StoppedEarly bool `json:"stoppedEarly,omitempty"`
}

type hygieneJournalDocument struct {
	SchemaVersion int          `json:"schemaVersion"`
	Runs          []HygieneRun `json:"runs"`
}

type hygieneJournal struct {
	path    string
	now     func() time.Time
	mu      sync.Mutex
	runs    []HygieneRun
	lastErr error
}

func hygieneJournalPath(dataDir string) string {
	return filepath.Join(dataDir, hygieneDirName, hygieneJournalFileName)
}

// openHygieneJournal loads the journal. A corrupt journal is kept as
// journal.json.corrupt and a new one is started.
func openHygieneJournal(dataDir string) *hygieneJournal {
	journal := &hygieneJournal{path: hygieneJournalPath(dataDir), now: func() time.Time { return time.Now().UTC() }}
	if dataDir == "" {
		journal.path = ""
		return journal
	}
	info, err := os.Stat(journal.path)
	if err != nil {
		if !os.IsNotExist(err) {
			journal.lastErr = err
		}
		return journal
	}
	var document hygieneJournalDocument
	data, readErr := os.ReadFile(journal.path)
	if readErr == nil && info.Size() <= hygieneJournalMaxFileBytes {
		readErr = json.Unmarshal(data, &document)
	} else if readErr == nil {
		readErr = fmt.Errorf("journal exceeds size limit")
	}
	if readErr != nil || document.SchemaVersion != hygieneJournalSchemaVersion {
		_ = os.Rename(journal.path, journal.path+".corrupt")
		journal.lastErr = fmt.Errorf("hygiene journal was unreadable and was moved to %s.corrupt", journal.path)
		return journal
	}
	journal.runs = document.Runs
	if len(journal.runs) > hygieneJournalLimit {
		journal.runs = journal.runs[len(journal.runs)-hygieneJournalLimit:]
	}
	return journal
}

// Append records a run and persists the ring. A write failure is kept in
// LastError and never returned to the run.
func (j *hygieneJournal) Append(run HygieneRun) {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if run.ID == "" {
		run.ID = fmt.Sprintf("%s-%d", run.StartedAt.UTC().Format("20060102T150405Z"), len(j.runs))
	}
	j.runs = append(j.runs, run)
	if len(j.runs) > hygieneJournalLimit {
		j.runs = append([]HygieneRun(nil), j.runs[len(j.runs)-hygieneJournalLimit:]...)
	}
	j.lastErr = j.persistLocked()
}

func (j *hygieneJournal) persistLocked() error {
	if j.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(hygieneJournalDocument{SchemaVersion: hygieneJournalSchemaVersion, Runs: j.runs}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(j.path), 0700); err != nil {
		return err
	}
	return writeFileAtomic(j.path, append(data, '\n'), 0600)
}

// Recent returns up to limit runs, newest first.
func (j *hygieneJournal) Recent(limit int) []HygieneRun {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if limit <= 0 || limit > len(j.runs) {
		limit = len(j.runs)
	}
	runs := make([]HygieneRun, 0, limit)
	for index := len(j.runs) - 1; index >= 0 && len(runs) < limit; index-- {
		runs = append(runs, j.runs[index])
	}
	return runs
}

// LastError reports the last load or write failure.
func (j *hygieneJournal) LastError() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.lastErr
}
