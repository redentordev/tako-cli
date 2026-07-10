package deployer

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takod"
)

const (
	maxOperatorFilesBytes        = 128 << 20
	maxOperatorFilesEncodedBytes = 256 << 20
)

// PrepareServiceFiles reads operator-managed files without text conversion,
// builds their request-scoped payload and read-only mounts, and returns a
// non-reversible content fingerprint.
func (d *Deployer) PrepareServiceFiles(serviceName string, service *config.ServiceConfig) ([]takod.ServiceFileBundle, []string, string, error) {
	return PrepareServiceFilesPayload(d.config.Project.Name, d.environment, serviceName, service)
}

func PrepareServiceFilesPayload(project, environment, serviceName string, service *config.ServiceConfig) ([]takod.ServiceFileBundle, []string, string, error) {
	if service == nil || len(service.Files) == 0 {
		return nil, nil, "", nil
	}
	if service.ReuseFiles {
		if service.FilesContentHash == "" {
			return nil, nil, "", fmt.Errorf("service %s rollback state does not include an operator file content hash", serviceName)
		}
		mounts, err := ServiceFileMounts(project, environment, serviceName, service.Files, service.FilesContentHash)
		return nil, mounts, service.FilesContentHash, err
	}
	bundles := make([]takod.ServiceFileBundle, 0, len(service.Files))
	total := 0
	defaultOwner := ""
	if _, _, configured, err := config.ParseServiceFileOwner(service.User); err == nil && configured {
		defaultOwner = service.User
	}
	for i, configured := range service.Files {
		bundleName := fmt.Sprintf("file-%03d", i)
		owner := configured.Owner
		if owner == "" {
			owner = defaultOwner
		}
		uid, gid, _, err := config.ParseServiceFileOwner(owner)
		if err != nil {
			return nil, nil, "", fmt.Errorf("service %s files[%d] owner: %w", serviceName, i, err)
		}
		bundle, size, err := loadServiceFileBundle(bundleName, configured, uid, gid, maxOperatorFilesBytes-total)
		if err != nil {
			return nil, nil, "", fmt.Errorf("service %s files[%d]: %w", serviceName, i, err)
		}
		total += size
		if total > maxOperatorFilesBytes {
			return nil, nil, "", fmt.Errorf("service %s files exceed %d MiB", serviceName, maxOperatorFilesBytes>>20)
		}
		bundles = append(bundles, bundle)
	}
	canonical, err := json.Marshal(bundles)
	if err != nil {
		return nil, nil, "", err
	}
	if len(canonical) > maxOperatorFilesEncodedBytes {
		return nil, nil, "", fmt.Errorf("service %s files exceed 256 MiB encoded payload limit", serviceName)
	}
	digest := sha256.Sum256(canonical)
	hash := fmt.Sprintf("sha256:%x", digest)
	if service.FilesContentHash != "" && service.FilesContentHash != hash {
		return nil, nil, "", fmt.Errorf("service %s operator files changed after planning; re-run the deploy to compute a fresh fingerprint", serviceName)
	}
	mounts, err := ServiceFileMounts(project, environment, serviceName, service.Files, hash)
	if err != nil {
		return nil, nil, "", err
	}
	return bundles, mounts, hash, nil
}

func ServiceFileMounts(project, environment, serviceName string, files []config.ServiceFileConfig, contentHash string) ([]string, error) {
	setID, err := serviceFileSetID(contentHash)
	if err != nil {
		return nil, err
	}
	mounts := make([]string, 0, len(files))
	for i, configured := range files {
		bundleName := fmt.Sprintf("file-%03d", i)
		source := takod.ServiceFileBundlePath(project, environment, serviceName, setID, bundleName)
		mounts = append(mounts, fmt.Sprintf("type=bind,source=%s,target=%s,readonly", source, configured.Target))
	}
	return mounts, nil
}

func serviceFileSetID(contentHash string) (string, error) {
	setID := strings.TrimPrefix(strings.TrimSpace(contentHash), "sha256:")
	if len(setID) != 64 {
		return "", fmt.Errorf("invalid operator file content hash")
	}
	for _, r := range setID {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return "", fmt.Errorf("invalid operator file content hash")
		}
	}
	return setID, nil
}

func ServiceFileSetID(contentHash string) (string, error) {
	return serviceFileSetID(contentHash)
}

func loadServiceFileBundle(name string, configured config.ServiceFileConfig, uid, gid int, remainingBytes int) (takod.ServiceFileBundle, int, error) {
	info, err := os.Lstat(configured.Source)
	if err != nil {
		return takod.ServiceFileBundle{}, 0, err
	}
	bundle := takod.ServiceFileBundle{Name: name, Target: configured.Target, Directory: info.IsDir(), Secret: configured.Secret, UID: uid, GID: gid}
	total := 0
	if info.Mode().IsRegular() {
		if info.Size() > int64(remainingBytes) {
			return takod.ServiceFileBundle{}, 0, fmt.Errorf("source exceeds %d MiB", maxOperatorFilesBytes>>20)
		}
		data, err := os.ReadFile(configured.Source)
		if err != nil {
			return takod.ServiceFileBundle{}, 0, err
		}
		bundle.Entries = []takod.ServiceFileEntry{{Data: data, Mode: serviceFileMode(info.Mode(), configured.Secret, false)}}
		return bundle, len(data), nil
	}
	if !info.IsDir() {
		return takod.ServiceFileBundle{}, 0, fmt.Errorf("source must be a regular file or directory")
	}
	err = filepath.WalkDir(configured.Source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink %s is not supported", path)
		}
		relative, err := filepath.Rel(configured.Source, path)
		if err != nil {
			return err
		}
		if relative == "." {
			relative = ""
		} else {
			relative = filepath.ToSlash(relative)
		}
		entryInfo, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			bundle.Entries = append(bundle.Entries, takod.ServiceFileEntry{Path: relative, Mode: serviceFileMode(entryInfo.Mode(), configured.Secret, true), Directory: true})
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("non-regular entry %s is not supported", path)
		}
		if entryInfo.Size() > int64(remainingBytes) || int64(total)+entryInfo.Size() > int64(remainingBytes) {
			return fmt.Errorf("directory exceeds %d MiB", maxOperatorFilesBytes>>20)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		total += len(data)
		bundle.Entries = append(bundle.Entries, takod.ServiceFileEntry{Path: relative, Data: data, Mode: serviceFileMode(entryInfo.Mode(), configured.Secret, false)})
		return nil
	})
	if err != nil {
		return takod.ServiceFileBundle{}, 0, err
	}
	sort.SliceStable(bundle.Entries, func(i, j int) bool {
		return strings.Compare(bundle.Entries[i].Path, bundle.Entries[j].Path) < 0
	})
	return bundle, total, nil
}

func serviceFileMode(mode os.FileMode, secret bool, directory bool) uint32 {
	if secret {
		if directory {
			return 0700
		}
		if mode.Perm()&0100 != 0 {
			return 0700
		}
		return 0600
	}
	permissions := mode.Perm()
	if directory {
		permissions |= 0500
	} else {
		permissions |= 0400
	}
	return uint32(permissions)
}
