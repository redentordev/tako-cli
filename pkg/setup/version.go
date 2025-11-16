package setup

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/ssh"
)

// CurrentVersion is the latest setup version
const CurrentVersion = "1.2.0"

// ServerVersion represents the installed setup version on a server
type ServerVersion struct {
	Version        string            `json:"version"`
	InstalledAt    time.Time         `json:"installed_at"`
	LastUpgrade    time.Time         `json:"last_upgrade,omitempty"`
	Components     map[string]string `json:"components"`
	Features       []string          `json:"features"`
	TakoCLIVersion string            `json:"tako_cli_version"`
}

// VersionFile is the location of the version manifest on servers
const VersionFile = "/etc/tako/version.json"

// LegacySetupMarker is the old setup completion marker
const LegacySetupMarker = "/etc/tako/setup.complete"

// DetectServerVersion detects the setup version installed on a server
func DetectServerVersion(client *ssh.Client) (*ServerVersion, error) {
	// Try to read version file
	output, err := client.Execute(fmt.Sprintf("cat %s 2>/dev/null", VersionFile))
	if err != nil || strings.TrimSpace(output) == "" {
		// No version file - check for legacy setup
		return detectLegacySetup(client)
	}

	var version ServerVersion
	if err := json.Unmarshal([]byte(output), &version); err != nil {
		return nil, fmt.Errorf("failed to parse version file: %w", err)
	}

	return &version, nil
}

// detectLegacySetup detects if server has old setup without version file
func detectLegacySetup(client *ssh.Client) (*ServerVersion, error) {
	// Check for legacy setup marker
	_, err := client.Execute(fmt.Sprintf("test -f %s", LegacySetupMarker))
	if err != nil {
		// No setup at all
		return nil, ErrNotSetup
	}

	// Legacy setup detected - version 1.0.0
	features := detectLegacyFeatures(client)

	return &ServerVersion{
		Version:  "1.0.0",
		Features: features,
		Components: map[string]string{
			"docker":  detectDockerVersion(client),
			"traefik": detectTraefikVersion(client),
		},
	}, nil
}

// detectLegacyFeatures detects what features are installed
func detectLegacyFeatures(client *ssh.Client) []string {
	features := []string{}

	// Check Docker Swarm
	output, _ := client.Execute("docker info --format '{{.Swarm.LocalNodeState}}' 2>/dev/null")
	if strings.TrimSpace(output) == "active" {
		features = append(features, "docker-swarm")
	}

	// Check Traefik
	_, err := client.Execute("docker ps --filter name=traefik --format '{{.Names}}' | grep traefik")
	if err == nil {
		features = append(features, "traefik-proxy")
	}

	// Check Nginx
	_, err = client.Execute("which nginx")
	if err == nil {
		features = append(features, "nginx")
	}

	// Check fail2ban
	_, err = client.Execute("which fail2ban-client")
	if err == nil {
		features = append(features, "fail2ban")
	}

	return features
}

// detectDockerVersion gets the installed Docker version
func detectDockerVersion(client *ssh.Client) string {
	output, err := client.Execute("docker --version | awk '{print $3}' | tr -d ','")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(output)
}

// detectTraefikVersion gets the installed Traefik version
func detectTraefikVersion(client *ssh.Client) string {
	output, err := client.Execute("docker exec traefik traefik version 2>/dev/null | grep 'Version:' | awk '{print $2}'")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(output)
}

// IsUpgradeAvailable checks if an upgrade is available
func (v *ServerVersion) IsUpgradeAvailable() bool {
	return v.Version != CurrentVersion && compareVersions(v.Version, CurrentVersion) < 0
}

// IsOutdated checks if version is more than one major version behind
func (v *ServerVersion) IsOutdated() bool {
	current := parseVersion(CurrentVersion)
	installed := parseVersion(v.Version)

	// More than 1 major version behind
	return (current.Major - installed.Major) > 1
}

// NeedsUpgrade checks if upgrade is recommended
func (v *ServerVersion) NeedsUpgrade() bool {
	return v.IsUpgradeAvailable()
}

// versionParts represents semantic version parts
type versionParts struct {
	Major int
	Minor int
	Patch int
}

// parseVersion parses a semantic version string
func parseVersion(version string) versionParts {
	var major, minor, patch int
	fmt.Sscanf(version, "%d.%d.%d", &major, &minor, &patch)
	return versionParts{Major: major, Minor: minor, Patch: patch}
}

// compareVersions compares two version strings
// Returns: -1 if v1 < v2, 0 if v1 == v2, 1 if v1 > v2
func compareVersions(v1, v2 string) int {
	p1 := parseVersion(v1)
	p2 := parseVersion(v2)

	if p1.Major != p2.Major {
		if p1.Major < p2.Major {
			return -1
		}
		return 1
	}

	if p1.Minor != p2.Minor {
		if p1.Minor < p2.Minor {
			return -1
		}
		return 1
	}

	if p1.Patch != p2.Patch {
		if p1.Patch < p2.Patch {
			return -1
		}
		return 1
	}

	return 0
}

// WriteVersionFile writes the version manifest to the server
func WriteVersionFile(client *ssh.Client, version *ServerVersion) error {
	data, err := json.MarshalIndent(version, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal version: %w", err)
	}

	// Create directory if it doesn't exist
	if _, err := client.Execute("mkdir -p /etc/tako"); err != nil {
		return fmt.Errorf("failed to create tako directory: %w", err)
	}

	// Write version file
	cmd := fmt.Sprintf("cat > %s << 'EOF'\n%s\nEOF", VersionFile, string(data))
	if _, err := client.Execute(cmd); err != nil {
		return fmt.Errorf("failed to write version file: %w", err)
	}

	return nil
}

// ErrNotSetup indicates server is not set up
var ErrNotSetup = fmt.Errorf("server not set up")
