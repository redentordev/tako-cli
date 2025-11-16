package setup

import (
	"fmt"
	"time"

	"github.com/redentordev/tako-cli/pkg/ssh"
)

// Upgrader handles server setup upgrades
type Upgrader struct {
	client *ssh.Client
	logger Logger
}

// Logger interface for upgrade logging
type Logger interface {
	Log(format string, args ...interface{})
}

// NewUpgrader creates a new upgrader instance
func NewUpgrader(client *ssh.Client, logger Logger) *Upgrader {
	return &Upgrader{
		client: client,
		logger: logger,
	}
}

// UpgradeStep represents a single upgrade step
type UpgradeStep struct {
	Version     string
	Description string
	Execute     func(*ssh.Client) error
	Rollback    func(*ssh.Client) error
	Validate    func(*ssh.Client) error
}

// UpgradePath represents the path from one version to another
type UpgradePath struct {
	From  string
	To    string
	Steps []UpgradeStep
}

// PlanUpgrade plans the upgrade path from current version to target
func PlanUpgrade(from, to string) (*UpgradePath, error) {
	// Get all steps needed
	steps := []UpgradeStep{}

	fromParts := parseVersion(from)
	toParts := parseVersion(to)

	// Build incremental upgrade path
	// Example: 1.0.0 -> 1.2.0 requires: 1.0.0->1.1.0, 1.1.0->1.2.0

	if fromParts.Major == 1 && fromParts.Minor == 0 && toParts.Minor >= 1 {
		// Need to upgrade to 1.1.0
		steps = append(steps, upgrade_1_0_to_1_1()...)
	}

	if fromParts.Major == 1 && fromParts.Minor <= 1 && toParts.Minor >= 2 {
		// Need to upgrade to 1.2.0
		steps = append(steps, upgrade_1_1_to_1_2()...)
	}

	if len(steps) == 0 {
		return nil, fmt.Errorf("no upgrade path found from %s to %s", from, to)
	}

	return &UpgradePath{
		From:  from,
		To:    to,
		Steps: steps,
	}, nil
}

// Execute executes the upgrade with automatic backup and rollback
func (u *Upgrader) Execute(path *UpgradePath) error {
	u.log("Starting upgrade from %s to %s", path.From, path.To)
	u.log("Total steps: %d", len(path.Steps))

	// Create backup
	backupID, err := u.createBackup()
	if err != nil {
		return fmt.Errorf("failed to create backup: %w", err)
	}
	u.log("Backup created: %s", backupID)

	// Execute each step
	for i, step := range path.Steps {
		u.log("Step %d/%d: %s (target: %s)", i+1, len(path.Steps), step.Description, step.Version)

		if err := step.Execute(u.client); err != nil {
			u.log("Step failed: %v", err)
			u.log("Rolling back...")

			// Rollback all previous steps in reverse order
			for j := i; j >= 0; j-- {
				if path.Steps[j].Rollback != nil {
					if rbErr := path.Steps[j].Rollback(u.client); rbErr != nil {
						u.log("Rollback step %d failed: %v", j, rbErr)
					}
				}
			}

			// Restore from backup
			if rbErr := u.restoreBackup(backupID); rbErr != nil {
				u.log("Failed to restore backup: %v", rbErr)
			}

			return fmt.Errorf("upgrade failed at step %d: %w", i+1, err)
		}

		// Validate step if validator exists
		if step.Validate != nil {
			if err := step.Validate(u.client); err != nil {
				u.log("Validation failed: %v", err)
				return fmt.Errorf("validation failed for step %d: %w", i+1, err)
			}
		}

		u.log("Step %d completed successfully", i+1)
	}

	// Update version file
	newVersion := &ServerVersion{
		Version:        path.To,
		LastUpgrade:    time.Now(),
		TakoCLIVersion: "0.3.0", // TODO: Get from build
		Components:     make(map[string]string),
		Features:       detectCurrentFeatures(u.client),
	}

	if err := WriteVersionFile(u.client, newVersion); err != nil {
		u.log("Warning: Failed to update version file: %v", err)
		// Don't fail the upgrade for this
	}

	u.log("Upgrade completed successfully!")
	u.log("Server is now at version %s", path.To)

	return nil
}

