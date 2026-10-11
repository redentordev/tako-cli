package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// scopedTestVariables are every ${VAR} the scoped expansion fixtures use.
// Each case sets some and unsets the rest.
var scopedTestVariables = []string{
	"SCOPE_PREVIEW_HOST", "SCOPE_PREVIEW_URL",
	"SCOPE_PRODUCTION_HOST", "SCOPE_PRODUCTION_URL", "SCOPE_PRODUCTION_REPLICAS",
	"SCOPE_BASE_TOKEN", "SCOPE_REGISTRY_TOKEN", "SCOPE_UNUSED",
}

const scopedServersYAML = `project:
  name: scoped-app
  version: 1.0.0
servers:
  preview:
    host: ${SCOPE_PREVIEW_HOST}
    user: deploy
    sshKey: ${SCOPE_SSH_KEY}
  production:
    host: ${SCOPE_PRODUCTION_HOST}
    user: deploy
    sshKey: ${SCOPE_SSH_KEY}
`

const scopedTwoEnvironmentsYAML = scopedServersYAML + `environments:
  preview:
    servers: [preview]
    services:
      web:
        image: nginx:alpine
        env:
          API_URL: ${SCOPE_PREVIEW_URL}
  production:
    servers: [production]
    services:
      web:
        image: nginx:alpine
        # a commented ${SCOPE_UNUSED} reference is ignored
        replicas: ${SCOPE_PRODUCTION_REPLICAS}
        env:
          API_URL: ${SCOPE_PRODUCTION_URL}
`

const scopedTwoEnvironmentsJSON = `{
  "project": {"name": "scoped-app", "version": "1.0.0"},
  "servers": {
    "preview": {"host": "${SCOPE_PREVIEW_HOST}", "user": "deploy", "sshKey": "${SCOPE_SSH_KEY}"},
    "production": {"host": "${SCOPE_PRODUCTION_HOST}", "user": "deploy", "sshKey": "${SCOPE_SSH_KEY}"}
  },
  "environments": {
    "preview": {"servers": ["preview"], "services": {"web": {"image": "nginx:alpine", "env": {"API_URL": "${SCOPE_PREVIEW_URL}"}}}},
    "production": {"servers": ["production"], "services": {"web": {"image": "nginx:alpine", "env": {"API_URL": "${SCOPE_PRODUCTION_URL}"}}}}
  }
}
`

const scopedExtendsYAML = scopedServersYAML + `environments:
  base:
    abstract: true
    services:
      web:
        image: nginx:alpine
        env:
          SHARED_TOKEN: ${SCOPE_BASE_TOKEN}
  preview:
    extends: base
    servers: [preview]
  production:
    extends: base
    servers: [production]
    services:
      web:
        env:
          API_URL: ${SCOPE_PRODUCTION_URL}
`

// staging extends the concrete production environment, so production's
// values and servers are part of staging's scope.
const scopedConcreteExtendsYAML = scopedServersYAML + `environments:
  production:
    servers: [production]
    services:
      web:
        image: nginx:alpine
        env:
          API_URL: ${SCOPE_PRODUCTION_URL}
  staging:
    extends: production
    servers: [preview]
`

const scopedSelectorYAML = scopedServersYAML + `environments:
  preview:
    serverSelector:
      any: true
    services:
      web:
        image: nginx:alpine
  production:
    servers: [production]
    services:
      web:
        image: nginx:alpine
        env:
          API_URL: ${SCOPE_PRODUCTION_URL}
`

// The preview environment reuses an anchor declared inside production, so
// production's reference is reachable from preview.
const scopedAliasYAML = scopedServersYAML + `environments:
  production:
    servers: [production]
    services:
      web:
        image: nginx:alpine
        env: &prodenv
          API_URL: ${SCOPE_PRODUCTION_URL}
  preview:
    servers: [preview]
    services:
      web:
        image: nginx:alpine
        env: *prodenv
`

const scopedFragmentYAML = `x-unused:
  token: ${SCOPE_UNUSED}
x-preview-env: &preview-env
  API_URL: ${SCOPE_PREVIEW_URL}
` + scopedServersYAML + `environments:
  preview:
    servers: [preview]
    services:
      web:
        image: nginx:alpine
        env: *preview-env
  production:
    servers: [production]
    services:
      web:
        image: nginx:alpine
        env:
          API_URL: ${SCOPE_PRODUCTION_URL}
`

