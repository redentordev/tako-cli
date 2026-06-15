package envexpand

import (
	"os"
	"sort"
	"strings"
)

// Lookup resolves an environment variable name.
type Lookup func(string) (string, bool)

// Braced expands ${VAR} placeholders using lookup. Bare $VAR sequences are
// preserved so values such as bcrypt hashes are not corrupted.
func Braced(value string, lookup Lookup) (string, []string) {
	var result strings.Builder
	missing := make([]string, 0)
	seenMissing := map[string]bool{}

	for i := 0; i < len(value); {
		if value[i] != '$' || i+1 >= len(value) || value[i+1] != '{' {
			result.WriteByte(value[i])
			i++
			continue
		}

		end := strings.IndexByte(value[i+2:], '}')
		if end < 0 {
			result.WriteByte(value[i])
			i++
			continue
		}

		key := value[i+2 : i+2+end]
		if resolved, ok := lookup(key); ok {
			result.WriteString(resolved)
		} else if !seenMissing[key] {
			seenMissing[key] = true
			missing = append(missing, key)
		}
		i += end + 3
	}

	sort.Strings(missing)
	return result.String(), missing
}

// BracedFromOS expands ${VAR} placeholders from process environment variables.
func BracedFromOS(value string) (string, []string) {
	return Braced(value, func(key string) (string, bool) {
		value, ok := os.LookupEnv(key)
		if !ok {
			return "", false
		}
		return strings.TrimSpace(value), true
	})
}
