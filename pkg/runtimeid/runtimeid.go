package runtimeid

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

const (
	ServiceIdentityLabel = "tako.runtimeId"

	dockerNameMax      = 128
	dockerNetworkMax   = 63
	proxyConfigNameMax = 120
	routerNameMax      = 96
)

func ServiceIdentity(project string, environment string, service string) string {
	return shortHash("service", project, environment, service)
}

func ContainerName(project string, environment string, service string, slot int) string {
	slotText := strconv.Itoa(slot)
	return compactName("_", dockerNameMax, []string{"tako", project, environment, service, slotText}, shortHash(project, environment, service, slotText))
}

func NetworkName(project string, environment string) string {
	return compactName("_", dockerNetworkMax, []string{"tako", project, environment}, shortHash(project, environment))
}

func NetworkProjectPrefix(project string) string {
	return strings.Join([]string{"tako", sanitizePart(project, "_")}, "_") + "_"
}

func VolumeName(project string, environment string, volume string) string {
	return compactName("_", dockerNameMax, []string{"tako", project, environment, volume}, shortHash(project, environment, volume))
}

func ProxyConfigFileName(project string, environment string) string {
	return compactName("-", proxyConfigNameMax-len(".yml"), []string{"tako", project, environment}, shortHash(project, environment)) + ".yml"
}

func MaintenanceProxyConfigFileName(project string, environment string, service string) string {
	return compactName("-", proxyConfigNameMax-len(".yml"), []string{"tako", project, environment, service, "maintenance"}, shortHash(project, environment, service, "maintenance")) + ".yml"
}

func RouterName(project string, environment string, service string) string {
	return compactName("-", routerNameMax, []string{"tako", project, environment, service}, shortHash(project, environment, service))
}

func compactName(separator string, maxLen int, parts []string, suffix string) string {
	sanitized := make([]string, 0, len(parts)+1)
	for _, part := range parts {
		part = sanitizePart(part, separator)
		if part != "" {
			sanitized = append(sanitized, part)
		}
	}
	if suffix == "" {
		suffix = shortHash(strings.Join(parts, "\x00"))
	}
	suffix = sanitizePart(suffix, separator)
	if suffix == "" {
		suffix = "id"
	}
	sanitized = append(sanitized, suffix)

	name := strings.Join(sanitized, separator)
	if len(name) <= maxLen {
		return name
	}

	requiredSuffix := separator + suffix
	prefixBudget := maxLen - len(requiredSuffix)
	if prefixBudget <= 0 {
		if len(suffix) <= maxLen {
			return suffix
		}
		return suffix[:maxLen]
	}

	prefix := strings.Join(sanitized[:len(sanitized)-1], separator)
	if len(prefix) > prefixBudget {
		prefix = prefix[:prefixBudget]
	}
	prefix = strings.Trim(prefix, separator)
	if prefix == "" {
		return suffix
	}
	return prefix + requiredSuffix
}

func sanitizePart(value string, separator string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	var out strings.Builder
	lastWasSeparator := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out.WriteRune(r)
			lastWasSeparator = false
			continue
		}
		if !lastWasSeparator {
			out.WriteString(separator)
			lastWasSeparator = true
		}
	}
	return strings.Trim(out.String(), separator)
}

func shortHash(parts ...string) string {
	hash := sha256.New()
	for index, part := range parts {
		if index > 0 {
			_, _ = hash.Write([]byte{0})
		}
		_, _ = hash.Write([]byte(part))
	}
	return hex.EncodeToString(hash.Sum(nil))[:10]
}

func LegacyProxyConfigFileName(project string, environment string) string {
	return legacyFileName(project + "-" + environment)
}

func LegacyMaintenanceProxyConfigFileName(project string, environment string, service string) string {
	return legacyFileName(fmt.Sprintf("%s-%s-%s-maintenance", project, environment, service))
}

func legacyFileName(value string) string {
	var out strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			out.WriteRune(r)
		} else {
			out.WriteRune('-')
		}
	}
	name := strings.Trim(out.String(), "-")
	if name == "" {
		name = "tako"
	}
	return name + ".yml"
}
