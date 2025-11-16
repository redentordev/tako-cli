package secrets

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// EnvFile represents a Docker environment file
type EnvFile struct {
	values    map[string]string
	id        string
	createdAt time.Time
}

// NewEnvFile creates a new environment file
func NewEnvFile() *EnvFile {
	// Generate unique ID for tracking
	b := make([]byte, 8)
	rand.Read(b)

	return &EnvFile{
		values:    make(map[string]string),
		id:        hex.EncodeToString(b),
		createdAt: time.Now(),
	}
}

// Set adds or updates an environment variable
func (ef *EnvFile) Set(key, value string) {
	ef.values[key] = value
}

// Get retrieves an environment variable value
func (ef *EnvFile) Get(key string) (string, bool) {
	val, exists := ef.values[key]
	return val, exists
}

// ToReader returns an io.Reader with the env file contents
func (ef *EnvFile) ToReader() io.Reader {
	var buf bytes.Buffer

	// Write header comments
	fmt.Fprintf(&buf, "# Tako Environment File\n")
	fmt.Fprintf(&buf, "# Generated: %s\n", ef.createdAt.Format(time.RFC3339))
	fmt.Fprintf(&buf, "# ID: %s\n", ef.id)
	fmt.Fprintf(&buf, "# This file is automatically deleted after deployment\n\n")

	// Sort keys for consistent output
	keys := make([]string, 0, len(ef.values))
	for k := range ef.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Write environment variables
	for _, key := range keys {
		value := ef.values[key]
		escaped := ef.escapeValue(value)
		fmt.Fprintf(&buf, "%s=%s\n", key, escaped)
	}

	return &buf
}

// escapeValue escapes a value for Docker env file format
func (ef *EnvFile) escapeValue(value string) string {
	// Docker env file format requires escaping certain characters
	var result strings.Builder

	for _, r := range value {
		switch r {
		case '"':
			result.WriteString(`\"`)
		case '\\':
			result.WriteString(`\\`)
		case '\n':
			result.WriteString(`\n`)
		case '\r':
			result.WriteString(`\r`)
		case '\t':
			result.WriteString(`\t`)
		case '$':
			// Escape $ to prevent variable expansion
			result.WriteString(`\$`)
		default:
			result.WriteRune(r)
		}
	}

	// If value contains spaces or special chars, wrap in quotes
	escaped := result.String()
	if strings.ContainsAny(value, " \t") && !strings.HasPrefix(escaped, "\"") {
		escaped = fmt.Sprintf("\"%s\"", escaped)
	}

	return escaped
}

// Size returns the approximate size of the env file in bytes
func (ef *EnvFile) Size() int {
	size := 0
	for key, value := range ef.values {
		size += len(key) + len(value) + 2 // key=value\n
	}
	return size
}

// Count returns the number of environment variables
func (ef *EnvFile) Count() int {
	return len(ef.values)
}

// Validate checks if the env file is valid
func (ef *EnvFile) Validate() error {
	// Check for empty
	if len(ef.values) == 0 {
		return fmt.Errorf("env file is empty")
	}

	// Check for invalid variable names
	for key := range ef.values {
		if err := ef.validateKey(key); err != nil {
			return err
		}
	}

	// Check size limit (Docker has limits)
	maxSize := 1024 * 1024 // 1MB
	if ef.Size() > maxSize {
		return fmt.Errorf("env file too large: %d bytes (max %d)", ef.Size(), maxSize)
	}

	return nil
}

// validateKey checks if an environment variable name is valid
func (ef *EnvFile) validateKey(key string) error {
	if key == "" {
		return fmt.Errorf("empty environment variable name")
	}

	// Must start with letter or underscore
	if !((key[0] >= 'A' && key[0] <= 'Z') ||
		(key[0] >= 'a' && key[0] <= 'z') ||
		key[0] == '_') {
		return fmt.Errorf("invalid environment variable name '%s': must start with letter or underscore", key)
	}

	// Check all characters
	for i, r := range key {
		valid := (r >= 'A' && r <= 'Z') ||
			(r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9') ||
			r == '_'

		if !valid {
			return fmt.Errorf("invalid character '%c' at position %d in environment variable name '%s'", r, i, key)
		}
	}

	return nil
}

// GetPath returns the temporary file path on the server
func (ef *EnvFile) GetPath(projectName, serviceName string) string {
	return fmt.Sprintf("/tmp/tako-%s-%s-%s.env",
		sanitizePathComponent(projectName),
		sanitizePathComponent(serviceName),
		ef.id)
}

// sanitizePathComponent removes dangerous characters from path components
func sanitizePathComponent(s string) string {
	// Replace non-alphanumeric with underscore
	result := strings.Builder{}
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') ||
			(r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9') ||
			r == '-' || r == '_' {
			result.WriteRune(r)
		} else {
			result.WriteRune('_')
		}
	}
	return result.String()
}

// GetID returns the unique ID of this env file
func (ef *EnvFile) GetID() string {
	return ef.id
}

// GetCreatedAt returns when this env file was created
func (ef *EnvFile) GetCreatedAt() time.Time {
	return ef.createdAt
}

// GetKeys returns all environment variable keys (for debugging)
func (ef *EnvFile) GetKeys() []string {
	keys := make([]string, 0, len(ef.values))
	for k := range ef.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// GetAll returns all key-value pairs (use cautiously, may contain secrets)
func (ef *EnvFile) GetAll() map[string]string {
	result := make(map[string]string, len(ef.values))
	for k, v := range ef.values {
		result[k] = v
	}
	return result
}
