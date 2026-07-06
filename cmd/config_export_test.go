package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/configmaterialize"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/stateclient"
	"github.com/spf13/cobra"
)

func TestConfigExportAndPullCommandsRegistered(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want *cobra.Command
	}{
		{[]string{"config", "export"}, configExportCmd},
		{[]string{"config", "pull"}, configPullCmd},
	} {
		cmd, _, err := rootCmd.Find(tt.args)
		if err != nil {
			t.Fatalf("Find(%v) returned error: %v", tt.args, err)
		}
		if cmd != tt.want {
			t.Fatalf("%v command was not registered", tt.args)
		}
		if !cmd.SilenceUsage {
			t.Fatalf("%v SilenceUsage = false, want true", tt.args)
		}
	}
}

func TestNormalizeConfigExportOptionsValidatesRequiredFlags(t *testing.T) {
	opts := configExportOptions{Environment: "production", Server: "prod-1", User: "deploy", SSHPort: 22}
	if err := normalizeConfigExportOptions(&opts); err == nil || !strings.Contains(err.Error(), "--project") {
		t.Fatalf("missing project err = %v, want --project error", err)
	}
	opts = configExportOptions{Project: "web", Environment: "production", User: "deploy", SSHPort: 22}
	if err := normalizeConfigExportOptions(&opts); err == nil || !strings.Contains(err.Error(), "--server") {
		t.Fatalf("missing server err = %v, want --server error", err)
	}
}

func TestConfigExportServerNameSanitizationAndDefaulting(t *testing.T) {
	opts := configExportOptions{Project: "web", Server: "Prod-1.example.com:2222", User: "deploy", SSHPort: 22, NoValidate: true}
	if err := normalizeConfigExportOptions(&opts); err != nil {
		t.Fatalf("normalizeConfigExportOptions returned error: %v", err)
	}
	if opts.Environment != "production" {
		t.Fatalf("environment = %q, want production", opts.Environment)
	}
	if opts.ServerName != "prod-1-example-com" {
		t.Fatalf("serverName = %q, want prod-1-example-com", opts.ServerName)
	}

	opts.ServerName = "123 Bad.Name!"
	if err := normalizeConfigExportOptions(&opts); err != nil {
		t.Fatalf("normalizeConfigExportOptions returned error: %v", err)
	}
	if opts.ServerName != "bad-name" {
		t.Fatalf("explicit serverName = %q, want bad-name", opts.ServerName)
	}
}

func TestMaterializeConfigExportRedactsPasswordPlaceholder(t *testing.T) {
	docs := configExportStateDocs{Desired: desiredDoc("web", "production")}
	cfg, _, err := materializeConfigExport(configExportOptions{
		Project: "web", Environment: "production", Server: "prod-1", ServerName: "prod-1", User: "deploy", SSHPort: 22, Password: "secret", NoValidate: true,
	}, docs)
	if err != nil {
		t.Fatalf("materializeConfigExport returned error: %v", err)
	}
	server := cfg.Servers["prod-1"]
	if server.Password != configExportPasswordPlaceholder {
		t.Fatalf("password = %q, want placeholder", server.Password)
	}
	if strings.Contains(server.Password, "secret") || server.SSHKey != "" {
		t.Fatalf("server config leaked connection details: %#v", server)
	}
}

func TestMaterializeConfigExportSingleTargetNodeRemapsConnectionDetails(t *testing.T) {
	desired := desiredDoc("web", "production")
	desired.TargetNodes = []string{"node-a"}
	cfg, warnings, err := materializeConfigExport(configExportOptions{
		Project: "web", Environment: "production", Server: "203.0.113.10", ServerName: "prod-1", User: "deploy", SSHPort: 2222, Password: "secret", NoValidate: true,
	}, configExportStateDocs{Desired: desired})
	if err != nil {
		t.Fatalf("materializeConfigExport returned error: %v", err)
	}
	if _, ok := cfg.Servers["prod-1"]; ok {
		t.Fatalf("unexpected server prod-1 in %#v", cfg.Servers)
	}
	server := cfg.Servers["node-a"]
	if server.Host != "203.0.113.10" || server.User != "deploy" || server.Port != 2222 {
		t.Fatalf("server node-a = %#v", server)
	}
	if !hasConfigExportWarning(warnings, "server_name_remapped") {
		t.Fatalf("warnings = %#v, want server_name_remapped", warnings)
	}
}

