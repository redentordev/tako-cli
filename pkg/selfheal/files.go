// Package selfheal holds the node-local self-healing model used by takod:
// converged records, control state, the heal journal, container
// classification, the eligibility rules, backoff, give-up and the node
// circuit breaker. It performs file I/O but never talks to Docker; takod
// supplies observations and executes the chosen action.
package selfheal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DirName is the heal tree under the takod data directory. Disk hygiene reads
// converged records from <dataDir>/heal/<project>/<env>/services/*.json.
const DirName = "heal"

const (
	servicesDirName  = "services"
	controlFileName  = "control.json"
	journalFileName  = "journal.jsonl"
	nodeStateFile    = ".node.json"
	maxStateFileSize = 4 << 20
)

// SchemaVersion is the on-disk schema of every heal document.
const SchemaVersion = 1

func safePathElement(value string) error {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, `/\`) || strings.ContainsRune(value, 0) {
		return fmt.Errorf("invalid self-healing path element %q", value)
	}
	return nil
}

// ScopeDir is <dataDir>/heal/<project>/<environment>.
func ScopeDir(dataDir, project, environment string) (string, error) {
	if dataDir == "" {
		return "", fmt.Errorf("data directory is required")
	}
	for _, value := range []string{project, environment} {
		if err := safePathElement(value); err != nil {
			return "", err
		}
	}
	return filepath.Join(dataDir, DirName, project, environment), nil
}

// RecordPath is the converged record of one service.
func RecordPath(dataDir, project, environment, service string) (string, error) {
	dir, err := ScopeDir(dataDir, project, environment)
	if err != nil {
		return "", err
	}
	if err := safePathElement(service); err != nil {
		return "", err
	}
	return filepath.Join(dir, servicesDirName, service+".json"), nil
}

func controlPath(dataDir, project, environment string) (string, error) {
	dir, err := ScopeDir(dataDir, project, environment)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, controlFileName), nil
}

func journalPath(dataDir, project, environment string) (string, error) {
	dir, err := ScopeDir(dataDir, project, environment)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, journalFileName), nil
}

func nodeStatePath(dataDir string) (string, error) {
	if dataDir == "" {
		return "", fmt.Errorf("data directory is required")
	}
	return filepath.Join(dataDir, DirName, nodeStateFile), nil
}

// writeJSONAtomic writes value to path with mode 0600 through a temp file
// and rename, so readers never see a partial document.
func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

// readJSON decodes path into value; found is false when the file is absent.
func readJSON(path string, value any) (bool, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Size() > maxStateFileSize {
		return false, fmt.Errorf("%s exceeds %d bytes", filepath.Base(path), maxStateFileSize)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return false, err
	}
	return true, nil
}

// ClearScope removes every heal document of one environment (records,
// control state and journal). Used by environment cleanup and preview
// teardown.
func ClearScope(dataDir, project, environment string) error {
	dir, err := ScopeDir(dataDir, project, environment)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ClearProject removes every heal document of a project.
func ClearProject(dataDir, project string) error {
	if dataDir == "" {
		return fmt.Errorf("data directory is required")
	}
	if err := safePathElement(project); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(dataDir, DirName, project)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Environments lists the environments of a project that have heal state.
func Environments(dataDir, project string) ([]string, error) {
	if err := safePathElement(project); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, DirName, project))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var environments []string
	for _, entry := range entries {
		if entry.IsDir() {
			environments = append(environments, entry.Name())
		}
	}
	return environments, nil
}
