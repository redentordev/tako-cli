package deployer

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// IgnoreParser handles parsing of .gitignore and .dockerignore files
type IgnoreParser struct {
	patterns []string
}

// NewIgnoreParser creates a new ignore parser
func NewIgnoreParser() *IgnoreParser {
	return &IgnoreParser{
		patterns: []string{},
	}
}

// LoadIgnoreFile loads patterns from a .gitignore or .dockerignore file
func (ip *IgnoreParser) LoadIgnoreFile(filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		// File doesn't exist, that's okay
		return nil
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Remove trailing slashes
		line = strings.TrimSuffix(line, "/")

		ip.patterns = append(ip.patterns, line)
	}

	return scanner.Err()
}

// AddDefaultExclusions adds commonly excluded patterns
func (ip *IgnoreParser) AddDefaultExclusions() {
	defaults := []string{
		".git",
		".gitignore",
		".dockerignore",
		".env",
		".env.*",
		".env.local",
		".env.development",
		".env.test",
		".env.production",
		"node_modules",
		".next",
		"dist",
		"build",
		"*.log",
		".DS_Store",
		"Thumbs.db",
		".tako",
		"tako.yaml",
		"tako.json",
		"tako-*.yaml",
		"tako-*.json",
		".vscode",
		".idea",
		"*.swp",
		"*.swo",
		".vagrant",
	}

	ip.patterns = append(ip.patterns, defaults...)
}

// ShouldIgnore checks if a file path should be ignored
func (ip *IgnoreParser) ShouldIgnore(relPath string) bool {
	// Normalize path separators to forward slashes
	relPath = filepath.ToSlash(relPath)

	for _, pattern := range ip.patterns {
		if ip.matchPattern(pattern, relPath) {
			return true
		}
	}

	return false
}

// matchPattern matches a gitignore-style pattern against a path
func (ip *IgnoreParser) matchPattern(pattern, path string) bool {
	// Exact match
	if pattern == path {
		return true
	}

	// Match anywhere in path (e.g., "*.log" matches "foo/bar.log")
	if strings.HasPrefix(pattern, "*") {
		suffix := strings.TrimPrefix(pattern, "*")
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}

	// Match as directory or file
	pathParts := strings.Split(path, "/")
	for _, part := range pathParts {
		if part == pattern {
			return true
		}

		// Wildcard match
		if strings.Contains(pattern, "*") {
			if matched, _ := filepath.Match(pattern, part); matched {
				return true
			}
		}
	}

	// Match with wildcards
	if strings.Contains(pattern, "*") {
		matched, _ := filepath.Match(pattern, path)
		return matched
	}

	// Match prefix (directory pattern)
	if strings.HasPrefix(path, pattern+"/") {
		return true
	}

	return false
}

// GetExcludedPatterns returns all exclusion patterns
func (ip *IgnoreParser) GetExcludedPatterns() []string {
	return ip.patterns
}