func TestMaterializeConfigExportMultiTargetNodeRequiresMatchingServerName(t *testing.T) {
	desired := desiredDoc("web", "production")
	desired.TargetNodes = []string{"node-b", "node-a"}
	_, _, err := materializeConfigExport(configExportOptions{
		Project: "web", Environment: "production", Server: "203.0.113.10", ServerName: "prod-1", User: "deploy", SSHPort: 22, Password: "secret", NoValidate: true,
	}, configExportStateDocs{Desired: desired})
	if err == nil {
		t.Fatal("materializeConfigExport returned nil error")
	}
	for _, want := range []string{"--server-name", "node-a", "node-b", "remote target node"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}

func TestReadConfigExportStateErrorsWhenDesiredAndActualMissing(t *testing.T) {
	_, err := readConfigExportState(fakeConfigExportReader{
		desiredErr: stateclient.ErrNotFound,
		actualErr:  stateclient.ErrNotFound,
		historyErr: stateclient.ErrNotFound,
	}, "web", "production")
	if err == nil || !strings.Contains(err.Error(), "neither desired nor actual") {
		t.Fatalf("err = %v, want missing desired+actual error", err)
	}
}

func TestRenderConfigExportResultEmitsStdoutYAMLInTextMode(t *testing.T) {
	withMachineOutput(t, outputFormatText, "", func() {
		result := testConfigExportResult(t)
		cmd := &cobra.Command{}
		var out bytes.Buffer
		var stderr bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&stderr)
		renderConfigExportWarnings(cmd, result)
		if err := renderConfigExportResult(cmd, result, ""); err != nil {
			t.Fatalf("renderConfigExportResult returned error: %v", err)
		}
		for _, want := range []string{"project:", "name: web", "servers:", "prod-1:", "password: ${TAKO_SSH_PASSWORD}", "services:", "web:"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("stdout = %q, want %q", out.String(), want)
			}
		}
		if strings.Contains(out.String(), "secret") {
			t.Fatalf("stdout leaked password: %q", out.String())
		}
		if !strings.Contains(stderr.String(), "redacted") {
			t.Fatalf("stderr = %q, want redaction warning", stderr.String())
		}
	})
}

func TestRenderConfigExportResultWritesFileWithShortFileFlag(t *testing.T) {
	withMachineOutput(t, outputFormatText, "", func() {
		result := testConfigExportResult(t)
		path := t.TempDir() + "/exported.yaml"
		cmd := &cobra.Command{}
		var out bytes.Buffer
		cmd.SetOut(&out)
		if err := renderConfigExportResult(cmd, result, path); err != nil {
			t.Fatalf("renderConfigExportResult returned error: %v", err)
		}
		if out.Len() != 0 {
			t.Fatalf("stdout = %q, want empty when writing file", out.String())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read output file: %v", err)
		}
		if !strings.Contains(string(data), "password: ${TAKO_SSH_PASSWORD}") {
			t.Fatalf("file = %q, want redacted config", data)
		}
	})
}

