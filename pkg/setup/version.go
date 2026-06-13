package setup

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
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

// DetectServerVersion detects the setup version installed on a server
func DetectServerVersion(client *ssh.Client) (*ServerVersion, error) {
	// Try to read version file
	output, err := client.Execute(fmt.Sprintf("cat -- %s 2>/dev/null", shellQuote(VersionFile)))
	if err != nil || output == "" {
		return nil, ErrNotSetup
	}

	var version ServerVersion
	if err := json.Unmarshal([]byte(output), &version); err != nil {
		return nil, fmt.Errorf("failed to parse version file: %w", err)
	}

	return &version, nil
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

	if _, err := client.Execute(buildWriteVersionFileCommand(data)); err != nil {
		return fmt.Errorf("failed to write version file: %w", err)
	}

	return nil
}

func buildWriteVersionFileCommand(data []byte) string {
	encoded := base64.StdEncoding.EncodeToString(data)
	dir := filepath.Dir(VersionFile)
	return fmt.Sprintf(
		"tmp=$(mktemp) && trap 'rm -f \"$tmp\"' EXIT && printf %%s %s | base64 -d > \"$tmp\" && sudo install -d -m 0755 %s && sudo install -m 0644 \"$tmp\" %s",
		shellQuote(encoded),
		shellQuote(dir),
		shellQuote(VersionFile),
	)
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// ErrNotSetup indicates server is not set up
var ErrNotSetup = fmt.Errorf("server not set up")
