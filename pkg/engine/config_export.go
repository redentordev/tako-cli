package engine

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/configmaterialize"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/stateclient"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"gopkg.in/yaml.v3"
)

const (
	// KindConfigExportResult identifies the machine-readable result document for
	// `tako config export` and `tako config pull`.
	KindConfigExportResult = "ConfigExportResult"
	// ConfigExportPasswordPlaceholder is written to generated configs instead of
	// a caller-provided SSH password.
	ConfigExportPasswordPlaceholder = "${TAKO_SSH_PASSWORD}"
)

// ConfigExportRequest describes a config materialization operation against a
// remote takod node. File writing is intentionally not part of the request; CLI
// and SDK adapters decide where to persist or render the returned config.
type ConfigExportRequest struct {
	Project     string
	Environment string
	Server      string
	ServerName  string
	User        string
	SSHPort     int
	SSHKey      string
	Password    string
	Socket      string
	NoValidate  bool
}

// ConfigExportStateReader is the subset of stateclient.Client used by config
// export. Tests and SDK users can provide fakes without opening SSH sessions.
type ConfigExportStateReader interface {
	ReadDesired(project, environment string) (*takoapi.DesiredStateDocument, error)
	ReadActual(project, environment string) (*takoapi.ActualStateDocument, error)
	ReadHistory(project, environment string) (*takoapi.DeploymentHistoryDocument, error)
}

// ConfigExportStateDocs groups the remote takod documents used to materialize a
// config. Desired and actual may be independently absent, but not both.
type ConfigExportStateDocs struct {
	Desired *takoapi.DesiredStateDocument
	Actual  *takoapi.ActualStateDocument
	History *takoapi.DeploymentHistoryDocument
}

// ConfigExportWarning is a JSON-tagged warning for machine result documents.
type ConfigExportWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Service string `json:"service,omitempty"`
	Server  string `json:"server,omitempty"`
}

// ConfigExportServer describes the connection details attached to one generated
// server entry. Passwords are never returned; PasswordRedacted reports whether a
// placeholder was written to the config.
type ConfigExportServer struct {
	Name             string `json:"name"`
	Host             string `json:"host"`
	User             string `json:"user"`
	Port             int    `json:"port,omitempty"`
	SSHKey           string `json:"sshKey,omitempty"`
	PasswordRedacted bool   `json:"passwordRedacted,omitempty"`
}

// ConfigExportSource describes the remote state source node supplied by the
// caller.
type ConfigExportSource struct {
	Host                string `json:"host"`
	User                string `json:"user"`
	Port                int    `json:"port,omitempty"`
	Socket              string `json:"socket,omitempty"`
	RequestedServerName string `json:"requestedServerName,omitempty"`
}

// ConfigExportDocuments reports which takod state documents were present.
type ConfigExportDocuments struct {
	Desired bool `json:"desired"`
	Actual  bool `json:"actual"`
	History bool `json:"history"`
}

// ConfigExportResult is the machine-readable outcome of config export/pull.
// Config always contains the materialized data. YAML contains the generated
// YAML text; CLI adapters omit it from machine results when an output file was
// written, so stdout never carries raw YAML outside a JSON/NDJSON envelope.
type ConfigExportResult struct {
	APIVersion       string                `json:"apiVersion"`
	Kind             string                `json:"kind"`
	Project          string                `json:"project"`
	Environment      string                `json:"environment"`
	Source           ConfigExportSource    `json:"source"`
	TargetNodes      []string              `json:"targetNodes,omitempty"`
	Servers          []ConfigExportServer  `json:"servers"`
	Documents        ConfigExportDocuments `json:"documents"`
	Warnings         []ConfigExportWarning `json:"warnings,omitempty"`
	PasswordRedacted bool                  `json:"passwordRedacted,omitempty"`
	OutputPath       string                `json:"outputPath,omitempty"`
	Config           *config.Config        `json:"config,omitempty"`
	YAML             string                `json:"yaml,omitempty"`
}

// ExportConfig reads replicated takod state over SSH and materializes it into a
// Tako config. The engine does not print or write files.
func (e *Engine) ExportConfig(ctx context.Context, req ConfigExportRequest) (*ConfigExportResult, error) {
	if err := NormalizeConfigExportRequest(&req); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.RegisterSecret(req.Password)

	client, err := ssh.NewClientWithAuth(req.Server, req.SSHPort, req.User, req.SSHKey, req.Password)
	if err != nil {
		return nil, &ConnectivityError{Server: req.Server, Err: err}
	}
	if err := client.Connect(); err != nil {
		_ = client.Close()
		return nil, &ConnectivityError{Server: req.Server, Err: err}
	}
	defer client.Close()

	reader := stateclient.New(client).WithSocket(req.Socket)
	return e.exportConfigFromState(ctx, req, reader)
}