// createBackup creates a backup of critical files
func (u *Upgrader) createBackup() (string, error) {
	backupID := fmt.Sprintf("backup-%s", time.Now().Format("20060102-150405"))
	backupDir := fmt.Sprintf("/var/backups/tako/%s", backupID)

	u.log("Creating backup directory: %s", backupDir)

	// Create backup directory
	_, err := u.client.Execute(fmt.Sprintf("mkdir -p %s", backupDir))
	if err != nil {
		return "", err
	}

	// Files to backup
	filesToBackup := []string{
		"/etc/tako/version.json",
		"/etc/traefik/traefik.yml",
		"/etc/traefik/dynamic.yml",
		"/etc/nginx/nginx.conf",
		"/etc/logrotate.d/docker",
	}

	for _, file := range filesToBackup {
		_, err := u.client.Execute(fmt.Sprintf("test -f %s && cp -p %s %s/ || true", file, file, backupDir))
		if err != nil {
			u.log("Warning: Failed to backup %s: %v", file, err)
		}
	}

	// Backup docker-compose files
	_, _ = u.client.Execute(fmt.Sprintf("find /opt/tako -name 'docker-compose.yml' -exec cp --parents -p {} %s \\; 2>/dev/null || true", backupDir))

	return backupID, nil
}

// restoreBackup restores files from a backup
func (u *Upgrader) restoreBackup(backupID string) error {
	backupDir := fmt.Sprintf("/var/backups/tako/%s", backupID)
	u.log("Restoring from backup: %s", backupDir)

	// Restore all files from backup
	cmd := fmt.Sprintf("cd %s && find . -type f -exec cp --parents {} / \\;", backupDir)
	_, err := u.client.Execute(cmd)
	return err
}

// log logs a message if logger is available
func (u *Upgrader) log(format string, args ...interface{}) {
	if u.logger != nil {
		u.logger.Log(format, args...)
	}
}

// detectCurrentFeatures detects currently installed features
func detectCurrentFeatures(client *ssh.Client) []string {
	features := []string{}

	// Check Docker Swarm
	output, _ := client.Execute("docker info --format '{{.Swarm.LocalNodeState}}' 2>/dev/null")
	if output == "active\n" {
		features = append(features, "docker-swarm")
	}

	// Check Traefik
	_, err := client.Execute("docker ps --filter name=traefik --format '{{.Names}}' | grep traefik")
	if err == nil {
		features = append(features, "traefik-proxy")
	}

	// Check if log rotation is installed
	_, err = client.Execute("test -f /etc/logrotate.d/docker")
	if err == nil {
		features = append(features, "log-rotation")
	}

	// Check if monitoring is installed
	_, err = client.Execute("docker ps --filter name=node_exporter --format '{{.Names}}' | grep node_exporter")
	if err == nil {
		features = append(features, "metrics-collection")
	}

	return features
}

// ========================================
// Migration Scripts
// ========================================

// upgrade_1_0_to_1_1 returns upgrade steps from v1.0.0 to v1.1.0
func upgrade_1_0_to_1_1() []UpgradeStep {
	return []UpgradeStep{
		{
			Version:     "1.1.0",
			Description: "Install log rotation for Docker containers",
			Execute:     installLogRotation,
			Rollback:    removeLogRotation,
			Validate:    validateLogRotation,
		},
	}
}

// upgrade_1_1_to_1_2 returns upgrade steps from v1.1.0 to v1.2.0
func upgrade_1_1_to_1_2() []UpgradeStep {
	return []UpgradeStep{
		{
			Version:     "1.2.0",
			Description: "Install Prometheus node exporter for metrics",
			Execute:     installMonitoring,
			Rollback:    removeMonitoring,
			Validate:    validateMonitoring,
		},
	}
}

