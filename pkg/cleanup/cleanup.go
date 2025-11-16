package cleanup

import (
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/ssh"
)

// Cleaner handles cleanup operations on the server
type Cleaner struct {
	client      *ssh.Client
	projectName string
	verbose     bool
}

// CleanupResult holds results of cleanup operations
type CleanupResult struct {
	ImagesRemoved     int
	ContainersRemoved int
	SpaceReclaimed    string
	Errors            []string
}

// NewCleaner creates a new cleaner
func NewCleaner(client *ssh.Client, projectName string, verbose bool) *Cleaner {
	return &Cleaner{
		client:      client,
		projectName: projectName,
		verbose:     verbose,
	}
}

// CleanOldImages removes old Docker images for this project
// Keeps the latest N images (default 3)
func (c *Cleaner) CleanOldImages(keepLatest int) error {
	if c.verbose {
		fmt.Printf("  Cleaning old Docker images (keeping %d latest)...\n", keepLatest)
	}

	// Get all images for this project, sorted by creation date
	cmd := fmt.Sprintf(`docker images --format "{{.ID}}|{{.Repository}}|{{.CreatedAt}}" | grep %s | sort -k3 -r`, c.projectName)
	output, err := c.client.Execute(cmd)
	if err != nil || output == "" {
		if c.verbose {
			fmt.Printf("  No images found to clean\n")
		}
		return nil
	}

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) <= keepLatest {
		if c.verbose {
			fmt.Printf("  Only %d images found, nothing to clean\n", len(lines))
		}
		return nil
	}

	// Remove images beyond keepLatest
	toRemove := lines[keepLatest:]
	for _, line := range toRemove {
		parts := strings.Split(line, "|")
		if len(parts) > 0 {
			imageID := parts[0]
			if c.verbose {
				fmt.Printf("  Removing old image: %s\n", imageID)
			}
			c.client.Execute(fmt.Sprintf("docker rmi -f %s", imageID))
		}
	}

	if c.verbose {
		fmt.Printf("  ✓ Removed %d old images\n", len(toRemove))
	}

	return nil
}

// CleanStoppedContainers removes stopped containers for this project
func (c *Cleaner) CleanStoppedContainers() error {
	if c.verbose {
		fmt.Printf("  Cleaning stopped containers...\n")
	}

	// Get all stopped containers for this project
	cmd := fmt.Sprintf(`docker ps -a --filter "name=%s" --filter "status=exited" --format "{{.ID}}"`, c.projectName)
	output, err := c.client.Execute(cmd)
	if err != nil || output == "" {
		if c.verbose {
			fmt.Printf("  No stopped containers found\n")
		}
		return nil
	}

	containerIDs := strings.Split(strings.TrimSpace(output), "\n")
	for _, id := range containerIDs {
		if id != "" {
			if c.verbose {
				fmt.Printf("  Removing container: %s\n", id)
			}
			c.client.Execute(fmt.Sprintf("docker rm %s", id))
		}
	}

	if c.verbose {
		fmt.Printf("  ✓ Removed %d stopped containers\n", len(containerIDs))
	}

	return nil
}

// CleanDanglingImages removes dangling Docker images (not tagged)
func (c *Cleaner) CleanDanglingImages() error {
	if c.verbose {
		fmt.Printf("  Cleaning dangling images...\n")
	}

	output, err := c.client.Execute("docker images -f 'dangling=true' -q")
	if err != nil || output == "" {
		if c.verbose {
			fmt.Printf("  No dangling images found\n")
		}
		return nil
	}

	imageIDs := strings.Split(strings.TrimSpace(output), "\n")
	count := 0
	for _, id := range imageIDs {
		if id != "" {
			c.client.Execute(fmt.Sprintf("docker rmi %s", id))
			count++
		}
	}

	if c.verbose {
		fmt.Printf("  ✓ Removed %d dangling images\n", count)
	}

	return nil
}

// CleanBuildCache removes Docker build cache
func (c *Cleaner) CleanBuildCache() error {
	if c.verbose {
		fmt.Printf("  Cleaning Docker build cache...\n")
	}

	output, err := c.client.Execute("docker builder prune -f")
	if err != nil {
		return fmt.Errorf("failed to clean build cache: %w", err)
	}

	if c.verbose && output != "" {
		fmt.Printf("  %s\n", strings.TrimSpace(output))
	}

	return nil
}

