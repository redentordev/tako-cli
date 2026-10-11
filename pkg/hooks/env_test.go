package hooks

import (
	"strings"
	"testing"
	"time"
)

func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		out[name] = value
	}
	return out
}

func TestBuildEnvExcludesUnlistedProcessVariables(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"HOME=/home/op",
		"LC_ALL=C.UTF-8",
		"DATABASE_PASSWORD=hunter2-from-dotenv", // exported by config.LoadConfig from .env
		"VAULT_TOKEN=s.abcdef",
		"GITHUB_TOKEN=ghp_123456",
		"SLACK_WEBHOOK=https://hooks.slack.test/x",
		"SLACK_CHANNEL=#deploys",
		"SLACKER=no",
		"TAKO_PROJECT=spoofed",
		"TAKO_NONINTERACTIVE=1",
	}
	env := envMap(BuildEnv(base, []string{"GITHUB_TOKEN", "SLACK_*"}, Context{Hook: PreDeploy, Command: "deploy", Project: "demo"}, "linux"))
	for _, absent := range []string{"DATABASE_PASSWORD", "VAULT_TOKEN", "SLACKER", "TAKO_NONINTERACTIVE"} {
		if _, ok := env[absent]; ok {
			t.Fatalf("%s leaked into hook env: %v", absent, env)
		}
	}
	for name, want := range map[string]string{
		"PATH":          "/usr/bin",
		"HOME":          "/home/op",
		"LC_ALL":        "C.UTF-8",
		"GITHUB_TOKEN":  "ghp_123456",
		"SLACK_WEBHOOK": "https://hooks.slack.test/x",
		"SLACK_CHANNEL": "#deploys",
		"TAKO_PROJECT":  "demo",
	} {
		if env[name] != want {
			t.Fatalf("%s = %q, want %q", name, env[name], want)
		}
	}
}

func TestBuildEnvWindowsBaselineIsCaseInsensitive(t *testing.T) {
	base := []string{"Path=C:\\Windows", "SystemRoot=C:\\Windows", "USERPROFILE=C:\\Users\\op", "ComSpec=cmd.exe", "github_token=x1234", "PSModulePath=C:\\ps", "Secret=1"}
	env := envMap(BuildEnv(base, []string{"GITHUB_TOKEN"}, Context{}, "windows"))
	for _, name := range []string{"Path", "SystemRoot", "USERPROFILE", "ComSpec", "github_token", "PSModulePath"} {
		if _, ok := env[name]; !ok {
			t.Fatalf("%s missing: %v", name, env)
		}
	}
	if _, ok := env["Secret"]; ok {
		t.Fatal("Secret leaked")
	}
	linux := envMap(BuildEnv(base, nil, Context{}, "linux"))
	if _, ok := linux["SystemRoot"]; ok {
		t.Fatal("Windows-only baseline applied on linux")
	}
}

func TestContextVars(t *testing.T) {
	yes, no := true, false
	duration := 12.5
	recorded := time.Date(2026, 10, 11, 12, 0, 0, 0, time.FixedZone("x", 3600))
	longError := strings.Repeat("é", MaxErrorBytes) // 2 bytes per rune
	vars := Context{
		Hook: OnFailure, Command: "deploy", Project: "demo", Environment: "production",
		Services: []string{"web", "api"}, Servers: []string{"node-b", "node-a"}, Hosts: []string{"203.0.113.2", "203.0.113.1"},
		Performer: "op", RecordedAt: recorded, Version: "v1.2.3",
		Revision: "abc1234", GitCommit: "abc1234def", GitCommitShort: "abc1234", GitBranch: "main", GitDirty: &no,
		PlanHash: "hash", PlanEmpty: &yes, PlanDestructive: &no, DeploymentID: "d-1",
		Result: "failed", DurationSeconds: &duration, ErrorClass: "failed", Error: longError, FailedHook: PreDeploy,
	}.Vars()
	for name, want := range map[string]string{
		"TAKO_HOOK":             "on-failure",
		"TAKO_COMMAND":          "deploy",
		"TAKO_SERVICES":         "api,web",
		"TAKO_SERVERS":          "node-a,node-b",
		"TAKO_HOSTS":            "203.0.113.1,203.0.113.2",
		"TAKO_RECORDED_AT":      "2026-10-11T11:00:00Z",
		"TAKO_GIT_DIRTY":        "false",
		"TAKO_PLAN_EMPTY":       "true",
		"TAKO_PLAN_DESTRUCTIVE": "false",
		"TAKO_DURATION_SECONDS": "12.500",
		"TAKO_FAILED_HOOK":      "pre-deploy",
		"TAKO_DEPLOYMENT_ID":    "d-1",
	} {
		if vars[name] != want {
			t.Fatalf("%s = %q, want %q", name, vars[name], want)
		}
	}
	if len(vars["TAKO_ERROR"]) > MaxErrorBytes || !strings.HasPrefix(longError, vars["TAKO_ERROR"]) || len(vars["TAKO_ERROR"]) < MaxErrorBytes-1 {
		t.Fatalf("TAKO_ERROR length = %d", len(vars["TAKO_ERROR"]))
	}

	minimal := Context{Hook: PreSetup, Command: "setup"}.Vars()
	for _, required := range []string{"TAKO_HOOK", "TAKO_COMMAND", "TAKO_PROJECT", "TAKO_ENVIRONMENT", "TAKO_SERVICES", "TAKO_SERVERS", "TAKO_HOSTS", "TAKO_PERFORMER", "TAKO_RECORDED_AT", "TAKO_VERSION"} {
		if _, ok := minimal[required]; !ok {
			t.Fatalf("%s missing from minimal context", required)
		}
	}
	for _, optional := range []string{"TAKO_REVISION", "TAKO_PLAN_EMPTY", "TAKO_ERROR", "TAKO_DURATION_SECONDS"} {
		if _, ok := minimal[optional]; ok {
			t.Fatalf("%s set without a value", optional)
		}
	}
}

func TestCredentialValuesRegisterOnlyCredentialNames(t *testing.T) {
	base := []string{"GITHUB_TOKEN=ghp_secret", "NODE_ENV=production", "SLACK_AUTH=xoxb", "SLACK_CHANNEL=#ops", "DB_PASSWORD=unlisted"}
	values := CredentialValues(base, []string{"GITHUB_TOKEN", "NODE_ENV", "SLACK_*"}, "linux")
	joined := strings.Join(values, ",")
	if !strings.Contains(joined, "ghp_secret") || !strings.Contains(joined, "xoxb") {
		t.Fatalf("credentials = %v", values)
	}
	if strings.Contains(joined, "production") || strings.Contains(joined, "#ops") || strings.Contains(joined, "unlisted") {
		t.Fatalf("non-credential or unlisted values registered: %v", values)
	}
}