const scopedRegistryYAML = scopedTwoEnvironmentsYAML + `registries:
  ghcr.io:
    username: ci
    password: ${SCOPE_REGISTRY_TOKEN}
`

func TestLoadConfigForEnvironmentScopesVariableExpansion(t *testing.T) {
	all := map[string]string{
		"SCOPE_PREVIEW_HOST":        "203.0.113.10",
		"SCOPE_PREVIEW_URL":         "https://preview.example.com",
		"SCOPE_PRODUCTION_HOST":     "203.0.113.20",
		"SCOPE_PRODUCTION_URL":      "https://example.com",
		"SCOPE_PRODUCTION_REPLICAS": "2",
		"SCOPE_BASE_TOKEN":          "test-only-value",
		"SCOPE_REGISTRY_TOKEN":      "test-only-value",
		"SCOPE_UNUSED":              "unused",
	}
	only := func(names ...string) map[string]string {
		out := map[string]string{}
		for _, name := range names {
			out[name] = all[name]
		}
		return out
	}

	cases := []struct {
		name         string
		file         string
		config       string
		environment  string
		set          map[string]string
		wantMissing  []string
		wantEnvs     []string
		wantServers  []string
		wantDeferred []string
	}{
		{
			name: "all set keeps every environment", file: "tako.yaml", config: scopedTwoEnvironmentsYAML,
			environment: "preview", set: all,
			wantEnvs: []string{"preview", "production"}, wantServers: []string{"preview", "production"},
		},
		{
			name: "other environment and its server may be unset", file: "tako.yaml", config: scopedTwoEnvironmentsYAML,
			environment: "preview", set: only("SCOPE_PREVIEW_HOST", "SCOPE_PREVIEW_URL"),
			wantEnvs: []string{"preview"}, wantServers: []string{"preview"},
			wantDeferred: []string{"SCOPE_PRODUCTION_HOST", "SCOPE_PRODUCTION_REPLICAS", "SCOPE_PRODUCTION_URL"},
		},
		{
			name: "selected environment is scoped symmetrically", file: "tako.yaml", config: scopedTwoEnvironmentsYAML,
			environment: "production", set: only("SCOPE_PRODUCTION_HOST", "SCOPE_PRODUCTION_URL", "SCOPE_PRODUCTION_REPLICAS"),
			wantEnvs: []string{"production"}, wantServers: []string{"production"},
			wantDeferred: []string{"SCOPE_PREVIEW_HOST", "SCOPE_PREVIEW_URL"},
		},
		{
			name: "selected environment reference is required and listed alone", file: "tako.yaml", config: scopedTwoEnvironmentsYAML,
			environment: "preview", set: only("SCOPE_PREVIEW_HOST"),
			wantMissing: []string{"SCOPE_PREVIEW_URL"},
		},
		{
			name: "selected environment server is required", file: "tako.yaml", config: scopedTwoEnvironmentsYAML,
			environment: "preview", set: only("SCOPE_PREVIEW_URL"),
			wantMissing: []string{"SCOPE_PREVIEW_HOST"},
		},
		{
			name: "without an environment every reference is required", file: "tako.yaml", config: scopedTwoEnvironmentsYAML,
			set:         only("SCOPE_PREVIEW_HOST", "SCOPE_PREVIEW_URL"),
			wantMissing: []string{"SCOPE_PRODUCTION_HOST", "SCOPE_PRODUCTION_REPLICAS", "SCOPE_PRODUCTION_URL"},
		},
		{
			name: "unknown environment falls back to every reference", file: "tako.yaml", config: scopedTwoEnvironmentsYAML,
			environment: "qa", set: only("SCOPE_PREVIEW_HOST", "SCOPE_PREVIEW_URL"),
			wantMissing: []string{"SCOPE_PRODUCTION_HOST", "SCOPE_PRODUCTION_REPLICAS", "SCOPE_PRODUCTION_URL"},
		},
		{
			name: "abstract base of the selected environment is required", file: "tako.yaml", config: scopedExtendsYAML,
			environment: "preview", set: only("SCOPE_PREVIEW_HOST"),
			wantMissing: []string{"SCOPE_BASE_TOKEN"},
		},
		{
			name: "abstract base set and sibling unset", file: "tako.yaml", config: scopedExtendsYAML,
			environment: "preview", set: only("SCOPE_PREVIEW_HOST", "SCOPE_BASE_TOKEN"),
			wantEnvs: []string{"preview"}, wantServers: []string{"preview"},
			wantDeferred: []string{"SCOPE_PRODUCTION_HOST", "SCOPE_PRODUCTION_URL"},
		},
		{
			name: "abstract environment cannot be scoped", file: "tako.yaml", config: scopedExtendsYAML,
			environment: "base", set: only("SCOPE_PREVIEW_HOST", "SCOPE_BASE_TOKEN"),
			wantMissing: []string{"SCOPE_PRODUCTION_HOST", "SCOPE_PRODUCTION_URL"},
		},
		{
			name: "concrete parent and its servers are required", file: "tako.yaml", config: scopedConcreteExtendsYAML,
			environment: "staging", set: only("SCOPE_PREVIEW_HOST"),
			wantMissing: []string{"SCOPE_PRODUCTION_HOST", "SCOPE_PRODUCTION_URL"},
		},
		{
			name: "concrete parent set", file: "tako.yaml", config: scopedConcreteExtendsYAML,
			environment: "staging", set: only("SCOPE_PREVIEW_HOST", "SCOPE_PRODUCTION_HOST", "SCOPE_PRODUCTION_URL"),
			wantEnvs: []string{"production", "staging"}, wantServers: []string{"preview", "production"},
		},
		{
			name: "server selector keeps every server in scope", file: "tako.yaml", config: scopedSelectorYAML,
			environment: "preview", set: only("SCOPE_PREVIEW_HOST"),
			wantMissing: []string{"SCOPE_PRODUCTION_HOST"},
		},
		{
			name: "server selector with every server set", file: "tako.yaml", config: scopedSelectorYAML,
			environment: "preview", set: only("SCOPE_PREVIEW_HOST", "SCOPE_PRODUCTION_HOST"),
			wantEnvs: []string{"preview"}, wantServers: []string{"preview", "production"},
			wantDeferred: []string{"SCOPE_PRODUCTION_URL"},
		},
		{
			name: "alias into another environment is required", file: "tako.yaml", config: scopedAliasYAML,
			environment: "preview", set: only("SCOPE_PREVIEW_HOST"),
			wantMissing: []string{"SCOPE_PRODUCTION_URL"},
		},
		{
			name: "used fragment is required", file: "tako.yaml", config: scopedFragmentYAML,
			environment: "preview", set: only("SCOPE_PREVIEW_HOST"),
			wantMissing: []string{"SCOPE_PREVIEW_URL"},
		},
		{
			name: "unused fragment is deferred", file: "tako.yaml", config: scopedFragmentYAML,
			environment: "preview", set: only("SCOPE_PREVIEW_HOST", "SCOPE_PREVIEW_URL"),
			wantEnvs: []string{"preview"}, wantServers: []string{"preview"},
			wantDeferred: []string{"SCOPE_PRODUCTION_HOST", "SCOPE_PRODUCTION_URL", "SCOPE_UNUSED"},
		},
		{
			name: "shared top-level registries are required", file: "tako.yaml", config: scopedRegistryYAML,
			environment: "preview", set: only("SCOPE_PREVIEW_HOST", "SCOPE_PREVIEW_URL"),
			wantMissing: []string{"SCOPE_REGISTRY_TOKEN"},
		},
		{
			name: "json other environment may be unset", file: "tako.json", config: scopedTwoEnvironmentsJSON,
			environment: "preview", set: only("SCOPE_PREVIEW_HOST", "SCOPE_PREVIEW_URL"),
			wantEnvs: []string{"preview"}, wantServers: []string{"preview"},
			wantDeferred: []string{"SCOPE_PRODUCTION_HOST", "SCOPE_PRODUCTION_URL"},
		},
		{
			name: "json selected environment reference is required", file: "tako.json", config: scopedTwoEnvironmentsJSON,
			environment: "preview", set: only("SCOPE_PREVIEW_HOST"),
			wantMissing: []string{"SCOPE_PREVIEW_URL"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			sshKey := filepath.Join(dir, "id_ed25519")
			if err := os.WriteFile(sshKey, []byte("test-key"), 0600); err != nil {
				t.Fatalf("write ssh key fixture: %v", err)
			}
			t.Setenv("SCOPE_SSH_KEY", filepath.ToSlash(sshKey))
			for _, name := range scopedTestVariables {
				if value, ok := tc.set[name]; ok {
					t.Setenv(name, value)
				} else {
					t.Setenv(name, "")
					os.Unsetenv(name)
				}
			}
			path := filepath.Join(dir, tc.file)
			if err := os.WriteFile(path, []byte(tc.config), 0600); err != nil {
				t.Fatalf("write config: %v", err)
			}

			cfg, err := LoadConfigForEnvironment(path, tc.environment)
			if tc.wantMissing != nil {
				want := "failed to expand config environment variables: missing environment variable(s): " + strings.Join(tc.wantMissing, ", ")
				if err == nil || err.Error() != want {
					t.Fatalf("err = %v, want %q", err, want)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfigForEnvironment: %v", err)
			}
			if got := sortedKeys(cfg.Environments); !reflect.DeepEqual(got, tc.wantEnvs) {
				t.Fatalf("environments = %v, want %v", got, tc.wantEnvs)
			}
			if got := sortedKeys(cfg.Servers); !reflect.DeepEqual(got, tc.wantServers) {
				t.Fatalf("servers = %v, want %v", got, tc.wantServers)
			}
			if got := cfg.DeferredEnvironmentVariables(); !reflect.DeepEqual(got, tc.wantDeferred) {
				t.Fatalf("deferred = %v, want %v", got, tc.wantDeferred)
			}
			// No unexpanded reference may survive into the loaded config.
			encoded, err := json.Marshal(cfg)
			if err != nil {
				t.Fatalf("marshal config: %v", err)
			}
			if strings.Contains(string(encoded), "${") {
				t.Fatalf("loaded config still holds an unexpanded reference: %s", encoded)
			}
		})
	}
}

func TestLoadConfigForEnvironmentKeepsInheritedValues(t *testing.T) {
	dir := t.TempDir()
	sshKey := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(sshKey, []byte("test-key"), 0600); err != nil {
		t.Fatalf("write ssh key fixture: %v", err)
	}
	t.Setenv("SCOPE_SSH_KEY", filepath.ToSlash(sshKey))
	t.Setenv("SCOPE_PREVIEW_HOST", "203.0.113.10")
	t.Setenv("SCOPE_BASE_TOKEN", "test-only-value")
	for _, name := range []string{"SCOPE_PRODUCTION_HOST", "SCOPE_PRODUCTION_URL"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	path := filepath.Join(dir, "tako.yaml")
	if err := os.WriteFile(path, []byte(scopedExtendsYAML), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadConfigForEnvironment(path, "preview")
	if err != nil {
		t.Fatalf("LoadConfigForEnvironment: %v", err)
	}
	if got := cfg.Environments["preview"].Services["web"].Env["SHARED_TOKEN"]; got != "test-only-value" {
		t.Fatalf("inherited SHARED_TOKEN = %q", got)
	}
	if chain := cfg.EnvironmentExtendsChain("preview"); !reflect.DeepEqual(chain, []string{"base"}) {
		t.Fatalf("chain = %v", chain)
	}
	if _, err := cfg.GetEnvironment("production"); err == nil {
		t.Fatal("pruned production environment is still selectable")
	}
}

func TestLoadConfigForEnvironmentReportsOriginalLines(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SCOPE_PREVIEW_HOST", "203.0.113.10")
	for _, name := range []string{"SCOPE_PRODUCTION_HOST"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	config := scopedServersYAML + `environments:
  preview:
    servers: [preview]
    services:
      web:
        image: nginx:alpine
    labels: not-a-map
  production:
    servers: [production]
    services:
      web:
        image: nginx:alpine
`
	t.Setenv("SCOPE_SSH_KEY", "unused")
	path := filepath.Join(dir, "tako.yaml")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	_, scopedErr := LoadConfigForEnvironment(path, "preview")
	if scopedErr == nil || !strings.HasPrefix(scopedErr.Error(), "failed to parse YAML config:") || !strings.Contains(scopedErr.Error(), "line 19") {
		t.Fatalf("err = %v, want a YAML error on line 19", scopedErr)
	}

	// With every reference set the strict path reports the same error.
	t.Setenv("SCOPE_PRODUCTION_HOST", "203.0.113.20")
	_, strictErr := LoadConfig(path)
	if strictErr == nil || strictErr.Error() != scopedErr.Error() {
		t.Fatalf("strict err = %v, scoped err = %v", strictErr, scopedErr)
	}
}

func TestEnvironmentScopeServers(t *testing.T) {
	cases := []struct {
		name        string
		document    string
		wantAll     bool
		wantServers []string
	}{
		{
			name: "listed and placement servers",
			document: `servers: {a: {}, b: {}, c: {}, d: {}}
environments:
  preview:
    servers: [a]
    proxy: {placement: {servers: [b]}}
    services: {web: {placement: {strategy: pinned, servers: [c]}}}
  production:
    servers: [d]
`,
			wantServers: []string{"a", "b", "c"},
		},
		{
			name: "inherited server list",
			document: `servers: {a: {}, b: {}}
environments:
  base: {abstract: true, servers: [a]}
  preview: {extends: base}
  production: {servers: [b]}
`,
			wantServers: []string{"a"},
		},
		{
			name: "no server list uses every server",
			document: `servers: {a: {}, b: {}}
environments:
  preview: {services: {}}
  production: {servers: [b]}
`,
			wantAll: true,
		},
		{
			name: "enrolled servers span environments",
			document: `servers: {a: {clusterId: c1, nodeId: n1}, b: {clusterId: c1, nodeId: n2}}
environments:
  preview: {servers: [a]}
  production: {servers: [b]}
`,
			wantAll: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, ok := newScopeDocument([]byte(tc.document), false)
			if !ok {
				t.Fatal("newScopeDocument failed")
			}
			scope, ok := doc.scopeFor("preview")
			if !ok {
				t.Fatal("scopeFor failed")
			}
			if scope.allServers != tc.wantAll {
				t.Fatalf("allServers = %v, want %v", scope.allServers, tc.wantAll)
			}
			if !tc.wantAll {
				if got := sortedKeys(scope.servers); !reflect.DeepEqual(got, tc.wantServers) {
					t.Fatalf("servers = %v, want %v", got, tc.wantServers)
				}
			}
		})
	}
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// scopedPreviewYAML has an abstract preview base (review) that extends the
// concrete staging environment, plus a production environment that no
// preview can reach.
const scopedPreviewYAML = scopedServersYAML + `environments:
  production:
    servers: [production]
    services:
      web:
        image: nginx:alpine
        env:
          API_URL: ${SCOPE_PRODUCTION_URL}
  staging:
    servers: [preview]
    services:
      web:
        image: nginx:alpine
        port: 3000
        proxy:
          domain: app.example.com
        env:
          SHARED_TOKEN: ${SCOPE_BASE_TOKEN}
  review:
    abstract: true
    extends: staging
    preview:
      domain: "{name}.preview.example.com"
    services:
      web:
        env:
          API_URL: ${SCOPE_PREVIEW_URL}
`

func TestLoadConfigWithOptionsScopesPreviewToItsBase(t *testing.T) {
	dir := t.TempDir()
	sshKey := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(sshKey, []byte("test-key"), 0600); err != nil {
		t.Fatalf("write ssh key fixture: %v", err)
	}
	path := filepath.Join(dir, "tako.yaml")
	if err := os.WriteFile(path, []byte(scopedPreviewYAML), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	set := func(t *testing.T, unset ...string) {
		t.Helper()
		values := map[string]string{
			"SCOPE_SSH_KEY":         filepath.ToSlash(sshKey),
			"SCOPE_PREVIEW_HOST":    "203.0.113.10",
			"SCOPE_PREVIEW_URL":     "https://preview.example.com",
			"SCOPE_BASE_TOKEN":      "test-only-value",
			"SCOPE_PRODUCTION_HOST": "203.0.113.20",
			"SCOPE_PRODUCTION_URL":  "https://example.com",
		}
		for name, value := range values {
			t.Setenv(name, value)
		}
		for _, name := range unset {
			os.Unsetenv(name)
		}
	}
	preview := func(base string) *PreviewInstance { return &PreviewInstance{Base: base, Name: "PR-42"} }

	for _, tc := range []struct {
		name string
		opts LoadOptions
	}{
		{name: "explicit base", opts: LoadOptions{Preview: preview("review"), Environment: "review"}},
		{name: "environment names the base", opts: LoadOptions{Preview: preview(""), Environment: "review"}},
	} {
		t.Run(tc.name+": unrelated environment with unset variables is pruned", func(t *testing.T) {
			set(t, "SCOPE_PRODUCTION_HOST", "SCOPE_PRODUCTION_URL")
			cfg, err := LoadConfigWithOptions(path, tc.opts)
			if err != nil {
				t.Fatalf("LoadConfigWithOptions: %v", err)
			}
			instance := cfg.PreviewInstance()
			if instance == nil || instance.Identity != "preview-pr-42" || instance.Base != "review" {
				t.Fatalf("preview instance = %+v", instance)
			}
			services, err := cfg.GetServices("preview-pr-42")
			if err != nil {
				t.Fatalf("preview environment was pruned: %v", err)
			}
			env := services["web"].Env
			if env["SHARED_TOKEN"] != "test-only-value" || env["API_URL"] != "https://preview.example.com" {
				t.Fatalf("preview web env = %v", env)
			}
			if _, err := cfg.GetEnvironment("production"); err == nil {
				t.Fatal("pruned production environment is still selectable")
			}
			if _, ok := cfg.Servers["production"]; ok {
				t.Fatal("out-of-scope server with unset variables was kept")
			}
			if got := cfg.DeferredEnvironmentVariables(); !reflect.DeepEqual(got, []string{"SCOPE_PRODUCTION_HOST", "SCOPE_PRODUCTION_URL"}) {
				t.Fatalf("deferred = %v", got)
			}
		})
	}

	for _, variable := range []string{"SCOPE_PREVIEW_URL", "SCOPE_BASE_TOKEN", "SCOPE_PREVIEW_HOST"} {
		t.Run("base requires "+variable, func(t *testing.T) {
			set(t, variable, "SCOPE_PRODUCTION_URL")
			_, err := LoadConfigWithOptions(path, LoadOptions{Preview: preview("review"), Environment: "review"})
			if err == nil || !strings.Contains(err.Error(), variable) {
				t.Fatalf("err = %v, want %s required", err, variable)
			}
			if strings.Contains(err.Error(), "SCOPE_PRODUCTION_URL") {
				t.Fatalf("err = %v names an out-of-scope variable", err)
			}
		})
	}

	t.Run("without an environment every variable is required", func(t *testing.T) {
		set(t, "SCOPE_PRODUCTION_URL")
		_, err := LoadConfigWithOptions(path, LoadOptions{Preview: preview("review")})
		if err == nil || !strings.Contains(err.Error(), "SCOPE_PRODUCTION_URL") {
			t.Fatalf("err = %v, want SCOPE_PRODUCTION_URL required", err)
		}
	})

	t.Run("all variables set prunes nothing", func(t *testing.T) {
		set(t)
		cfg, err := LoadConfigWithOptions(path, LoadOptions{Preview: preview("review"), Environment: "review"})
		if err != nil {
			t.Fatalf("LoadConfigWithOptions: %v", err)
		}
		if _, err := cfg.GetEnvironment("production"); err != nil {
			t.Fatalf("production: %v", err)
		}
		if got := cfg.DeferredEnvironmentVariables(); got != nil {
			t.Fatalf("deferred = %v", got)
		}
	})
}

func TestLoadConfigWithOptionsPreviewIdentityCollisionIsNotPruned(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SCOPE_SSH_KEY", "unused")
	t.Setenv("SCOPE_PREVIEW_HOST", "203.0.113.10")
	t.Setenv("SCOPE_PREVIEW_URL", "https://preview.example.com")
	t.Setenv("SCOPE_BASE_TOKEN", "test-only-value")
	for _, name := range []string{"SCOPE_PRODUCTION_HOST", "SCOPE_PRODUCTION_URL"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	config := scopedPreviewYAML + `  preview-pr-42:
    servers: [preview]
    services:
      web:
        image: nginx:alpine
`
	path := filepath.Join(dir, "tako.yaml")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	_, err := LoadConfigWithOptions(path, LoadOptions{Preview: &PreviewInstance{Base: "review", Name: "pr-42"}, Environment: "review"})
	if err == nil || !strings.Contains(err.Error(), "conflicts with the identity of preview") {
		t.Fatalf("err = %v, want the identity conflict", err)
	}
}
