package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/spf13/cobra"
)

// fakeAliasLoader returns fixed aliases and records the config paths it was
// asked to read.
type fakeAliasLoader struct {
	aliases config.AliasMap
	err     error
	calls   []string
}

func (f *fakeAliasLoader) load(configPath string) (*config.ProjectAliases, error) {
	f.calls = append(f.calls, configPath)
	if f.err != nil {
		return nil, f.err
	}
	if err := config.ValidateAliases(f.aliases); err != nil {
		return nil, err
	}
	path := configPath
	if path == "" {
		path = "tako.yaml"
	}
	return &config.ProjectAliases{ConfigPath: path, Aliases: f.aliases}, nil
}

func TestResolveAliasArgsPreScan(t *testing.T) {
	aliases := config.AliasMap{
		"logsw":   config.ListValue("logs", "web"),
		"console": config.StringValue(`exec -it web -- sh -c "bin/rails console"`),
	}
	tests := []struct {
		name       string
		args       []string
		want       []string
		wantConfig []string // loader calls; nil means the config must not be read
	}{
		{name: "plain alias", args: []string{"logsw", "--follow"}, want: []string{"logs", "web", "--follow"}, wantConfig: []string{""}},
		{name: "config space", args: []string{"--config", "deploy/tako.yaml", "-v", "logsw"}, want: []string{"--config", "deploy/tako.yaml", "-v", "logs", "web"}, wantConfig: []string{"deploy/tako.yaml"}},
		{name: "config equals", args: []string{"--config=deploy/tako.yaml", "logsw"}, want: []string{"--config=deploy/tako.yaml", "logs", "web"}, wantConfig: []string{"deploy/tako.yaml"}},
		{name: "short env attached", args: []string{"-estaging", "logsw"}, want: []string{"-estaging", "logs", "web"}, wantConfig: []string{""}},
		{name: "short env separate", args: []string{"-e", "staging", "logsw"}, want: []string{"-e", "staging", "logs", "web"}, wantConfig: []string{""}},
		{name: "verbose", args: []string{"-v", "logsw"}, want: []string{"-v", "logs", "web"}, wantConfig: []string{""}},
		{name: "output and events", args: []string{"--output", "json", "--events=ndjson", "logsw"}, want: []string{"--output", "json", "--events=ndjson", "logs", "web"}, wantConfig: []string{""}},
		{name: "string form", args: []string{"console"}, want: []string{"exec", "-it", "web", "--", "sh", "-c", "bin/rails console"}, wantConfig: []string{""}},
		{name: "extra args after separator", args: []string{"console", "--", "x"}, want: []string{"exec", "-it", "web", "--", "sh", "-c", "bin/rails console", "--", "x"}, wantConfig: []string{""}},
		{name: "terminator first", args: []string{"--", "logsw"}, want: []string{"--", "logsw"}},
		{name: "unknown leading flag", args: []string{"--bogus", "logsw"}, want: []string{"--bogus", "logsw"}},
		{name: "built-in", args: []string{"logs", "web"}, want: []string{"logs", "web"}},
		{name: "built-in after flags", args: []string{"-e", "staging", "deploy"}, want: []string{"-e", "staging", "deploy"}},
		{name: "built-in subcommand alias", args: []string{"certs", "list"}, want: []string{"certs", "list"}},
		{name: "version", args: []string{"--version"}, want: []string{"--version"}},
		{name: "completion command", args: []string{"completion", "bash"}, want: []string{"completion", "bash"}},
		{name: "unknown name with config", args: []string{"consle"}, want: []string{"consle"}, wantConfig: []string{""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loader := &fakeAliasLoader{aliases: aliases}
			dispatch, err := resolveAliasArgs(rootCmd, tt.args, loader.load)
			if err != nil {
				t.Fatalf("resolveAliasArgs: %v", err)
			}
			if !reflect.DeepEqual(dispatch.args, tt.want) {
				t.Fatalf("args = %q, want %q", dispatch.args, tt.want)
			}
			if !reflect.DeepEqual(loader.calls, tt.wantConfig) {
				t.Fatalf("loader calls = %q, want %q", loader.calls, tt.wantConfig)
			}
			if dispatch.placeholders != nil {
				t.Fatal("dispatch paths must not register help placeholders")
			}
		})
	}
}