func TestRenderConfigExportResultMachineJSONKeepsStdoutParseable(t *testing.T) {
	withMachineOutput(t, outputFormatJSON, "", func() {
		result := testConfigExportResult(t)
		cmd := &cobra.Command{}
		var out bytes.Buffer
		var stderr bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&stderr)
		renderConfigExportWarnings(cmd, result)

		stdout := captureConfigExportStdout(t, func() {
			if err := renderConfigExportResult(cmd, result, ""); err != nil {
				t.Fatalf("renderConfigExportResult returned error: %v", err)
			}
		})
		if out.Len() != 0 {
			t.Fatalf("cmd stdout = %q, want no raw YAML", out.String())
		}
		var doc engine.ConfigExportResult
		if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
			t.Fatalf("stdout was not a JSON document: %v\n%s", err, stdout)
		}
		if doc.Kind != engine.KindConfigExportResult || doc.YAML == "" || doc.Config == nil || !doc.PasswordRedacted {
			t.Fatalf("doc = %#v, want config export result with YAML/config/redaction", doc)
		}
		if strings.Contains(stdout, "secret") {
			t.Fatalf("machine stdout leaked secret: %s", stdout)
		}
		if !strings.Contains(stderr.String(), "redacted") {
			t.Fatalf("stderr = %q, want warning", stderr.String())
		}
	})
}

func TestResolveConfigExportOutputSupportsFileAndLegacyOutput(t *testing.T) {
	withMachineOutput(t, outputFormatText, "", func() {
		cmd, opts := testConfigExportFlagCommand(t, "--file", "generated.yaml")
		path, err := resolveConfigExportOutput(cmd, opts)
		if err != nil {
			t.Fatalf("resolveConfigExportOutput returned error: %v", err)
		}
		if path != "generated.yaml" {
			t.Fatalf("path = %q, want generated.yaml", path)
		}

		cmd, opts = testConfigExportFlagCommand(t, "-o", "short.yaml")
		path, err = resolveConfigExportOutput(cmd, opts)
		if err != nil || path != "short.yaml" {
			t.Fatalf("-o path/err = %q/%v, want short.yaml/nil", path, err)
		}

		cmd, opts = testConfigExportFlagCommand(t, "--output", "legacy.yaml")
		path, err = resolveConfigExportOutput(cmd, opts)
		if err != nil || path != "legacy.yaml" {
			t.Fatalf("legacy path/err = %q/%v, want legacy.yaml/nil", path, err)
		}

		cmd, opts = testConfigExportFlagCommand(t, "--output", "json")
		path, err = resolveConfigExportOutput(cmd, opts)
		if err != nil || path != "" || outputFormatFlag != outputFormatJSON {
			t.Fatalf("json path/err/output = %q/%v/%q, want empty/nil/json", path, err, outputFormatFlag)
		}
	})
}

func TestRunConfigExportPrintsWarningsBeforeEngineError(t *testing.T) {
	withMachineOutput(t, outputFormatText, "", func() {
		oldRun := runEngineExportConfig
		defer func() { runEngineExportConfig = oldRun }()
		runEngineExportConfig = func(ctx context.Context, req engine.ConfigExportRequest) (*engine.ConfigExportResult, error) {
			return &engine.ConfigExportResult{Warnings: []engine.ConfigExportWarning{{Code: "env_redacted", Message: "environment values are redacted"}}}, fmt.Errorf("validation failed")
		}

		cmd := &cobra.Command{}
		var stderr bytes.Buffer
		cmd.SetErr(&stderr)
		err := runConfigExport(cmd, configExportOptions{Project: "web", Environment: "production", Server: "prod-1", ServerName: "prod-1", User: "deploy", SSHPort: 22})
		if err == nil {
			t.Fatal("runConfigExport returned nil error")
		}
		if !strings.Contains(stderr.String(), "environment values are redacted") {
			t.Fatalf("stderr = %q, want warning", stderr.String())
		}
	})
}

func TestReadConfigExportStateWithFakeReader(t *testing.T) {
	docs, err := readConfigExportState(fakeConfigExportReader{
		desired:    desiredDoc("web", "production"),
		actualErr:  stateclient.ErrNotFound,
		historyErr: stateclient.ErrNotFound,
	}, "web", "production")
	if err != nil {
		t.Fatalf("readConfigExportState returned error: %v", err)
	}
	if docs.Desired == nil || docs.Actual != nil || docs.History != nil {
		t.Fatalf("docs = %#v, want desired only", docs)
	}
}

