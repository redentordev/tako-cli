// Package configtest provides test helpers that validate Tako config
// documents and fragments through the same strict loader path that
// `tako validate` uses. It is shared by the init template tests and the docs
// snippet harness.
package configtest

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"gopkg.in/yaml.v3"
)

// BaseEnvironment is the environment name the base config declares.
const BaseEnvironment = "production"

// BaseYAML is the config a partial fragment is merged onto: one project, one
// server named production, and one environment named production that targets
// it.
const BaseYAML = `project:
  name: docs-example
  version: 1.0.0
servers:
  production:
    host: ${TAKO_PRODUCTION_HOST}
    user: root
    sshKey: ${TAKO_SSH_KEY}
environments:
  production:
    servers: [production]
`

// AutoFillValue is assigned to any ${VAR} a document references that neither
// the process environment nor Placeholders defines.
const AutoFillValue = "example-value"

// Placeholders is the fixed environment used for validation. Hosts use the
// TEST-NET-3 documentation range. TAKO_SSH_KEY is set by SetupEnv because it
// points into the temporary home directory.
var Placeholders = map[string]string{
	"TAKO_PRODUCTION_HOST":  "203.0.113.10",
	"TAKO_STAGING_HOST":     "203.0.113.11",
	"TAKO_SERVER_HOST":      "203.0.113.10",
	"SERVER_HOST":           "203.0.113.10",
	"SERVER_IP":             "203.0.113.10",
	"SERVER1_HOST":          "203.0.113.11",
	"SERVER2_HOST":          "203.0.113.12",
	"SERVER3_HOST":          "203.0.113.13",
	"SERVER_USER":           "root",
	"LETSENCRYPT_EMAIL":     "ops@example.com",
	"ACME_EMAIL":            "ops@example.com",
	"MAILER_EMAIL":          "ops@example.com",
	"POSTGRES_PASSWORD":     "example-postgres-password",
	"MYSQL_PASSWORD":        "example-mysql-password",
	"MYSQL_ROOT_PASSWORD":   "example-mysql-root-password",
	"REDIS_PASSWORD":        "example-redis-password",
	"REGISTRY_USER":         "example-registry-user",
	"REGISTRY_TOKEN":        "example-registry-token",
	"SLACK_WEBHOOK_URL":     "https://hooks.slack.com/services/T000/B000/XXXX",
	"DISCORD_WEBHOOK_URL":   "https://discord.com/api/webhooks/000/XXXX",
	"WEBHOOK_URL":           "https://hooks.example.com/tako",
	"CLOUDFLARE_API_TOKEN":  "example-cloudflare-token",
	"AWS_ACCESS_KEY_ID":     "example-access-key",
	"AWS_SECRET_ACCESS_KEY": "example-secret-key",
}

var envRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// SetupEnv points HOME (and USERPROFILE, which Windows uses for the home
// directory) at a temporary directory that contains .ssh/id_ed25519, sets
// TAKO_SSH_KEY to that key and sets every Placeholders entry. It returns the
// temporary home directory. Tests using it must not call t.Parallel.
func SetupEnv(t testing.TB) string {
	t.Helper()
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatalf("create ssh dir: %v", err)
	}
	keyPath := filepath.Join(sshDir, "id_ed25519")
	if err := os.WriteFile(keyPath, []byte("test-key\n"), 0o600); err != nil {
		t.Fatalf("write ssh key fixture: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("TAKO_SKIP_UPDATE_CHECK", "1")
	// Forward slashes keep the path valid inside JSON strings on Windows.
	t.Setenv("TAKO_SSH_KEY", filepath.ToSlash(keyPath))
	for key, value := range Placeholders {
		t.Setenv(key, value)
	}
	return home
}

// FillEnv sets every ${VAR} referenced by content that is not already set to
// AutoFillValue, so documents do not need to register each variable.
func FillEnv(t testing.TB, content []byte) {
	t.Helper()
	for _, name := range ReferencedVars(content) {
		if _, ok := os.LookupEnv(name); !ok {
			t.Setenv(name, AutoFillValue)
		}
	}
}

// ReferencedVars returns the sorted, unique ${VAR} names content references.
func ReferencedVars(content []byte) []string {
	seen := map[string]bool{}
	var names []string
	for _, match := range envRefPattern.FindAllSubmatch(content, -1) {
		name := string(match[1])
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// MergeYAML deep-merges fragment onto base and returns the merged document.
//
//   - Mappings merge recursively.
//   - Scalars and sequences in the fragment replace the base value.
//   - If the fragment defines top-level servers, the base servers are dropped.
//     When the fragment also leaves environments.production.servers unset, it
//     defaults to every fragment server name in sorted order.
func MergeYAML(base, fragment []byte) ([]byte, error) {
	baseDoc, err := decodeMapping(base)
	if err != nil {
		return nil, fmt.Errorf("parse base: %w", err)
	}
	fragmentDoc, err := decodeMapping(fragment)
	if err != nil {
		return nil, fmt.Errorf("parse fragment: %w", err)
	}

	fragmentServers, replaceServers := fragmentDoc["servers"].(map[string]any)
	if replaceServers {
		delete(baseDoc, "servers")
	}
	merged := mergeMaps(baseDoc, fragmentDoc)

	if replaceServers && !fragmentSetsEnvironmentServers(fragmentDoc, BaseEnvironment) {
		environments, _ := merged["environments"].(map[string]any)
		if environment, ok := environments[BaseEnvironment].(map[string]any); ok {
			names := make([]string, 0, len(fragmentServers))
			for name := range fragmentServers {
				names = append(names, name)
			}
			sort.Strings(names)
			servers := make([]any, len(names))
			for i, name := range names {
				servers[i] = name
			}
			environment["servers"] = servers
		}
	}

	out, err := yaml.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("encode merged config: %w", err)
	}
	return out, nil
}

func decodeMapping(data []byte) (map[string]any, error) {
	doc := map[string]any{}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

func fragmentSetsEnvironmentServers(fragment map[string]any, environment string) bool {
	environments, _ := fragment["environments"].(map[string]any)
	env, _ := environments[environment].(map[string]any)
	_, ok := env["servers"]
	return ok
}

func mergeMaps(base, overlay map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(overlay))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range overlay {
		overlayMap, overlayIsMap := value.(map[string]any)
		baseMap, baseIsMap := out[key].(map[string]any)
		if overlayIsMap && baseIsMap {
			out[key] = mergeMaps(baseMap, overlayMap)
			continue
		}
		out[key] = value
	}
	return out
}

// MaterializePaths creates the local files a document points at, relative to
// dir, so validation does not fail on missing fixtures: build contexts (with a
// Dockerfile, or the declared dockerfile), shared build contexts, envFile and
// envFiles entries, and files[].source. Absolute, home-relative and
// parent-escaping paths are left alone.
func MaterializePaths(t testing.TB, dir string, content []byte) {
	t.Helper()
	var doc any
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return // the loader reports the syntax error
	}
	materializeNode(t, dir, doc)
}

func materializeNode(t testing.TB, dir string, node any) {
	switch value := node.(type) {
	case []any:
		for _, item := range value {
			materializeNode(t, dir, item)
		}
	case map[string]any:
		dockerfile, _ := value["dockerfile"].(string)
		switch build := value["build"].(type) {
		case string:
			materializeBuildContext(t, dir, build, dockerfile)
		case map[string]any:
			context, _ := build["context"].(string)
			buildDockerfile, _ := build["dockerfile"].(string)
			materializeBuildContext(t, dir, context, buildDockerfile)
		}
		if builds, ok := value["builds"].(map[string]any); ok {
			for _, raw := range builds {
				if build, ok := raw.(map[string]any); ok {
					context, _ := build["context"].(string)
					buildDockerfile, _ := build["dockerfile"].(string)
					materializeBuildContext(t, dir, context, buildDockerfile)
				}
			}
		}
		if envFile, ok := value["envFile"].(string); ok {
			materializeFile(t, dir, envFile)
		}
		if envFiles, ok := value["envFiles"].([]any); ok {
			for _, raw := range envFiles {
				if envFile, ok := raw.(string); ok {
					materializeFile(t, dir, envFile)
				}
			}
		}
		if files, ok := value["files"].([]any); ok {
			for _, raw := range files {
				if file, ok := raw.(map[string]any); ok {
					if source, ok := file["source"].(string); ok {
						materializeFile(t, dir, source)
					}
				}
			}
		}
		for _, child := range value {
			materializeNode(t, dir, child)
		}
	}
}

func localPath(dir, path string) (string, bool) {
	path = strings.TrimSpace(path)
	if path == "" || strings.Contains(path, "${") || strings.HasPrefix(path, "~") || filepath.IsAbs(path) || strings.HasPrefix(path, "/") {
		return "", false
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.Join(dir, clean), true
}

func materializeBuildContext(t testing.TB, dir, context, dockerfile string) {
	t.Helper()
	contextPath, ok := localPath(dir, context)
	if !ok {
		return
	}
	if err := os.MkdirAll(contextPath, 0o755); err != nil {
		t.Fatalf("create build context %s: %v", context, err)
	}
	if strings.TrimSpace(dockerfile) == "" {
		dockerfile = "Dockerfile"
	}
	dockerfilePath, ok := localPath(contextPath, dockerfile)
	if !ok {
		return
	}
	if _, err := os.Stat(dockerfilePath); err == nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(dockerfilePath), 0o755); err != nil {
		t.Fatalf("create dockerfile dir: %v", err)
	}
	if err := os.WriteFile(dockerfilePath, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write dockerfile fixture: %v", err)
	}
}

func materializeFile(t testing.TB, dir, path string) {
	t.Helper()
	filePath, ok := localPath(dir, path)
	if !ok {
		return
	}
	if _, err := os.Stat(filePath); err == nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("create fixture dir: %v", err)
	}
	if err := os.WriteFile(filePath, []byte("EXAMPLE=value\n"), 0o644); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
}

// LoadAndValidate writes content to dir/name, auto-fills any unset ${VAR} it
// references, and runs it through config.LoadConfig and
// config.ValidationWarnings, the same strict path `tako validate` uses. Call
// SetupEnv first.
func LoadAndValidate(t testing.TB, dir, name string, content []byte) (*config.Config, []config.ValidationWarning, error) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	FillEnv(t, content)
	MaterializePaths(t, dir, content)
	cfg, err := config.LoadConfig(path)
	if err != nil {
		return nil, nil, err
	}
	return cfg, config.ValidationWarnings(cfg), nil
}
