package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const extendsTestHeader = `project:
  name: demo
  version: 1.0.0
servers:
  node-a:
    host: 192.0.2.10
    user: root
    password: test-password
  node-b:
    host: 192.0.2.11
    user: root
    password: test-password
    labels:
      tier: edge
`

func writeExtendsTestConfig(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func loadExtendsTestConfig(t *testing.T, environments string) (*Config, error) {
	t.Helper()
	return LoadConfig(writeExtendsTestConfig(t, "tako.yaml", extendsTestHeader+environments))
}

func mustLoadExtendsTestConfig(t *testing.T, environments string) *Config {
	t.Helper()
	cfg, err := loadExtendsTestConfig(t, environments)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

// ---- 1. top-level x- keys ----

func TestXKeysYAMLAnchorSharedByTwoEnvironments(t *testing.T) {
	cfg, err := LoadConfig(writeExtendsTestConfig(t, "tako.yaml", `x-web: &web
  image: app:1
  port: 3000
`+extendsTestHeader+`environments:
  production:
    servers: [node-a]
    services:
      web: *web
  staging:
    servers: [node-b]
    services:
      web: *web
`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	for _, env := range []string{"production", "staging"} {
		web := cfg.Environments[env].Services["web"]
		if web.Image != "app:1" || web.Port != 3000 {
			t.Fatalf("%s web = %+v, want app:1 on 3000", env, web)
		}
	}
}

func TestXKeysJSONExtensionKeyIsIgnored(t *testing.T) {
	cfg, err := LoadConfig(writeExtendsTestConfig(t, "tako.json", `{
  "x-notes": {"owner": "ops"},
  "project": {"name": "demo", "version": "1.0.0"},
  "servers": {"node-a": {"host": "192.0.2.10", "user": "root", "password": "test-password"}},
  "environments": {"production": {"servers": ["node-a"], "services": {"web": {"image": "nginx:alpine"}}}}
}`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Environments["production"].Services["web"].Image != "nginx:alpine" {
		t.Fatalf("environments = %+v", cfg.Environments)
	}
}

func TestXKeysNonPrefixedUnknownKeyStillRejected(t *testing.T) {
	body := extendsTestHeader + `extra: true
environments:
  production:
    servers: [node-a]
    services:
      web:
        image: nginx:alpine
`
	_, plainErr := LoadConfig(writeExtendsTestConfig(t, "tako.yaml", body))
	want := "failed to parse YAML config: yaml: unmarshal errors:\n  line 15: field extra not found in type config.Config"
	if plainErr == nil || plainErr.Error() != want {
		t.Fatalf("plain error = %v, want %q", plainErr, want)
	}

	// The same typo next to an x- key takes the resolving path and must
	// still report the original line.
	_, err := LoadConfig(writeExtendsTestConfig(t, "tako.yaml", body+"x-notes: hi\n"))
	if err == nil || err.Error() != want {
		t.Fatalf("x- error = %v, want %q", err, want)
	}
}

func TestXKeysNestedExtensionKeysRejected(t *testing.T) {
	for name, environments := range map[string]string{
		"service": `environments:
  production:
    servers: [node-a]
    services:
      web:
        image: nginx:alpine
        x-notes: hi
`,
		"environment": `environments:
  production:
    servers: [node-a]
    x-notes: hi
    services:
      web:
        image: nginx:alpine
`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadExtendsTestConfig(t, environments)
			if err == nil || !strings.Contains(err.Error(), "field x-notes not found") {
				t.Fatalf("err = %v, want unknown field x-notes", err)
			}
			// Same with a top-level x- key forcing the resolving path.
			_, err = loadExtendsTestConfig(t, "x-top: 1\n"+environments)
			if err == nil || !strings.Contains(err.Error(), "field x-notes not found") {
				t.Fatalf("resolving path err = %v, want unknown field x-notes", err)
			}
		})
	}
}

// ---- 1.2 fast path ----

func TestFastPathReturnsInputUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		isJSON bool
		data   string
	}{
		{"yaml", false, extendsTestHeader + "environments:\n  production:\n    servers: [node-a]\n    services:\n      web:\n        image: nginx\n"},
		{"json", true, `{"project": {"name": "demo"}, "environments": {"production": {"servers": ["a"]}}}`},
		{"yaml anchors without x-", false, "base: &b {a: 1}\nother: *b\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(tc.data)
			out, meta, err := resolveConfigDocument(data, tc.isJSON)
			if err != nil || meta != nil {
				t.Fatalf("resolveConfigDocument = meta %v err %v, want fast path", meta, err)
			}
			if &out[0] != &data[0] || !bytes.Equal(out, data) {
				t.Fatal("fast path must return the input bytes untouched")
			}
		})
	}
}

func TestFastPathUnknownFieldReportsOriginalLine(t *testing.T) {
	_, err := loadExtendsTestConfig(t, `environments:
  production:
    servers: [node-a]
    servrs: [node-a]
    services:
      web:
        image: nginx:alpine
`)
	if err == nil || !strings.Contains(err.Error(), "line 18: field servrs not found in type config.EnvironmentConfig") {
		t.Fatalf("err = %v, want original line 18", err)
	}
}

// ---- 2. merge engine ----

type mergeCase struct {
	name     string
	parent   string
	child    string
	want     string
	wantKeys []string          // top-level key order of the result, when set
	wantProv map[string]string // provenance of the result, when set
}

var environmentMergeCases = []mergeCase{
	{
		name:     "mapping merge",
		parent:   `{"services": {"web": {"image": "app:1", "replicas": 1, "env": {"LOG_LEVEL": "info", "REGION": "eu"}}}}`,
		child:    `{"services": {"web": {"replicas": 3, "env": {"LOG_LEVEL": "debug"}}}}`,
		want:     `{"services": {"web": {"image": "app:1", "replicas": 3, "env": {"LOG_LEVEL": "debug", "REGION": "eu"}}}}`,
		wantProv: map[string]string{"services.web.image": "parent", "services.web.replicas": "child", "services.web.env.LOG_LEVEL": "child", "services.web.env.REGION": "parent"},
	},
	{
		name:     "scalar replace",
		parent:   `{"servers": ["a"], "services": {"web": {"image": "app:1"}}}`,
		child:    `{"services": {"web": {"image": "app:2"}}}`,
		want:     `{"servers": ["a"], "services": {"web": {"image": "app:2"}}}`,
		wantProv: map[string]string{"servers": "parent", "services.web.image": "child"},
	},
	{
		name:     "sequence replace",
		parent:   `{"servers": ["a", "b"], "services": {"web": {"volumes": ["data:/var/lib/app"]}}}`,
		child:    `{"servers": ["c"], "services": {"web": {"volumes": ["scratch:/tmp/app"]}}}`,
		want:     `{"servers": ["c"], "services": {"web": {"volumes": ["scratch:/tmp/app"]}}}`,
		wantProv: map[string]string{"servers": "child", "services.web.volumes": "child"},
	},
	{
		name:     "kind mismatch scalar to mapping",
		parent:   `{"services": {"web": {"build": "."}}}`,
		child:    `{"services": {"web": {"build": {"context": ".", "target": "prod"}}}}`,
		want:     `{"services": {"web": {"build": {"context": ".", "target": "prod"}}}}`,
		wantProv: map[string]string{"services.web.build.context": "child", "services.web.build.target": "child"},
	},
	{
		name:     "kind mismatch mapping to scalar",
		parent:   `{"services": {"web": {"build": {"context": ".", "target": "prod"}}}}`,
		child:    `{"services": {"web": {"build": "./web"}}}`,
		want:     `{"services": {"web": {"build": "./web"}}}`,
		wantProv: map[string]string{"services.web.build": "child"},
	},
	{
		name:     "kind mismatch string to list",
		parent:   `{"services": {"web": {"command": "serve --port 80"}}}`,
		child:    `{"services": {"web": {"command": ["serve", "--port", "8080"]}}}`,
		want:     `{"services": {"web": {"command": ["serve", "--port", "8080"]}}}`,
		wantProv: map[string]string{"services.web.command": "child"},
	},
	{
		name:     "null removes inherited service",
		parent:   `{"services": {"web": {"image": "app:1"}, "worker": {"image": "app:1", "command": "work"}}}`,
		child:    `{"services": {"worker": null}}`,
		want:     `{"services": {"web": {"image": "app:1"}}}`,
		wantProv: map[string]string{"services.web.image": "parent"},
	},
	{
		name:     "null switches servers to serverSelector",
		parent:   `{"servers": ["a"], "services": {"web": {"image": "app:1"}}}`,
		child:    `{"servers": null, "serverSelector": {"labels": {"tier": "edge"}}}`,
		want:     `{"services": {"web": {"image": "app:1"}}, "serverSelector": {"labels": {"tier": "edge"}}}`,
		wantKeys: []string{"services", "serverSelector"},
		wantProv: map[string]string{"services.web.image": "parent", "serverSelector.labels.tier": "child"},
	},
	{
		name:   "null for an absent key is dropped",
		parent: `{"servers": ["a"]}`,
		child:  `{"proxy": null}`,
		want:   `{"servers": ["a"]}`,
	},
	{
		name:     "key order keeps parent keys then new child keys",
		parent:   `{"servers": ["a"], "services": {"web": {"image": "app:1"}}, "proxy": {"placement": {"strategy": "any"}}}`,
		child:    `{"secretSources": [], "services": {"web": {"port": 80}}, "servers": ["b"]}`,
		want:     `{"servers": ["b"], "services": {"web": {"image": "app:1", "port": 80}}, "proxy": {"placement": {"strategy": "any"}}, "secretSources": []}`,
		wantKeys: []string{"servers", "services", "proxy", "secretSources"},
	},
	{
		name:     "child adds a service",
		parent:   `{"services": {"web": {"image": "app:1"}}}`,
		child:    `{"services": {"debug-ui": {"image": "debug:1"}}}`,
		want:     `{"services": {"web": {"image": "app:1"}, "debug-ui": {"image": "debug:1"}}}`,
		wantProv: map[string]string{"services.web.image": "parent", "services.debug-ui.image": "child"},
	},
}

type mergeAdapter struct {
	name   string
	parse  func(t *testing.T, text string) *envNode
	encode func(t *testing.T, node *envNode) any
	decode func(t *testing.T, text string) any
}

var mergeAdapters = []mergeAdapter{
	{
		name: "yaml",
		parse: func(t *testing.T, text string) *envNode {
			var document yaml.Node
			if err := yaml.Unmarshal([]byte(text), &document); err != nil {
				t.Fatal(err)
			}
			return yamlToEnvNode(document.Content[0])
		},
		encode: func(t *testing.T, node *envNode) any {
			data, err := yaml.Marshal(envNodeToYAML(node))
			if err != nil {
				t.Fatal(err)
			}
			var out any
			if err := yaml.Unmarshal(data, &out); err != nil {
				t.Fatal(err)
			}
			return out
		},
		decode: func(t *testing.T, text string) any {
			var out any
			if err := yaml.Unmarshal([]byte(text), &out); err != nil {
				t.Fatal(err)
			}
			return out
		},
	},
	{
		name: "json",
		parse: func(t *testing.T, text string) *envNode {
			return jsonToEnvNode(json.RawMessage(text))
		},
		encode: func(t *testing.T, node *envNode) any {
			var buf bytes.Buffer
			writeEnvNodeJSON(&buf, node)
			var out any
			if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
				t.Fatalf("re-encoded JSON is invalid: %v\n%s", err, buf.String())
			}
			return out
		},
		decode: func(t *testing.T, text string) any {
			var out any
			if err := json.Unmarshal([]byte(text), &out); err != nil {
				t.Fatal(err)
			}
			return out
		},
	},
}

