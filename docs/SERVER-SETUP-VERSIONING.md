# Server Setup Versioning & Upgrade System

## Overview

Tako CLI needs to gracefully handle server setup updates as new features are added. This system provides:

1. **Version Detection** - Identify what version of setup is installed on each server
2. **Upgrade Path** - Incrementally upgrade from any version to the latest
3. **Backward Compatibility** - Old setups continue working while showing upgrade prompts
4. **Rollback Support** - Revert failed upgrades automatically
5. **Zero Downtime** - Upgrades don't affect running services

## Version Manifest

### Location
`/etc/tako/version.json` - Single source of truth for server setup version

### Format
```json
{
  "version": "1.2.0",
  "installed_at": "2024-01-15T10:30:00Z",
  "last_upgrade": "2024-02-10T14:20:00Z",
  "components": {
    "docker": "24.0.7",
    "traefik": "2.10",
    "nginx": "1.24",
    "fail2ban": "1.0.2"
  },
  "features": [
    "docker-swarm",
    "traefik-proxy",
    "ssl-auto-renew",
    "log-rotation",
    "metrics-collection"
  ],
  "tako_cli_version": "0.3.0"
}
```

## Version History

### v1.0.0 (Initial Release)
**Features:**
- Basic Docker installation
- Traefik reverse proxy
- Nginx for static files
- Basic firewall rules

**Detection:**
- File exists: `/etc/tako/setup.complete`
- No version file exists

### v1.1.0 (Log Rotation)
**Added:**
- Logrotate configuration for Docker
- Journal log limits
- Old log cleanup cron

**Upgrade Steps:**
1. Install logrotate config
2. Set journal limits
3. Add cleanup cron job

### v1.2.0 (Monitoring & Metrics)
**Added:**
- Docker metrics collection
- Prometheus node exporter
- Grafana agent (optional)
- Health check endpoints

**Upgrade Steps:**
1. Install node exporter
2. Configure metrics collection
3. Set up health endpoints
4. Restart Traefik with new config

### v2.0.0 (Swarm Improvements)
**Added:**
- Swarm overlay network optimization
- Service mesh features
- Auto-scaling hooks
- Enhanced security policies

**Breaking Changes:**
- New network topology (requires service restart)
- Updated Traefik labels

**Upgrade Steps:**
1. Backup current configs
2. Create new overlay networks
3. Update Traefik config
4. Migrate services gradually
5. Remove old networks

## Upgrade Architecture

### 1. Version Detection

```go
// pkg/setup/version.go
type ServerVersion struct {
    Version       string                 `json:"version"`
    InstalledAt   time.Time             `json:"installed_at"`
    LastUpgrade   time.Time             `json:"last_upgrade"`
    Components    map[string]string      `json:"components"`
    Features      []string              `json:"features"`
    TakoCLIVersion string               `json:"tako_cli_version"`
}

func DetectServerVersion(client *ssh.Client) (*ServerVersion, error) {
    // Try to read version file
    output, err := client.Execute("cat /etc/tako/version.json")
    if err != nil {
        // No version file - check for legacy setup
        if hasLegacySetup(client) {
            return &ServerVersion{
                Version: "1.0.0",
                Features: detectLegacyFeatures(client),
            }, nil
        }
        return nil, ErrNotSetup
    }
    
    var version ServerVersion
    if err := json.Unmarshal([]byte(output), &version); err != nil {
        return nil, err
    }
    
    return &version, nil
}
```

### 2. Upgrade Path Planning

```go
// pkg/setup/upgrader.go
type UpgradePath struct {
    From       string
    To         string
    Steps      []UpgradeStep
    RequiresRestart bool
    BreakingChanges []string
}

type UpgradeStep struct {
    Version     string
    Description string
    Execute     func(*ssh.Client) error
    Rollback    func(*ssh.Client) error
    Validate    func(*ssh.Client) error
}

var upgradePaths = map[string][]UpgradeStep{
    "1.0.0->1.1.0": {
        {
            Version: "1.1.0",
            Description: "Install log rotation",
            Execute: installLogRotation,
            Rollback: removeLogRotation,
            Validate: validateLogRotation,
        },
    },
    "1.1.0->1.2.0": {
        {
            Version: "1.2.0",
            Description: "Install monitoring",
            Execute: installMonitoring,
            Rollback: removeMonitoring,
            Validate: validateMonitoring,
        },
    },
    // ... more paths
}

func PlanUpgrade(from, to string) (*UpgradePath, error) {
    // Find shortest path from -> to
    // Return ordered steps
}
```

### 3. Upgrade Execution

```go
func (u *Upgrader) Execute(client *ssh.Client, path *UpgradePath) error {
    // Create backup
    backupID := createBackup(client)
    defer func() {
        if err := recover(); err != nil {
            rollbackToBackup(client, backupID)
        }
    }()
    
    // Execute each step
    for i, step := range path.Steps {
        log.Info("Executing step %d/%d: %s", i+1, len(path.Steps), step.Description)
        
        if err := step.Execute(client); err != nil {
            log.Error("Step failed: %v", err)
            
            // Rollback previous steps in reverse order
            for j := i - 1; j >= 0; j-- {
                path.Steps[j].Rollback(client)
            }
            
            return fmt.Errorf("upgrade failed at step %d: %w", i+1, err)
        }
        
        // Validate step
        if step.Validate != nil {
            if err := step.Validate(client); err != nil {
                return fmt.Errorf("validation failed: %w", err)
            }
        }
    }
    
    // Update version file
    updateVersionFile(client, path.To)
    
    return nil
}
```