func (e *Engine) exportConfigFromState(ctx context.Context, req ConfigExportRequest, reader ConfigExportStateReader) (*ConfigExportResult, error) {
	if err := NormalizeConfigExportRequest(&req); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.RegisterSecret(req.Password)

	docs, err := ReadConfigExportState(reader, req.Project, req.Environment)
	if err != nil {
		return nil, err
	}
	cfg, warnings, err := MaterializeConfigExport(req, docs)
	result := newConfigExportResult(req, docs, cfg, warnings)
	if err != nil {
		return result, err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return result, fmt.Errorf("failed to marshal YAML config: %w", err)
	}
	result.YAML = string(data)
	return result, nil
}

// NormalizeConfigExportRequest trims and defaults a config export request.
func NormalizeConfigExportRequest(req *ConfigExportRequest) error {
	req.Project = strings.TrimSpace(req.Project)
	req.Environment = strings.TrimSpace(req.Environment)
	req.Server = strings.TrimSpace(req.Server)
	req.ServerName = strings.TrimSpace(req.ServerName)
	req.User = strings.TrimSpace(req.User)
	req.SSHKey = strings.TrimSpace(req.SSHKey)
	req.Socket = strings.TrimSpace(req.Socket)
	if req.Project == "" {
		return invalidRequestf("--project is required")
	}
	if req.Environment == "" {
		req.Environment = "production"
	}
	if req.Server == "" {
		return invalidRequestf("--server is required")
	}
	if req.ServerName == "" {
		req.ServerName = SanitizeConfigExportServerName(req.Server)
	} else {
		req.ServerName = SanitizeConfigExportServerName(req.ServerName)
	}
	if req.ServerName == "" {
		return invalidRequestf("--server-name could not be derived from --server")
	}
	if req.User == "" {
		return invalidRequestf("--user is required")
	}
	if req.SSHPort <= 0 {
		return invalidRequestf("--ssh-port must be greater than 0")
	}
	return nil
}

// ReadConfigExportState reads desired, actual, and history documents, treating
// individual not-found responses as absent and requiring desired or actual.
func ReadConfigExportState(reader ConfigExportStateReader, project, environment string) (ConfigExportStateDocs, error) {
	var docs ConfigExportStateDocs
	var err error
	docs.Desired, err = reader.ReadDesired(project, environment)
	if err != nil && !errors.Is(err, stateclient.ErrNotFound) {
		return docs, err
	}
	if errors.Is(err, stateclient.ErrNotFound) {
		docs.Desired = nil
	}
	docs.Actual, err = reader.ReadActual(project, environment)
	if err != nil && !errors.Is(err, stateclient.ErrNotFound) {
		return docs, err
	}
	if errors.Is(err, stateclient.ErrNotFound) {
		docs.Actual = nil
	}
	if docs.Desired == nil && docs.Actual == nil {
		return docs, invalidRequestf("remote takod state for project %q environment %q has neither desired nor actual state", project, environment)
	}
	docs.History, err = reader.ReadHistory(project, environment)
	if err != nil && !errors.Is(err, stateclient.ErrNotFound) {
		return docs, err
	}
	if errors.Is(err, stateclient.ErrNotFound) {
		docs.History = nil
	}
	return docs, nil
}

// MaterializeConfigExport builds a config from already-read takod documents.
func MaterializeConfigExport(req ConfigExportRequest, docs ConfigExportStateDocs) (*config.Config, []configmaterialize.Warning, error) {
	if err := NormalizeConfigExportRequest(&req); err != nil {
		return nil, nil, err
	}
	if docs.Desired == nil && docs.Actual == nil {
		return nil, nil, invalidRequestf("remote takod state for project %q environment %q has neither desired nor actual state", req.Project, req.Environment)
	}
	server := config.ServerConfig{Host: req.Server, User: req.User, Port: req.SSHPort}
	if req.Password != "" {
		server.Password = ConfigExportPasswordPlaceholder
	} else if req.SSHKey != "" {
		server.SSHKey = req.SSHKey
	}
	servers, mappingWarnings, err := configExportServerMapping(req, docs, server)
	if err != nil {
		return nil, mappingWarnings, err
	}
	cfg, warnings, err := configmaterialize.BuildConfig(configmaterialize.Options{
		Desired:  docs.Desired,
		Actual:   docs.Actual,
		History:  docs.History,
		Servers:  servers,
		Validate: !req.NoValidate,
	})
	warnings = append(mappingWarnings, warnings...)
	return cfg, warnings, err
}