func TestEnvironmentMergeTable(t *testing.T) {
	for _, adapter := range mergeAdapters {
		for _, tc := range environmentMergeCases {
			t.Run(adapter.name+"/"+tc.name, func(t *testing.T) {
				parent := adapter.parse(t, tc.parent)
				child := adapter.parse(t, tc.child)
				prov := map[string]string{}
				recordValueProvenance(parent, "", "parent", prov)
				merged := mergeEnvironmentNodes(parent, child, "", "child", prov)

				got := adapter.encode(t, merged)
				want := adapter.decode(t, tc.want)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("merged = %#v, want %#v", got, want)
				}
				if tc.wantKeys != nil && !reflect.DeepEqual(merged.keys, tc.wantKeys) {
					t.Fatalf("key order = %v, want %v", merged.keys, tc.wantKeys)
				}
				for path, owner := range tc.wantProv {
					if prov[path] != owner {
						t.Fatalf("provenance[%s] = %q, want %q (all: %v)", path, prov[path], owner, prov)
					}
				}
				if tc.wantProv != nil && len(prov) != len(tc.wantProv) {
					t.Fatalf("provenance = %v, want exactly %v", prov, tc.wantProv)
				}
				// The parent must not be mutated by the merge.
				if again := adapter.encode(t, parent); !reflect.DeepEqual(again, adapter.decode(t, tc.parent)) {
					t.Fatalf("parent mutated: %#v", again)
				}
			})
		}
	}
}

