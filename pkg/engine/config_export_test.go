package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/configmaterialize"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/stateclient"
)

func TestNormalizeConfigExportRequestValidatesAndDefaults(t *testing.T) {
	req := ConfigExportRequest{Environment: "production", Server: "prod-1", User: "deploy", SSHPort: 22}
	if err := NormalizeConfigExportRequest(&req); err == nil || !strings.Contains(err.Error(), "--project") {
		t.Fatalf("missing project err = %v, want --project error", err)
	}

	req = ConfigExportRequest{Project: "web", Server: "prod-1", SSHPort: 22}
	if err := NormalizeConfigExportRequest(&req); err == nil || !strings.Contains(err.Error(), "--user") {
		t.Fatalf("missing user err = %v, want --user error", err)
	}

	req = ConfigExportRequest{Project: "web", Server: "Prod-1.example.com:2222", User: "deploy", SSHPort: 22, NoValidate: true}
	if err := NormalizeConfigExportRequest(&req); err != nil {
		t.Fatalf("NormalizeConfigExportRequest returned error: %v", err)
	}
	if req.Environment != "production" {
		t.Fatalf("environment = %q, want production", req.Environment)
	}
	if req.ServerName != "prod-1-example-com" {
		t.Fatalf("serverName = %q, want prod-1-example-com", req.ServerName)
	}

	req.ServerName = "123 Bad.Name!"
	if err := NormalizeConfigExportRequest(&req); err != nil {
		t.Fatalf("NormalizeConfigExportRequest returned error: %v", err)
	}
	if req.ServerName != "bad-name" {
		t.Fatalf("explicit serverName = %q, want bad-name", req.ServerName)
	}
}

func TestMaterializeConfigExportRedactsAndRemapsConnectionDetails(t *testing.T) {
	desired := engineDesiredDoc("web", "production")
	desired.TargetNodes = []string{"node-a"}

	cfg, warnings, err := MaterializeConfigExport(ConfigExportRequest{
		Project: "web", Environment: "production", Server: "203.0.113.10", ServerName: "prod-1", User: "deploy", SSHPort: 2222, Password: "secret", NoValidate: true,
	}, ConfigExportStateDocs{Desired: desired})
	if err != nil {
		t.Fatalf("MaterializeConfigExport returned error: %v", err)
	}
	if _, ok := cfg.Servers["prod-1"]; ok {
		t.Fatalf("unexpected server prod-1 in %#v", cfg.Servers)
	}
	server := cfg.Servers["node-a"]
	if server.Host != "203.0.113.10" || server.User != "deploy" || server.Port != 2222 {
		t.Fatalf("server node-a = %#v", server)
	}
	if server.Password != ConfigExportPasswordPlaceholder || strings.Contains(server.Password, "secret") || server.SSHKey != "" {
		t.Fatalf("server config leaked connection details: %#v", server)
	}
	if !hasEngineConfigExportWarning(warnings, "server_name_remapped") {
		t.Fatalf("warnings = %#v, want server_name_remapped", warnings)
	}
}