func TestResolveAliasArgsHelpAndCompletionLoadAliasesQuietly(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"-v"},
		{"--help"},
		{"-h", "logs"},
		{"help"},
		{"help", "console"},
		{"__complete", "con"},
		{"__completeNoDesc", ""},
	} {
		loader := &fakeAliasLoader{aliases: config.AliasMap{"console": config.ListValue("exec", "-it", "web", "--", "sh")}}
		dispatch, err := resolveAliasArgs(rootCmd, args, loader.load)
		if err != nil {
			t.Fatalf("%q: %v", args, err)
		}
		if dispatch.placeholders == nil || !reflect.DeepEqual(dispatch.args, append([]string{}, args...)) {
			t.Fatalf("%q: dispatch = %+v", args, dispatch)
		}

		failing := &fakeAliasLoader{err: errors.New("broken config")}
		dispatch, err = resolveAliasArgs(rootCmd, args, failing.load)
		if err != nil || dispatch.placeholders != nil {
			t.Fatalf("%q: help/completion must ignore alias load errors, got %+v, %v", args, dispatch, err)
		}
	}

	loader := &fakeAliasLoader{aliases: config.AliasMap{"console": config.ListValue("ps")}}
	if _, err := resolveAliasArgs(rootCmd, []string{"__complete", "--config", "deploy/tako.yaml", "con"}, loader.load); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loader.calls, []string{"deploy/tako.yaml"}) {
		t.Fatalf("completion config = %q", loader.calls)
	}
}

func TestResolveAliasArgsTargetChecks(t *testing.T) {
	tests := []struct {
		name  string
		value config.StringOrList
		want  string
	}{
		{"unknown", config.ListValue("deplyo"), `alias "x" targets unknown command "deplyo"`},
		{"help", config.ListValue("help", "logs"), `targets "help", which aliases cannot run`},
		{"completion", config.ListValue("completion", "bash"), `targets "completion"`},
		{"takod", config.ListValue("takod", "run"), `targets "takod"`},
		{"hidden internal", config.ListValue("internal", "e2e-server-ssh"), `targets "internal"`},
		{"hidden subcommand", config.ListValue("platform", "worker", "run"), "hidden command"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loader := &fakeAliasLoader{aliases: config.AliasMap{"x": tt.value}}
			_, err := resolveAliasArgs(rootCmd, []string{"x"}, loader.load)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if code := exitCodeForError(err); code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
		})
	}

	loader := &fakeAliasLoader{aliases: config.AliasMap{"a": config.ListValue("logs", "web"), "b": config.ListValue("a", "--follow")}}
	_, err := resolveAliasArgs(rootCmd, []string{"b"}, loader.load)
	if err == nil || !strings.Contains(err.Error(), "aliases cannot reference other aliases") || exitCodeForError(err) != 2 {
		t.Fatalf("alias-to-alias error = %v", err)
	}
}

func TestCheckAliasTargets(t *testing.T) {
	findings := checkAliasTargets(rootCmd, config.AliasMap{
		"logs":    config.ListValue("logs", "web"),
		"ls":      config.ListValue("certs", "ls"),
		"console": config.ListValue("exec", "-it", "web", "--", "sh"),
		"oops":    config.ListValue("deplyo"),
		// "logs" names both the built-in and the shadowed alias: built-in.
		"logsw": config.ListValue("logs", "--follow"),
		"b":     config.ListValue("console"),
	})
	if len(findings) != 3 {
		t.Fatalf("findings = %+v, want alias-to-alias error, shadow warning and unknown target error", findings)
	}
	if findings[0].Severity != engine.ValidateSeverityError || findings[0].Field != "aliases.b" || !strings.Contains(findings[0].Message, "aliases cannot reference other aliases") {
		t.Fatalf("alias-to-alias finding = %+v", findings[0])
	}
	if findings[1].Severity != engine.ValidateSeverityWarn || findings[1].Field != "aliases.logs" || !strings.Contains(findings[1].Message, `shadowed by built-in command "logs"`) {
		t.Fatalf("shadow finding = %+v", findings[1])
	}
	if findings[2].Severity != engine.ValidateSeverityError || findings[2].Field != "aliases.oops" || !strings.Contains(findings[2].Message, `unknown command "deplyo"`) {
		t.Fatalf("target finding = %+v", findings[2])
	}
}

