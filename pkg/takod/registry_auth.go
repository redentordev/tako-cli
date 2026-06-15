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

const maxRegistryAuthFieldBytes = 8192

type RegistryAuth struct {
	Server        string `json:"server,omitempty"`
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	IdentityToken string `json:"identityToken,omitempty"`
}

type dockerConfigFile struct {
	Auths map[string]dockerAuthConfig `json:"auths"`
}

type dockerAuthConfig struct {
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	Auth          string `json:"auth,omitempty"`
	IdentityToken string `json:"identitytoken,omitempty"`
}

func pullImage(ctx context.Context, image string, auth *RegistryAuth) (string, error) {
	if auth == nil || auth.empty() {
		return runDocker(ctx, "pull", image)
	}
	if err := validateRegistryAuth(*auth); err != nil {
		return "", err
	}
	configDir, cleanup, err := writeTemporaryDockerConfig(*auth)
	if err != nil {
		return "", err
	}
	defer cleanup()

	env := append(os.Environ(), "DOCKER_CONFIG="+configDir)
	return runDockerWithEnv(ctx, env, "pull", image)
}

func (auth RegistryAuth) empty() bool {
	return strings.TrimSpace(auth.Server) == "" &&
		auth.Username == "" &&
		auth.Password == "" &&
		auth.IdentityToken == ""
}

func validateRegistryAuth(auth RegistryAuth) error {
	if strings.TrimSpace(auth.Server) == "" {
		return fmt.Errorf("registry auth server is required")
	}
	if auth.IdentityToken == "" && (auth.Username == "" || auth.Password == "") {
		return fmt.Errorf("registry auth requires username/password or identityToken")
	}
	for field, value := range map[string]string{
		"server":        auth.Server,
		"username":      auth.Username,
		"password":      auth.Password,
		"identityToken": auth.IdentityToken,
	} {
		if err := validateRegistryAuthField(field, value); err != nil {
			return err
		}
	}
	return nil
}

func validateRegistryAuthField(field string, value string) error {
	if value == "" {
		return nil
	}
	if len(value) > maxRegistryAuthFieldBytes {
		return fmt.Errorf("registry auth %s is too long", field)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("registry auth %s must not contain control characters", field)
		}
	}
	return nil
}

func writeTemporaryDockerConfig(auth RegistryAuth) (string, func(), error) {
	dir, err := os.MkdirTemp("", "tako-docker-auth-*")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create temporary Docker auth directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	entry := dockerAuthConfig{
		Username:      auth.Username,
		Password:      auth.Password,
		IdentityToken: auth.IdentityToken,
	}
	if auth.Username != "" || auth.Password != "" {
		entry.Auth = base64.StdEncoding.EncodeToString([]byte(auth.Username + ":" + auth.Password))
	}
	config := dockerConfigFile{
		Auths: map[string]dockerAuthConfig{
			strings.TrimSpace(auth.Server): entry,
		},
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("failed to encode temporary Docker auth config: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("failed to write temporary Docker auth config: %w", err)
	}
	return dir, cleanup, nil
}
