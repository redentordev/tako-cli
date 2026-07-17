package recovery

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

const (
	BundleKind                = "TakoPlatformRecoveryBundle"
	BundleAPIVersion          = "tako.io/v1"
	maxBundleEntries          = 100000
	maxBundleFileBytes  int64 = 2 << 30
	maxBundleTotalBytes int64 = 16 << 30
	maxManifestBytes    int64 = 8 << 20
)

type Source struct {
	Path     string
	Archive  string
	Required bool
}

type Entry struct {
	Path   string      `json:"path"`
	Mode   fs.FileMode `json:"mode"`
	Size   int64       `json:"size"`
	SHA256 string      `json:"sha256,omitempty"`
	Link   string      `json:"link,omitempty"`
}

type Manifest struct {
	APIVersion string    `json:"apiVersion"`
	Kind       string    `json:"kind"`
	ClusterID  string    `json:"clusterId"`
	CreatedAt  time.Time `json:"createdAt"`
	Roots      []string  `json:"roots"`
	Entries    []Entry   `json:"entries"`
}

type Result struct {
	Path     string
	SHA256   string
	Manifest Manifest
}

func Create(path, clusterID string, sources []Source) (*Result, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("recovery bundle path and sources are required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".tako-recovery-*.tmp")
	if err != nil {
		return nil, err
	}
	temporaryPath := temporary.Name()
	cleanup := func() { _ = temporary.Close(); _ = os.Remove(temporaryPath) }
	defer cleanup()
	if err := temporary.Chmod(0600); err != nil {
		return nil, err
	}
	hash := sha256.New()
	manifest, err := writeBundleArchive(io.MultiWriter(temporary, hash), clusterID, sources)
	if err != nil {
		return nil, err
	}
	if err := temporary.Sync(); err != nil {
		return nil, err
	}
	if err := temporary.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return nil, err
	}
	return &Result{Path: path, SHA256: hex.EncodeToString(hash.Sum(nil)), Manifest: manifest}, nil
}

// writeBundleArchive streams the complete compressed archive to writer. It is
// shared by plaintext test tooling and the production encrypted writer so the
// controller never needs to materialize an unencrypted recovery archive.
func writeBundleArchive(writer io.Writer, clusterID string, sources []Source) (Manifest, error) {
	if err := nodeidentity.ValidateClusterID(clusterID); err != nil {
		return Manifest{}, err
	}
	if len(sources) == 0 {
		return Manifest{}, fmt.Errorf("recovery bundle sources are required")
	}
	gzipWriter := gzip.NewWriter(writer)
	tarWriter := tar.NewWriter(gzipWriter)
	manifest := Manifest{APIVersion: BundleAPIVersion, Kind: BundleKind, ClusterID: clusterID, CreatedAt: time.Now().UTC()}
	var totalBytes int64
	for _, source := range sources {
		root := filepath.ToSlash(strings.Trim(strings.TrimSpace(source.Archive), "/"))
		if !safeArchivePath(root) {
			return Manifest{}, fmt.Errorf("invalid recovery source root")
		}
		manifest.Roots = append(manifest.Roots, root)
		if err := appendSource(tarWriter, &manifest, source, &totalBytes); err != nil {
			return Manifest{}, err
		}
	}
	sort.Slice(manifest.Entries, func(i, j int) bool { return manifest.Entries[i].Path < manifest.Entries[j].Path })
	sort.Strings(manifest.Roots)
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	manifestData = append(manifestData, '\n')
	if err := tarWriter.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0600, Size: int64(len(manifestData)), ModTime: manifest.CreatedAt}); err != nil {
		return Manifest{}, err
	}
	if _, err := tarWriter.Write(manifestData); err != nil {
		return Manifest{}, err
	}
	if err := tarWriter.Close(); err != nil {
		return Manifest{}, err
	}
	if err := gzipWriter.Close(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func appendSource(writer *tar.Writer, manifest *Manifest, source Source, totalBytes *int64) error {
	source.Path = filepath.Clean(source.Path)
	source.Archive = filepath.ToSlash(strings.Trim(strings.TrimSpace(source.Archive), "/"))
	if source.Path == "." || !safeArchivePath(source.Archive) {
		return fmt.Errorf("invalid recovery source")
	}
	_, err := os.Lstat(source.Path)
	if os.IsNotExist(err) && !source.Required {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect recovery source %s: %w", source.Path, err)
	}
	root := source.Path
	return filepath.WalkDir(root, func(current string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		entryInfo, err := os.Lstat(current)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		name := source.Archive
		if relative != "." {
			name += "/" + filepath.ToSlash(relative)
		}
		if !safeArchivePath(name) {
			return fmt.Errorf("unsafe recovery archive path %s", name)
		}
		if len(manifest.Entries) >= maxBundleEntries {
			return fmt.Errorf("recovery bundle exceeds %d entries", maxBundleEntries)
		}
		if !entryInfo.Mode().IsRegular() && !entryInfo.IsDir() && entryInfo.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("recovery source %s has unsupported special file type", current)
		}
		header, err := tar.FileInfoHeader(entryInfo, "")
		if err != nil {
			return err
		}
		header.Name = name
		header.Uid, header.Gid = 0, 0
		entry := Entry{Path: name, Mode: entryInfo.Mode(), Size: header.Size}
		if entryInfo.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(current)
			if err != nil {
				return err
			}
			if !safeArchiveLink(name, filepath.ToSlash(link), source.Archive) {
				return fmt.Errorf("recovery source %s has unsafe symlink target", current)
			}
			header.Linkname, entry.Link = link, link
		}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if entryInfo.Mode().IsRegular() {
			if header.Size < 0 || header.Size > maxBundleFileBytes || *totalBytes > maxBundleTotalBytes-header.Size {
				return fmt.Errorf("recovery source %s exceeds bundle size limits", current)
			}
			*totalBytes += header.Size
			file, err := os.Open(current)
			if err != nil {
				return err
			}
			digest := sha256.New()
			_, copyErr := io.Copy(io.MultiWriter(writer, digest), file)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			entry.SHA256 = hex.EncodeToString(digest.Sum(nil))
		}
		manifest.Entries = append(manifest.Entries, entry)
		return nil
	})
}

