package provisioner

import (
	"fmt"
	"net"
	"path/filepath"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

// NFSProvisioner handles NFS server and client provisioning
type NFSProvisioner struct {
	verbose     bool
	projectName string
	environment string
}

// NewNFSProvisioner creates a new NFS provisioner
func NewNFSProvisioner(projectName, environment string, verbose bool) *NFSProvisioner {
	return &NFSProvisioner{
		verbose:     verbose,
		projectName: projectName,
		environment: environment,
	}
}

// NFSServerInfo contains information about the NFS server setup
type NFSServerInfo struct {
	ServerName string
	Host       string
	Exports    []NFSExportInfo
}

// NFSExportInfo contains information about a single NFS export
type NFSExportInfo struct {
	Name       string
	Path       string
	MountPoint string // Client mount point
}

// NFSStatus represents the status of NFS on a node
type NFSStatus struct {
	IsServer      bool
	ServiceActive bool
	ExportsRaw    string
	ClientCount   int
	Mounts        []NFSMountStatus
}

// NFSMountStatus represents the status of a single NFS mount
type NFSMountStatus struct {
	Source     string
	MountPoint string
	Mounted    bool
	Error      string
}

// Blocked paths that should never be used as NFS exports
var blockedNFSPaths = []string{
	"/",
	"/etc",
	"/root",
	"/home",
	"/var",
	"/usr",
	"/bin",
	"/sbin",
	"/lib",
	"/lib64",
	"/boot",
	"/proc",
	"/sys",
	"/dev",
	"/run",
	"/tmp",
}

// ValidateNFSExportPath checks if a path is safe to use as an NFS export
func ValidateNFSExportPath(path string) error {
	// Must be absolute path
	if !filepath.IsAbs(path) {
		return fmt.Errorf("NFS export path must be absolute: %s", path)
	}

	// Clean the path to prevent traversal
	cleanPath := filepath.Clean(path)

	// Check against blocked paths
	for _, blocked := range blockedNFSPaths {
		if cleanPath == blocked {
			return fmt.Errorf("NFS export path '%s' is not allowed (system directory)", path)
		}
		// Also block direct children of critical directories
		if blocked != "/" && strings.HasPrefix(cleanPath, blocked+"/") {
			// Allow subdirectories under /srv, /data, /mnt, /opt
			allowedPrefixes := []string{"/srv/", "/data/", "/mnt/", "/opt/", "/nfs/"}
			allowed := false
			for _, prefix := range allowedPrefixes {
				if strings.HasPrefix(cleanPath, prefix) {
					allowed = true
					break
				}
			}
			if !allowed {
				return fmt.Errorf("NFS export path '%s' is not allowed (under system directory %s)", path, blocked)
			}
		}
	}

	// Path must be at least 2 levels deep (e.g., /srv/nfs)
	parts := strings.Split(strings.Trim(cleanPath, "/"), "/")
	if len(parts) < 2 {
		return fmt.Errorf("NFS export path must be at least 2 levels deep (e.g., /srv/nfs): %s", path)
	}

	// Path cannot contain path traversal
	if strings.Contains(path, "..") {
		return fmt.Errorf("NFS export path cannot contain '..': %s", path)
	}

	return nil
}

// ValidateIPAddress checks if a string is a valid IP address
func ValidateIPAddress(ip string) error {
	if net.ParseIP(ip) == nil {
		return fmt.Errorf("invalid IP address: %s", ip)
	}
	return nil
}

// ShouldSetupNFS determines if NFS should be set up based on configuration and server count
func ShouldSetupNFS(cfg *config.Config, serverCount int) (bool, string) {
	if !cfg.IsNFSEnabled() {
		return false, "NFS is not enabled in configuration"
	}

	// NFS is only useful for multi-server setups
	// For single-server, use regular Docker volumes instead
	if serverCount < 2 {
		return false, "NFS is only set up for multi-server deployments (2+ servers). For single-server, use regular Docker volumes."
	}

	return true, ""
}

// SetupNFSServer sets up the NFS server on the specified host
func (n *NFSProvisioner) SetupNFSServer(client *ssh.Client, nfsConfig *config.NFSConfig, allowedIPs []string) (*NFSServerInfo, error) {
	if n.verbose {
		fmt.Printf("→ Setting up NFS server...\n")
	}

	// Validate all export paths first
	for _, export := range nfsConfig.Exports {
		if err := ValidateNFSExportPath(export.Path); err != nil {
			return nil, fmt.Errorf("invalid NFS export '%s': %w", export.Name, err)
		}
	}

	// Validate all allowed IPs
	for _, ip := range allowedIPs {
		if err := ValidateIPAddress(ip); err != nil {
			return nil, fmt.Errorf("invalid allowed IP: %w", err)
		}
	}

	// Install NFS server packages
	if err := n.installNFSServer(client); err != nil {
		return nil, fmt.Errorf("failed to install NFS server: %w", err)
	}

	// Create export directories and configure exports
	serverInfo := &NFSServerInfo{
		Exports: make([]NFSExportInfo, 0, len(nfsConfig.Exports)),
	}

	for _, export := range nfsConfig.Exports {
		exportInfo, err := n.configureExport(client, &export, allowedIPs)
		if err != nil {
			return nil, fmt.Errorf("failed to configure export '%s': %w", export.Name, err)
		}
		serverInfo.Exports = append(serverInfo.Exports, *exportInfo)
	}

	// Restart NFS server to apply changes
	if err := n.restartNFSServer(client); err != nil {
		return nil, fmt.Errorf("failed to restart NFS server: %w", err)
	}

	// Configure firewall for NFS (restrict to allowed IPs only)
	if err := n.configureNFSFirewall(client, allowedIPs); err != nil {
		return nil, fmt.Errorf("failed to configure firewall: %w", err)
	}

	if n.verbose {
		fmt.Printf("  ✓ NFS server setup completed\n")
		fmt.Printf("  Exports configured: %d\n", len(serverInfo.Exports))
	}

	return serverInfo, nil
}

// SetupNFSClient sets up NFS client and mounts on a worker node
func (n *NFSProvisioner) SetupNFSClient(client *ssh.Client, nfsServerHost string, exports []NFSExportInfo) error {
	if n.verbose {
		fmt.Printf("→ Setting up NFS client...\n")
	}

	// Validate NFS server host
	if nfsServerHost != "localhost" && nfsServerHost != "127.0.0.1" {
		if err := ValidateIPAddress(nfsServerHost); err != nil {
			return fmt.Errorf("invalid NFS server host: %w", err)
		}
	}

	// Install NFS client packages
	if err := n.installNFSClient(client); err != nil {
		return fmt.Errorf("failed to install NFS client: %w", err)
	}

	// Mount each export
	for _, export := range exports {
		if err := n.mountNFSExport(client, nfsServerHost, &export); err != nil {
			return fmt.Errorf("failed to mount export '%s': %w", export.Name, err)
		}
	}

	if n.verbose {
		fmt.Printf("  ✓ NFS client setup completed\n")
		fmt.Printf("  Mounts configured: %d\n", len(exports))
	}

	return nil
}

// installNFSServer installs NFS server packages on Ubuntu/Debian
func (n *NFSProvisioner) installNFSServer(client *ssh.Client) error {
	if n.verbose {
		fmt.Printf("  Installing NFS server packages...\n")
	}

	commands := []string{
		"sudo apt-get update",
		"sudo DEBIAN_FRONTEND=noninteractive apt-get install -y nfs-kernel-server",
	}

	for _, cmd := range commands {
		if n.verbose {
			fmt.Printf("  Running: %s\n", cmd)
		}
		if _, err := client.Execute(cmd); err != nil {
			return fmt.Errorf("command failed '%s': %w", cmd, err)
		}
	}

	// Enable NFS server to start on boot
	enableCommands := []string{
		"sudo systemctl enable nfs-kernel-server",
		"sudo systemctl start nfs-kernel-server",
	}

	for _, cmd := range enableCommands {
		client.Execute(cmd)
	}

	return nil
}

// installNFSClient installs NFS client packages on Ubuntu/Debian
func (n *NFSProvisioner) installNFSClient(client *ssh.Client) error {
	if n.verbose {
		fmt.Printf("  Installing NFS client packages...\n")
	}

	commands := []string{
		"sudo apt-get update",
		"sudo DEBIAN_FRONTEND=noninteractive apt-get install -y nfs-common",
	}

	for _, cmd := range commands {
		if n.verbose {
			fmt.Printf("  Running: %s\n", cmd)
		}
		if _, err := client.Execute(cmd); err != nil {
			return fmt.Errorf("command failed '%s': %w", cmd, err)
		}
	}

	return nil
}

// configureExport creates the export directory and adds it to /etc/exports
func (n *NFSProvisioner) configureExport(client *ssh.Client, export *config.NFSExportConfig, allowedIPs []string) (*NFSExportInfo, error) {
	if n.verbose {
		fmt.Printf("  Configuring export: %s -> %s\n", export.Name, export.Path)
	}

	// Validate path again (defense in depth)
	if err := ValidateNFSExportPath(export.Path); err != nil {
		return nil, err
	}

	// Create export directory with secure permissions
	createDirCmd := fmt.Sprintf("sudo mkdir -p %s", export.Path)
	if _, err := client.Execute(createDirCmd); err != nil {
		return nil, fmt.Errorf("failed to create directory: %w", err)
	}

	// Set ownership - use nobody:nogroup for NFS (standard for anonymous access)
	// This allows containers with different UIDs to access the files
	chownCmd := fmt.Sprintf("sudo chown -R nobody:nogroup %s", export.Path)
	if _, err := client.Execute(chownCmd); err != nil {
		return nil, fmt.Errorf("failed to set ownership: %w", err)
	}

	// Set directory permissions (rwxrwxr-x - group writable, not world writable)
	chmodCmd := fmt.Sprintf("sudo chmod -R 775 %s", export.Path)
	if _, err := client.Execute(chmodCmd); err != nil {
		return nil, fmt.Errorf("failed to set permissions: %w", err)
	}

	// Build export options
	options := n.buildExportOptions(export.Options)

	// Build allowed hosts string (restrict to specific IPs)
	allowedHosts := n.buildAllowedHosts(allowedIPs, options)

	// Create export line
	exportLine := fmt.Sprintf("%s %s", export.Path, allowedHosts)

	// Remove existing export for this path (if any) and add new one
	checkCmd := fmt.Sprintf("grep -q '^%s ' /etc/exports 2>/dev/null && echo 'exists' || echo 'new'", export.Path)
	output, _ := client.Execute(checkCmd)

	if strings.TrimSpace(output) == "exists" {
		// Remove existing line
		removeCmd := fmt.Sprintf("sudo sed -i '\\|^%s |d' /etc/exports", export.Path)
		client.Execute(removeCmd)
	}

	// Add new export line
	addCmd := fmt.Sprintf("echo '%s' | sudo tee -a /etc/exports > /dev/null", exportLine)
	if _, err := client.Execute(addCmd); err != nil {
		return nil, fmt.Errorf("failed to add export: %w", err)
	}

	// Calculate client mount point
	mountPoint := n.getMountPoint(export.Name)

	return &NFSExportInfo{
		Name:       export.Name,
		Path:       export.Path,
		MountPoint: mountPoint,
	}, nil
}

// buildExportOptions builds NFS export options string
func (n *NFSProvisioner) buildExportOptions(userOptions []string) string {
	// Default options for Tako NFS - balanced security and functionality
	defaultOptions := []string{
		"rw",               // Read-write access
		"sync",             // Synchronous writes for data integrity
		"no_subtree_check", // Improves reliability
		"no_root_squash",   // Allow root access (needed for containers)
		"secure",           // Require requests from ports < 1024 (more secure)
	}

	// If user provided options, use those instead
	if len(userOptions) > 0 {
		return strings.Join(userOptions, ",")
	}

	return strings.Join(defaultOptions, ",")
}

// buildAllowedHosts builds the allowed hosts string for /etc/exports
func (n *NFSProvisioner) buildAllowedHosts(allowedIPs []string, options string) string {
	if len(allowedIPs) == 0 {
		// If no specific IPs, allow localhost only (for single-server setup)
		return fmt.Sprintf("127.0.0.1(%s)", options)
	}

	// Build host entries for each allowed IP
	hosts := make([]string, 0, len(allowedIPs))
	for _, ip := range allowedIPs {
		hosts = append(hosts, fmt.Sprintf("%s(%s)", ip, options))
	}

	return strings.Join(hosts, " ")
}

// getMountPoint returns the client-side mount point for an export
func (n *NFSProvisioner) getMountPoint(exportName string) string {
	return fmt.Sprintf("/mnt/tako-nfs/%s_%s_%s", n.projectName, n.environment, exportName)
}

// restartNFSServer restarts the NFS server and exports filesystems
func (n *NFSProvisioner) restartNFSServer(client *ssh.Client) error {
	if n.verbose {
		fmt.Printf("  Restarting NFS server...\n")
	}

	// Export all filesystems
	if _, err := client.Execute("sudo exportfs -ra"); err != nil {
		return fmt.Errorf("failed to export filesystems: %w", err)
	}

	// Restart NFS server
	if _, err := client.Execute("sudo systemctl restart nfs-kernel-server"); err != nil {
		return fmt.Errorf("failed to restart NFS server: %w", err)
	}

	// Verify NFS server is running
	output, err := client.Execute("sudo systemctl is-active nfs-kernel-server")
	if err != nil || strings.TrimSpace(output) != "active" {
		return fmt.Errorf("NFS server is not running")
	}

	return nil
}

// configureNFSFirewall configures UFW firewall rules for NFS
// SECURITY: Only allows NFS connections from specified cluster IPs
func (n *NFSProvisioner) configureNFSFirewall(client *ssh.Client, allowedIPs []string) error {
	if n.verbose {
		fmt.Printf("  Configuring firewall for NFS (restricting to cluster IPs)...\n")
	}

	// First, remove any existing Tako NFS rules to start fresh
	client.Execute("sudo ufw status numbered | grep 'Tako NFS' | awk -F'[][]' '{print $2}' | sort -rn | while read num; do sudo ufw --force delete $num 2>/dev/null; done || true")

	// NFS uses port 2049 (TCP and UDP)
	// For NFSv4, only port 2049 is needed
	for _, ip := range allowedIPs {
		// Validate IP before adding to firewall
		if err := ValidateIPAddress(ip); err != nil {
			continue
		}

		// Allow NFS port from each client IP only
		cmd := fmt.Sprintf("sudo ufw allow from %s to any port 2049 proto tcp comment 'Tako NFS' || true", ip)
		if n.verbose {
			fmt.Printf("  Running: %s\n", cmd)
		}
		client.Execute(cmd)
	}

	// Always allow localhost for local access
	client.Execute("sudo ufw allow from 127.0.0.1 to any port 2049 proto tcp comment 'Tako NFS localhost' || true")

	// DENY all other NFS traffic (defense in depth)
	// This ensures only explicitly allowed IPs can connect
	client.Execute("sudo ufw deny 2049/tcp comment 'Tako NFS deny others' || true")

	return nil
}

// mountNFSExport mounts an NFS export on a client
func (n *NFSProvisioner) mountNFSExport(client *ssh.Client, nfsServerHost string, export *NFSExportInfo) error {
	if n.verbose {
		fmt.Printf("  Mounting %s from %s to %s...\n", export.Path, nfsServerHost, export.MountPoint)
	}

	// Create mount point directory with restrictive permissions
	createDirCmd := fmt.Sprintf("sudo mkdir -p %s && sudo chmod 755 %s", export.MountPoint, export.MountPoint)
	if _, err := client.Execute(createDirCmd); err != nil {
		return fmt.Errorf("failed to create mount point: %w", err)
	}

	// Check if already mounted
	checkMountCmd := fmt.Sprintf("mountpoint -q %s && echo 'mounted' || echo 'not_mounted'", export.MountPoint)
	output, _ := client.Execute(checkMountCmd)

	if strings.TrimSpace(output) == "mounted" {
		if n.verbose {
			fmt.Printf("  Already mounted, verifying...\n")
		}
		// Verify it's the correct mount
		verifyCmd := fmt.Sprintf("mount | grep '%s' | grep '%s' && echo 'correct' || echo 'wrong'", export.MountPoint, nfsServerHost)
		verifyOutput, _ := client.Execute(verifyCmd)
		if strings.Contains(verifyOutput, "correct") {
			return nil
		}
		// Wrong mount, unmount first
		client.Execute(fmt.Sprintf("sudo umount -f %s 2>/dev/null || true", export.MountPoint))
	}

	// NFS mount options for performance, reliability, and security
	mountOptions := "nfsvers=4.2,rsize=1048576,wsize=1048576,hard,timeo=600,retrans=2,noresvport,noatime"

	// Add to /etc/fstab for persistent mount
	fstabEntry := fmt.Sprintf("%s:%s %s nfs4 %s 0 0",
		nfsServerHost, export.Path, export.MountPoint, mountOptions)

	// Remove old fstab entry for this mount point (if any)
	removeFstabCmd := fmt.Sprintf("sudo sed -i '\\|%s|d' /etc/fstab", export.MountPoint)
	client.Execute(removeFstabCmd)

	// Add new fstab entry
	addFstabCmd := fmt.Sprintf("echo '%s' | sudo tee -a /etc/fstab > /dev/null", fstabEntry)
	if _, err := client.Execute(addFstabCmd); err != nil {
		return fmt.Errorf("failed to add fstab entry: %w", err)
	}

	// Mount the filesystem
	mountCmd := fmt.Sprintf("sudo mount %s", export.MountPoint)
	if _, err := client.Execute(mountCmd); err != nil {
		// Try mounting with explicit options if fstab mount fails
		explicitMountCmd := fmt.Sprintf("sudo mount -t nfs4 -o %s %s:%s %s",
			mountOptions, nfsServerHost, export.Path, export.MountPoint)
		if _, err := client.Execute(explicitMountCmd); err != nil {
			return fmt.Errorf("failed to mount NFS: %w", err)
		}
	}

	// Verify mount
	verifyCmd := fmt.Sprintf("mountpoint -q %s && echo 'success' || echo 'failed'", export.MountPoint)
	verifyOutput, _ := client.Execute(verifyCmd)

	if strings.TrimSpace(verifyOutput) != "success" {
		return fmt.Errorf("mount verification failed for %s", export.MountPoint)
	}

	if n.verbose {
		fmt.Printf("  ✓ Mounted successfully\n")
	}

	return nil
}

// GetNFSMountPoint returns the mount point path for an NFS export
func (n *NFSProvisioner) GetNFSMountPoint(exportName string) string {
	return n.getMountPoint(exportName)
}

// CleanupNFSServer removes NFS server configuration and exports
func (n *NFSProvisioner) CleanupNFSServer(client *ssh.Client, exports []NFSExportInfo) error {
	if n.verbose {
		fmt.Printf("→ Cleaning up NFS server...\n")
	}

	// Remove exports from /etc/exports
	for _, export := range exports {
		removeCmd := fmt.Sprintf("sudo sed -i '\\|^%s |d' /etc/exports", export.Path)
		client.Execute(removeCmd)

		if n.verbose {
			fmt.Printf("  Removed export: %s\n", export.Path)
		}
	}

	// Re-export to apply changes
	client.Execute("sudo exportfs -ra")

	// Remove Tako NFS firewall rules
	client.Execute("sudo ufw status numbered | grep 'Tako NFS' | awk -F'[][]' '{print $2}' | sort -rn | while read num; do sudo ufw --force delete $num 2>/dev/null; done || true")

	// Note: We don't remove the export directories themselves as they may contain data
	// We also don't stop/uninstall NFS server - it might be used by other projects

	if n.verbose {
		fmt.Printf("  ✓ NFS server cleanup completed\n")
		fmt.Printf("  Note: Export directories were preserved. Remove manually if needed.\n")
	}

	return nil
}

// CleanupNFSClient removes NFS client mounts and configuration
func (n *NFSProvisioner) CleanupNFSClient(client *ssh.Client, exports []NFSExportInfo) error {
	if n.verbose {
		fmt.Printf("→ Cleaning up NFS client...\n")
	}

	for _, export := range exports {
		// Force unmount the filesystem (lazy unmount if busy)
		unmountCmd := fmt.Sprintf("sudo umount -l %s 2>/dev/null || sudo umount -f %s 2>/dev/null || true", export.MountPoint, export.MountPoint)
		client.Execute(unmountCmd)

		// Remove from /etc/fstab
		removeFstabCmd := fmt.Sprintf("sudo sed -i '\\|%s|d' /etc/fstab", export.MountPoint)
		client.Execute(removeFstabCmd)

		// Remove mount point directory
		removeDirCmd := fmt.Sprintf("sudo rmdir %s 2>/dev/null || true", export.MountPoint)
		client.Execute(removeDirCmd)

		if n.verbose {
			fmt.Printf("  Unmounted and removed: %s\n", export.MountPoint)
		}
	}

	// Clean up parent directories if empty
	parentDir := fmt.Sprintf("/mnt/tako-nfs/%s_%s", n.projectName, n.environment)
	client.Execute(fmt.Sprintf("sudo rmdir %s 2>/dev/null || true", parentDir))
	client.Execute("sudo rmdir /mnt/tako-nfs 2>/dev/null || true")

	if n.verbose {
		fmt.Printf("  ✓ NFS client cleanup completed\n")
	}

	return nil
}

// RemountNFS remounts all NFS exports on a client
func (n *NFSProvisioner) RemountNFS(client *ssh.Client, exports []NFSExportInfo) error {
	if n.verbose {
		fmt.Printf("→ Remounting NFS exports...\n")
	}

	for _, export := range exports {
		// Lazy unmount first (doesn't fail if busy)
		unmountCmd := fmt.Sprintf("sudo umount -l %s 2>/dev/null || true", export.MountPoint)
		client.Execute(unmountCmd)

		// Wait a moment
		client.Execute("sleep 1")

		// Remount
		mountCmd := fmt.Sprintf("sudo mount %s", export.MountPoint)
		if _, err := client.Execute(mountCmd); err != nil {
			return fmt.Errorf("failed to remount %s: %w", export.MountPoint, err)
		}

		if n.verbose {
			fmt.Printf("  ✓ Remounted: %s\n", export.MountPoint)
		}
	}

	return nil
}

// GetNFSStatus checks the status of NFS on a server/client
func (n *NFSProvisioner) GetNFSStatus(client *ssh.Client, isServer bool) (*NFSStatus, error) {
	status := &NFSStatus{
		IsServer: isServer,
		Mounts:   make([]NFSMountStatus, 0),
	}

	if isServer {
		// Check NFS server status
		output, err := client.Execute("sudo systemctl is-active nfs-kernel-server 2>/dev/null")
		status.ServiceActive = err == nil && strings.TrimSpace(output) == "active"

		// Get exports
		exportsOutput, _ := client.Execute("sudo exportfs -v 2>/dev/null")
		status.ExportsRaw = strings.TrimSpace(exportsOutput)

		// Count connected clients
		clientsOutput, _ := client.Execute("sudo showmount -a 2>/dev/null | tail -n +2 | wc -l")
		fmt.Sscanf(strings.TrimSpace(clientsOutput), "%d", &status.ClientCount)
	} else {
		// Check NFS client mounts
		output, _ := client.Execute("mount -t nfs4 2>/dev/null")
		lines := strings.Split(strings.TrimSpace(output), "\n")
		for _, line := range lines {
			if line == "" {
				continue
			}
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				status.Mounts = append(status.Mounts, NFSMountStatus{
					Source:     parts[0],
					MountPoint: parts[2],
					Mounted:    true,
				})
			}
		}
	}

	return status, nil
}