func TestMaterializeConfigExportMultiTargetNodeRequiresMatchingServerName(t *testing.T) {
	desired := engineDesiredDoc("web", "production")
	desired.TargetNodes = []string{"node-b", "node-a"}
	_, _, err := MaterializeConfigExport(ConfigExportRequest{
		Project: "web", Environment: "production", Server: "203.0.113.10", ServerName: "prod-1", User: "deploy", SSHPort: 22, Password: "secret", NoValidate: true,
	}, ConfigExportStateDocs{Desired: desired})
	if err == nil {
		t.Fatal("MaterializeConfigExport returned nil error")
	}
	for _, want := range []string{"--server-name", "node-a", "node-b", "remote target node"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}

func TestReadConfigExportStateHandlesMissingDocuments(t *testing.T) {
	_, err := ReadConfigExportState(engineFakeConfigExportReader{
		desiredErr: stateclient.ErrNotFound,
		actualErr:  stateclient.ErrNotFound,
		historyErr: stateclient.ErrNotFound,
	}, "web", "production")
	if err == nil || !strings.Contains(err.Error(), "neither desired nor actual") {
		t.Fatalf("err = %v, want missing desired+actual error", err)
	}

	docs, err := ReadConfigExportState(engineFakeConfigExportReader{
		desired:    engineDesiredDoc("web", "production"),
		actualErr:  stateclient.ErrNotFound,
		historyErr: stateclient.ErrNotFound,
	}, "web", "production")
	if err != nil {
		t.Fatalf("ReadConfigExportState returned error: %v", err)
	}
	if docs.Desired == nil || docs.Actual != nil || docs.History != nil {
		t.Fatalf("docs = %#v, want desired only", docs)
	}
}

func TestExportConfigFromStateBuildsMachineResult(t *testing.T) {
	eng := New(Options{})
	result, err := eng.exportConfigFromState(context.Background(), ConfigExportRequest{
		Project: "web", Environment: "production", Server: "prod-1", ServerName: "prod-1", User: "deploy", SSHPort: 22, Password: "secret", NoValidate: true,
	}, engineFakeConfigExportReader{
		desired:    engineDesiredDoc("web", "production"),
		actualErr:  stateclient.ErrNotFound,
		historyErr: stateclient.ErrNotFound,
	})
	if err != nil {
		t.Fatalf("exportConfigFromState returned error: %v", err)
	}
	if result.Kind != KindConfigExportResult || result.Project != "web" || result.Environment != "production" {
		t.Fatalf("result identity = %#v", result)
	}
	if !result.Documents.Desired || result.Documents.Actual || result.Documents.History {
		t.Fatalf("documents = %#v", result.Documents)
	}
	if !result.PasswordRedacted || !hasResultConfigExportWarning(result.Warnings, "ssh_password_redacted") {
		t.Fatalf("warnings/passwordRedacted = %#v/%v", result.Warnings, result.PasswordRedacted)
	}
	if len(result.Servers) != 1 || result.Servers[0].Name != "prod-1" || !result.Servers[0].PasswordRedacted {
		t.Fatalf("servers = %#v", result.Servers)
	}
	if result.Config == nil || !strings.Contains(result.YAML, "password: ${TAKO_SSH_PASSWORD}") || strings.Contains(result.YAML, "secret") {
		t.Fatalf("result config/yaml invalid: yaml=%q config=%#v", result.YAML, result.Config)
	}
}

func engineDesiredDoc(project, environment string) *takoapi.DesiredStateDocument {
	return &takoapi.DesiredStateDocument{
		Project:     project,
		Environment: environment,
		TargetNodes: []string{"prod-1"},
		Services: map[string]takoapi.DesiredServiceDocument{
			"web": {Name: "web", Image: "nginx:alpine", Replicas: 1},
		},
	}
}

type engineFakeConfigExportReader struct {
	desired    *takoapi.DesiredStateDocument
	actual     *takoapi.ActualStateDocument
	history    *takoapi.DeploymentHistoryDocument
	desiredErr error
	actualErr  error
	historyErr error
}

func (f engineFakeConfigExportReader) ReadDesired(project, environment string) (*takoapi.DesiredStateDocument, error) {
	if f.desiredErr != nil {
		return nil, f.desiredErr
	}
	return f.desired, nil
}

func (f engineFakeConfigExportReader) ReadActual(project, environment string) (*takoapi.ActualStateDocument, error) {
	if f.actualErr != nil {
		return nil, f.actualErr
	}
	return f.actual, nil
}

func (f engineFakeConfigExportReader) ReadHistory(project, environment string) (*takoapi.DeploymentHistoryDocument, error) {
	if f.historyErr != nil {
		return nil, f.historyErr
	}
	return f.history, nil
}

func hasEngineConfigExportWarning(warnings []configmaterialize.Warning, code string) bool {
	for _, warning := range warnings {
		if warning.Code == code {
			return true
		}
	}
	return false
}

func hasResultConfigExportWarning(warnings []ConfigExportWarning, code string) bool {
	for _, warning := range warnings {
		if warning.Code == code {
			return true
		}
	}
	return false
}

var _ ConfigExportStateReader = engineFakeConfigExportReader{}
