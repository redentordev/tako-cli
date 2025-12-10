package updater

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

const (
	repoOwner     = "redentordev"
	repoName      = "tako-cli"
	githubAPIURL  = "https://api.github.com/repos/%s/%s/releases/latest"
	downloadURL   = "https://github.com/%s/%s/releases/download/%s/%s"
	checkInterval = 24 * time.Hour // Check once per day
)

// Release represents a GitHub release
type Release struct {
	TagName string `json:"tag_name"`
	Name    string `json:"name"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
}

// GetLatestVersion fetches the latest release version from GitHub
func GetLatestVersion() (string, error) {
	url := fmt.Sprintf(githubAPIURL, repoOwner, repoName)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("failed to fetch latest version: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	var release Release
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", fmt.Errorf("failed to parse release info: %w", err)
	}

	return release.TagName, nil
}

// IsUpdateAvailable checks if a newer version is available
func IsUpdateAvailable(currentVersion string) (bool, string, error) {
	latestVersion, err := GetLatestVersion()
	if err != nil {
		return false, "", err
	}

	// Normalize versions (remove 'v' prefix if present)
	current := strings.TrimPrefix(currentVersion, "v")
	latest := strings.TrimPrefix(latestVersion, "v")

	// For dev builds, always suggest update
	if current == "dev" || current == "unknown" || current == "" {
		return true, latestVersion, nil
	}

	// Use semantic version comparison
	return compareVersions(latest, current) > 0, latestVersion, nil
}

// compareVersions compares two semantic versions
// Returns: 1 if v1 > v2, -1 if v1 < v2, 0 if equal
func compareVersions(v1, v2 string) int {
	// Remove 'v' prefix if present
	v1 = strings.TrimPrefix(v1, "v")
	v2 = strings.TrimPrefix(v2, "v")

	// Split into parts (major.minor.patch)
	parts1 := strings.Split(v1, ".")
	parts2 := strings.Split(v2, ".")

	// Compare each part
	maxLen := len(parts1)
	if len(parts2) > maxLen {
		maxLen = len(parts2)
	}

	for i := 0; i < maxLen; i++ {
		var n1, n2 int

		if i < len(parts1) {
			// Handle versions like "1.0.0-beta" by taking only the numeric part
			numPart := strings.Split(parts1[i], "-")[0]
			fmt.Sscanf(numPart, "%d", &n1)
		}

		if i < len(parts2) {
			numPart := strings.Split(parts2[i], "-")[0]
			fmt.Sscanf(numPart, "%d", &n2)
		}

		if n1 > n2 {
			return 1
		}
		if n1 < n2 {
			return -1
		}
	}

	return 0
}

// GetBinaryName returns the binary name for the current platform
func GetBinaryName() string {
	os := runtime.GOOS
	arch := runtime.GOARCH

	binary := fmt.Sprintf("tako-%s-%s", os, arch)

	if runtime.GOOS == "windows" {
		binary += ".exe"
	}

	return binary
}

// DownloadUpdate downloads the latest version and replaces the current binary
func DownloadUpdate(version string) error {
	binaryName := GetBinaryName()
	url := fmt.Sprintf(downloadURL, repoOwner, repoName, version, binaryName)

	// Get current executable path
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	// Download to temporary file
	tmpFile := execPath + ".new"

	fmt.Printf("Downloading Tako CLI %s...\n", version)

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("failed to download update: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed with status %d", resp.StatusCode)
	}

	// Create temporary file
	out, err := os.Create(tmpFile)
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %w", err)
	}
	defer out.Close()

	// Copy download to file
	_, err = io.Copy(out, resp.Body)
	if err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("failed to save update: %w", err)
	}

	// Make executable
	if err := os.Chmod(tmpFile, 0755); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("failed to make binary executable: %w", err)
	}

	// Backup current binary
	backupPath := execPath + ".bak"
	if err := os.Rename(execPath, backupPath); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("failed to backup current binary: %w", err)
	}

	// Replace with new binary
	if err := os.Rename(tmpFile, execPath); err != nil {
		// Restore backup on failure
		os.Rename(backupPath, execPath)
		os.Remove(tmpFile)
		return fmt.Errorf("failed to replace binary: %w", err)
	}

	// Remove backup
	os.Remove(backupPath)

	fmt.Printf("✓ Successfully upgraded to Tako CLI %s\n", version)
	fmt.Println("Please restart your terminal or run 'tako --version' to verify")

	return nil
}

// CheckForUpdate checks if an update is available and prints a message
func CheckForUpdate(currentVersion string, silent bool) {
	available, version, err := IsUpdateAvailable(currentVersion)
	if err != nil {
		if !silent {
			fmt.Printf("⚠️  Failed to check for updates: %v\n", err)
		}
		return
	}

	if available {
		fmt.Printf("\n🐙 A new version of Tako CLI is available: %s (current: %s)\n", version, currentVersion)
		fmt.Printf("   Run 'tako upgrade' to update\n\n")
	}
}

// ShouldCheckForUpdate checks if it's time to check for updates based on last check time
func ShouldCheckForUpdate() bool {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return true // Check if we can't determine
	}

	lastCheckFile := homeDir + "/.tako_last_update_check"

	info, err := os.Stat(lastCheckFile)
	if err != nil {
		// File doesn't exist, create it and return true
		os.WriteFile(lastCheckFile, []byte(time.Now().Format(time.RFC3339)), 0644)
		return true
	}

	// Check if 24 hours have passed
	if time.Since(info.ModTime()) > checkInterval {
		os.WriteFile(lastCheckFile, []byte(time.Now().Format(time.RFC3339)), 0644)
		return true
	}

	return false
}
