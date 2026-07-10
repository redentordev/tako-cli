package engine

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployplan"
)

// ValidateImageOptions validates the --image deploy override.
func ValidateImageOptions(serviceName string, imageRef string, source string) (string, error) {
	trimmedImage := strings.TrimSpace(imageRef)
	if trimmedImage == "" {
		if imageRef != "" {
			return "", invalidRequestf("--image must not be empty")
		}
		return "", nil
	}
	if strings.TrimSpace(serviceName) == "" {
		return "", invalidRequestf("--image requires --service to select the target service")
	}
	if strings.TrimSpace(source) != "" {
		return "", invalidRequestf("--image cannot be combined with --source")
	}
	return trimmedImage, nil
}

// ValidateArchiveOptions validates the --archive deploy override.
func ValidateArchiveOptions(serviceName string, archivePath string, source string, imageRef string) (string, error) {
	trimmedArchive := strings.TrimSpace(archivePath)
	if trimmedArchive == "" {
		if archivePath != "" {
			return "", invalidRequestf("--archive must not be empty")
		}
		return "", nil
	}
	if strings.TrimSpace(serviceName) == "" {
		return "", invalidRequestf("--archive requires --service to select the target service")
	}
	if strings.TrimSpace(source) != "" {
		return "", invalidRequestf("--archive cannot be combined with --source")
	}
	if strings.TrimSpace(imageRef) != "" {
		return "", invalidRequestf("--archive cannot be combined with --image")
	}
	if !IsSupportedArchive(trimmedArchive) {
		return "", invalidRequestf("unsupported archive format %q: supported formats are .tar, .tar.gz, .tgz, .zip", trimmedArchive)
	}
	info, err := os.Stat(trimmedArchive)
	if err != nil {
		return "", invalidRequestf("archive %q is not accessible: %w", trimmedArchive, err)
	}
	if !info.Mode().IsRegular() {
		return "", invalidRequestf("archive %q must be a regular file", trimmedArchive)
	}
	return trimmedArchive, nil
}

// IsSupportedArchive reports whether the path has a supported archive suffix.
func IsSupportedArchive(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasSuffix(lower, ".tar") || strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz") || strings.HasSuffix(lower, ".zip")
}

// SourceLabelForImageOverride returns the state source label for an --image deploy.
func SourceLabelForImageOverride(source string, imageRef string) string {
	if strings.TrimSpace(imageRef) != "" && strings.TrimSpace(source) == "" {
		return "image"
	}
	return source
}

// ApplyImageOverride points a service at a prebuilt image.
func ApplyImageOverride(service config.ServiceConfig, imageRef string) config.ServiceConfig {
	trimmedImage := strings.TrimSpace(imageRef)
	if trimmedImage == "" {
		return service
	}
	service.Image = trimmedImage
	service.ClearBuild()
	return service
}

// ApplySourceOverride points a service at a local build context.
func ApplySourceOverride(service config.ServiceConfig, source string) config.ServiceConfig {
	trimmedSource := strings.TrimSpace(source)
	if trimmedSource == "" {
		return service
	}
	service.Build = trimmedSource
	service.Image = ""
	return service
}

// SourceLabelForArchive returns the state source label for an --archive deploy.
func SourceLabelForArchive(archivePath string) string {
	return "archive:" + filepath.Base(strings.TrimSpace(archivePath))
}

// ApplyArchiveOverride points a service at an extracted archive build context.
func ApplyArchiveOverride(service config.ServiceConfig, buildContext string) config.ServiceConfig {
	trimmedBuildContext := strings.TrimSpace(buildContext)
	if trimmedBuildContext == "" {
		return service
	}
	service.Build = trimmedBuildContext
	service.Image = ""
	return service
}

// ArchiveBuildTag derives the build tag for an archive deploy from an explicit
// revision or the archive content hash.
func ArchiveBuildTag(explicitRevision string, archivePath string) (string, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return "", fmt.Errorf("failed to open archive %q: %w", archivePath, err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("failed to hash archive %q: %w", archivePath, err)
	}
	return deployplan.ArchiveBuildTag(strings.TrimSpace(explicitRevision), hash.Sum(nil))
}

// ExtractArchive extracts a supported source archive into destDir, rejecting
// links, absolute paths, and traversal.
func ExtractArchive(archivePath string, destDir string) error {
	lower := strings.ToLower(archivePath)
	if strings.HasSuffix(lower, ".zip") {
		return extractZipArchive(archivePath, destDir)
	}
	return extractTarArchive(archivePath, destDir, strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz"))
}

func extractTarArchive(archivePath string, destDir string, gzipCompressed bool) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	var reader io.Reader = file
	if gzipCompressed {
		gz, err := gzip.NewReader(file)
		if err != nil {
			return err
		}
		defer gz.Close()
		reader = gz
	}
	tr := tar.NewReader(reader)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeXHeader, tar.TypeXGlobalHeader, tar.TypeGNULongName, tar.TypeGNULongLink:
			continue
		}
		target, err := safeArchivePath(destDir, header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, header.FileInfo().Mode().Perm())
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, tr)
			closeErr := out.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("archive entry %q is a link; links are not supported", header.Name)
		default:
			return fmt.Errorf("archive entry %q has unsupported type", header.Name)
		}
	}
}

func extractZipArchive(archivePath string, destDir string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, entry := range zr.File {
		if entry.FileInfo().Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive entry %q is a link; links are not supported", entry.Name)
		}
		target, err := safeArchivePath(destDir, entry.Name)
		if err != nil {
			return err
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if !entry.FileInfo().Mode().IsRegular() {
			return fmt.Errorf("archive entry %q has unsupported type", entry.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		in, err := entry.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, entry.FileInfo().Mode().Perm())
		if err != nil {
			in.Close()
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeInErr := in.Close()
		closeOutErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeInErr != nil {
			return closeInErr
		}
		if closeOutErr != nil {
			return closeOutErr
		}
	}
	return nil
}

func safeArchivePath(destDir string, entryName string) (string, error) {
	if entryName == "" {
		return "", fmt.Errorf("archive entry name must not be empty")
	}
	if strings.Contains(entryName, "\\") {
		return "", fmt.Errorf("archive entry %q contains backslashes; use slash-separated paths", entryName)
	}
	if strings.Contains(entryName, ":") {
		return "", fmt.Errorf("archive entry %q contains colons; colons are not supported in archive paths", entryName)
	}
	if strings.HasPrefix(entryName, "/") {
		return "", fmt.Errorf("archive entry %q uses an absolute path", entryName)
	}
	cleanName := path.Clean(entryName)
	if cleanName == "." {
		return destDir, nil
	}
	if cleanName == ".." || strings.HasPrefix(cleanName, "../") {
		return "", fmt.Errorf("archive entry %q would escape destination", entryName)
	}
	target := filepath.Join(destDir, filepath.FromSlash(cleanName))
	rel, err := filepath.Rel(destDir, target)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("archive entry %q would escape destination", entryName)
	}
	return target, nil
}