// DetectExistingNFS checks if NFS is already set up for this project
func (n *NFSProvisioner) DetectExistingNFS(client *ssh.Client) (*NFSStatus, error) {
	return n.GetNFSStatus(client, true)
}

// MigrateNFSServer handles migration when NFS server changes
// This cleans up the old server and sets up on the new one
func (n *NFSProvisioner) MigrateNFSServer(
	oldServerClient *ssh.Client,
	newServerClient *ssh.Client,
	exports []NFSExportInfo,
	allowedIPs []string,
	nfsConfig *config.NFSConfig,
) error {
	if n.verbose {
		fmt.Printf("→ Migrating NFS server...\n")
	}

	// First, clean up old server exports (but keep data)
	if oldServerClient != nil {
		if err := n.CleanupNFSServer(oldServerClient, exports); err != nil {
			// Non-fatal, continue with setup on new server
			if n.verbose {
				fmt.Printf("  ⚠ Warning: Failed to cleanup old NFS server: %v\n", err)
			}
		}
	}

	// Setup on new server
	_, err := n.SetupNFSServer(newServerClient, nfsConfig, allowedIPs)
	if err != nil {
		return fmt.Errorf("failed to setup NFS on new server: %w", err)
	}

	if n.verbose {
		fmt.Printf("  ✓ NFS server migrated successfully\n")
		fmt.Printf("  Note: Data from old server must be migrated manually\n")
	}

	return nil
}

