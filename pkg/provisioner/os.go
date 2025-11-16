package provisioner

import (
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/ssh"
)

// OSFamily represents different Linux distribution families
type OSFamily string

const (
	OSFamilyDebian  OSFamily = "debian"
	OSFamilyRHEL    OSFamily = "rhel"
	OSFamilySUSE    OSFamily = "suse"
	OSFamilyAlpine  OSFamily = "alpine"
	OSFamilyUnknown OSFamily = "unknown"
)

// OSInfo contains detected operating system information
type OSInfo struct {
	Family         OSFamily
	Name           string
	Version        string
	PackageManager string
}

// DetectOS detects the operating system of the remote server
func DetectOS(client *ssh.Client) (*OSInfo, error) {
	// Read /etc/os-release file
	output, err := client.Execute("cat /etc/os-release 2>/dev/null || cat /etc/lsb-release 2>/dev/null")
	if err != nil {
		return nil, fmt.Errorf("failed to read OS information: %w", err)
	}

	info := parseOSRelease(output)

	// Detect package manager
	info.PackageManager = detectPackageManager(client, info.Family)

	return info, nil
}

// parseOSRelease parses the /etc/os-release content
func parseOSRelease(content string) *OSInfo {
	info := &OSInfo{
		Family: OSFamilyUnknown,
	}

	lines := strings.Split(content, "\n")
	idLike := ""

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ID=") {
			info.Name = strings.Trim(strings.TrimPrefix(line, "ID="), `"`)
		} else if strings.HasPrefix(line, "VERSION_ID=") {
			info.Version = strings.Trim(strings.TrimPrefix(line, "VERSION_ID="), `"`)
		} else if strings.HasPrefix(line, "ID_LIKE=") {
			idLike = strings.Trim(strings.TrimPrefix(line, "ID_LIKE="), `"`)
		}
	}

	// Determine OS family
	name := strings.ToLower(info.Name)
	idLikeLower := strings.ToLower(idLike)

	if strings.Contains(name, "debian") || strings.Contains(name, "ubuntu") ||
		strings.Contains(idLikeLower, "debian") || strings.Contains(idLikeLower, "ubuntu") {
		info.Family = OSFamilyDebian
	} else if strings.Contains(name, "rhel") || strings.Contains(name, "centos") ||
		strings.Contains(name, "fedora") || strings.Contains(name, "rocky") ||
		strings.Contains(idLikeLower, "rhel") || strings.Contains(idLikeLower, "fedora") {
		info.Family = OSFamilyRHEL
	} else if strings.Contains(name, "suse") || strings.Contains(name, "opensuse") ||
		strings.Contains(idLikeLower, "suse") {
		info.Family = OSFamilySUSE
	} else if strings.Contains(name, "alpine") {
		info.Family = OSFamilyAlpine
	}

	return info
}

// detectPackageManager detects the package manager available
func detectPackageManager(client *ssh.Client, family OSFamily) string {
	switch family {
	case OSFamilyDebian:
		if _, err := client.Execute("command -v apt-get"); err == nil {
			return "apt"
		}
	case OSFamilyRHEL:
		if _, err := client.Execute("command -v dnf"); err == nil {
			return "dnf"
		}
		if _, err := client.Execute("command -v yum"); err == nil {
			return "yum"
		}
	case OSFamilySUSE:
		if _, err := client.Execute("command -v zypper"); err == nil {
			return "zypper"
		}
	case OSFamilyAlpine:
		if _, err := client.Execute("command -v apk"); err == nil {
			return "apk"
		}
	}
	return "unknown"
}

// String returns a human-readable representation of the OS
func (info *OSInfo) String() string {
	return fmt.Sprintf("%s %s (%s, %s)", info.Name, info.Version, info.Family, info.PackageManager)
}

// IsSupported returns true if the OS is supported
func (info *OSInfo) IsSupported() bool {
	return info.Family != OSFamilyUnknown && info.PackageManager != "unknown"
}
