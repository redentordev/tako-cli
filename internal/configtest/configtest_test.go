package configtest

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func decode(t *testing.T, data []byte) map[string]any {
	t.Helper()
	doc := map[string]any{}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, data)
	}
	return doc
}

func lookup(doc map[string]any, path ...string) any {
	var current any = doc
	for _, key := range path {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[key]
	}
	return current
}

func TestMergeYAMLMergesMappingsRecursively(t *testing.T) {
	merged, err := MergeYAML([]byte(BaseYAML), []byte(`environments:
  production:
    services:
      web:
        image: nginx:alpine
`))
	if err != nil {
		t.Fatal(err)
	}
	doc := decode(t, merged)
	if got := lookup(doc, "project", "name"); got != "docs-example" {
		t.Fatalf("project.name = %v", got)
	}
	if got := lookup(doc, "environments", "production", "servers"); !reflect.DeepEqual(got, []any{"production"}) {
		t.Fatalf("environment servers = %#v", got)
	}
	if got := lookup(doc, "environments", "production", "services", "web", "image"); got != "nginx:alpine" {
		t.Fatalf("web image = %v", got)
	}
	if got := lookup(doc, "servers", "production", "user"); got != "root" {
		t.Fatalf("base server lost: %v", got)
	}
}

func TestMergeYAMLReplacesScalarsAndSequences(t *testing.T) {
	base := []byte("a:\n  list: [1, 2, 3]\n  scalar: base\n  keep: yes\n")
	merged, err := MergeYAML(base, []byte("a:\n  list: [9]\n  scalar: fragment\n"))
	if err != nil {
		t.Fatal(err)
	}
	doc := decode(t, merged)
	if got := lookup(doc, "a", "list"); !reflect.DeepEqual(got, []any{9}) {
		t.Fatalf("list = %#v, want replacement", got)
	}
	if got := lookup(doc, "a", "scalar"); got != "fragment" {
		t.Fatalf("scalar = %v", got)
	}
	if got := lookup(doc, "a", "keep"); got != "yes" {
		t.Fatalf("keep = %v", got)
	}
}

func TestMergeYAMLFragmentServersReplaceBaseServers(t *testing.T) {
	merged, err := MergeYAML([]byte(BaseYAML), []byte(`servers:
  node2:
    host: 203.0.113.12
  node1:
    host: 203.0.113.11
`))
	if err != nil {
		t.Fatal(err)
	}
	doc := decode(t, merged)
	servers := lookup(doc, "servers").(map[string]any)
	if _, ok := servers["production"]; ok {
		t.Fatalf("base server should be dropped: %#v", servers)
	}
	if len(servers) != 2 {
		t.Fatalf("servers = %#v", servers)
	}
	if got := lookup(doc, "environments", "production", "servers"); !reflect.DeepEqual(got, []any{"node1", "node2"}) {
		t.Fatalf("environment servers = %#v, want sorted fragment servers", got)
	}
}

func TestMergeYAMLAddsPlaceholderServiceOnlyWhenNoneDeclared(t *testing.T) {
	merged, err := MergeYAML([]byte(BaseYAML), []byte("deployment:\n  build:\n    strategy: auto\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := lookup(decode(t, merged), "environments", "production", "services", PlaceholderService, "image"); got != "nginx:alpine" {
		t.Fatalf("placeholder service image = %v", got)
	}
	merged, err = MergeYAML([]byte(BaseYAML), []byte("environments:\n  production:\n    services:\n      web:\n        image: nginx\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := lookup(decode(t, merged), "environments", "production", "services", PlaceholderService); got != nil {
		t.Fatalf("placeholder added despite declared services: %v", got)
	}
}

func TestMergeYAMLKeepsExplicitEnvironmentServers(t *testing.T) {
	merged, err := MergeYAML([]byte(BaseYAML), []byte(`servers:
  node1:
    host: 203.0.113.11
  node2:
    host: 203.0.113.12
environments:
  production:
    servers: [node2]
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := lookup(decode(t, merged), "environments", "production", "servers"); !reflect.DeepEqual(got, []any{"node2"}) {
		t.Fatalf("environment servers = %#v", got)
	}
}

func TestSetupEnvAndAutoFill(t *testing.T) {
	home := SetupEnv(t)
	if got := os.Getenv("HOME"); got != home {
		t.Fatalf("HOME = %q, want %q", got, home)
	}
	if got := os.Getenv("TAKO_PRODUCTION_HOST"); got != "203.0.113.10" {
		t.Fatalf("TAKO_PRODUCTION_HOST = %q", got)
	}
	if _, err := os.Stat(os.Getenv("TAKO_SSH_KEY")); err != nil {
		t.Fatalf("ssh key fixture: %v", err)
	}

	const unset = "TAKO_CONFIGTEST_UNREGISTERED_VAR"
	_ = os.Unsetenv(unset)
	t.Cleanup(func() { _ = os.Unsetenv(unset) })
	content := []byte("value: ${" + unset + "}\nhost: ${TAKO_PRODUCTION_HOST}\n")
	if got := ReferencedVars(content); !reflect.DeepEqual(got, []string{unset, "TAKO_PRODUCTION_HOST"}) {
		t.Fatalf("ReferencedVars = %#v", got)
	}
	FillEnv(t, content)
	if got := os.Getenv(unset); got != AutoFillValue {
		t.Fatalf("%s = %q, want auto-fill", unset, got)
	}
	if got := os.Getenv("TAKO_PRODUCTION_HOST"); got != "203.0.113.10" {
		t.Fatalf("auto-fill overwrote a placeholder: %q", got)
	}
}

func TestMaterializePathsCreatesLocalFixturesOnly(t *testing.T) {
	dir := t.TempDir()
	MaterializePaths(t, dir, []byte(`environments:
  production:
    services:
      api:
        build: ./api
      web:
        build: ./web
        dockerfile: docker/Dockerfile.prod
        envFile: .env.web
      escape:
        build: ../outside
`))
	for _, path := range []string{"api/Dockerfile", "web/docker/Dockerfile.prod", ".env.web"} {
		if _, err := os.Stat(dir + "/" + path); err != nil {
			t.Fatalf("expected fixture %s: %v", path, err)
		}
	}
	if _, err := os.Stat(dir + "/../outside"); err == nil {
		t.Fatal("parent-escaping build path should not be created")
	}
}

func TestLoadAndValidateBaseWithService(t *testing.T) {
	SetupEnv(t)
	merged, err := MergeYAML([]byte(BaseYAML), []byte(`environments:
  production:
    services:
      web:
        image: nginx:alpine
        port: 80
        env:
          API_TOKEN: ${TAKO_CONFIGTEST_AUTOFILLED}
`))
	if err != nil {
		t.Fatal(err)
	}
	cfg, warnings, err := LoadAndValidate(t, t.TempDir(), "tako.yaml", merged)
	if err != nil {
		t.Fatalf("LoadAndValidate: %v\n%s", err, merged)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %#v", warnings)
	}
	if cfg.Project.Name != "docs-example" {
		t.Fatalf("project = %q", cfg.Project.Name)
	}
	if !strings.Contains(string(merged), "TAKO_CONFIGTEST_AUTOFILLED") {
		t.Fatal("merged doc lost env reference")
	}
}