func TestEnvironmentMergeJSONKeepsNumberLiterals(t *testing.T) {
	parent := jsonToEnvNode(json.RawMessage(`{"big": 123456789012345678901234567890, "float": 1.000000000000000000001, "exp": 1e400}`))
	child := jsonToEnvNode(json.RawMessage(`{"small": 0.1}`))
	merged := mergeEnvironmentNodes(parent, child, "", "child", map[string]string{})
	var buf bytes.Buffer
	writeEnvNodeJSON(&buf, merged)
	want := `{"big":123456789012345678901234567890,"float":1.000000000000000000001,"exp":1e400,"small":0.1}`
	if buf.String() != want {
		t.Fatalf("encoded = %s, want %s", buf.String(), want)
	}
}

func TestEnvironmentMergeYAMLFlattensMergeKeys(t *testing.T) {
	cfg := mustLoadExtendsTestConfig(t, `x-defaults: &defaults
  image: app:1
  port: 3000
  env:
    LOG_LEVEL: info
environments:
  production:
    servers: [node-a]
    services:
      web:
        <<: *defaults
        replicas: 2
  staging:
    extends: production
    servers: [node-b]
    services:
      web:
        <<: *defaults
        image: app:2
`)
	web := cfg.Environments["staging"].Services["web"]
	if web.Image != "app:2" || web.Port != 3000 || web.Replicas != 2 || web.Env["LOG_LEVEL"] != "info" {
		t.Fatalf("staging web = %+v", web)
	}
}

