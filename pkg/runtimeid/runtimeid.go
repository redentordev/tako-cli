package runtimeid

import (
	"crypto/sha256"
	"encoding/hex"
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

func ContainerAlias(project string, environment string, service string, slot int) string {
	slotText := strconv.Itoa(slot)
	return compactName("-", dockerNetworkMax, []string{"tako", project, environment, service, slotText}, shortHash(project, environment, service, slotText))
}

func ExportNetworkName(project string, environment string, service string) string {
	return compactName("_", dockerNetworkMax, []string{"tako", project, environment, service, "export"}, shortHash(project, environment, service, "export"))
}

func ExportAlias(project string, environment string, service string) string {
	return readableName("-", dockerNetworkMax, []string{project, environment, service}, shortHash(project, environment, service, "export-alias"))
}

func NetworkName(project string, environment string) string {
	return compactName("_", dockerNetworkMax, []string{"tako", project, environment}, shortHash(project, environment))
}

func NetworkProjectPrefix(project string) string {
	return strings.Join([]string{"tako", sanitizePart(project, "_")}, "_") + "_"
}

func NetworkEnvironmentPrefix(project string, environment string) string {
	return strings.Join([]string{"tako", sanitizePart(project, "_"), sanitizePart(environment, "_")}, "_") + "_"
}

func VolumeName(project string, environment string, volume string) string {
	return compactName("_", dockerNameMax, []string{"tako", project, environment, volume}, shortHash(project, environment, volume))
}

func VolumeProjectPrefix(project string) string {
	return strings.Join([]string{"tako", sanitizePart(project, "_")}, "_") + "_"
}

func VolumeEnvironmentPrefix(project string, environment string) string {
	return strings.Join([]string{"tako", sanitizePart(project, "_"), sanitizePart(environment, "_")}, "_") + "_"
}

func ProxyConfigFileName(project string, environment string) string {
	return compactName("-", proxyConfigNameMax-len(".json"), []string{"tako", project, environment}, shortHash(project, environment)) + ".json"
}

func MaintenanceProxyConfigFileName(project string, environment string, service string) string {
	return compactName("-", proxyConfigNameMax-len(".json"), []string{"tako", project, environment, service, "maintenance"}, shortHash(project, environment, service, "maintenance")) + ".json"
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

func readableName(separator string, maxLen int, parts []string, suffix string) string {
	sanitized := make([]string, 0, len(parts))
	for _, part := range parts {
		part = sanitizePart(part, separator)
		if part != "" {
			sanitized = append(sanitized, part)
		}
	}
	name := strings.Join(sanitized, separator)
	name = strings.Trim(name, separator)
	if name == "" {
		name = sanitizePart(suffix, separator)
	}
	if name == "" {
		name = "id"
	}
	if len(name) <= maxLen {
		return name
	}
	suffix = sanitizePart(suffix, separator)
	if suffix == "" {
		suffix = shortHash(strings.Join(parts, "\x00"))
	}
	requiredSuffix := separator + suffix
	prefixBudget := maxLen - len(requiredSuffix)
	if prefixBudget <= 0 {
		if len(suffix) <= maxLen {
			return suffix
		}
		return suffix[:maxLen]
	}
	prefix := strings.Trim(name[:prefixBudget], separator)
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