func newConfigExportResult(req ConfigExportRequest, docs ConfigExportStateDocs, cfg *config.Config, warnings []configmaterialize.Warning) *ConfigExportResult {
	result := &ConfigExportResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        KindConfigExportResult,
		Project:     req.Project,
		Environment: req.Environment,
		Source: ConfigExportSource{
			Host:                req.Server,
			User:                req.User,
			Port:                req.SSHPort,
			Socket:              req.Socket,
			RequestedServerName: req.ServerName,
		},
		TargetNodes: RemoteConfigExportTargetNodes(docs),
		Documents: ConfigExportDocuments{
			Desired: docs.Desired != nil,
			Actual:  docs.Actual != nil,
			History: docs.History != nil,
		},
		PasswordRedacted: req.Password != "",
		Config:           cfg,
	}
	for _, warning := range warnings {
		result.Warnings = append(result.Warnings, ConfigExportWarning{
			Code:    warning.Code,
			Message: warning.Message,
			Service: warning.Service,
			Server:  warning.Server,
		})
	}
	if req.Password != "" {
		result.Warnings = append(result.Warnings, ConfigExportWarning{
			Code:    "ssh_password_redacted",
			Message: fmt.Sprintf("SSH password was redacted; generated config uses %s", ConfigExportPasswordPlaceholder),
		})
	}
	if cfg != nil {
		for name, server := range cfg.Servers {
			result.Servers = append(result.Servers, ConfigExportServer{
				Name:             name,
				Host:             server.Host,
				User:             server.User,
				Port:             server.Port,
				SSHKey:           server.SSHKey,
				PasswordRedacted: server.Password == ConfigExportPasswordPlaceholder,
			})
		}
		sort.Slice(result.Servers, func(i, j int) bool { return result.Servers[i].Name < result.Servers[j].Name })
	}
	return result
}

func configExportServerMapping(req ConfigExportRequest, docs ConfigExportStateDocs, server config.ServerConfig) (map[string]config.ServerConfig, []configmaterialize.Warning, error) {
	targetNodes := RemoteConfigExportTargetNodes(docs)
	if len(targetNodes) == 0 {
		return map[string]config.ServerConfig{req.ServerName: server}, nil, nil
	}
	if len(targetNodes) == 1 {
		targetNode := targetNodes[0]
		var warnings []configmaterialize.Warning
		if targetNode != req.ServerName {
			warnings = append(warnings, configmaterialize.Warning{
				Code:    "server_name_remapped",
				Server:  targetNode,
				Message: fmt.Sprintf("remote state targets server key %q; attached supplied connection details there instead of %q", targetNode, req.ServerName),
			})
		}
		return map[string]config.ServerConfig{targetNode: server}, warnings, nil
	}
	for _, targetNode := range targetNodes {
		if targetNode == req.ServerName {
			return map[string]config.ServerConfig{targetNode: server}, nil, nil
		}
	}
	return nil, nil, invalidRequestf("--server-name %q does not match any remote target node (%s); pass --server-name with one of the remote target node keys", req.ServerName, strings.Join(targetNodes, ", "))
}

// RemoteConfigExportTargetNodes returns the clean sorted target node list from
// desired state, or actual state when desired has no targets.
func RemoteConfigExportTargetNodes(docs ConfigExportStateDocs) []string {
	if docs.Desired != nil {
		if nodes := CleanConfigExportTargetNodes(docs.Desired.TargetNodes); len(nodes) > 0 {
			return nodes
		}
	}
	if docs.Actual != nil {
		if nodes := CleanConfigExportTargetNodes(docs.Actual.TargetNodes); len(nodes) > 0 {
			return nodes
		}
	}
	return nil
}

// CleanConfigExportTargetNodes trims, deduplicates, and sorts target node keys.
func CleanConfigExportTargetNodes(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

var invalidConfigExportServerNameChars = regexp.MustCompile(`[^a-z0-9_-]+`)

// SanitizeConfigExportServerName converts a host or supplied name into a valid
// generated server key.
func SanitizeConfigExportServerName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if host, _, ok := strings.Cut(value, ":"); ok {
		value = host
	}
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
			lastDash = false
		case r == '-':
			if !lastDash {
				b.WriteRune('-')
				lastDash = true
			}
		default:
			if !lastDash {
				b.WriteRune('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-_")
	out = invalidConfigExportServerNameChars.ReplaceAllString(out, "-")
	for out != "" && !unicode.IsLower(rune(out[0])) {
		out = out[1:]
	}
	if len(out) > 63 {
		out = strings.TrimRight(out[:63], "-_")
	}
	if out == "" {
		return "server"
	}
	return out
}

var _ takodclient.RequestExecutor = (*ssh.Client)(nil)
