package config

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigAcceptsHooksBlock(t *testing.T) {
	cfg, err := LoadConfig(writeAliasesTestFile(t, "tako.yaml", aliasesTestConfigBase+`hooks:
  path: deploy/hooks
  timeout: 2m
  output: quiet
  passEnv: [GITHUB_TOKEN, SLACK_*]
  commands:
    pre-deploy: [make, check]
`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Hooks == nil {
		t.Fatal("Hooks = nil")
	}
	if cfg.Hooks.HooksDir() != "deploy/hooks" || cfg.Hooks.TimeoutDuration() != 2*time.Minute || cfg.Hooks.OutputMode() != HookOutputQuiet {
		t.Fatalf("hooks = %+v", cfg.Hooks)
	}
	if !reflect.DeepEqual(cfg.Hooks.PassEnv, []string{"GITHUB_TOKEN", "SLACK_*"}) || !reflect.DeepEqual(cfg.Hooks.Commands["pre-deploy"], []string{"make", "check"}) {
		t.Fatalf("hooks = %+v", cfg.Hooks)
	}

	jsonCfg, err := LoadConfig(writeAliasesTestFile(t, "tako.json", `{
  "project": {"name": "demo", "version": "1.0.0"},
  "servers": {"node-a": {"host": "10.0.0.1", "user": "deploy", "password": "sshpass"}},
  "environments": {"production": {"servers": ["node-a"], "services": {"web": {"image": "nginx:alpine"}}}},
  "hooks": {"commands": {"post-deploy": ["./scripts/smoke.sh", "--fast"]}}
}`))
	if err != nil {
		t.Fatalf("LoadConfig(json): %v", err)
	}
	if !reflect.DeepEqual(jsonCfg.Hooks.Commands["post-deploy"], []string{"./scripts/smoke.sh", "--fast"}) {
		t.Fatalf("json hooks = %+v", jsonCfg.Hooks)
	}
}

func TestLoadConfigWithoutHooksIsUnchanged(t *testing.T) {
	without, err := LoadConfig(writeAliasesTestFile(t, "tako.yaml", aliasesTestConfigBase))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if without.Hooks != nil {
		t.Fatalf("Hooks = %#v, want nil", without.Hooks)
	}
	with, err := LoadConfig(writeAliasesTestFile(t, "tako.yaml", aliasesTestConfigBase+"hooks:\n  timeout: 30s\n"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	with.Hooks = nil
	if !reflect.DeepEqual(without, with) {
		t.Fatalf("hooks changed other config fields:\nwithout=%+v\nwith=%+v", without, with)
	}
}

func TestHooksConfigDefaults(t *testing.T) {
	var hooks *HooksConfig
	if hooks.HooksDir() != DefaultHooksPath || hooks.TimeoutDuration() != DefaultHookTimeout || hooks.OutputMode() != HookOutputVerbose {
		t.Fatalf("nil defaults = %q %s %q", hooks.HooksDir(), hooks.TimeoutDuration(), hooks.OutputMode())
	}
	if got := (&HooksConfig{Path: `hooks\local\`}).HooksDir(); got != "hooks/local" {
		t.Fatalf("HooksDir = %q", got)
	}
}

// HookValidationRejections lists every hooks block validation rejects. Each
// surfaces through LoadConfig as an "invalid config" error, which the CLI
// maps to exit code 2.
var hookValidationRejections = []struct {
	name  string
	block string
	want  string
}{
	{"absolute path", "hooks:\n  path: /etc/tako-hooks\n", "must be relative"},
	{"windows drive path", "hooks:\n  path: 'C:\\hooks'\n", "must be relative"},
	{"escaping path", "hooks:\n  path: ../shared-hooks\n", "escapes the project directory"},
	{"escaping after clean", "hooks:\n  path: hooks/../../x\n", "escapes the project directory"},
	{"inside .tako", "hooks:\n  path: .tako/hooks\n", ".tako/, which `tako init` adds to .gitignore"},
	{"the .tako dir itself", "hooks:\n  path: ./.tako\n", "use the default .tako-hooks"},
	{"timeout unparseable", "hooks:\n  timeout: soon\n", "hooks.timeout"},
	{"timeout too long", "hooks:\n  timeout: 2h\n", "between 1s and 1h"},
	{"timeout too short", "hooks:\n  timeout: 500ms\n", "between 1s and 1h"},
	{"output enum", "hooks:\n  output: events-only\n", "use verbose or quiet"},
	{"passEnv pattern", "hooks:\n  passEnv: [\"SLACK-*\"]\n", "hooks.passEnv[0]"},
	{"passEnv middle star", "hooks:\n  passEnv: [\"A*B\"]\n", "hooks.passEnv[0]"},
	{"passEnv TAKO_", "hooks:\n  passEnv: [OK, TAKO_TOKEN]\n", "Tako owns the TAKO_ namespace"},
	{"passEnv TAKO_ prefix", "hooks:\n  passEnv: [\"tako_*\"]\n", "Tako owns the TAKO_ namespace"},
	{"unknown hook name", "hooks:\n  commands:\n    pre-build: [make]\n", "valid hooks are pre-deploy, post-deploy"},
	{"empty argv", "hooks:\n  commands:\n    pre-deploy: []\n", "non-empty argv"},
	{"blank argv0", "hooks:\n  commands:\n    pre-deploy: [\" \", x]\n", "non-empty argv"},
}

func TestLoadConfigRejectsInvalidHooks(t *testing.T) {
	for _, tc := range hookValidationRejections {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(writeAliasesTestFile(t, "tako.yaml", aliasesTestConfigBase+tc.block))
			if err == nil {
				t.Fatal("LoadConfig succeeded, want rejection")
			}
			if !strings.HasPrefix(err.Error(), "invalid config:") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want invalid config containing %q", err, tc.want)
			}
		})
	}
}

func TestValidateHooksAcceptsValidBlocks(t *testing.T) {
	for _, hooks := range []*HooksConfig{
		nil,
		{},
		{Path: ".tako-hooks", Timeout: "1s", Output: "verbose"},
		{Path: "./ops/hooks", Timeout: "1h", Output: "quiet"},
		{Path: ".takohooks"},
		{PassEnv: []string{"GITHUB_TOKEN", "SLACK_*", "_X", "AWS_*"}},
		{Commands: map[string][]string{"on-failure": {"notify"}, "post-setup": {"echo", ""}}},
	} {
		if err := validateHooks(hooks); err != nil {
			t.Fatalf("validateHooks(%+v): %v", hooks, err)
		}
	}
}

func TestLoadConfigRejectsEnvReferenceInHookCommandsBeforeExpansion(t *testing.T) {
	t.Setenv("TAKO_HOOK_TEST_UNSET", "")
	os.Unsetenv("TAKO_HOOK_TEST_UNSET")
	for name, content := range map[string]string{
		"tako.yaml": aliasesTestConfigBase + "hooks:\n  commands:\n    post-deploy: [notify, \"${TAKO_HOOK_TEST_UNSET}\"]\n",
		"tako.json": `{"project":{"name":"demo","version":"1"},"servers":{},"environments":{},"hooks":{"commands":{"post-deploy":["notify","--token=${SLACK_TOKEN}"]}}}`,
	} {
		_, err := LoadConfig(writeAliasesTestFile(t, name, content))
		if err == nil || !strings.Contains(err.Error(), "hooks.commands.post-deploy: ${...} references are not allowed") || !strings.Contains(err.Error(), "hooks.passEnv") {
			t.Fatalf("%s: error = %v, want hook env-reference rejection naming hooks.passEnv", name, err)
		}
		if strings.Contains(err.Error(), "missing environment variable") {
			t.Fatalf("%s: missing-variable error masked the hook error: %v", name, err)
		}
	}
}

func TestIsCredentialEnvName(t *testing.T) {
	for name, want := range map[string]bool{
		"GITHUB_TOKEN":     true,
		"SLACK_WEBHOOK":    false,
		"AWS_SECRET_KEY":   true,
		"DB_PASSWORD":      true,
		"NPM_AUTH":         true,
		"GCP_CREDENTIALS":  true,
		"NODE_ENV":         false,
		"CI_COMMIT_BRANCH": false,
	} {
		if got := IsCredentialEnvName(name); got != want {
			t.Fatalf("IsCredentialEnvName(%s) = %v, want %v", name, got, want)
		}
	}
}