// ---- 3. extends resolution ----

func TestEnvironmentExtendsChildInheritsParentServices(t *testing.T) {
	cfg := mustLoadExtendsTestConfig(t, `environments:
  production:
    servers: [node-a]
    services:
      web:
        image: app:1
        port: 3000
  staging:
    extends: production
    servers: [node-b]
`)
	staging := cfg.Environments["staging"]
	if !reflect.DeepEqual(staging.Servers, []string{"node-b"}) || staging.Services["web"].Image != "app:1" || staging.Services["web"].Port != 3000 {
		t.Fatalf("staging = %+v", staging)
	}
	production := cfg.Environments["production"]
	if !reflect.DeepEqual(production.Servers, []string{"node-a"}) || production.Extends != "" || staging.Extends != "" {
		t.Fatalf("production = %+v", production)
	}
}

func TestEnvironmentExtendsSpecScenarios(t *testing.T) {
	cfg := mustLoadExtendsTestConfig(t, `environments:
  production:
    servers: [node-a]
    services:
      web:
        build: .
        port: 3000
        replicas: 1
        volumes: [data:/var/lib/app]
        env:
          LOG_LEVEL: info
          REGION: eu
      worker:
        image: app:1
        command: work
  staging:
    extends: production
    servers: null
    serverSelector:
      labels:
        tier: edge
    services:
      worker: null
      web:
        build:
          context: .
          target: prod
        replicas: 3
        volumes: [scratch:/tmp/app]
        env:
          LOG_LEVEL: debug
      debug-ui:
        image: debug:1
`)
	staging := cfg.Environments["staging"]
	if len(staging.Servers) != 0 || staging.ServerSelector == nil || staging.ServerSelector.Labels["tier"] != "edge" {
		t.Fatalf("staging servers = %v selector = %+v", staging.Servers, staging.ServerSelector)
	}
	if _, ok := staging.Services["worker"]; ok {
		t.Fatal("worker should be removed by null")
	}
	if _, ok := staging.Services["debug-ui"]; !ok {
		t.Fatal("debug-ui should be added")
	}
	web := staging.Services["web"]
	if web.Replicas != 3 || web.Port != 3000 || web.BuildTarget != "prod" || !reflect.DeepEqual(web.Volumes, []string{"scratch:/tmp/app"}) {
		t.Fatalf("staging web = %+v", web)
	}
	if !reflect.DeepEqual(web.Env, map[string]string{"LOG_LEVEL": "debug", "REGION": "eu"}) {
		t.Fatalf("staging web env = %v", web.Env)
	}
	if prod := cfg.Environments["production"]; len(prod.Services) != 2 || prod.Services["web"].Replicas != 1 {
		t.Fatalf("production changed: %+v", prod)
	}
}

