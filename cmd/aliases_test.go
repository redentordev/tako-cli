package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/spf13/cobra"
)

// aliasesCommandConfig has an invalid servers section that references an
// unset variable: `tako aliases` must still work because it reads only the
// aliases section.
const aliasesCommandConfig = `project:
  name: demo
servers:
  node-a:
    host: ${TAKO_ALIASES_TEST_UNSET_HOST}
aliases:
  logsw: logs web --follow
  console: [exec, -it, web, --, bin/rails, console]
  logs: [logs, worker]
`

func TestAliasesCommandRegistered(t *testing.T) {
	found, _, err := rootCmd.Find([]string{"aliases"})
	if err != nil || found != aliasesCmd {
		t.Fatalf("Find(aliases) = %v, %v", found, err)
	}
	if aliasesCmd.Annotations[humanOnlyAnnotation] == "true" {
		t.Fatal("tako aliases keeps the full machine contract")
	}
}

func runAliasesForTest(t *testing.T) (string, *bytes.Buffer, error) {
	t.Helper()
	resetValidateGlobals(t)
	cmd := &cobra.Command{}
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	var runErr error
	stdout := captureStdout(t, func() {
		runErr = runAliases(cmd, nil)
	})
	return stdout, &stderr, runErr
}

func TestRunAliasesHumanOutput(t *testing.T) {
	writeAliasConfig(t, aliasesCommandConfig)
	stdout, stderr, err := runAliasesForTest(t)
	if err != nil {
		t.Fatalf("runAliases: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "ALIAS") {
		t.Fatalf("output = %q", stdout)
	}
	for i, want := range []string{"console  tako exec -it web -- bin/rails console", "logs     tako logs worker  (shadowed by built-in \"logs\")", "logsw    tako logs web --follow"} {
		if lines[i+1] != want {
			t.Fatalf("line %d = %q, want %q", i+1, lines[i+1], want)
		}
	}
	if !strings.Contains(stderr.String(), `Warning: alias "logs" is shadowed by built-in command "logs"`) {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestRunAliasesMachineOutput(t *testing.T) {
	writeAliasConfig(t, aliasesCommandConfig)
	var stdout string
	var runErr error
	withMachineOutput(t, outputFormatJSON, "", func() {
		stdout, _, runErr = runAliasesForTest(t)
	})
	if runErr != nil {
		t.Fatalf("runAliases: %v", runErr)
	}
	var result engine.AliasListResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout is not a single JSON document: %v\n%s", err, stdout)
	}
	if result.Kind != engine.KindAliasList || result.APIVersion == "" || result.ConfigPath != "tako.yaml" {
		t.Fatalf("document header = %+v", result)
	}
	if len(result.Aliases) != 3 || result.Aliases[0].Name != "console" || result.Aliases[1].Name != "logs" || result.Aliases[2].Name != "logsw" {
		t.Fatalf("aliases = %+v", result.Aliases)
	}
	logsw := result.Aliases[2]
	if logsw.Form != "string" || logsw.Target != "logs" || strings.Join(logsw.Args, " ") != "logs web --follow" || logsw.Shadowed {
		t.Fatalf("logsw = %+v", logsw)
	}
	if !result.Aliases[1].Shadowed || result.Aliases[1].ShadowedBy != "logs" {
		t.Fatalf("logs = %+v", result.Aliases[1])
	}
	if len(result.Findings) != 1 || result.Findings[0].Severity != engine.ValidateSeverityWarn || result.Findings[0].Field != "aliases.logs" {
		t.Fatalf("findings = %+v", result.Findings)
	}
}

func TestRunAliasesEventsNDJSON(t *testing.T) {
	writeAliasConfig(t, aliasesCommandConfig)
	var stdout string
	var runErr error
	withMachineOutput(t, outputFormatText, eventsFormatNDJSON, func() {
		stdout, _, runErr = runAliasesForTest(t)
	})
	if runErr != nil {
		t.Fatalf("runAliases: %v", runErr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	var event struct {
		Type string `json:"type"`
		Data struct {
			Result engine.AliasListResult `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &event); err != nil {
		t.Fatalf("last line is not an event: %v\n%s", err, stdout)
	}
	if event.Type != "result" || event.Data.Result.Kind != engine.KindAliasList || len(event.Data.Result.Aliases) != 3 {
		t.Fatalf("terminal event = %+v", event)
	}
	if strings.Contains(stdout, "ALIAS") {
		t.Fatalf("human table leaked into the NDJSON stream: %q", stdout)
	}
}

func TestRunAliasesEmptyList(t *testing.T) {
	writeAliasConfig(t, "project:\n  name: demo\n")
	stdout, _, err := runAliasesForTest(t)
	if err != nil {
		t.Fatalf("runAliases: %v", err)
	}
	if !strings.Contains(stdout, "No aliases defined in tako.yaml") {
		t.Fatalf("output = %q", stdout)
	}

	withMachineOutput(t, outputFormatJSON, "", func() {
		stdout, _, err = runAliasesForTest(t)
	})
	if err != nil {
		t.Fatalf("runAliases json: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		t.Fatalf("stdout = %q: %v", stdout, err)
	}
	if string(raw["aliases"]) != "[]" {
		t.Fatalf("aliases = %s, want []", raw["aliases"])
	}
}

func TestRunAliasesMissingConfig(t *testing.T) {
	switchToTempDir(t)
	_, _, err := runAliasesForTest(t)
	if err == nil || !strings.Contains(err.Error(), "config file not found: tako.yaml") {
		t.Fatalf("error = %v", err)
	}
	if code := exitCodeForError(err); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestRunAliasesReportsTargetErrors(t *testing.T) {
	writeAliasConfig(t, "aliases:\n  oops: [deplyo]\n")
	var stdout string
	var err error
	withMachineOutput(t, outputFormatJSON, "", func() {
		stdout, _, err = runAliasesForTest(t)
	})
	if err == nil || exitCodeForError(err) != 2 || !strings.Contains(err.Error(), `unknown command "deplyo"`) {
		t.Fatalf("error = %v", err)
	}
	var result engine.AliasListResult
	if jsonErr := json.Unmarshal([]byte(stdout), &result); jsonErr != nil || len(result.Findings) != 1 || result.Findings[0].Severity != engine.ValidateSeverityError {
		t.Fatalf("document = %+v (%v)", result, jsonErr)
	}
}

func writeValidateAliasConfig(t *testing.T, aliases string) {
	t.Helper()
	root := switchToTempDir(t)
	sshKey := filepath.Join(root, "id_ed25519")
	if err := os.WriteFile(sshKey, []byte("test-key"), 0600); err != nil {
		t.Fatalf("failed to write ssh key fixture: %v", err)
	}
	configData := []byte(`project:
  name: demo
  version: 1.0.0
servers:
  node-a:
    host: 203.0.113.10
    user: deploy
    sshKey: ` + sshKey + `
environments:
  production:
    servers: [node-a]
    services:
      web:
        image: nginx:alpine
` + aliases)
	if err := os.WriteFile(filepath.Join(root, "tako.yaml"), configData, 0600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
}

func TestRunValidateWarnsAboutShadowedAlias(t *testing.T) {
	writeValidateAliasConfig(t, "aliases:\n  logs: [logs, web]\n  console: [exec, -it, web, --, sh]\n")
	resetValidateGlobals(t)
	validateQuiet = true
	restoreOutput := outputFormatFlag
	outputFormatFlag = outputFormatJSON
	t.Cleanup(func() { outputFormatFlag = restoreOutput })

	var runErr error
	stdout := captureStdout(t, func() {
		runErr = runValidate(&cobra.Command{}, nil)
	})
	if runErr != nil {
		t.Fatalf("runValidate: %v", runErr)
	}
	var result engine.ValidateResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout: %v\n%s", err, stdout)
	}
	if !result.Valid {
		t.Fatalf("result = %+v, want valid", result)
	}
	if len(result.Findings) != 1 || result.Findings[0].Severity != engine.ValidateSeverityWarn || result.Findings[0].Field != "aliases.logs" || !strings.Contains(result.Findings[0].Message, "shadowed by built-in command") {
		t.Fatalf("findings = %+v, want one shadow warning", result.Findings)
	}
}

func TestRunValidateRejectsUnknownAliasTarget(t *testing.T) {
	writeValidateAliasConfig(t, "aliases:\n  oops: [deplyo, web]\n")
	resetValidateGlobals(t)
	restoreOutput := outputFormatFlag
	outputFormatFlag = outputFormatJSON
	t.Cleanup(func() { outputFormatFlag = restoreOutput })

	var runErr error
	stdout := captureStdout(t, func() {
		runErr = runValidate(&cobra.Command{}, nil)
	})
	if runErr == nil || exitCodeForError(runErr) != 2 || !strings.Contains(runErr.Error(), `unknown command "deplyo"`) {
		t.Fatalf("error = %v", runErr)
	}
	var result engine.ValidateResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout: %v\n%s", err, stdout)
	}
	if result.Valid || len(result.Findings) != 1 || result.Findings[0].Severity != engine.ValidateSeverityError || result.Findings[0].Field != "aliases.oops" {
		t.Fatalf("result = %+v", result)
	}
}