// CreateDockerVolume creates a Docker volume backed by an NFS mount
func (n *NFSProvisioner) CreateDockerVolume(client *ssh.Client, exportName string, volumeName string) error {
	mountPoint := n.getMountPoint(exportName)

	// Verify mount point exists and is mounted
	checkCmd := fmt.Sprintf("mountpoint -q %s && echo 'ok' || echo 'not_mounted'", mountPoint)
	output, _ := client.Execute(checkCmd)
	if strings.TrimSpace(output) != "ok" {
		return fmt.Errorf("NFS mount point %s is not mounted", mountPoint)
	}

	// Create Docker volume with local driver pointing to NFS mount
	cmd := fmt.Sprintf(`docker volume create --driver local \
		--opt type=none \
		--opt device=%s \
		--opt o=bind \
		%s`, mountPoint, volumeName)

	if n.verbose {
		fmt.Printf("  Creating Docker volume: %s -> %s\n", volumeName, mountPoint)
	}

	if _, err := client.Execute(cmd); err != nil {
		return fmt.Errorf("failed to create Docker volume: %w", err)
	}

	return nil
}

// RemoveDockerVolume removes a Docker volume
func (n *NFSProvisioner) RemoveDockerVolume(client *ssh.Client, volumeName string) error {
	cmd := fmt.Sprintf("docker volume rm %s 2>/dev/null || true", volumeName)
	client.Execute(cmd)
	return nil
}
