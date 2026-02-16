package utils

import (
	"fmt"
	"regexp"
	"strings"
)

// SanitizeName sanitizes a string for use in Docker/network names
// Replaces invalid characters with underscores
func SanitizeName(name string) string {
	// Docker names: alphanumeric, underscores, periods, hyphens
	sanitized := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			return r
		}
		return '_'
	}, name)
	return sanitized
}

// SanitizeDomainForLabel converts a domain to a safe label name
// Example: "app.example.com" → "app-example-com"
func SanitizeDomainForLabel(domain string) string {
	return strings.ReplaceAll(domain, ".", "-")
}

// TruncateString truncates a string to maxLength, adding "..." if truncated
func TruncateString(s string, maxLength int) string {
	if len(s) <= maxLength {
		return s
	}
	if maxLength <= 3 {
		return s[:maxLength]
	}
	return s[:maxLength-3] + "..."
}

// JoinNonEmpty joins non-empty strings with a separator
func JoinNonEmpty(sep string, parts ...string) string {
	var nonEmpty []string
	for _, part := range parts {
		if part != "" {
			nonEmpty = append(nonEmpty, part)
		}
	}
	return strings.Join(nonEmpty, sep)
}

// Quote wraps a string in single quotes if it contains spaces
func Quote(s string) string {
	if strings.Contains(s, " ") {
		return fmt.Sprintf("'%s'", s)
	}
	return s
}

// ContainerName generates a standardized container name
func ContainerName(project, environment, service string, replica int) string {
	if replica > 0 {
		return fmt.Sprintf("%s_%s_%s_%d", project, environment, service, replica)
	}
	return fmt.Sprintf("%s_%s_%s", project, environment, service)
}

// NetworkName generates a standardized network name
func NetworkName(project, environment string) string {
	return fmt.Sprintf("tako_%s_%s", project, environment)
}

// ImageTag generates a standardized image tag
func ImageTag(project, service, version, environment string) string {
	return fmt.Sprintf("%s/%s:%s-%s", project, service, version, environment)
}

// ShellQuote wraps a string in single quotes with proper escaping for safe
// use in shell commands. Single quotes within the string are handled by ending
// the quoted section, adding an escaped single quote, and resuming quoting.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// validUnixUsername matches POSIX username format:
// starts with letter or underscore, followed by letters, digits, underscores, or hyphens
var validUnixUsername = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_-]{0,31}$`)

// IsValidUnixUsername validates that a string is a safe POSIX username.
// Rules: starts with letter or underscore, contains only letters, digits,
// underscores, and hyphens, max 32 characters.
func IsValidUnixUsername(name string) bool {
	return validUnixUsername.MatchString(name)
}
