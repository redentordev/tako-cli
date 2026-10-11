package secrets

import (
	"fmt"
	"strings"
	"unicode"
)

// MapProviderSecrets turns provider names (paths, item/field pairs, ...)
// into env-style secret keys. Explicit mappings win; every other name is
// derived by stripping prefixStrip and normalizing separators. Two provider
// names that land on the same key are an error so nothing is silently
// dropped.
func MapProviderSecrets(values map[string]string, prefixStrip string, mappings map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(values))
	sources := make(map[string]string, len(values))
	for source, value := range values {
		key, ok := mappings[source]
		if !ok {
			key = ProviderSecretKey(source, prefixStrip)
		}
		if !IsValidSecretKey(key) {
			return nil, fmt.Errorf("provider secret %q maps to invalid env key %q", source, key)
		}
		if previous, exists := sources[key]; exists {
			return nil, fmt.Errorf("provider secrets %q and %q both map to %s; use a mapping to disambiguate", previous, source, key)
		}
		sources[key] = source
		out[key] = value
	}
	return out, nil
}

// ProviderSecretKey derives an env-style key from a provider secret name:
// /app/prod/db-url with prefix /app/prod becomes DB_URL.
func ProviderSecretKey(source string, prefixStrip string) string {
	key := strings.TrimSpace(source)
	if prefixStrip != "" {
		prefix := strings.TrimSpace(prefixStrip)
		if strings.HasPrefix(key, prefix) {
			key = strings.TrimPrefix(key, prefix)
		}
		key = strings.TrimLeft(key, "/")
	}
	key = strings.Trim(key, "/")
	var out strings.Builder
	lastWasSeparator := false
	for _, r := range key {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out.WriteRune(unicode.ToUpper(r))
			lastWasSeparator = false
			continue
		}
		if !lastWasSeparator {
			out.WriteRune('_')
			lastWasSeparator = true
		}
	}
	result := strings.Trim(out.String(), "_")
	if result == "" {
		return "SECRET"
	}
	if result[0] >= '0' && result[0] <= '9' {
		result = "SECRET_" + result
	}
	return result
}

// IsValidSecretKey reports whether key is a usable env variable name.
func IsValidSecretKey(key string) bool {
	if key == "" {
		return false
	}
	first := rune(key[0])
	if !((first >= 'A' && first <= 'Z') || (first >= 'a' && first <= 'z') || first == '_') {
		return false
	}
	for _, r := range key {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			continue
		}
		return false
	}
	return true
}
