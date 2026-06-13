package takod

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
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
