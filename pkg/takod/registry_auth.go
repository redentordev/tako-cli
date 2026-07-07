package takod

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RegistryAuth carries request-scoped credentials for one registry host.
// Credentials ride the request body only: they are written to an ephemeral
// docker config dir for the single docker invocation and never persist to
// takod state, argv, query strings, or logs (ADR 10).
type RegistryAuth struct {
	Registry string `json:"registry"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// RegistryAuthFailedMarker prefixes auth-classified docker failures so the
// CLI can raise a typed error distinct from image-not-found.
const RegistryAuthFailedMarker = "registry authentication failed"

const maxRegistryAuthFieldBytes = 4096

func validateRegistryAuths(auths []RegistryAuth) error {
	for _, auth := range auths {
		registry := strings.TrimSpace(auth.Registry)
		if registry == "" {
			return fmt.Errorf("registry auth requires a registry host")
		}
		if len(registry) > maxRegistryAuthFieldBytes || hasControlChars(registry) || strings.ContainsAny(registry, " \t\"\\") {
			return fmt.Errorf("invalid registry host")
		}
		if auth.Username == "" || auth.Password == "" {
			return fmt.Errorf("registry auth for %s requires username and password", registry)
		}
		if len(auth.Username) > maxRegistryAuthFieldBytes || hasControlChars(auth.Username) {
			return fmt.Errorf("invalid registry username for %s", registry)
		}
		if len(auth.Password) > maxRegistryAuthFieldBytes || hasControlChars(auth.Password) {
			return fmt.Errorf("invalid registry password for %s", registry)
		}
	}
	return nil
}

// normalizeRegistryAuthKey maps a registry host to the key docker matches
// in config.json auths; Docker Hub uses its legacy index URL.
func normalizeRegistryAuthKey(registry string) string {
	registry = strings.TrimSpace(registry)
	switch strings.ToLower(registry) {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return "https://index.docker.io/v1/"
	}
	return registry
}

// writeEphemeralDockerConfig materializes credentials as a 0700 temp dir
// holding a 0600 config.json for use via DOCKER_CONFIG. The caller must run
// cleanup as soon as the docker invocation finishes.
func writeEphemeralDockerConfig(auths []RegistryAuth) (string, func(), error) {
	if err := validateRegistryAuths(auths); err != nil {
		return "", nil, err
	}
	entries := make(map[string]map[string]string, len(auths))
	for _, auth := range auths {
		key := normalizeRegistryAuthKey(auth.Registry)
		entries[key] = map[string]string{
			"auth": base64.StdEncoding.EncodeToString([]byte(auth.Username + ":" + auth.Password)),
		}
	}
	data, err := json.Marshal(map[string]any{"auths": entries})
	if err != nil {
		return "", nil, fmt.Errorf("failed to encode docker auth config: %w", err)
	}

	dir, err := os.MkdirTemp("", "tako-registry-auth-*")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create ephemeral docker config dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("failed to write ephemeral docker config: %w", err)
	}
	return dir, cleanup, nil
}

// dockerAuthEnv returns the process env with DOCKER_CONFIG pointed at the
// ephemeral config dir.
func dockerAuthEnv(dir string) []string {
	return append(os.Environ(), "DOCKER_CONFIG="+dir)
}

// runDockerWithAuth runs a docker command like runDocker; with auths it
// injects the ephemeral DOCKER_CONFIG for the duration of the command.
func runDockerWithAuth(ctx context.Context, auths []RegistryAuth, args ...string) (string, error) {
	cmd := dockerCommandContext(ctx, "docker", args...)
	if len(auths) > 0 {
		dir, cleanup, err := writeEphemeralDockerConfig(auths)
		if err != nil {
			return "", err
		}
		defer cleanup()
		cmd.Env = dockerAuthEnv(dir)
	}
	output := newCappedOutputBuffer(defaultCommandOutputMaxBytes)
	cmd.Stdout = output
	cmd.Stderr = output
	err := cmd.Run()
	return output.String(), err
}

// isDockerAuthFailure classifies docker pull/build output that indicates a
// credential problem rather than a missing image.
func isDockerAuthFailure(output string) bool {
	lowered := strings.ToLower(output)
	for _, marker := range []string{
		"unauthorized",
		"authentication required",
		"no basic auth credentials",
		"pull access denied",
		"denied: requested access to the resource is denied",
		"401 ",
		"login attempt to",
	} {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// annotateRegistryAuthFailure prefixes auth-classified docker output with
// the typed marker; other output passes through unchanged.
func annotateRegistryAuthFailure(output string) string {
	if isDockerAuthFailure(output) {
		return RegistryAuthFailedMarker + ": " + output
	}
	return output
}