func Verify(path, expectedClusterID string) (*Manifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return verifyBundleArchive(file, expectedClusterID, "")
}

func verifyBundleArchive(archive io.Reader, expectedClusterID, destination string) (*Manifest, error) {
	gzipReader, err := gzip.NewReader(archive)
	if err != nil {
		return nil, err
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)
	observed := map[string]Entry{}
	extractedModes := map[string]fs.FileMode{}
	var manifest *Manifest
	var totalBytes int64
	entries := 0
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		entries++
		if entries > maxBundleEntries+1 {
			return nil, fmt.Errorf("recovery archive exceeds entry limit")
		}
		if header.Name == "manifest.json" {
			if manifest != nil || header.Size < 0 || header.Size > maxManifestBytes {
				return nil, fmt.Errorf("recovery manifest is duplicated or oversized")
			}
			data, err := io.ReadAll(io.LimitReader(reader, maxManifestBytes+1))
			if err != nil {
				return nil, err
			}
			var decoded Manifest
			if err := json.Unmarshal(data, &decoded); err != nil {
				return nil, err
			}
			manifest = &decoded
			continue
		}
		if !safeArchivePath(header.Name) || header.Size < 0 || header.Size > maxBundleFileBytes {
			return nil, fmt.Errorf("unsafe archive entry %q", header.Name)
		}
		if _, duplicate := observed[header.Name]; duplicate {
			return nil, fmt.Errorf("duplicate recovery archive entry %q", header.Name)
		}
		mode := header.FileInfo().Mode()
		if !mode.IsRegular() && !mode.IsDir() && mode&os.ModeSymlink == 0 {
			return nil, fmt.Errorf("unsupported recovery archive entry type %q", header.Name)
		}
		if mode.IsRegular() {
			if totalBytes > maxBundleTotalBytes-header.Size {
				return nil, fmt.Errorf("recovery archive exceeds total size limit")
			}
			totalBytes += header.Size
		}
		entry := Entry{Path: header.Name, Mode: mode, Size: header.Size, Link: header.Linkname}
		if mode.IsRegular() {
			digest := sha256.New()
			var output *os.File
			writers := []io.Writer{digest}
			if destination != "" {
				target := filepath.Join(destination, filepath.FromSlash(header.Name))
				if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
					return nil, err
				}
				output, err = os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				if err != nil {
					return nil, err
				}
				writers = append(writers, output)
			}
			_, copyErr := io.CopyN(io.MultiWriter(writers...), reader, header.Size)
			if output != nil {
				closeErr := output.Close()
				if copyErr == nil {
					copyErr = closeErr
				}
			}
			if copyErr != nil {
				return nil, copyErr
			}
			entry.SHA256 = hex.EncodeToString(digest.Sum(nil))
		} else if mode.IsDir() && destination != "" {
			target := filepath.Join(destination, filepath.FromSlash(header.Name))
			if err := os.MkdirAll(target, 0700); err != nil {
				return nil, err
			}
		}
		if destination != "" && mode&os.ModeSymlink == 0 {
			extractedModes[entry.Path] = entry.Mode
		}
		observed[entry.Path] = entry
	}
	if manifest == nil || manifest.APIVersion != BundleAPIVersion || manifest.Kind != BundleKind {
		return nil, fmt.Errorf("recovery manifest is missing or invalid")
	}
	if expectedClusterID != "" && manifest.ClusterID != expectedClusterID {
		return nil, fmt.Errorf("recovery bundle cluster %s does not match %s", manifest.ClusterID, expectedClusterID)
	}
	if len(manifest.Roots) == 0 {
		return nil, fmt.Errorf("recovery manifest has no source roots")
	}
	for _, root := range manifest.Roots {
		if !safeArchivePath(root) {
			return nil, fmt.Errorf("recovery manifest contains unsafe source root")
		}
	}
	if len(observed) != len(manifest.Entries) {
		return nil, fmt.Errorf("recovery manifest entry count does not match archive")
	}
	seenManifest := make(map[string]struct{}, len(manifest.Entries))
	for _, expected := range manifest.Entries {
		if !safeArchivePath(expected.Path) {
			return nil, fmt.Errorf("recovery manifest contains unsafe entry %q", expected.Path)
		}
		if _, duplicate := seenManifest[expected.Path]; duplicate {
			return nil, fmt.Errorf("recovery manifest duplicates entry %q", expected.Path)
		}
		seenManifest[expected.Path] = struct{}{}
		actual, ok := observed[expected.Path]
		if !ok || actual.Size != expected.Size || actual.SHA256 != expected.SHA256 || actual.Link != expected.Link || actual.Mode != expected.Mode {
			return nil, fmt.Errorf("recovery entry %s failed integrity verification", expected.Path)
		}
		if expected.Mode&os.ModeSymlink != 0 {
			root := matchingArchiveRoot(expected.Path, manifest.Roots)
			if root == "" || !safeArchiveLink(expected.Path, expected.Link, root) {
				return nil, fmt.Errorf("recovery entry %s has unsafe link target", expected.Path)
			}
		}
	}
	if destination != "" {
		// Symlinks are created only after every path, digest, root, and link has
		// been authenticated. Until this point extraction cannot traverse a link
		// supplied by the archive.
		for _, expected := range manifest.Entries {
			if expected.Mode&os.ModeSymlink == 0 {
				continue
			}
			target := filepath.Join(destination, filepath.FromSlash(expected.Path))
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return nil, err
			}
			if err := os.Symlink(expected.Link, target); err != nil {
				return nil, err
			}
		}
		var directories []string
		for name, mode := range extractedModes {
			target := filepath.Join(destination, filepath.FromSlash(name))
			if mode.IsDir() {
				directories = append(directories, name)
				continue
			}
			if err := os.Chmod(target, mode.Perm()); err != nil {
				return nil, err
			}
		}
		sort.Slice(directories, func(i, j int) bool { return len(directories[i]) > len(directories[j]) })
		for _, name := range directories {
			if err := os.Chmod(filepath.Join(destination, filepath.FromSlash(name)), extractedModes[name].Perm()); err != nil {
				return nil, err
			}
		}
	}
	return manifest, nil
}

func safeArchivePath(value string) bool {
	value = filepath.ToSlash(value)
	if value != strings.TrimSpace(value) {
		return false
	}
	if value == "" || value == "." || strings.HasPrefix(value, "/") || strings.ContainsRune(value, '\x00') {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(value))
	return clean == value && clean != ".." && !strings.HasPrefix(clean, "../")
}

func matchingArchiveRoot(name string, roots []string) string {
	best := ""
	for _, root := range roots {
		if (name == root || strings.HasPrefix(name, root+"/")) && len(root) > len(best) {
			best = root
		}
	}
	return best
}

func safeArchiveLink(name, target, root string) bool {
	target = filepath.ToSlash(target)
	if target != strings.TrimSpace(target) {
		return false
	}
	if target == "" || strings.HasPrefix(target, "/") || strings.ContainsRune(target, '\x00') {
		return false
	}
	resolved := filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(name), target)))
	return resolved == root || strings.HasPrefix(resolved, root+"/")
}