### 4. Backup & Rollback

```go
type Backup struct {
    ID        string
    Timestamp time.Time
    Version   string
    Files     map[string][]byte // path -> content
}

func createBackup(client *ssh.Client) string {
    backupID := fmt.Sprintf("backup-%s", time.Now().Format("20060102-150405"))
    
    // Backup critical files
    filesToBackup := []string{
        "/etc/tako/version.json",
        "/etc/traefik/traefik.yml",
        "/etc/traefik/dynamic.yml",
        "/etc/nginx/nginx.conf",
        // ... more
    }
    
    for _, file := range filesToBackup {
        content, _ := client.Execute(fmt.Sprintf("cat %s", file))
        // Store in /var/backups/tako/{backupID}/
        client.Execute(fmt.Sprintf("mkdir -p /var/backups/tako/%s", backupID))
        client.Execute(fmt.Sprintf("echo '%s' > /var/backups/tako/%s/%s", 
            content, backupID, filepath.Base(file)))
    }
    
    return backupID
}
```



## CLI Commands

### Check Version
```bash
tako setup version
# Output:
# Server: production-1
# Version: v1.2.0 (latest)
# Installed: 2024-01-15 10:30:00
# Features: docker-swarm, traefik-proxy, ssl-auto-renew, log-rotation, metrics-collection
```

### Upgrade Server
```bash
# Upgrade to latest
tako setup upgrade production-1

# Upgrade to specific version
tako setup upgrade production-1 --to=1.2.0

# Dry run (show what would happen)
tako setup upgrade production-1 --dry-run

# All servers at once
tako setup upgrade --all
```

### Rollback
```bash
tako setup rollback production-1

# Rollback to specific backup
tako setup rollback production-1 --backup=backup-20240215-143052
```

## Migration Scripts

### Example: v1.0.0 → v1.1.0 (Log Rotation)

```bash
#!/bin/bash
# migrations/1.0.0-to-1.1.0.sh

set -e

echo "Upgrading to v1.1.0: Adding log rotation..."

# Install logrotate
if ! command -v logrotate &> /dev/null; then
    apt-get update
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
find /var/log -type f -name "*.log" -mtime +30 -delete
find /var/lib/docker/containers -name "*.log" -mtime +30 -delete
EOF
chmod +x /etc/cron.daily/tako-log-cleanup

echo "Log rotation installed successfully"
```

## Configuration

### Enable/Disable Auto-Upgrade Checks

```yaml
# tako.yaml
setup:
  auto_check_updates: true  # Check for updates on health check
  auto_upgrade: false       # Don't auto-upgrade (prompt user)
  upgrade_strategy: "conservative"  # conservative, balanced, aggressive
  
  # Maintenance window for upgrades
  maintenance_window:
    enabled: true
    days: ["saturday", "sunday"]
    start_time: "02:00"
    end_time: "06:00"
```

## Upgrade Strategies

### Conservative (Default)
- Never auto-upgrade
- Show warnings for outdated servers
- Require manual approval

### Balanced
- Auto-upgrade patch versions (1.2.0 → 1.2.1)
- Prompt for minor versions (1.2.0 → 1.3.0)
- Require approval for major versions

### Aggressive
- Auto-upgrade all versions during maintenance window
- Create backups before each upgrade
- Auto-rollback on failure

## Safety Checks

Before any upgrade:
1. ✅ Server is accessible via SSH
2. ✅ No deployments in progress
3. ✅ Sufficient disk space (> 1GB free)
4. ✅ Backup created successfully
5. ✅ All services are healthy
6. ✅ Not during peak hours (configurable)

## Error Handling

### Upgrade Failures
- Automatic rollback to previous version
- Detailed error logging
- Notification to user
- Retry option with debug mode

### Network Issues
- Graceful timeout
- Partial progress saved
- Resume capability

### Breaking Changes
- Extra confirmation required
- Display what will break
- Migration guide shown
- Backup verified before proceeding

## Testing Upgrades

```bash
# Test upgrade in staging first
tako setup upgrade staging-1 --to=1.2.0

# If successful, upgrade production
tako setup upgrade production-1 --to=1.2.0 --skip-backup  # backup already tested
```

## Implementation Checklist

- [ ] Create version manifest structure
- [ ] Implement version detection
- [ ] Build upgrade path planner
- [ ] Create migration scripts for each version
- [ ] Add backup/rollback system

- [ ] Add CLI commands (upgrade, rollback, version)
- [ ] Write tests for each migration
- [ ] Create documentation
- [ ] Add telemetry for upgrade success/failure rates

## Future Enhancements

1. **Delta Upgrades** - Only transfer changed files
2. **Parallel Upgrades** - Upgrade multiple servers simultaneously
3. **Canary Upgrades** - Test on subset first
4. **A/B Testing** - Run old and new versions side-by-side
5. **Remote Monitoring** - Track upgrade progress from dashboard
6. **Scheduled Upgrades** - Cron-based automatic upgrades
7. **Compliance Reports** - Track which servers are up-to-date

This system ensures Tako CLI can evolve gracefully while maintaining backward compatibility and zero-downtime upgrades! 🚀
