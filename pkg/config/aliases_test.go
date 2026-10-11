package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const aliasesTestConfigBase = `project:
  name: demo
  version: 1.0.0
servers:
  node-a:
    host: 10.0.0.1
    user: deploy
    password: sshpass
environments:
  production:
    servers: [node-a]
    services:
      web:
        image: nginx:alpine
`

func writeAliasesTestFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadConfigAcceptsAliasesYAMLAndJSON(t *testing.T) {
	yamlPath := writeAliasesTestFile(t, "tako.yaml", aliasesTestConfigBase+`aliases:
  console: [exec, -it, web, --, bin/rails, console]
  logsw: logs web --follow
`)
	cfg, err := LoadConfig(yamlPath)
	if err != nil {
		t.Fatalf("LoadConfig(yaml): %v", err)
	}
	console, err := AliasArguments(cfg.Aliases["console"])
	if err != nil || !reflect.DeepEqual(console, []string{"exec", "-it", "web", "--", "bin/rails", "console"}) {
		t.Fatalf("console = %v, %v", console, err)
	}
	if AliasForm(cfg.Aliases["console"]) != AliasFormList || AliasForm(cfg.Aliases["logsw"]) != AliasFormString {
		t.Fatalf("forms not preserved: %+v", cfg.Aliases)
	}

	jsonPath := writeAliasesTestFile(t, "tako.json", `{
  "project": {"name": "demo", "version": "1.0.0"},
  "servers": {"node-a": {"host": "10.0.0.1", "user": "deploy", "password": "sshpass"}},
  "environments": {"production": {"servers": ["node-a"], "services": {"web": {"image": "nginx:alpine"}}}},
  "aliases": {"console": ["exec", "-it", "web", "--", "sh"], "logsw": "logs web"}
}`)
	jsonCfg, err := LoadConfig(jsonPath)
	if err != nil {
		t.Fatalf("LoadConfig(json): %v", err)
	}
	if args, _ := AliasArguments(jsonCfg.Aliases["logsw"]); !reflect.DeepEqual(args, []string{"logs", "web"}) {
		t.Fatalf("json logsw = %v", args)
	}
}

func TestLoadConfigWithoutAliasesIsUnchanged(t *testing.T) {
	without, err := LoadConfig(writeAliasesTestFile(t, "tako.yaml", aliasesTestConfigBase))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if without.Aliases != nil {
		t.Fatalf("Aliases = %#v, want nil", without.Aliases)
	}
	with, err := LoadConfig(writeAliasesTestFile(t, "tako.yaml", aliasesTestConfigBase+"aliases:\n  ps2: [ps]\n"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	with.Aliases = nil
	if !reflect.DeepEqual(without, with) {
		t.Fatalf("aliases changed other config fields:\nwithout=%+v\nwith=%+v", without, with)
	}
}

func TestLoadConfigRejectsNonStringAliasValues(t *testing.T) {
	for name, value := range map[string]string{
		"number":     "  bad: 42\n",
		"map":        "  bad: {cmd: ps}\n",
		"list-item":  "  bad: [logs, 3]\n",
		"nested":     "  bad: [logs, [web]]\n",
		"null-value": "  bad:\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadConfig(writeAliasesTestFile(t, "tako.yaml", aliasesTestConfigBase+"aliases:\n"+value))
			if err == nil || !strings.Contains(err.Error(), "aliases.bad") {
				t.Fatalf("error = %v, want aliases.bad", err)
			}
		})
	}
	_, err := LoadConfig(writeAliasesTestFile(t, "tako.json", `{"project":{"name":"demo","version":"1"},"servers":{},"environments":{},"aliases":{"bad":7}}`))
	if err == nil || !strings.Contains(err.Error(), "aliases.bad") {
		t.Fatalf("json error = %v, want aliases.bad", err)
	}
}

