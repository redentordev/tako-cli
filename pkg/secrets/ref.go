package secrets

import "strings"

// SecretRefKey returns the secret key a service `secrets` entry resolves.
// Refs may alias a secret into a different container variable with the
// `CONTAINER_VAR:SECRET_KEY` form; the key is everything after the first
// colon. A ref without a colon names the key directly.
func SecretRefKey(ref string) string {
	if _, key, ok := strings.Cut(ref, ":"); ok {
		return key
	}
	return ref
}
