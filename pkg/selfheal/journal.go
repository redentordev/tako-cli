package selfheal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"time"
)

// MaxJournalEntries caps the heal journal per environment.
const MaxJournalEntries = 1000

// Journal decisions (D8).
const (
	DecisionRestarted      = "restarted"
	DecisionStarted        = "started"
	DecisionFailed         = "failed"
	DecisionGaveUp         = "gave_up"
	DecisionReset          = "reset"
	DecisionPaused         = "paused"
	DecisionResumed        = "resumed"
	DecisionCircuitOpen    = "circuit_open"
	DecisionCircuitClosed  = "circuit_closed"
	DecisionSkippedStopped = "skipped_stopped"
	DecisionCompleted      = "completed"
	DecisionMissing        = "missing"
	// DecisionAllReplicasUnhealthy: every local replica needed an action
	// at once; the service is held instead of restarted.
	DecisionAllReplicasUnhealthy = "all_replicas_unhealthy"
)

var knownDecisions = map[string]bool{
	DecisionRestarted: true, DecisionStarted: true, DecisionFailed: true, DecisionGaveUp: true,
	DecisionReset: true, DecisionPaused: true, DecisionResumed: true, DecisionCircuitOpen: true,
	DecisionCircuitClosed: true, DecisionSkippedStopped: true, DecisionCompleted: true, DecisionMissing: true,
	DecisionAllReplicasUnhealthy: true,
}

var (
	tokenPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	namePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
)

// JournalEntry is one heal decision. Every string field is a token or a
// runtime name; container logs and raw error text never enter the journal.
type JournalEntry struct {
	Time      time.Time `json:"time"`
	Node      string    `json:"node,omitempty"`
	Service   string    `json:"service,omitempty"`
	Container string    `json:"container,omitempty"`
	Revision  string    `json:"revision,omitempty"`
	Decision  string    `json:"decision"`
	Reason    string    `json:"reason,omitempty"`
	Code      int       `json:"code,omitempty"`
	Result    string    `json:"result,omitempty"`
}

// SafeToken returns value when it is a snake_case token and "unknown"
// otherwise.
func SafeToken(value string) string {
	if value == "" {
		return ""
	}
	if tokenPattern.MatchString(value) {
		return value
	}
	return "unknown"
}

// SafeName returns value when it is a runtime name and "unknown" otherwise.
func SafeName(value string) string {
	if value == "" {
		return ""
	}
	if namePattern.MatchString(value) {
		return value
	}
	return "unknown"
}

// Sanitize forces every field into its allowed shape.
func (e JournalEntry) Sanitize() JournalEntry {
	e.Time = e.Time.UTC()
	e.Node = SafeName(e.Node)
	e.Service = SafeName(e.Service)
	e.Container = SafeName(e.Container)
	e.Revision = SafeName(e.Revision)
	if !knownDecisions[e.Decision] {
		e.Decision = "unknown"
	}
	e.Reason = SafeToken(e.Reason)
	e.Result = SafeToken(e.Result)
	if e.Code < 0 || e.Code > 99999 {
		e.Code = 0
	}
	return e
}

// AppendJournal appends sanitized entries, keeping the last
// MaxJournalEntries lines.
func AppendJournal(dataDir, project, environment string, entries ...JournalEntry) error {
	if len(entries) == 0 {
		return nil
	}
	path, err := journalPath(dataDir, project, environment)
	if err != nil {
		return err
	}
	lines, err := readJournalLines(path)
	if err != nil {
		lines = nil
	}
	for _, entry := range entries {
		data, err := json.Marshal(entry.Sanitize())
		if err != nil {
			return err
		}
		lines = append(lines, data)
	}
	if len(lines) > MaxJournalEntries {
		lines = lines[len(lines)-MaxJournalEntries:]
	}
	var buffer bytes.Buffer
	for _, line := range lines {
		buffer.Write(line)
		buffer.WriteByte('\n')
	}
	return writeFileAtomic(path, buffer.Bytes())
}

// ReadJournal returns the last limit entries (all when limit <= 0), oldest
// first.
func ReadJournal(dataDir, project, environment string, limit int) ([]JournalEntry, error) {
	path, err := journalPath(dataDir, project, environment)
	if err != nil {
		return nil, err
	}
	lines, err := readJournalLines(path)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	entries := make([]JournalEntry, 0, len(lines))
	for _, line := range lines {
		var entry JournalEntry
		if json.Unmarshal(line, &entry) == nil {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

func readJournalLines(path string) ([][]byte, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var lines [][]byte
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		lines = append(lines, append([]byte(nil), line...))
	}
	return lines, scanner.Err()
}
