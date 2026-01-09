package infra

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	// PulumiVersion is the version of Pulumi to install
	PulumiVersion = "3.215.0"

	// PulumiBaseURL is the base URL for Pulumi releases
	PulumiBaseURL = "https://get.pulumi.com/releases/sdk"
)

// EnsurePulumi checks if Pulumi is installed and installs it if not
func EnsurePulumi(verbose bool) error {
	// Check if Pulumi is already installed
	if isPulumiInstalled() {
		if verbose {
			fmt.Println("Pulumi is already installed")
		}
		return nil
	}

	if verbose {
		fmt.Println("Pulumi not found, installing...")
	}

	return installPulumi(verbose)
}

// isPulumiInstalled checks if Pulumi CLI is available in PATH
func isPulumiInstalled() bool {
	_, err := exec.LookPath("pulumi")
	return err == nil
}

// GetPulumiInstallDir returns the directory where Pulumi should be installed
func GetPulumiInstallDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	if runtime.GOOS == "windows" {
		return filepath.Join(homeDir, ".pulumi", "bin"), nil
	}
	return filepath.Join(homeDir, ".pulumi", "bin"), nil
}

// installPulumi downloads and installs Pulumi
func installPulumi(verbose bool) error {
	installDir, err := GetPulumiInstallDir()
	if err != nil {
		return err
	}

	// Create install directory
	if err := os.MkdirAll(installDir, 0755); err != nil {
		return fmt.Errorf("failed to create install directory: %w", err)
	}

	// Determine platform and architecture
	platform := runtime.GOOS
	arch := runtime.GOARCH

	// Map Go arch names to Pulumi arch names
	var pulumiArch string
	switch arch {
	case "amd64":
		pulumiArch = "x64"
	case "arm64":
		pulumiArch = "arm64"
	default:
		return fmt.Errorf("unsupported architecture: %s (only amd64 and arm64 are supported)", arch)
	}

	// Build download URL
	var downloadURL string
	var archiveType string

	switch platform {
	case "darwin":
		downloadURL = fmt.Sprintf("%s/pulumi-v%s-darwin-%s.tar.gz", PulumiBaseURL, PulumiVersion, pulumiArch)
		archiveType = "tar.gz"
	case "linux":
		downloadURL = fmt.Sprintf("%s/pulumi-v%s-linux-%s.tar.gz", PulumiBaseURL, PulumiVersion, pulumiArch)
		archiveType = "tar.gz"
	case "windows":
		if arch == "arm64" {
			return fmt.Errorf("Pulumi does not provide Windows arm64 builds. Please use Windows x64 or install Pulumi manually")
		}
		downloadURL = fmt.Sprintf("%s/pulumi-v%s-windows-%s.zip", PulumiBaseURL, PulumiVersion, pulumiArch)
		archiveType = "zip"
	default:
		return fmt.Errorf("unsupported platform: %s (supported: darwin, linux, windows)", platform)
	}

	if verbose {
		fmt.Printf("Downloading Pulumi v%s for %s/%s...\n", PulumiVersion, platform, arch)
	}

	// Download archive
	tempFile, err := downloadFile(downloadURL)
	if err != nil {
		return fmt.Errorf("failed to download Pulumi: %w", err)
	}
	defer os.Remove(tempFile)

	if verbose {
		fmt.Println("Extracting Pulumi...")
	}

	// Extract archive
	switch archiveType {
	case "tar.gz":
		if err := extractTarGz(tempFile, installDir); err != nil {
			return fmt.Errorf("failed to extract archive: %w", err)
		}
	case "zip":
		if err := extractZip(tempFile, installDir); err != nil {
			return fmt.Errorf("failed to extract archive: %w", err)
		}
	}

	// Verify installation
	pulumiPath := filepath.Join(installDir, "pulumi")
	if runtime.GOOS == "windows" {
		pulumiPath += ".exe"
	}

	if _, err := os.Stat(pulumiPath); os.IsNotExist(err) {
		return fmt.Errorf("Pulumi binary not found after extraction")
	}

	// Add to PATH hint
	if verbose {
		fmt.Printf("Pulumi installed to %s\n", installDir)
		fmt.Println("Add to your PATH:")
		if runtime.GOOS == "windows" {
			fmt.Printf("  set PATH=%%PATH%%;%s\n", installDir)
		} else {
			fmt.Printf("  export PATH=\"$PATH:%s\"\n", installDir)
		}
	}

	// Update PATH for current process
	currentPath := os.Getenv("PATH")
	if runtime.GOOS == "windows" {
		os.Setenv("PATH", currentPath+";"+installDir)
	} else {
		os.Setenv("PATH", currentPath+":"+installDir)
	}

	return nil
}

// downloadFile downloads a file and returns the path to the temp file
func downloadFile(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed with status: %s", resp.Status)
	}

	// Create temp file
	tempFile, err := os.CreateTemp("", "pulumi-*")
	if err != nil {
		return "", err
	}
	defer tempFile.Close()

	_, err = io.Copy(tempFile, resp.Body)
	if err != nil {
		os.Remove(tempFile.Name())
		return "", err
	}

	return tempFile.Name(), nil
}

// extractTarGz extracts a .tar.gz archive
func extractTarGz(archivePath, destDir string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()

	gzReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzReader.Close()

	tarReader := tar.NewReader(gzReader)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		// Skip directories and non-pulumi files
		if header.Typeflag == tar.TypeDir {
			continue
		}

		// Extract only files from pulumi/ directory
		name := header.Name
		if strings.HasPrefix(name, "pulumi/") {
			name = strings.TrimPrefix(name, "pulumi/")
		}

		// Skip if empty name
		if name == "" {
			continue
		}

		destPath := filepath.Join(destDir, name)

		// Create parent directories
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return err
		}

		// Create file
		outFile, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
		if err != nil {
			return err
		}

		if _, err := io.Copy(outFile, tarReader); err != nil {
			outFile.Close()
			return err
		}
		outFile.Close()
	}

	return nil
}

// extractZip extracts a .zip archive (for Windows)
func extractZip(archivePath, destDir string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer reader.Close()

	for _, file := range reader.File {
		// Skip directories
		if file.FileInfo().IsDir() {
			continue
		}

		// Extract only files from pulumi/ directory
		name := file.Name
		if strings.HasPrefix(name, "pulumi/") {
			name = strings.TrimPrefix(name, "pulumi/")
		}
		if strings.HasPrefix(name, "Pulumi/") {
			name = strings.TrimPrefix(name, "Pulumi/")
		}

		// Skip if empty name
		if name == "" {
			continue
		}

		destPath := filepath.Join(destDir, name)

		// Create parent directories
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return err
		}

		// Open source file
		srcFile, err := file.Open()
		if err != nil {
			return err
		}

		// Create destination file
		destFile, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, file.Mode())
		if err != nil {
			srcFile.Close()
			return err
		}

		if _, err := io.Copy(destFile, srcFile); err != nil {
			srcFile.Close()
			destFile.Close()
			return err
		}

		srcFile.Close()
		destFile.Close()
	}

	return nil
}

// GetPulumiVersion returns the installed Pulumi version
func GetPulumiVersion() (string, error) {
	cmd := exec.Command("pulumi", "version")
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}
