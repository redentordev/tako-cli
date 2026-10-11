package secrets

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/redentordev/tako-cli/pkg/config"
)

// ShouldRedactEnvValue reports whether a plain service `env:` value should be
// registered with the redactor. Values under credential-like names always
// are. Other values only when they look like a credential: a URL with
// embedded userinfo or a token-like path segment or query value (webhook
// URLs), or a long single token mixing letters and digits.
// Ordinary settings such as `CMS_RUNTIME_MODE: site` or `NODE_ENV:
// production` would otherwise mask every occurrence of that word, including
// inside service names like `site-renderer`. Values under `secrets:` and
// `secretSources` are registered separately and are always redacted.
func ShouldRedactEnvValue(name, value string) bool {
	if value == "" {
		return false
	}
	if config.IsCredentialEnvName(name) {
		return true
	}
	if scheme, rest, ok := strings.Cut(value, "://"); ok && scheme != "" {
		host, path, _ := strings.Cut(rest, "/")
		if strings.Contains(host, "@") {
			return true
		}
		for _, part := range strings.FieldsFunc(path, func(r rune) bool {
			return r == '/' || r == '?' || r == '&' || r == '=' || r == '#'
		}) {
			if looksLikeCredential(part) {
				return true
			}
		}
		return false
	}
	return looksLikeCredential(value)
}

func looksLikeCredential(value string) bool {
	if len(value) < 16 {
		return false
	}
	var letters, digits bool
	for _, r := range value {
		switch {
		case unicode.IsSpace(r):
			return false
		case unicode.IsLetter(r):
			letters = true
		case unicode.IsDigit(r):
			digits = true
		}
	}
	return letters && digits
}

// Redactor automatically redacts sensitive information from output
type Redactor struct {
	mu        sync.RWMutex
	secrets   map[string]string // original -> redacted
	public    map[string]struct{}
	patterns  []*regexp.Regexp
	minLength int
}

// NewRedactor creates a new redactor
func NewRedactor() *Redactor {
	return &Redactor{
		secrets:   make(map[string]string),
		public:    make(map[string]struct{}),
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
		`dop_v1_[a-f0-9]{64}`,    // DigitalOcean personal access token (new format)
		`[a-f0-9]{64}`,           // Hetzner/Linode tokens (64 char hex)

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

// RegisterPublic marks a value that is known not to be secret, such as a
// project, service or environment name, an image reference, or a git commit.
// Occurrences of a public value are left intact even when a registered secret
// is a substring of it or a pattern would otherwise match it.
func (r *Redactor) RegisterPublic(value string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(value) < r.minLength {
		return
	}
	r.public[value] = struct{}{}
}

// publicPlaceholder marks a protected public value while redaction runs. NUL
// bytes never occur in registered secrets or pattern matches.
func publicPlaceholder(index int) string {
	return fmt.Sprintf("\x00%d\x00", index)
}

// Redact removes sensitive information from text
func (r *Redactor) Redact(input string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	output, protected := r.protectPublic(input)

	// First, redact registered secret values
	for secret, redacted := range r.secrets {
		output = strings.ReplaceAll(output, secret, redacted)
	}

	// Then apply pattern-based redaction
	for _, pattern := range r.patterns {
		output = redactPatternMatches(pattern, output)
	}

	for index, value := range protected {
		output = strings.ReplaceAll(output, publicPlaceholder(index), value)
	}
	return output
}

// protectPublic swaps public values present in input for placeholders,
// longest first so a longer public value wins over one it contains.
func (r *Redactor) protectPublic(input string) (string, []string) {
	if len(r.public) == 0 {
		return input, nil
	}
	var present []string
	for value := range r.public {
		if _, isSecret := r.secrets[value]; isSecret {
			continue
		}
		if strings.Contains(input, value) {
			present = append(present, value)
		}
	}
	if len(present) == 0 {
		return input, nil
	}
	sort.Slice(present, func(i, j int) bool {
		if len(present[i]) != len(present[j]) {
			return len(present[i]) > len(present[j])
		}
		return present[i] < present[j]
	})
	output := input
	for index, value := range present {
		output = strings.ReplaceAll(output, value, publicPlaceholder(index))
	}
	return output, present
}

// redactPatternMatches applies one pattern, leaving hex image tags and
// digests (`repo:<hex>`, `sha256:<hex>`) alone: those are commit SHAs and
// content hashes, not credentials. Key/value patterns run first, so a hex
// secret written as `token: <hex>` is still redacted.
func redactPatternMatches(pattern *regexp.Regexp, input string) string {
	matches := pattern.FindAllStringIndex(input, -1)
	if len(matches) == 0 {
		return input
	}
	var b strings.Builder
	last := 0
	for _, match := range matches {
		start, end := match[0], match[1]
		b.WriteString(input[last:start])
		value := input[start:end]
		if isHexReference(input, start, value) {
			b.WriteString(value)
		} else {
			b.WriteString(redactMatch(value))
		}
		last = end
	}
	b.WriteString(input[last:])
	return b.String()
}

func isHexReference(input string, start int, value string) bool {
	if start == 0 || input[start-1] != ':' || !isLowerHex(value) {
		return false
	}
	return len(value) == 40 || len(value) == 64
}

func isLowerHex(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return value != ""
}

// redactMatch redacts a regex match
func redactMatch(match string) string {
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