func TestResolveAliasArgsEnvironmentPin(t *testing.T) {
	aliases := config.AliasMap{
		"stg-console": config.ListValue("exec", "-it", "web", "-e", "staging", "--", "bin/rails", "console"),
		"stg-logs":    config.ListValue("logs", "web", "--env=staging"),
		"logsw":       config.ListValue("logs", "web"),
	}
	ok := [][]string{
		{"stg-console"},
		{"-e", "staging", "stg-console"},
		{"--env=staging", "stg-logs"},
		{"stg-logs", "-estaging"},
		{"stg-console", "-e", "production"}, // after the alias's `--`: remote data
		{"stg-logs", "--", "-e", "production"},
		{"-e", "production", "logsw"}, // unpinned alias follows -e
	}
	for _, args := range ok {
		loader := &fakeAliasLoader{aliases: aliases}
		if _, err := resolveAliasArgs(rootCmd, args, loader.load); err != nil {
			t.Fatalf("%q: unexpected error %v", args, err)
		}
	}

	conflicts := [][]string{
		{"-e", "production", "stg-console"},
		{"--env", "production", "stg-console"},
		{"stg-logs", "-e", "production"},
		{"-eproduction", "stg-logs"},
	}
	for _, args := range conflicts {
		loader := &fakeAliasLoader{aliases: aliases}
		_, err := resolveAliasArgs(rootCmd, args, loader.load)
		if err == nil || !strings.Contains(err.Error(), "pins environment staging") {
			t.Fatalf("%q: error = %v, want pin conflict", args, err)
		}
		if code := exitCodeForError(err); code != 2 {
			t.Fatalf("%q: exit code = %d, want 2", args, code)
		}
	}
}

// aliasTestRun records what the test command tree dispatched.
type aliasTestRun struct {
	path   string
	args   []string
	follow bool
	env    string
}

// newAliasTestRoot builds a small command tree with the real global flags
// and PersistentPreRunE, so alias dispatch can be exercised end to end
// without mutating the process-wide rootCmd.
func newAliasTestRoot(t *testing.T) (*cobra.Command, *aliasTestRun, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	oldCfg, oldVerbose, oldEnv, oldHostKey := cfgFile, verbose, envFlag, hostKeyModeFlag
	oldOutput, oldEvents, oldEngine := outputFormatFlag, eventsFormatFlag, cliEngineInstance
	t.Cleanup(func() {
		cfgFile, verbose, envFlag, hostKeyModeFlag = oldCfg, oldVerbose, oldEnv, oldHostKey
		outputFormatFlag, eventsFormatFlag, cliEngineInstance = oldOutput, oldEvents, oldEngine
	})
	cliEngineInstance = nil

	run := &aliasTestRun{}
	record := func(cmd *cobra.Command, args []string) error {
		run.path = cmd.CommandPath()
		run.args = args
		run.env = envFlag
		if flag := cmd.Flags().Lookup("follow"); flag != nil {
			run.follow = flag.Value.String() == "true"
		}
		return nil
	}

	root := &cobra.Command{Use: "tako", Short: "test root", PersistentPreRunE: rootCmd.PersistentPreRunE}
	root.PersistentFlags().StringVar(&cfgFile, "config", "", "config file")
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "verbose output")
	root.PersistentFlags().StringVarP(&envFlag, "env", "e", "", "environment")
	root.PersistentFlags().StringVar(&hostKeyModeFlag, "host-key-mode", "", "host key mode")
	root.PersistentFlags().StringVar(&outputFormatFlag, "output", outputFormatText, "output format")
	root.PersistentFlags().StringVar(&eventsFormatFlag, "events", "", "events")

	logs := &cobra.Command{Use: "logs", Short: "Show logs", RunE: record}
	logs.Flags().BoolP("follow", "f", false, "follow")
	exec := &cobra.Command{Use: "exec", Short: "Run a command", RunE: record}
	exec.Flags().BoolP("interactive", "i", false, "stdin")
	exec.Flags().BoolP("tty", "t", false, "tty")
	ps := &cobra.Command{Use: "ps", Short: "List services", RunE: func(cmd *cobra.Command, args []string) error {
		run.path = cmd.CommandPath()
		return emitResultDocument(map[string]string{"kind": "StatusResult"})
	}}
	env := &cobra.Command{Use: "env", Short: "Manage env files"}
	pull := &cobra.Command{Use: "pull", Short: "Pull env", RunE: record}
	markHumanOnly(pull)
	env.AddCommand(pull)
	internal := &cobra.Command{Use: "internal", Hidden: true}
	internal.AddCommand(&cobra.Command{Use: "e2e", RunE: record})
	root.AddCommand(logs, exec, ps, env, internal)

	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	return root, run, &stdout, &stderr
}

