package secrets

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// Redactor automatically redacts sensitive information from output
type Redactor struct {
	mu        sync.RWMutex
	secrets   map[string]string // original -> redacted
	patterns  []*regexp.Regexp
	minLength int
}

// NewRedactor creates a new redactor
func NewRedactor() *Redactor {
	return &Redactor{
		secrets:   make(map[string]string),
		patterns:  compilePatterns(),
		minLength: 4, // Don't redact very short values
	}
}

// compilePatterns compiles regex patterns for sensitive data detection
func compilePatterns() []*regexp.Regexp {
	patternStrings := []string{
		// Environment variable assignments
		`(?i)(password|passwd|pwd|pass)[\s=:]+([^\s]+)`,
		`(?i)(secret|token|key|apikey|api_key|api-key)[\s=:]+([^\s]+)`,
		`(?i)(auth|authorization|bearer|credential)[\s=:]+([^\s]+)`,

		// Connection strings
		`(?i)mysql:\/\/[^:]+:([^@]+)@`,
		`(?i)postgres(ql)?:\/\/[^:]+:([^@]+)@`,
		`(?i)mongodb(\+srv)?:\/\/[^:]+:([^@]+)@`,
		`(?i)redis:\/\/[^:]+:([^@]+)@`,

		// API keys and tokens
		`sk_live_[A-Za-z0-9]+`,     // Stripe
		`pk_live_[A-Za-z0-9]+`,     // Stripe publishable
		`ghp_[A-Za-z0-9]{36}`,      // GitHub personal token
		`ghs_[A-Za-z0-9]{36}`,      // GitHub server token
		`github_pat_[A-Za-z0-9_]+`, // GitHub fine-grained PAT
		`npm_[A-Za-z0-9]{36}`,      // NPM
		`dop_[A-Za-z0-9]{32}`,      // Digital Ocean

		// Cloud provider patterns
		`AKIA[0-9A-Z]{16}`,       // AWS Access Key
		`AIza[0-9A-Za-z\-_]{35}`, // Google API Key

		// JWT tokens
		`eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`,

		// Base64 encoded secrets (40+ characters)
		`[A-Za-z0-9+/]{40,}={0,2}`,

		// Hex encoded secrets (32+ characters)
		`[a-f0-9]{32,}`,
	}

	patterns := make([]*regexp.Regexp, 0, len(patternStrings))
	for _, p := range patternStrings {
		if compiled, err := regexp.Compile(p); err == nil {
			patterns = append(patterns, compiled)
		}
	}

	return patterns
}

// Register marks a specific value for redaction
func (r *Redactor) Register(value string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Skip very short values
	if len(value) < r.minLength {
		return
	}

	// Create a redacted version
	redacted := r.createRedacted(value)
	r.secrets[value] = redacted
}

// createRedacted creates a redacted version of a secret
func (r *Redactor) createRedacted(value string) string {
	length := len(value)

	if length <= 4 {
		return "[REDACTED]"
	} else if length <= 8 {
		// Show first char only
		return fmt.Sprintf("%c***", value[0])
	} else {
		// Show first and last 2 chars
		return fmt.Sprintf("%s***%s", value[:2], value[length-2:])
	}
}

// Redact removes sensitive information from text
func (r *Redactor) Redact(input string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	output := input

	// First, redact registered secret values
	for secret, redacted := range r.secrets {
		output = strings.ReplaceAll(output, secret, redacted)
	}

	// Then apply pattern-based redaction
	for _, pattern := range r.patterns {
		output = pattern.ReplaceAllStringFunc(output, r.redactMatch)
	}

	return output
}

// redactMatch redacts a regex match
func (r *Redactor) redactMatch(match string) string {
	// Try to preserve the key part of key=value pairs
	if idx := strings.IndexAny(match, "=: "); idx > 0 {
		key := match[:idx+1]
		return key + "[REDACTED]"
	}
	return "[REDACTED]"
}

// RedactMap redacts sensitive values in a map
func (r *Redactor) RedactMap(data map[string]string) map[string]string {
	redacted := make(map[string]string, len(data))

	for key, value := range data {
		if r.IsSensitiveKey(key) {
			redacted[key] = "[REDACTED]"
		} else {
			redacted[key] = r.Redact(value)
		}
	}

	return redacted
}

// IsSensitiveKey checks if a key name indicates sensitive data
func (r *Redactor) IsSensitiveKey(key string) bool {
	sensitiveKeywords := []string{
		"PASSWORD", "PASSWD", "PWD",
		"SECRET", "TOKEN", "KEY",
		"API", "APIKEY", "API_KEY",
		"AUTH", "AUTHORIZATION", "BEARER",
		"CREDENTIAL", "CRED",
		"PRIVATE", "CERT", "CERTIFICATE",
		"SIGNING", "ENCRYPTION",
	}

	keyUpper := strings.ToUpper(key)
	for _, keyword := range sensitiveKeywords {
		if strings.Contains(keyUpper, keyword) {
			return true
		}
	}

	return false
}

// Clear removes all registered secrets
func (r *Redactor) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.secrets = make(map[string]string)
}