// ========================================
// Migration: Log Rotation (v1.1.0)
// ========================================

func installLogRotation(client *ssh.Client) error {
	script := `#!/bin/bash
set -e

echo "Installing log rotation..."

# Install logrotate if not present
if ! command -v logrotate &> /dev/null; then
    apt-get update -qq
    apt-get install -y logrotate
fi

# Create logrotate config for Docker
cat > /etc/logrotate.d/docker <<'EOF'
/var/lib/docker/containers/*/*.log {
    daily
    rotate 7
    compress
    delaycompress
    missingok
    notifempty
    copytruncate
    maxsize 100M
}
EOF

# Set journal size limits
mkdir -p /etc/systemd/journald.conf.d
cat > /etc/systemd/journald.conf.d/00-journal-size.conf <<'EOF'
[Journal]
SystemMaxUse=500M
SystemMaxFileSize=50M
EOF

# Restart journald
systemctl restart systemd-journald

# Add cleanup cron
cat > /etc/cron.daily/tako-log-cleanup <<'EOF'
#!/bin/bash
# Clean logs older than 30 days
find /var/log -type f -name "*.log" -mtime +30 -delete 2>/dev/null || true
find /var/lib/docker/containers -name "*.log" -mtime +30 -delete 2>/dev/null || true
EOF
chmod +x /etc/cron.daily/tako-log-cleanup

echo "Log rotation installed successfully"
`

	_, err := client.Execute(script)
	return err
}

func removeLogRotation(client *ssh.Client) error {
	script := `#!/bin/bash
rm -f /etc/logrotate.d/docker
rm -f /etc/systemd/journald.conf.d/00-journal-size.conf
rm -f /etc/cron.daily/tako-log-cleanup
systemctl restart systemd-journald
`
	_, err := client.Execute(script)
	return err
}

func validateLogRotation(client *ssh.Client) error {
	_, err := client.Execute("test -f /etc/logrotate.d/docker")
	if err != nil {
		return fmt.Errorf("logrotate config not found")
	}
	return nil
}

// ========================================
// Migration: Monitoring (v1.2.0)
// ========================================

func installMonitoring(client *ssh.Client) error {
	script := `#!/bin/bash
set -e

echo "Installing monitoring..."

# Check if already running
if docker ps --filter name=node_exporter --format '{{.Names}}' | grep -q node_exporter; then
    echo "Node exporter already running, skipping..."
    exit 0
fi

# Create monitoring network if it doesn't exist
docker network ls | grep -q tako-monitoring || docker network create tako-monitoring

# Start Prometheus node exporter
docker run -d \
    --name node_exporter \
    --network tako-monitoring \
    --restart unless-stopped \
    -p 9100:9100 \
    -v /proc:/host/proc:ro \
    -v /sys:/host/sys:ro \
    -v /:/rootfs:ro \
    prom/node-exporter:latest \
    --path.procfs=/host/proc \
    --path.sysfs=/host/sys \
    --collector.filesystem.mount-points-exclude="^/(sys|proc|dev|host|etc)($$|/)"

echo "Monitoring installed successfully"
echo "Node exporter accessible at: http://localhost:9100/metrics"
`

	_, err := client.Execute(script)
	return err
}

func removeMonitoring(client *ssh.Client) error {
	script := `#!/bin/bash
docker stop node_exporter 2>/dev/null || true
docker rm node_exporter 2>/dev/null || true
`
	_, err := client.Execute(script)
	return err
}

func validateMonitoring(client *ssh.Client) error {
	output, err := client.Execute("docker ps --filter name=node_exporter --format '{{.Names}}'")
	if err != nil || output != "node_exporter\n" {
		return fmt.Errorf("node exporter not running")
	}
	return nil
}