func TestEnvironmentExtendsNullOutsideExtendsKeepsMeaning(t *testing.T) {
	cfg := mustLoadExtendsTestConfig(t, `x-note: forces the resolving path
environments:
  production:
    servers: [node-a]
    proxy: null
    services:
      web:
        image: app:1
        env: null
`)
	production := cfg.Environments["production"]
	if production.Proxy != nil || production.Services["web"].Env != nil {
		t.Fatalf("production = %+v", production)
	}
}

func TestEnvironmentExtendsThreeLevelChain(t *testing.T) {
	cfg := mustLoadExtendsTestConfig(t, `environments:
  a:
    abstract: true
    servers: [node-a]
    services:
      web:
        image: app:1
        replicas: 1
  b:
    extends: a
    services:
      web:
        replicas: 2
  c:
    extends: b
`)
	if got := cfg.Environments["c"].Services["web"].Replicas; got != 2 {
		t.Fatalf("c replicas = %d, want 2", got)
	}
	if got := cfg.EnvironmentExtendsChain("c"); !reflect.DeepEqual(got, []string{"b", "a"}) {
		t.Fatalf("chain = %v", got)
	}
}

func TestEnvironmentExtendsErrors(t *testing.T) {
	chain := func(n int) string {
		var b strings.Builder
		b.WriteString("environments:\n")
		for i := 0; i <= n; i++ {
			fmt.Fprintf(&b, "  e%d:\n", i)
			if i < n {
				fmt.Fprintf(&b, "    extends: e%d\n", i+1)
			} else {
				b.WriteString("    servers: [node-a]\n    services:\n      web:\n        image: app:1\n")
			}
		}
		return b.String()
	}
	for _, tc := range []struct {
		name         string
		environments string
		want         string
	}{
		{"cycle", "environments:\n  a:\n    extends: b\n  b:\n    extends: a\n", "invalid config: environment extends cycle: a -> b -> a"},
		{"self reference", "environments:\n  a:\n    extends: a\n", "invalid config: environment extends cycle: a -> a"},
		{"unknown target", "environments:\n  production:\n    servers: [node-a]\n    services:\n      web:\n        image: app:1\n  staging:\n    extends: prod\n", `invalid config: environment staging extends unknown environment "prod"`},
		{"non-string extends", "environments:\n  staging:\n    extends: [production]\n", "invalid config: environment staging: line 17: extends must be the name of another environment"},
		{"non-bool abstract", "environments:\n  base:\n    abstract: yes please\n", "invalid config: environment base: line 17: abstract must be true or false"},
		{"too deep", chain(9), "invalid config: environment e0 extends chain exceeds the maximum depth of 8: e0 -> e1 -> e2 -> e3 -> e4 -> e5 -> e6 -> e7 -> e8 -> e9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadExtendsTestConfig(t, tc.environments)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	t.Run("depth 8 accepted", func(t *testing.T) {
		cfg := mustLoadExtendsTestConfig(t, chain(8))
		if got := len(cfg.EnvironmentExtendsChain("e0")); got != 8 {
			t.Fatalf("e0 chain length = %d, want 8", got)
		}
	})
}