func testConfigExportResult(t *testing.T) *engine.ConfigExportResult {
	t.Helper()
	cfg, warnings, err := materializeConfigExport(configExportOptions{
		Project: "web", Environment: "production", Server: "prod-1", ServerName: "prod-1", User: "deploy", SSHPort: 22, Password: "secret", NoValidate: true,
	}, configExportStateDocs{Desired: desiredDoc("web", "production")})
	if err != nil {
		t.Fatalf("materializeConfigExport returned error: %v", err)
	}
	data := "project:\n  name: web\n  version: exported\nservers:\n  prod-1:\n    host: prod-1\n    user: deploy\n    port: 22\n    password: ${TAKO_SSH_PASSWORD}\nenvironments:\n  production:\n    services:\n      web:\n        image: nginx:alpine\n        replicas: 1\n"
	result := &engine.ConfigExportResult{
		APIVersion:       takoapi.APIVersionCurrent,
		Kind:             engine.KindConfigExportResult,
		Project:          "web",
		Environment:      "production",
		PasswordRedacted: true,
		Config:           cfg,
		YAML:             data,
		Warnings:         []engine.ConfigExportWarning{{Code: "ssh_password_redacted", Message: "SSH password was redacted; generated config uses " + configExportPasswordPlaceholder}},
	}
	for _, warning := range warnings {
		result.Warnings = append(result.Warnings, engine.ConfigExportWarning{Code: warning.Code, Message: warning.Message, Service: warning.Service, Server: warning.Server})
	}
	return result
}

func testConfigExportFlagCommand(t *testing.T, args ...string) (*cobra.Command, *configExportOptions) {
	t.Helper()
	opts := &configExportOptions{}
	cmd := &cobra.Command{Use: "export"}
	addConfigExportFlags(cmd, opts)
	cmd.SetArgs(args)
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("ParseFlags(%v): %v", args, err)
	}
	return cmd, opts
}

func withMachineOutput(t *testing.T, output, events string, fn func()) {
	t.Helper()
	oldOutput := outputFormatFlag
	oldEvents := eventsFormatFlag
	oldEngine := cliEngineInstance
	outputFormatFlag = output
	eventsFormatFlag = events
	cliEngineInstance = nil
	defer func() {
		outputFormatFlag = oldOutput
		eventsFormatFlag = oldEvents
		cliEngineInstance = oldEngine
	}()
	fn()
}

func captureConfigExportStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return string(data)
}

func desiredDoc(project, environment string) *takoapi.DesiredStateDocument {
	return &takoapi.DesiredStateDocument{
		Project:     project,
		Environment: environment,
		TargetNodes: []string{"prod-1"},
		Services: map[string]takoapi.DesiredServiceDocument{
			"web": {Name: "web", Image: "nginx:alpine", Replicas: 1},
		},
	}
}

type fakeConfigExportReader struct {
	desired    *takoapi.DesiredStateDocument
	actual     *takoapi.ActualStateDocument
	history    *takoapi.DeploymentHistoryDocument
	desiredErr error
	actualErr  error
	historyErr error
}

func (f fakeConfigExportReader) ReadDesired(project, environment string) (*takoapi.DesiredStateDocument, error) {
	if f.desiredErr != nil {
		return nil, f.desiredErr
	}
	return f.desired, nil
}

func (f fakeConfigExportReader) ReadActual(project, environment string) (*takoapi.ActualStateDocument, error) {
	if f.actualErr != nil {
		return nil, f.actualErr
	}
	return f.actual, nil
}

func (f fakeConfigExportReader) ReadHistory(project, environment string) (*takoapi.DeploymentHistoryDocument, error) {
	if f.historyErr != nil {
		return nil, f.historyErr
	}
	return f.history, nil
}

func hasConfigExportWarning(warnings []configmaterialize.Warning, code string) bool {
	for _, warning := range warnings {
		if warning.Code == code {
			return true
		}
	}
	return false
}

var _ configExportStateReader = fakeConfigExportReader{}