func TestSplitAliasString(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{`logs web --follow`, []string{"logs", "web", "--follow"}},
		{"  exec\tweb \n -- sh ", []string{"exec", "web", "--", "sh"}},
		{`exec -it web -- sh -c "bin/rails console"`, []string{"exec", "-it", "web", "--", "sh", "-c", "bin/rails console"}},
		{`exec web -- echo 'a "b" $HOME'`, []string{"exec", "web", "--", "echo", `a "b" $HOME`}},
		{`exec web -- echo "say \"hi\" \\ \n"`, []string{"exec", "web", "--", "echo", `say "hi" \ \n`}},
		{`exec web -- echo a\ b \'c`, []string{"exec", "web", "--", "echo", "a b", "'c"}},
		{`exec web -- a;b | c && d $(x) * ~`, []string{"exec", "web", "--", "a;b", "|", "c", "&&", "d", "$(x)", "*", "~"}},
		{`exec web -- echo ""`, []string{"exec", "web", "--", "echo", ""}},
		{`exec web -- x"y"'z'`, []string{"exec", "web", "--", "xyz"}},
	}
	for _, tt := range tests {
		got, err := SplitAliasString(tt.in)
		if err != nil {
			t.Fatalf("SplitAliasString(%q) error: %v", tt.in, err)
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Fatalf("SplitAliasString(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}

	for in, want := range map[string]string{
		`exec web -- echo "hi`: "unterminated double quote",
		`exec web -- echo 'hi`: "unterminated single quote",
		`exec web \`:           "trailing backslash",
		"   ":                  "at least one argument",
		"":                     "at least one argument",
	} {
		if _, err := SplitAliasString(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("SplitAliasString(%q) error = %v, want %q", in, err, want)
		}
	}
}

func TestValidateAliases(t *testing.T) {
	valid := []AliasMap{
		{"db_shell-2": ListValue("exec", "db", "--", "psql")},
		{"9lives": StringValue("ps")},
		{"cfg": ListValue("exec", "web", "--", "mytool", "--config", "/etc/app.conf")},
		{"hk": ListValue("exec", "web", "--", "x", "--host-key-mode=tofu")},
		{"logs": ListValue("logs", "web")}, // shadowing is a CLI-side warning
		{"stg": ListValue("logs", "web", "-e", "staging")},
	}
	for _, aliases := range valid {
		if err := ValidateAliases(aliases); err != nil {
			t.Fatalf("ValidateAliases(%v) = %v, want nil", aliases, err)
		}
	}

	invalid := []struct {
		name    string
		aliases AliasMap
		want    string
	}{
		{"uppercase", AliasMap{"Console": ListValue("ps")}, `aliases.Console: invalid alias name "Console"`},
		{"leading dash", AliasMap{"-x": ListValue("ps")}, "aliases.-x: invalid alias name"},
		{"colon", AliasMap{"db:shell": ListValue("ps")}, "aliases.db:shell: invalid alias name"},
		{"too long", AliasMap{strings.Repeat("a", 64): ListValue("ps")}, "at most 63 characters"},
		{"reserved help", AliasMap{"help": ListValue("ps")}, `"help" is reserved`},
		{"reserved completion", AliasMap{"completion": ListValue("ps")}, "is reserved"},
		{"reserved complete", AliasMap{"__complete": ListValue("ps")}, "is reserved"},
		{"reserved completeNoDesc", AliasMap{"__completeNoDesc": ListValue("ps")}, "is reserved"},
		{"reserved aliases", AliasMap{"aliases": ListValue("ps")}, "is reserved"},
		{"empty list", AliasMap{"x": ListValue()}, "aliases.x: must contain at least one argument"},
		{"empty string", AliasMap{"x": StringValue("  ")}, "aliases.x: must contain at least one argument"},
		{"unterminated", AliasMap{"x": StringValue(`exec web -- echo "hi`)}, "aliases.x: unterminated double quote"},
		{"empty first", AliasMap{"x": ListValue("", "ps")}, "first argument must name a Tako command"},
		{"flag first", AliasMap{"x": ListValue("-e", "staging", "ps")}, "not the flag"},
		{"leading tako", AliasMap{"x": StringValue("tako exec web -- env")}, `omit the leading "tako"`},
		{"alias to alias", AliasMap{"a": ListValue("logs", "web"), "b": ListValue("a", "--follow")}, "aliases cannot reference other aliases"},
		{"config flag", AliasMap{"x": ListValue("deploy", "--config", "other.yaml")}, "--config is not allowed"},
		{"config equals", AliasMap{"x": ListValue("deploy", "--config=other.yaml")}, "--config is not allowed"},
		{"host key mode first", AliasMap{"x": ListValue("--host-key-mode", "tofu", "deploy")}, "not the flag"},
		{"host key mode", AliasMap{"x": ListValue("deploy", "--host-key-mode", "tofu")}, "--host-key-mode is not allowed"},
		{"host key mode equals", AliasMap{"x": ListValue("deploy", "--host-key-mode=tofu")}, "--host-key-mode is not allowed"},
		{"control chars", AliasMap{"x": ListValue("exec", "web\n")}, "control characters"},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateAliases(tt.aliases)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLoadConfigValidatesAliases(t *testing.T) {
	_, err := LoadConfig(writeAliasesTestFile(t, "tako.yaml", aliasesTestConfigBase+"aliases:\n  help: [ps]\n"))
	if err == nil || !strings.Contains(err.Error(), `"help" is reserved`) {
		t.Fatalf("error = %v, want reserved name rejection", err)
	}
}

func TestLoadConfigRejectsEnvReferenceInAliasBeforeExpansion(t *testing.T) {
	t.Setenv("TAKO_ALIAS_TEST_UNSET", "")
	os.Unsetenv("TAKO_ALIAS_TEST_UNSET")
	for name, content := range map[string]string{
		"tako.yaml": aliasesTestConfigBase + "aliases:\n  logsvc: [logs, \"${TAKO_ALIAS_TEST_UNSET}\"]\n",
		"tako.json": `{"project":{"name":"demo","version":"1"},"servers":{},"environments":{},"aliases":{"logsvc":"logs ${TAKO_ALIAS_TEST_UNSET}"}}`,
	} {
		_, err := LoadConfig(writeAliasesTestFile(t, name, content))
		if err == nil || !strings.Contains(err.Error(), "aliases.logsvc: aliases do not support ${...} references") {
			t.Fatalf("%s: error = %v, want alias env-reference rejection", name, err)
		}
		if strings.Contains(err.Error(), "missing environment variable") {
			t.Fatalf("%s: missing-variable error masked the alias error: %v", name, err)
		}
		if _, err := LoadAliases(writeAliasesTestFile(t, name, content)); err == nil || !strings.Contains(err.Error(), "${...}") {
			t.Fatalf("%s: LoadAliases error = %v", name, err)
		}
	}
}

func TestLoadAliasesReadsOnlyAliasesSection(t *testing.T) {
	// The servers section is invalid and references an unset variable; the
	// lightweight loader must still return the aliases.
	content := `project:
  name: Not_Valid
servers:
  node-a:
    host: ${TAKO_ALIAS_TEST_UNSET_HOST}
aliases:
  logsw: [logs, web]
  console: exec -it web -- sh
`
	path := writeAliasesTestFile(t, "tako.yaml", content)
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("LoadConfig should reject the invalid config")
	}
	loaded, err := LoadAliases(path)
	if err != nil {
		t.Fatalf("LoadAliases: %v", err)
	}
	if loaded.ConfigPath != path || !reflect.DeepEqual(loaded.Aliases.Names(), []string{"console", "logsw"}) {
		t.Fatalf("loaded = %+v", loaded)
	}

	jsonPath := writeAliasesTestFile(t, "tako.json", `{"project":{"name":"x"},"servers":"nope","aliases":{"logsw":["logs","web"]}}`)
	jsonLoaded, err := LoadAliases(jsonPath)
	if err != nil || len(jsonLoaded.Aliases) != 1 {
		t.Fatalf("LoadAliases(json) = %+v, %v", jsonLoaded, err)
	}

	none, err := LoadAliases(writeAliasesTestFile(t, "tako.yaml", aliasesTestConfigBase))
	if err != nil || len(none.Aliases) != 0 {
		t.Fatalf("LoadAliases(no aliases) = %+v, %v", none, err)
	}
}

func TestLoadAliasesErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "tako.yaml")
	_, err := LoadAliases(missing)
	if !IsConfigNotFound(err) {
		t.Fatalf("missing file error = %v, want ConfigNotFoundError", err)
	}

	_, err = LoadAliases(writeAliasesTestFile(t, "tako.yaml", "aliases:\n  console: exec web -- echo \"hi\n"))
	if err == nil || !strings.Contains(err.Error(), "aliases.console: unterminated double quote") {
		t.Fatalf("invalid alias error = %v", err)
	}
	if IsConfigNotFound(err) {
		t.Fatal("validation error must not be a not-found error")
	}
}

func TestSaveConfigRoundTripPreservesAliasForms(t *testing.T) {
	for _, name := range []string{"tako.yaml", "tako.json"} {
		t.Run(name, func(t *testing.T) {
			source, err := LoadConfig(writeAliasesTestFile(t, "tako.yaml", aliasesTestConfigBase+`aliases:
  console: [exec, -it, web, --, sh, -c, "bin/rails console"]
  logsw: logs web --follow
`))
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			out := filepath.Join(t.TempDir(), name)
			if err := SaveConfig(out, source); err != nil {
				// Directory fsync is unsupported on Windows; fall back to
				// SaveConfig's own marshalers so the round trip is still
				// exercised.
				if runtime.GOOS != "windows" || !strings.Contains(err.Error(), "sync publication directory") {
					t.Fatalf("SaveConfig: %v", err)
				}
				var data []byte
				if strings.HasSuffix(name, ".json") {
					data, err = json.MarshalIndent(source, "", "  ")
				} else {
					data, err = yaml.Marshal(source)
				}
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				if err := os.WriteFile(out, data, 0600); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			reloaded, err := LoadConfig(out)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}
			if !reflect.DeepEqual(source.Aliases, reloaded.Aliases) {
				t.Fatalf("aliases changed on round trip:\nbefore=%#v\nafter=%#v", source.Aliases, reloaded.Aliases)
			}
			if AliasForm(reloaded.Aliases["console"]) != AliasFormList || AliasForm(reloaded.Aliases["logsw"]) != AliasFormString {
				t.Fatalf("forms not preserved: %#v", reloaded.Aliases)
			}
		})
	}
}