func TestEnvironmentExtendsTypoInParentNamesDeclaringEnvironment(t *testing.T) {
	_, err := loadExtendsTestConfig(t, `environments:
  base:
    abstract: true
    services:
      web:
        image: app:1
        replcas: 2
  production:
    extends: base
    servers: [node-a]
`)
	if err == nil || !strings.Contains(err.Error(), "environment base") || !strings.Contains(err.Error(), "replcas") {
		t.Fatalf("err = %v, want environment base and replcas", err)
	}
	if strings.Contains(err.Error(), "production") {
		t.Fatalf("err = %v, must not attribute the typo to production", err)
	}

	_, err = loadExtendsTestConfig(t, `environments:
  base:
    abstract: true
    servrs: [node-a]
  production:
    extends: base
    servers: [node-a]
    services:
      web:
        image: app:1
`)
	want := "failed to parse YAML config: environment base: yaml: unmarshal errors:\n  line 18: field servrs not found in type config.EnvironmentConfig"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

func TestEnvironmentExtendsJSON(t *testing.T) {
	cfg, err := LoadConfig(writeExtendsTestConfig(t, "tako.json", `{
  "project": {"name": "demo", "version": "1.0.0"},
  "servers": {"node-a": {"host": "192.0.2.10", "user": "root", "password": "test-password"}},
  "environments": {
    "base": {"abstract": true, "services": {"web": {"image": "app:1", "replicas": 1}, "worker": {"image": "app:1"}}},
    "production": {"extends": "base", "servers": ["node-a"], "services": {"web": {"replicas": 2}, "worker": null}}
  }
}`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if _, ok := cfg.Environments["base"]; ok {
		t.Fatal("abstract base must not be resolved into Environments")
	}
	production := cfg.Environments["production"]
	if production.Services["web"].Replicas != 2 || production.Services["web"].Image != "app:1" || len(production.Services) != 1 {
		t.Fatalf("production = %+v", production)
	}

	_, err = LoadConfig(writeExtendsTestConfig(t, "tako.json", `{
  "project": {"name": "demo", "version": "1.0.0"},
  "servers": {"node-a": {"host": "192.0.2.10", "user": "root", "password": "test-password"}},
  "environments": {
    "base": {"abstract": true, "servrs": ["node-a"]},
    "production": {"extends": "base", "servers": ["node-a"], "services": {"web": {"image": "app:1"}}}
  }
}`))
	if err == nil || !strings.Contains(err.Error(), `failed to parse JSON config: environment base: json: unknown field "servrs"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestSaveConfigOmitsExtendsAndAbstract(t *testing.T) {
	cfg := mustLoadExtendsTestConfig(t, `environments:
  base:
    abstract: true
    services:
      web:
        image: app:1
  production:
    extends: base
    servers: [node-a]
`)
	// SaveConfig writes exactly these encodings; marshal directly so the
	// check does not depend on directory fsync support.
	yamlData, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	jsonData, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"yaml": yamlData, "json": jsonData} {
		if strings.Contains(string(data), "extends") || strings.Contains(string(data), "abstract") {
			t.Fatalf("%s contains extends/abstract:\n%s", name, data)
		}
	}
}

func TestEnvironmentResolutionAccessors(t *testing.T) {
	cfg := mustLoadExtendsTestConfig(t, `environments:
  base:
    abstract: true
    services:
      web:
        image: app:1
        replicas: 2
  production:
    extends: base
    servers: [node-a]
  staging:
    extends: production
    servers: [node-b]
    services:
      web:
        replicas: 1
`)
	if !cfg.IsAbstractEnvironment("base") || cfg.IsAbstractEnvironment("production") || cfg.IsAbstractEnvironment("missing") {
		t.Fatal("IsAbstractEnvironment mismatch")
	}
	if got := cfg.EnvironmentExtendsChain("staging"); !reflect.DeepEqual(got, []string{"production", "base"}) {
		t.Fatalf("staging chain = %v", got)
	}
	if got := cfg.EnvironmentExtendsChain("base"); got != nil {
		t.Fatalf("base chain = %v", got)
	}
	for path, want := range map[string]string{
		"services.web.replicas": "staging",
		"services.web.image":    "base",
		"servers":               "staging",
		"services.web":          "staging",
		"services.missing":      "",
	} {
		if got := cfg.EnvironmentValueSource("staging", path); got != want {
			t.Fatalf("EnvironmentValueSource(staging, %s) = %q, want %q", path, got, want)
		}
	}
	if got := cfg.EnvironmentValueSource("production", "services.web.replicas"); got != "base" {
		t.Fatalf("production replicas source = %q", got)
	}

	plain := &Config{}
	if plain.EnvironmentExtendsChain("x") != nil || plain.EnvironmentValueSource("x", "servers") != "" || plain.IsAbstractEnvironment("x") {
		t.Fatal("accessors on a config without inheritance must be empty")
	}
}

func TestAbstractEnvironmentSelectionAndDefaults(t *testing.T) {
	cfg := mustLoadExtendsTestConfig(t, `environments:
  base:
    abstract: true
    services:
      web:
        image: app:1
  staging:
    extends: base
    servers: [node-a]
  qa:
    extends: base
    servers: [node-b]
`)
	_, err := cfg.GetEnvironment("base")
	if err == nil || err.Error() != "environment 'base' is abstract and cannot be deployed; use one of: qa, staging" {
		t.Fatalf("GetEnvironment(base) err = %v", err)
	}
	if _, err := cfg.GetEnvironment("nope"); err == nil || err.Error() != "environment 'nope' not found" {
		t.Fatalf("GetEnvironment(nope) err = %v", err)
	}

	single := mustLoadExtendsTestConfig(t, `environments:
  base:
    abstract: true
    services:
      web:
        image: app:1
  staging:
    extends: base
    servers: [node-a]
`)
	if name, implicit := single.ResolveDefaultEnvironment(); name != "staging" || implicit {
		t.Fatalf("default = %q implicit=%t, want staging", name, implicit)
	}

	_, err = loadExtendsTestConfig(t, `environments:
  base:
    abstract: true
    services:
      web:
        image: app:1
`)
	if err == nil || err.Error() != "invalid config: at least one non-abstract environment must be configured" {
		t.Fatalf("all-abstract err = %v", err)
	}
	_, err = loadExtendsTestConfig(t, "environments: {}\n")
	if err == nil || err.Error() != "invalid config: at least one environment must be configured" {
		t.Fatalf("empty err = %v", err)
	}
}

func TestEnvironmentExtendsValidationErrorNamesChain(t *testing.T) {
	_, err := loadExtendsTestConfig(t, `environments:
  production:
    servers: [node-a]
    services:
      web:
        image: app:1
        port: 80
        proxy:
          domain: app.example.com
  staging:
    extends: production
    servers: [missing]
`)
	want := "invalid config: environment staging: server 'missing' not found in servers section (environment staging extends production)"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}

	_, err = loadExtendsTestConfig(t, `environments:
  production:
    servers: [missing]
    services:
      web:
        image: app:1
`)
	if want := "invalid config: environment production: server 'missing' not found in servers section"; err == nil || err.Error() != want {
		t.Fatalf("plain err = %v, want %q", err, want)
	}
}

func TestUnusedAbstractEnvironmentWarns(t *testing.T) {
	cfg := mustLoadExtendsTestConfig(t, `environments:
  old-base:
    abstract: true
    services:
      web:
        image: app:1
  production:
    servers: [node-a]
    services:
      web:
        image: app:1
`)
	warnings := ValidationWarnings(cfg)
	if len(warnings) != 1 || warnings[0].Environment != "old-base" || warnings[0].Field != "abstract" ||
		warnings[0].Message != "environment old-base is abstract and no environment extends it; it has no effect" {
		t.Fatalf("warnings = %+v", warnings)
	}

	used := mustLoadExtendsTestConfig(t, `environments:
  base:
    abstract: true
    services:
      web:
        image: app:1
  production:
    extends: base
    servers: [node-a]
`)
	if warnings := ValidationWarnings(used); len(warnings) != 0 {
		t.Fatalf("warnings = %+v, want none", warnings)
	}
}

// ---- 5. deterministic default environment ----

func TestResolveDefaultEnvironment(t *testing.T) {
	envs := func(names ...string) *Config {
		cfg := &Config{Environments: map[string]EnvironmentConfig{}}
		for _, name := range names {
			cfg.Environments[name] = EnvironmentConfig{}
		}
		return cfg
	}
	for _, tc := range []struct {
		name         string
		cfg          *Config
		want         string
		wantImplicit bool
	}{
		{"none", envs(), "", false},
		{"only one", envs("staging"), "staging", false},
		{"production present", envs("staging", "production", "dev"), "production", false},
		{"alphabetical fallback", envs("staging", "dev"), "dev", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name, implicit := tc.cfg.ResolveDefaultEnvironment()
			if name != tc.want || implicit != tc.wantImplicit {
				t.Fatalf("ResolveDefaultEnvironment = %q, %t; want %q, %t", name, implicit, tc.want, tc.wantImplicit)
			}
			if got := tc.cfg.GetDefaultEnvironment(); got != tc.want {
				t.Fatalf("GetDefaultEnvironment = %q, want %q", got, tc.want)
			}
		})
	}

	cfg := envs("staging", "dev", "qa", "preview")
	for i := 0; i < 100; i++ {
		if name, _ := cfg.ResolveDefaultEnvironment(); name != "dev" {
			t.Fatalf("run %d picked %q, want dev", i, name)
		}
	}
}
