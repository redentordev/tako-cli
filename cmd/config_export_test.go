package cmd

import (
	"bytes"
	"strings"
	"testing"

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

func TestWriteMaterializedConfigEmitsStdoutYAML(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	err := writeMaterializedConfig(cmd, configExportOptions{
		Project: "web", Environment: "production", Server: "prod-1", ServerName: "prod-1", User: "deploy", SSHPort: 22, Password: "secret", NoValidate: true,
	}, configExportStateDocs{Desired: desiredDoc("web", "production")})
	if err != nil {
		t.Fatalf("writeMaterializedConfig returned error: %v", err)
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

var _ configExportStateReader = fakeConfigExportReader{}