func writeAliasConfig(t *testing.T, content string) {
	t.Helper()
	root := switchToTempDir(t)
	if err := os.WriteFile(filepath.Join(root, "tako.yaml"), []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func TestExecuteRootDispatchesAlias(t *testing.T) {
	writeAliasConfig(t, "aliases:\n  logsw: [logs, web]\n  stg: logs web -e staging\n")
	root, run, stdout, stderr := newAliasTestRoot(t)

	if err := executeRoot(context.Background(), root, []string{"logsw", "--follow"}, config.LoadAliases); err != nil {
		t.Fatalf("executeRoot: %v", err)
	}
	if run.path != "tako logs" || !reflect.DeepEqual(run.args, []string{"web"}) || !run.follow {
		t.Fatalf("dispatched %+v, want tako logs [web] --follow", run)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("alias expansion wrote output: stdout=%q stderr=%q", stdout, stderr)
	}

	root, run, _, _ = newAliasTestRoot(t)
	if err := executeRoot(context.Background(), root, []string{"stg"}, config.LoadAliases); err != nil {
		t.Fatalf("executeRoot(stg): %v", err)
	}
	if run.env != "staging" {
		t.Fatalf("pinned env = %q, want staging", run.env)
	}
}

func TestExecuteRootVerboseTraceGoesToStderr(t *testing.T) {
	writeAliasConfig(t, "aliases:\n  logsw: [logs, web]\n")
	root, run, stdout, stderr := newAliasTestRoot(t)
	if err := executeRoot(context.Background(), root, []string{"-v", "logsw", "--follow"}, config.LoadAliases); err != nil {
		t.Fatalf("executeRoot: %v", err)
	}
	if run.path != "tako logs" {
		t.Fatalf("dispatched %+v", run)
	}
	if !strings.Contains(stderr.String(), "alias logsw -> tako -v logs web --follow") {
		t.Fatalf("stderr = %q, want expansion trace", stderr)
	}
	if stdout.Len() != 0 {
		t.Fatalf("trace must not reach stdout: %q", stdout)
	}
}

func TestExecuteRootAliasErrors(t *testing.T) {
	t.Run("missing config", func(t *testing.T) {
		switchToTempDir(t)
		root, run, _, stderr := newAliasTestRoot(t)
		err := executeRoot(context.Background(), root, []string{"console"}, config.LoadAliases)
		if err == nil || !strings.Contains(err.Error(), `unknown command "console" for "tako"`) || !strings.Contains(err.Error(), "no tako.yaml or tako.json found for project aliases") {
			t.Fatalf("error = %v", err)
		}
		if code := exitCodeForError(err); code != 1 {
			t.Fatalf("exit code = %d, want 1 like any unknown command", code)
		}
		if !strings.Contains(stderr.String(), "Error: unknown command") || !strings.Contains(stderr.String(), "Run 'tako --help' for usage.") {
			t.Fatalf("stderr = %q", stderr)
		}
		if run.path != "" {
			t.Fatalf("nothing should run, got %+v", run)
		}
	})

	t.Run("missing explicit config", func(t *testing.T) {
		switchToTempDir(t)
		root, _, _, _ := newAliasTestRoot(t)
		err := executeRoot(context.Background(), root, []string{"--config", "deploy/tako.yaml", "console"}, config.LoadAliases)
		if err == nil || !strings.Contains(err.Error(), "no config file found at deploy/tako.yaml for project aliases") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("unknown name with config", func(t *testing.T) {
		writeAliasConfig(t, "aliases:\n  console: [exec, web, --, sh]\n")
		root, _, _, _ := newAliasTestRoot(t)
		err := executeRoot(context.Background(), root, []string{"consle"}, config.LoadAliases)
		if err == nil || !strings.HasPrefix(err.Error(), `unknown command "consle" for "tako"`) || strings.Contains(err.Error(), "project aliases") {
			t.Fatalf("error = %v, want cobra's unknown-command error", err)
		}
	})

	t.Run("invalid aliases", func(t *testing.T) {
		writeAliasConfig(t, "aliases:\n  console: exec web -- echo \"hi\n")
		root, _, _, stderr := newAliasTestRoot(t)
		err := executeRoot(context.Background(), root, []string{"console"}, config.LoadAliases)
		if err == nil || !strings.Contains(err.Error(), "aliases.console: unterminated double quote") {
			t.Fatalf("error = %v", err)
		}
		if code := exitCodeForError(err); code != 2 {
			t.Fatalf("exit code = %d, want 2", code)
		}
		if !strings.Contains(stderr.String(), "aliases.console") {
			t.Fatalf("stderr = %q", stderr)
		}
	})

	t.Run("built-ins ignore invalid aliases", func(t *testing.T) {
		writeAliasConfig(t, "aliases:\n  console: exec web -- echo \"hi\n")
		root, run, _, _ := newAliasTestRoot(t)
		if err := executeRoot(context.Background(), root, []string{"logs", "web"}, config.LoadAliases); err != nil {
			t.Fatalf("built-in failed because of invalid aliases: %v", err)
		}
		if run.path != "tako logs" {
			t.Fatalf("dispatched %+v", run)
		}
	})
}

func TestExecuteRootAliasInheritsMachineContract(t *testing.T) {
	writeAliasConfig(t, "aliases:\n  pull: [env, pull]\n  status: [ps]\n")

	root, run, _, _ := newAliasTestRoot(t)
	err := executeRoot(context.Background(), root, []string{"pull", "--output", "json"}, config.LoadAliases)
	if err == nil || !strings.Contains(err.Error(), "tako env pull is human-only") {
		t.Fatalf("error = %v, want the human-only rejection of tako env pull", err)
	}
	if code := exitCodeForError(err); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if run.path != "" {
		t.Fatalf("human-only command ran: %+v", run)
	}

	root, run, _, stderr := newAliasTestRoot(t)
	var runErr error
	stdout := captureStdout(t, func() {
		runErr = executeRoot(context.Background(), root, []string{"status", "--output", "json"}, config.LoadAliases)
	})
	if runErr != nil {
		t.Fatalf("executeRoot(status): %v", runErr)
	}
	if run.path != "tako ps" {
		t.Fatalf("dispatched %+v", run)
	}
	var document map[string]string
	if err := json.Unmarshal([]byte(stdout), &document); err != nil || document["kind"] != "StatusResult" {
		t.Fatalf("stdout is not exactly the ps result document: %q (%v)", stdout, err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %q", stderr)
	}
}

func renderTestRootHelp(t *testing.T) string {
	t.Helper()
	root, _, stdout, _ := newAliasTestRoot(t)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("help: %v", err)
	}
	return stdout.String()
}

func TestExecuteRootHelpListsProjectAliases(t *testing.T) {
	switchToTempDir(t)
	baseline := renderTestRootHelp(t)

	root, _, stdout, _ := newAliasTestRoot(t)
	if err := executeRoot(context.Background(), root, []string{"--help"}, config.LoadAliases); err != nil {
		t.Fatalf("help without config: %v", err)
	}
	if stdout.String() != baseline {
		t.Fatalf("help without config changed:\n%s\n--- want ---\n%s", stdout, baseline)
	}
	if strings.Contains(baseline, "Project Aliases") {
		t.Fatal("baseline help must not mention aliases")
	}

	writeAliasConfig(t, "aliases:\n  console: [exec, -it, web, --, bin/rails, console]\n  logs: [logs, worker]\n")
	root, _, stdout, _ = newAliasTestRoot(t)
	if err := executeRoot(context.Background(), root, []string{"--help"}, config.LoadAliases); err != nil {
		t.Fatalf("help with aliases: %v", err)
	}
	help := stdout.String()
	available := strings.Index(help, "Available Commands:")
	aliasesAt := strings.Index(help, "Project Aliases:")
	if available < 0 || aliasesAt < 0 || aliasesAt < available {
		t.Fatalf("help sections out of order:\n%s", help)
	}
	if strings.Contains(help, "Additional Commands:") {
		t.Fatalf("built-ins must stay under Available Commands:\n%s", help)
	}
	if !strings.Contains(help[aliasesAt:], "console") || !strings.Contains(help[aliasesAt:], "alias for: tako exec -it web -- bin/rails console") {
		t.Fatalf("alias missing from Project Aliases:\n%s", help)
	}
	if !strings.Contains(help[available:aliasesAt], "logs") || strings.Contains(help[aliasesAt:], "logs") {
		t.Fatalf("shadowed alias must not be listed and logs must stay a built-in:\n%s", help)
	}

	root, _, stdout, _ = newAliasTestRoot(t)
	if err := executeRoot(context.Background(), root, []string{"help", "console"}, config.LoadAliases); err != nil {
		t.Fatalf("help console: %v", err)
	}
	if !strings.Contains(stdout.String(), "Runs: tako exec -it web -- bin/rails console [args...]") {
		t.Fatalf("tako help console = %q", stdout)
	}

	writeAliasConfig(t, "aliases:\n  console: exec \"web\n")
	root, _, stdout, _ = newAliasTestRoot(t)
	if err := executeRoot(context.Background(), root, []string{"--help"}, config.LoadAliases); err != nil {
		t.Fatalf("help with invalid aliases: %v", err)
	}
	if stdout.String() != baseline {
		t.Fatalf("help with invalid aliases must fall back to the plain output:\n%s", stdout)
	}
}

func TestExecuteRootCompletionOffersAliases(t *testing.T) {
	writeAliasConfig(t, "aliases:\n  console: [exec, -it, web, --, sh]\n")
	root, _, stdout, _ := newAliasTestRoot(t)
	if err := executeRoot(context.Background(), root, []string{"__complete", "con"}, config.LoadAliases); err != nil {
		t.Fatalf("__complete con: %v", err)
	}
	if !strings.Contains(stdout.String(), "console\talias for: tako exec -it web -- sh") {
		t.Fatalf("completion output = %q", stdout)
	}

	root, _, stdout, _ = newAliasTestRoot(t)
	if err := executeRoot(context.Background(), root, []string{"__complete", ""}, config.LoadAliases); err != nil {
		t.Fatalf("__complete '': %v", err)
	}
	for _, want := range []string{"console\t", "logs\t", "ps\t"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("completion output missing %q: %q", want, stdout)
		}
	}

	for name, content := range map[string]string{"missing": "", "invalid": "aliases:\n  console: exec \"web\n"} {
		if content == "" {
			switchToTempDir(t)
		} else {
			writeAliasConfig(t, content)
		}
		root, _, stdout, _ = newAliasTestRoot(t)
		if err := executeRoot(context.Background(), root, []string{"__complete", "con"}, config.LoadAliases); err != nil {
			t.Fatalf("%s: completion must not fail: %v", name, err)
		}
		if strings.Contains(stdout.String(), "console") {
			t.Fatalf("%s: completion offered an alias: %q", name, stdout)
		}
	}
}

func TestExecuteRootCompletionOffersNoTargetFlagsAfterAlias(t *testing.T) {
	writeAliasConfig(t, "aliases:\n  logsw: [logs, web]\n")
	root, _, stdout, _ := newAliasTestRoot(t)
	if err := executeRoot(context.Background(), root, []string{"__complete", "logsw", "--"}, config.LoadAliases); err != nil {
		t.Fatalf("__complete logsw --: %v", err)
	}
	if strings.Contains(stdout.String(), "--follow") {
		t.Fatalf("completion after an alias offered target flags: %q", stdout)
	}
}

func TestExecuteRootAliasEventsHaveNoAliasEvent(t *testing.T) {
	writeAliasConfig(t, "aliases:\n  status: [ps]\n")

	runEvents := func(args []string) string {
		root, _, _, _ := newAliasTestRoot(t)
		var runErr error
		stdout := captureStdout(t, func() {
			runErr = executeRoot(context.Background(), root, args, config.LoadAliases)
		})
		if runErr != nil {
			t.Fatalf("executeRoot(%v): %v", args, runErr)
		}
		return stdout
	}

	viaAlias := runEvents([]string{"status", "--events", "ndjson"})
	direct := runEvents([]string{"ps", "--events", "ndjson"})
	if strings.Contains(viaAlias, "alias") {
		t.Fatalf("alias invocation emitted an alias-specific event: %q", viaAlias)
	}
	if strings.Count(viaAlias, "\n") != strings.Count(direct, "\n") {
		t.Fatalf("alias stream differs from target stream:\nalias:  %q\ndirect: %q", viaAlias, direct)
	}
}

// Man page generation runs in a directory whose tako.yaml defines aliases in
// TestTrackedManPagesMatchGeneratedOutput; this pins that the static tree
// never carries placeholders.
func TestStaticCommandTreeHasNoAliasPlaceholders(t *testing.T) {
	for _, command := range rootCmd.Commands() {
		if isAliasPlaceholder(command) {
			t.Fatalf("placeholder %q leaked into the static command tree", command.Name())
		}
	}
}
