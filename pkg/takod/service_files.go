package takod

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

const DefaultServiceFilesRoot = "/var/lib/tako/files"

const (
	maxServiceFilePayloadBytes = 128 << 20
	maxServiceFileEntries      = 16384
	maxServiceFileJSONBytes    = 256 << 20
)

var serviceFilesRoot = DefaultServiceFilesRoot
var serviceFileLocks sync.Map

const serviceFileManifestName = ".tako-manifest.json"

type serviceFileManifest struct {
	SetID   string                      `json:"setId"`
	Bundles []serviceFileManifestBundle `json:"bundles"`
}

type serviceFileManifestBundle struct {
	Name    string                     `json:"name"`
	UID     int                        `json:"uid"`
	GID     int                        `json:"gid"`
	Entries []serviceFileManifestEntry `json:"entries"`
}

type serviceFileManifestEntry struct {
	Path      string `json:"path"`
	Directory bool   `json:"directory,omitempty"`
	Mode      uint32 `json:"mode"`
	Size      int64  `json:"size,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
}

type ServiceFilesRequest struct {
	Project     string              `json:"project"`
	Environment string              `json:"environment"`
	Service     string              `json:"service"`
	FileSetID   string              `json:"fileSetId"`
	Files       []ServiceFileBundle `json:"files"`
}

type ServiceFilesCheckRequest struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Service     string `json:"service"`
	FileSetID   string `json:"fileSetId"`
}

func CheckServiceFiles(request ServiceFilesCheckRequest) error {
	if !isSafeProjectName(request.Project) || !isSafeRuntimeName(request.Environment) || !isSafeServiceName(request.Service) {
		return fmt.Errorf("invalid service file identity")
	}
	return ensureServiceFileSet(request.Project, request.Environment, request.Service, request.FileSetID)
}

func PublishServiceFiles(ctx context.Context, request ServiceFilesRequest) error {
	if !isSafeProjectName(request.Project) || !isSafeRuntimeName(request.Environment) || !isSafeServiceName(request.Service) {
		return fmt.Errorf("invalid service file identity")
	}
	return prepareServiceFiles(ctx, request.Project, request.Environment, request.Service, request.FileSetID, request.Files)
}

func ServiceFileBundlePath(project, environment, service, setID, bundle string) string {
	return filepath.Join(DefaultServiceFilesRoot, project, environment, service, setID, bundle)
}

func validateServiceFileSetID(setID string) error {
	if len(setID) != 64 {
		return fmt.Errorf("invalid service file set id")
	}
	for _, r := range setID {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return fmt.Errorf("invalid service file set id")
		}
	}
	return nil
}

func validateServiceFileBundles(files []ServiceFileBundle) error {
	if len(files) > 128 {
		return fmt.Errorf("too many service file bundles")
	}
	seen := make(map[string]bool, len(files))
	targets := make(map[string]bool, len(files))
	totalBytes := 0
	totalEntries := 0
	for _, bundle := range files {
		if !isSafeRuntimeName(bundle.Name) || seen[bundle.Name] {
			return fmt.Errorf("invalid or duplicate service file bundle name")
		}
		if bundle.UID < 0 || bundle.UID > math.MaxInt32 || bundle.GID < 0 || bundle.GID > math.MaxInt32 {
			return fmt.Errorf("invalid service file ownership")
		}
		seen[bundle.Name] = true
		cleanTarget := filepath.Clean(bundle.Target)
		if len(bundle.Target) > 4096 || !filepath.IsAbs(cleanTarget) || cleanTarget == string(filepath.Separator) || cleanTarget != bundle.Target || strings.ContainsAny(bundle.Target, "\x00,\\\r\n\t") {
			return fmt.Errorf("service file target must be a canonical absolute path below /")
		}
		if targets[cleanTarget] {
			return fmt.Errorf("duplicate service file target")
		}
		targets[cleanTarget] = true
		if len(bundle.Entries) == 0 {
			return fmt.Errorf("service file bundle %s has no entries", bundle.Name)
		}
		entryKinds := make(map[string]bool, len(bundle.Entries)) // true means directory
		for _, entry := range bundle.Entries {
			totalEntries++
			totalBytes += len(entry.Data)
			if totalEntries > maxServiceFileEntries || totalBytes > maxServiceFilePayloadBytes {
				return fmt.Errorf("service files exceed payload limits")
			}
			clean := filepath.Clean(filepath.FromSlash(entry.Path))
			if entry.Path == "" {
				clean = "."
			}
			canonicalPath := filepath.ToSlash(clean)
			if entry.Path == "" {
				canonicalPath = ""
			}
			if len(entry.Path) > 4096 || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || strings.ContainsAny(entry.Path, "\x00\\\r\n\t") || entry.Path != canonicalPath {
				return fmt.Errorf("service file bundle %s contains an invalid path", bundle.Name)
			}
			if _, duplicate := entryKinds[clean]; duplicate {
				return fmt.Errorf("service file bundle %s contains a duplicate path", bundle.Name)
			}
			entryKinds[clean] = entry.Directory
			mode := os.FileMode(entry.Mode)
			if entry.Mode > 0777 || mode.Perm() == 0 {
				return fmt.Errorf("service file bundle %s contains an invalid mode", bundle.Name)
			}
			if entry.Directory && len(entry.Data) > 0 {
				return fmt.Errorf("service file directory entry must not contain data")
			}
			if bundle.Secret && mode.Perm()&0077 != 0 {
				return fmt.Errorf("secret service file modes must not grant group or world access")
			}
		}
		for entryPath := range entryKinds {
			for parent := filepath.Dir(entryPath); parent != "."; parent = filepath.Dir(parent) {
				directory, exists := entryKinds[parent]
				if !exists {
					return fmt.Errorf("service file bundle %s omits directory entry %s", bundle.Name, filepath.ToSlash(parent))
				}
				if !directory {
					return fmt.Errorf("service file bundle %s contains a file/descendant conflict", bundle.Name)
				}
			}
		}
		if bundle.Directory {
			if directory, exists := entryKinds["."]; !exists || !directory {
				return fmt.Errorf("directory service file bundle %s must contain a root directory", bundle.Name)
			}
		}
		if !bundle.Directory && (len(bundle.Entries) != 1 || bundle.Entries[0].Path != "" || bundle.Entries[0].Directory) {
			return fmt.Errorf("regular service file bundle %s must contain one root file", bundle.Name)
		}
	}
	return nil
}

func serviceFilesLock(project, environment, service string) *sync.Mutex {
	key := project + "\x00" + environment + "\x00" + service
	value, _ := serviceFileLocks.LoadOrStore(key, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func prepareServiceFiles(ctx context.Context, project, environment, service, setID string, files []ServiceFileBundle) error {
	if err := validateServiceFileSetID(setID); err != nil {
		return err
	}
	if err := validateServiceFileBundles(files); err != nil {
		return err
	}
	canonical, err := json.Marshal(files)
	if err != nil {
		return err
	}
	if len(canonical) > maxServiceFileJSONBytes {
		return fmt.Errorf("service files exceed encoded payload limit")
	}
	digest := sha256.Sum256(canonical)
	if hex.EncodeToString(digest[:]) != setID {
		return fmt.Errorf("service file set id does not match payload content")
	}
	lock := serviceFilesLock(project, environment, service)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	serviceRoot := filepath.Join(serviceFilesRoot, project, environment, service)
	if err := os.MkdirAll(serviceRoot, 0750); err != nil {
		return fmt.Errorf("failed to create service files root: %w", err)
	}
	if err := sweepServiceFileStaging(serviceRoot); err != nil {
		return err
	}
	versionRoot := filepath.Join(serviceRoot, setID)
	if info, err := os.Stat(versionRoot); err == nil && info.IsDir() {
		return verifyServiceFileSet(versionRoot, setID)
	}
	staging, err := os.MkdirTemp(serviceRoot, "."+setID+".next-")
	if err != nil {
		return fmt.Errorf("failed to stage service files: %w", err)
	}
	defer os.RemoveAll(staging)
	for _, bundle := range files {
		bundlePath := filepath.Join(staging, bundle.Name)
		if bundle.Directory {
			if err := os.MkdirAll(bundlePath, 0750); err != nil {
				return err
			}
		}
		for _, entry := range bundle.Entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			path := bundlePath
			if entry.Path != "" {
				path = filepath.Join(bundlePath, filepath.FromSlash(entry.Path))
			}
			mode := os.FileMode(entry.Mode).Perm()
			if entry.Directory {
				if err := os.MkdirAll(path, mode); err != nil {
					return fmt.Errorf("failed to create service file directory: %w", err)
				}
				if err := setServiceFileMetadata(path, mode, bundle.UID, bundle.GID); err != nil {
					return err
				}
				continue
			}
			if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
				return err
			}
			if err := writeFileAtomic(path, entry.Data, mode); err != nil {
				return fmt.Errorf("failed to write service file: %w", err)
			}
			if err := setServiceFileMetadata(path, mode, bundle.UID, bundle.GID); err != nil {
				return err
			}
		}
	}
	manifestData, err := json.Marshal(serviceFileManifestFor(setID, files))
	if err != nil {
		return fmt.Errorf("failed to encode service file manifest: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(staging, serviceFileManifestName), manifestData, 0400); err != nil {
		return fmt.Errorf("failed to write service file manifest: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(staging, versionRoot); err != nil {
		if info, statErr := os.Stat(versionRoot); statErr == nil && info.IsDir() {
			return verifyServiceFileSet(versionRoot, setID)
		}
		return fmt.Errorf("failed to publish service files: %w", err)
	}
	return nil
}

func setServiceFileMetadata(path string, mode os.FileMode, uid, gid int) error {
	if err := os.Chown(path, uid, gid); err != nil {
		return fmt.Errorf("failed to set service file ownership: %w", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("failed to set service file mode: %w", err)
	}
	return nil
}

func ensureServiceFileSet(project, environment, service, setID string) error {
	if err := validateServiceFileSetID(setID); err != nil {
		return err
	}
	lock := serviceFilesLock(project, environment, service)
	lock.Lock()
	defer lock.Unlock()
	root := filepath.Join(serviceFilesRoot, project, environment, service, setID)
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("operator file set %s is unavailable on this node; deploy it before reuse", setID)
	}
	if err := verifyServiceFileSet(root, setID); err != nil {
		return fmt.Errorf("operator file set %s failed integrity validation: %w", setID, err)
	}
	return nil
}

func serviceFileManifestFor(setID string, files []ServiceFileBundle) serviceFileManifest {
	manifest := serviceFileManifest{SetID: setID, Bundles: make([]serviceFileManifestBundle, 0, len(files))}
	for _, bundle := range files {
		manifestBundle := serviceFileManifestBundle{Name: bundle.Name, UID: bundle.UID, GID: bundle.GID, Entries: make([]serviceFileManifestEntry, 0, len(bundle.Entries))}
		for _, entry := range bundle.Entries {
			manifestEntry := serviceFileManifestEntry{Path: entry.Path, Directory: entry.Directory, Mode: entry.Mode}
			if !entry.Directory {
				digest := sha256.Sum256(entry.Data)
				manifestEntry.Size = int64(len(entry.Data))
				manifestEntry.SHA256 = hex.EncodeToString(digest[:])
			}
			manifestBundle.Entries = append(manifestBundle.Entries, manifestEntry)
		}
		manifest.Bundles = append(manifest.Bundles, manifestBundle)
	}
	return manifest
}

func verifyServiceFileSet(root string, setID string) error {
	data, err := os.ReadFile(filepath.Join(root, serviceFileManifestName))
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	var manifest serviceFileManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("decode manifest: %w", err)
	}
	if manifest.SetID != setID {
		return fmt.Errorf("manifest content address mismatch")
	}
	expected := map[string]bool{serviceFileManifestName: true}
	for _, bundle := range manifest.Bundles {
		if !isSafeRuntimeName(bundle.Name) {
			return fmt.Errorf("manifest contains invalid bundle name")
		}
		for _, entry := range bundle.Entries {
			relative := bundle.Name
			if entry.Path != "" {
				clean := filepath.Clean(filepath.FromSlash(entry.Path))
				if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.ToSlash(clean) != entry.Path {
					return fmt.Errorf("manifest contains invalid entry path")
				}
				relative = filepath.Join(relative, clean)
			}
			if expected[relative] {
				return fmt.Errorf("manifest contains duplicate entry")
			}
			expected[relative] = true
			path := filepath.Join(root, relative)
			info, err := os.Lstat(path)
			if err != nil {
				return fmt.Errorf("stat %s: %w", filepath.ToSlash(relative), err)
			}
			if info.Mode()&os.ModeSymlink != 0 || info.IsDir() != entry.Directory || (!entry.Directory && !info.Mode().IsRegular()) {
				return fmt.Errorf("entry type changed for %s", filepath.ToSlash(relative))
			}
			if uint32(info.Mode().Perm()) != entry.Mode {
				return fmt.Errorf("entry mode changed for %s", filepath.ToSlash(relative))
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || int(stat.Uid) != bundle.UID || int(stat.Gid) != bundle.GID {
				return fmt.Errorf("entry ownership changed for %s", filepath.ToSlash(relative))
			}
			if !entry.Directory {
				if info.Size() != entry.Size {
					return fmt.Errorf("entry size changed for %s", filepath.ToSlash(relative))
				}
				file, err := os.Open(path)
				if err != nil {
					return err
				}
				digest := sha256.New()
				_, copyErr := io.Copy(digest, file)
				closeErr := file.Close()
				if copyErr != nil {
					return copyErr
				}
				if closeErr != nil {
					return closeErr
				}
				if hex.EncodeToString(digest.Sum(nil)) != entry.SHA256 {
					return fmt.Errorf("entry content changed for %s", filepath.ToSlash(relative))
				}
			}
		}
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if !expected[relative] {
			return fmt.Errorf("unexpected entry %s", filepath.ToSlash(relative))
		}
		return nil
	})
}

func cleanupServiceFileVersions(project, environment, service, keepSetID string) error {
	lock := serviceFilesLock(project, environment, service)
	lock.Lock()
	defer lock.Unlock()
	root := filepath.Join(serviceFilesRoot, project, environment, service)
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == keepSetID {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return err
		}
	}
	if keepSetID == "" {
		return os.RemoveAll(root)
	}
	return nil
}

func sweepServiceFileStaging(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") && strings.Contains(entry.Name(), ".next-") {
			if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func removeServiceFiles(project, environment, service string) error {
	lock := serviceFilesLock(project, environment, service)
	lock.Lock()
	defer lock.Unlock()
	return os.RemoveAll(filepath.Join(serviceFilesRoot, project, environment, service))
}
