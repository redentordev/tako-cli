package takod

// Image hygiene scopes (design D2 of add-disk-hygiene-defaults). takod has no
// tako.yaml, so it cannot compute an environment's built-image repositories
// itself. Every accepted scoped old-image cleanup (deploy-time or `tako
// cleanup`) records the repositories the CLI sent; the autonomous image GC
// selects only from recorded scopes that still have a desired document, with
// exactly those repositories. Pulled `image:` repositories are never sent by
// the CLI, so they are never recorded and never collected.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	hygieneDirName            = "hygiene"
	hygieneScopesDirName      = "scopes"
	hygieneScopeSchemaVersion = 1
	hygieneScopeMaxFileBytes  = 1 << 20
)

// HygieneScopeRecord is the recorded image GC scope of one environment.
type HygieneScopeRecord struct {
	SchemaVersion     int       `json:"schemaVersion"`
	Project           string    `json:"project"`
	Environment       string    `json:"environment"`
	ImageRepositories []string  `json:"imageRepositories"`
	RecordedAt        time.Time `json:"recordedAt"`
}

func hygieneScopePath(dataDir, project, environment string) string {
	return filepath.Join(dataDir, hygieneDirName, hygieneScopesDirName, project, environment+".json")
}

// recordHygieneScope records the repositories of one accepted scoped
// cleanup. An empty repository list records nothing.
func recordHygieneScope(dataDir, project, environment string, repositories []string, now time.Time) error {
	if dataDir == "" || len(repositories) == 0 {
		return nil
	}
	if !isSafeProjectName(project) || !isSafeRuntimeName(environment) {
		return fmt.Errorf("invalid hygiene scope")
	}
	unique := make([]string, 0, len(repositories))
	seen := make(map[string]bool, len(repositories))
	for _, repository := range repositories {
		if !isSafeImageRepository(repository) {
			return fmt.Errorf("invalid image repository %q", repository)
		}
		if !seen[repository] {
			seen[repository] = true
			unique = append(unique, repository)
		}
	}
	sort.Strings(unique)
	record := HygieneScopeRecord{
		SchemaVersion:     hygieneScopeSchemaVersion,
		Project:           project,
		Environment:       environment,
		ImageRepositories: unique,
		RecordedAt:        now.UTC(),
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	path := hygieneScopePath(dataDir, project, environment)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), 0600)
}

// removeHygieneScope deletes an environment's record, or every record of
// the project when environment is empty.
func removeHygieneScope(dataDir, project, environment string) error {
	if dataDir == "" || !isSafeProjectName(project) {
		return nil
	}
	if environment == "" {
		return os.RemoveAll(filepath.Join(dataDir, hygieneDirName, hygieneScopesDirName, project))
	}
	if !isSafeRuntimeName(environment) {
		return nil
	}
	if err := os.Remove(hygieneScopePath(dataDir, project, environment)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func readHygieneScope(path string) (HygieneScopeRecord, error) {
	var record HygieneScopeRecord
	info, err := os.Stat(path)
	if err != nil {
		return record, err
	}
	if info.Size() > hygieneScopeMaxFileBytes {
		return record, fmt.Errorf("hygiene scope %s exceeds size limit", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return record, fmt.Errorf("parse %s: %w", path, err)
	}
	if record.SchemaVersion != hygieneScopeSchemaVersion || !isSafeProjectName(record.Project) || !isSafeRuntimeName(record.Environment) {
		return record, fmt.Errorf("hygiene scope %s is invalid", path)
	}
	for _, repository := range record.ImageRepositories {
		if !isSafeImageRepository(repository) {
			return record, fmt.Errorf("hygiene scope %s names an invalid repository", path)
		}
	}
	return record, nil
}

// listHygieneScopes returns recorded scopes that still have a desired
// document, sorted by project and environment. problems lists records that
// could not be used, so callers can journal them; such records are never
// collected.
func listHygieneScopes(dataDir string) (scopes []HygieneScopeRecord, problems []string, err error) {
	root := filepath.Join(dataDir, hygieneDirName, hygieneScopesDirName)
	projects, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read hygiene scopes: %w", err)
	}
	for _, projectEntry := range projects {
		project := projectEntry.Name()
		if !projectEntry.IsDir() || !isSafeProjectName(project) {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(root, project))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", project, err))
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".json") {
				continue
			}
			environment := strings.TrimSuffix(name, ".json")
			if !isSafeRuntimeName(environment) {
				continue
			}
			record, err := readHygieneScope(filepath.Join(root, project, name))
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s/%s: %v", project, environment, err))
				continue
			}
			if record.Project != project || record.Environment != environment {
				problems = append(problems, fmt.Sprintf("%s/%s: record names %s/%s", project, environment, record.Project, record.Environment))
				continue
			}
			if _, err := os.Stat(filepath.Join(dataDir, "desired", project, environment, "revision.json")); err != nil {
				if !os.IsNotExist(err) {
					problems = append(problems, fmt.Sprintf("%s/%s: desired state: %v", project, environment, err))
				}
				continue
			}
			scopes = append(scopes, record)
		}
	}
	sort.Slice(scopes, func(i, j int) bool {
		if scopes[i].Project != scopes[j].Project {
			return scopes[i].Project < scopes[j].Project
		}
		return scopes[i].Environment < scopes[j].Environment
	})
	return scopes, problems, nil
}
