package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// LoadEnvFile reads a .env file and returns a map of environment variables
// Supports:
// - KEY=value format
// - Comments (lines starting with #)
// - Empty lines
// - Single and double quoted values
// - Variable expansion ${VAR} syntax
func LoadEnvFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open env file: %w", err)
	}
	defer file.Close()

	envVars := make(map[string]string)
	scanner := bufio.NewScanner(file)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Parse KEY=VALUE format
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			// Skip invalid lines with a warning (don't fail)
			fmt.Printf("Warning: Invalid line %d in %s: %s\n", lineNum, path, line)
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		// Remove quotes if present
		value = unquoteValue(value)

		// Expand environment variables in the value
		value = os.ExpandEnv(value)

		envVars[key] = value
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading env file: %w", err)
	}

	return envVars, nil
}

// unquoteValue removes surrounding quotes from a value
func unquoteValue(value string) string {
	if len(value) < 2 {
		return value
	}

	// Remove double quotes
	if strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\"") {
		return value[1 : len(value)-1]
	}

	// Remove single quotes
	if strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'") {
		return value[1 : len(value)-1]
	}

	return value
}

// MergeEnvVars merges environment variables from multiple sources
// Priority (highest to lowest):
// 1. Explicit env vars from config (service.Env)
// 2. Variables from envFile (service.EnvFile)
// 3. System environment variables (already expanded via ${VAR} syntax)
func MergeEnvVars(explicitEnv map[string]string, envFileVars map[string]string) map[string]string {
	// Start with envFile vars (lowest priority)
	merged := make(map[string]string)
	for k, v := range envFileVars {
		merged[k] = v
	}

	// Override with explicit env vars (highest priority)
	for k, v := range explicitEnv {
		merged[k] = v
	}

	return merged
}