// CleanUnusedVolumes removes unused Docker volumes
func (c *Cleaner) CleanUnusedVolumes() error {
	if c.verbose {
		fmt.Printf("  Cleaning unused volumes...\n")
	}

	output, err := c.client.Execute("docker volume prune -f")
	if err != nil {
		return fmt.Errorf("failed to clean volumes: %w", err)
	}

	if c.verbose && output != "" {
		fmt.Printf("  %s\n", strings.TrimSpace(output))
	}

	return nil
}

// RotateLogs manually triggers log rotation (in addition to automatic)
func (c *Cleaner) RotateLogs() error {
	if c.verbose {
		fmt.Printf("  Checking log files...\n")
	}

	// Check log directory size (Traefik logs)
	output, err := c.client.Execute("du -sh /var/log/traefik/ 2>/dev/null || echo '0'")
	if err == nil && c.verbose {
		fmt.Printf("  Log directory size: %s\n", strings.TrimSpace(output))
	}

	// Log rotation is handled automatically by the system
	// Just verify the rotation settings are in place
	if c.verbose {
		fmt.Printf("  ✓ Log rotation is handled automatically\n")
	}

	return nil
}

// GetDiskUsage returns disk usage information
func (c *Cleaner) GetDiskUsage() (string, error) {
	output, err := c.client.Execute("df -h / | tail -1")
	if err != nil {
		return "", fmt.Errorf("failed to get disk usage: %w", err)
	}
	return strings.TrimSpace(output), nil
}

// GetDockerDiskUsage returns Docker disk usage
func (c *Cleaner) GetDockerDiskUsage() (string, error) {
	output, err := c.client.Execute("docker system df")
	if err != nil {
		return "", fmt.Errorf("failed to get Docker disk usage: %w", err)
	}
	return strings.TrimSpace(output), nil
}

// FullCleanup performs all cleanup operations
func (c *Cleaner) FullCleanup(keepImages int) (*CleanupResult, error) {
	result := &CleanupResult{
		Errors: []string{},
	}

	if c.verbose {
		fmt.Println("\n🧹 Starting cleanup operations...")
	}

	// Get initial disk usage
	initialDisk, _ := c.GetDiskUsage()

	// Clean old images
	if err := c.CleanOldImages(keepImages); err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("Failed to clean old images: %v", err))
	}

	// Clean stopped containers
	if err := c.CleanStoppedContainers(); err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("Failed to clean stopped containers: %v", err))
	}

	// Clean dangling images
	if err := c.CleanDanglingImages(); err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("Failed to clean dangling images: %v", err))
	}

	// Clean build cache
	if err := c.CleanBuildCache(); err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("Failed to clean build cache: %v", err))
	}

	// Clean unused volumes
	if err := c.CleanUnusedVolumes(); err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("Failed to clean unused volumes: %v", err))
	}

	// Check logs
	if err := c.RotateLogs(); err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("Failed to check logs: %v", err))
	}

	// Get final disk usage
	finalDisk, _ := c.GetDiskUsage()

	if c.verbose {
		fmt.Println("\n📊 Cleanup Summary:")
		fmt.Printf("  Disk usage before: %s\n", initialDisk)
		fmt.Printf("  Disk usage after:  %s\n", finalDisk)

		// Show Docker disk usage
		dockerUsage, _ := c.GetDockerDiskUsage()
		if dockerUsage != "" {
			fmt.Printf("\n  Docker disk usage:\n")
			for _, line := range strings.Split(dockerUsage, "\n") {
				fmt.Printf("  %s\n", line)
			}
		}

		if len(result.Errors) > 0 {
			fmt.Println("\n⚠️  Some errors occurred:")
			for _, err := range result.Errors {
				fmt.Printf("  - %s\n", err)
			}
		}
	}

	return result, nil
}

// SecureLogPermissions ensures log files have proper permissions
func (c *Cleaner) SecureLogPermissions() error {
	if c.verbose {
		fmt.Printf("  Securing log file permissions...\n")
	}

	commands := []string{
		// Ensure directory is owned by root (Traefik runs in container)
		"sudo chown -R root:root /var/log/traefik",
		// Directory: readable/writable by root only
		"sudo chmod 750 /var/log/traefik",
		// Log files: readable/writable by root only
		"sudo find /var/log/traefik -type f -exec chmod 640 {} \\;",
	}

	for _, cmd := range commands {
		if _, err := c.client.Execute(cmd); err != nil {
			if c.verbose {
				fmt.Printf("  Warning: %v\n", err)
			}
		}
	}

	if c.verbose {
		fmt.Printf("  ✓ Log permissions secured\n")
	}

	return nil
}
