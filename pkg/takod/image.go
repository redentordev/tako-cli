package takod

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type ImageExistsResponse struct {
	Image  string `json:"image"`
	Exists bool   `json:"exists"`
}

type ImageImportResponse struct {
	Image  string `json:"image"`
	Output string `json:"output,omitempty"`
}

type ImageBuildResponse struct {
	Image  string `json:"image"`
	Output string `json:"output,omitempty"`
}

func ImageExists(ctx context.Context, image string) (*ImageExistsResponse, error) {
	if err := validateImageName(image); err != nil {
		return nil, err
	}
	_, err := runDocker(ctx, "image", "inspect", image)
	return &ImageExistsResponse{Image: image, Exists: err == nil}, nil
}

func ExportImage(ctx context.Context, image string, w io.Writer) error {
	if err := validateImageName(image); err != nil {
		return err
	}
	cmd := dockerCommandContext(ctx, "docker", "save", image)
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to export image %s: %w, output: %s", image, err, stderr.String())
	}
	return nil
}

func ImportImage(ctx context.Context, image string, r io.Reader) (*ImageImportResponse, error) {
	if err := validateImageName(image); err != nil {
		return nil, err
	}
	cmd := dockerCommandContext(ctx, "docker", "load")
	cmd.Stdin = r
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("failed to import image %s: %w, output: %s", image, err, output.String())
	}
	if _, err := runDocker(ctx, "image", "inspect", image); err != nil {
		return nil, fmt.Errorf("imported image %s is not inspectable: %w", image, err)
	}
	return &ImageImportResponse{Image: image, Output: strings.TrimSpace(output.String())}, nil
}

func BuildImage(ctx context.Context, image string, r io.Reader) (*ImageBuildResponse, error) {
	if err := validateImageName(image); err != nil {
		return nil, err
	}
	buildDir, err := os.MkdirTemp("", "tako-build-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create build directory: %w", err)
	}
	defer os.RemoveAll(buildDir)

	if err := extractTarGz(r, buildDir); err != nil {
		return nil, err
	}

	cmd := dockerCommandContext(ctx, "docker", "build", "-t", image, ".")
	cmd.Dir = buildDir
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("failed to build image %s: %w, output: %s", image, err, output.String())
	}
	if _, err := runDocker(ctx, "image", "inspect", image); err != nil {
		return nil, fmt.Errorf("built image %s is not inspectable: %w", image, err)
	}
	return &ImageBuildResponse{Image: image, Output: strings.TrimSpace(output.String())}, nil
}

func extractTarGz(r io.Reader, destDir string) error {
	gzipReader, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("failed to read build context gzip stream: %w", err)
	}
	defer gzipReader.Close()

	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("failed to read build context tar stream: %w", err)
		}

		target, err := safeArchiveTarget(destDir, header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return fmt.Errorf("failed to create build context directory %s: %w", header.Name, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("failed to create build context parent for %s: %w", header.Name, err)
			}
			mode := header.FileInfo().Mode().Perm()
			if mode == 0 {
				mode = 0644
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return fmt.Errorf("failed to create build context file %s: %w", header.Name, err)
			}
			_, copyErr := io.Copy(file, tarReader)
			closeErr := file.Close()
			if copyErr != nil {
				return fmt.Errorf("failed to write build context file %s: %w", header.Name, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("failed to close build context file %s: %w", header.Name, closeErr)
			}
		default:
			return fmt.Errorf("unsupported build context entry %s", header.Name)
		}
	}
}

func safeArchiveTarget(destDir string, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("build context archive contains an empty path")
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("build context archive contains absolute path %s", name)
	}
	clean := filepath.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("build context archive path escapes root: %s", name)
	}
	destDir, err := filepath.Abs(destDir)
	if err != nil {
		return "", err
	}
	target := filepath.Join(destDir, clean)
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	if targetAbs != destDir && !strings.HasPrefix(targetAbs, destDir+string(filepath.Separator)) {
		return "", fmt.Errorf("build context archive path escapes root: %s", name)
	}
	return targetAbs, nil
}

func validateImageName(image string) error {
	if strings.TrimSpace(image) == "" {
		return fmt.Errorf("image is required")
	}
	if strings.ContainsAny(image, "\x00\r\n") {
		return fmt.Errorf("image contains unsupported characters")
	}
	return nil
}

func writeImageTarHeaders(w http.ResponseWriter, image string) {
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeImageArchiveName(image)+`.tar"`)
}

func sanitizeImageArchiveName(image string) string {
	var b strings.Builder
	for _, r := range image {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		return "image"
	}
	return name
}
