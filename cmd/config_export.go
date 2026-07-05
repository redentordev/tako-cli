package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	takoconfig "github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/configmaterialize"
	pkgssh "github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/stateclient"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const configExportPasswordPlaceholder = "${TAKO_SSH_PASSWORD}"

type configExportOptions struct {
	Project     string
	Environment string
	Server      string
	ServerName  string
	User        string
	SSHPort     int
	SSHKey      string
	Password    string
	Socket      string
	Output      string
	NoValidate  bool
}

type configExportStateReader interface {
	ReadDesired(project, environment string) (*takoapi.DesiredStateDocument, error)
	ReadActual(project, environment string) (*takoapi.ActualStateDocument, error)
	ReadHistory(project, environment string) (*takoapi.DeploymentHistoryDocument, error)
}

type configExportStateDocs struct {
	Desired *takoapi.DesiredStateDocument
	Actual  *takoapi.ActualStateDocument
	History *takoapi.DeploymentHistoryDocument
}

var configExportCmd = newConfigExportCommand("export")
var configPullCmd = newConfigExportCommand("pull")

func init() {
	configCmd.AddCommand(configExportCmd, configPullCmd)
}

func newConfigExportCommand(name string) *cobra.Command {
	opts := configExportOptions{
		Environment: "production",
		SSHPort:     22,
		SSHKey:      "~/.ssh/id_rsa",
	}
	cmd := &cobra.Command{
		Use:          name,
		Short:        "Materialize remote takod state into a tako.yaml",
		SilenceUsage: true,
		Long: `Read desired, actual, and deployment history state from a remote takod node
through SSH and takod's Unix socket, then materialize that Tako state into a
valid tako.yaml. This reads Tako-managed replicated state; it does not discover
arbitrary non-Tako containers.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigExport(cmd, opts)
		},
	}
	addConfigExportFlags(cmd, &opts)
	return cmd
}

func addConfigExportFlags(cmd *cobra.Command, opts *configExportOptions) {
	flags := cmd.Flags()
	flags.StringVar(&opts.Project, "project", opts.Project, "Project name to read from takod state (required)")
	flags.StringVar(&opts.Environment, "environment", opts.Environment, "Environment name to read from takod state")
	flags.StringVar(&opts.Server, "server", opts.Server, "SSH host for the remote takod node (required)")
	flags.StringVar(&opts.ServerName, "server-name", opts.ServerName, "Remote target node/server key to attach connection details to when exporting multi-node state (defaults to a sanitized form of --server)")
	flags.StringVar(&opts.User, "user", opts.User, "SSH user for the remote takod node (defaults to current user)")
	flags.IntVar(&opts.SSHPort, "ssh-port", opts.SSHPort, "SSH port for the remote takod node")
	flags.StringVar(&opts.SSHKey, "ssh-key", opts.SSHKey, "SSH private key for the remote takod node")
	flags.StringVar(&opts.Password, "password", opts.Password, "SSH password for the remote takod node (not written to output)")
	flags.StringVar(&opts.Socket, "socket", opts.Socket, "Remote takod Unix socket path")
	flags.StringVarP(&opts.Output, "output", "o", opts.Output, "Write generated config to this file instead of stdout")
	flags.BoolVar(&opts.NoValidate, "no-validate", opts.NoValidate, "Skip validation of the generated config")
}

func runConfigExport(cmd *cobra.Command, opts configExportOptions) error {
	if err := normalizeConfigExportOptions(&opts); err != nil {
		return err
	}

	client, err := pkgssh.NewClientWithAuth(opts.Server, opts.SSHPort, opts.User, expandHome(opts.SSHKey), opts.Password)
	if err != nil {
		return err
	}
	if err := client.Connect(); err != nil {
		_ = client.Close()
		return err
	}
	defer client.Close()

	reader := stateclient.New(client).WithSocket(opts.Socket)
	docs, err := readConfigExportState(reader, opts.Project, opts.Environment)
	if err != nil {
		return err
	}
	return writeMaterializedConfig(cmd, opts, docs)
}

func normalizeConfigExportOptions(opts *configExportOptions) error {
	opts.Project = strings.TrimSpace(opts.Project)
	opts.Environment = strings.TrimSpace(opts.Environment)
	opts.Server = strings.TrimSpace(opts.Server)
	opts.ServerName = strings.TrimSpace(opts.ServerName)
	opts.User = strings.TrimSpace(opts.User)
	opts.SSHKey = strings.TrimSpace(opts.SSHKey)
	opts.Socket = strings.TrimSpace(opts.Socket)
	opts.Output = strings.TrimSpace(opts.Output)
	if opts.Project == "" {
		return fmt.Errorf("--project is required")
	}
	if opts.Environment == "" {
		opts.Environment = "production"
	}
	if opts.Server == "" {
		return fmt.Errorf("--server is required")
	}
	if opts.ServerName == "" {
		opts.ServerName = sanitizeConfigExportServerName(opts.Server)
	} else {
		opts.ServerName = sanitizeConfigExportServerName(opts.ServerName)
	}
	if opts.ServerName == "" {
		return fmt.Errorf("--server-name could not be derived from --server")
	}
	if opts.User == "" {
		opts.User = currentUsername()
	}
	if opts.User == "" {
		return fmt.Errorf("--user is required when the current user cannot be determined")
	}
	if opts.SSHPort <= 0 {
		return fmt.Errorf("--ssh-port must be greater than 0")
	}
	return nil
}

func readConfigExportState(reader configExportStateReader, project, environment string) (configExportStateDocs, error) {
	var docs configExportStateDocs
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
		return docs, fmt.Errorf("remote takod state for project %q environment %q has neither desired nor actual state", project, environment)
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

func writeMaterializedConfig(cmd *cobra.Command, opts configExportOptions, docs configExportStateDocs) error {
	cfg, warnings, err := materializeConfigExport(opts, docs)
	for _, warning := range warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warning.Message)
	}
	if opts.Password != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: SSH password was redacted; generated config uses %s\n", configExportPasswordPlaceholder)
	}
	if err != nil {
		return err
	}
	if opts.Output != "" {
		return takoconfig.SaveConfig(opts.Output, cfg)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal YAML config: %w", err)
	}
	_, err = cmd.OutOrStdout().Write(data)
	return err
}

func materializeConfigExport(opts configExportOptions, docs configExportStateDocs) (*takoconfig.Config, []configmaterialize.Warning, error) {
	if err := normalizeConfigExportOptions(&opts); err != nil {
		return nil, nil, err
	}
	if docs.Desired == nil && docs.Actual == nil {
		return nil, nil, fmt.Errorf("remote takod state for project %q environment %q has neither desired nor actual state", opts.Project, opts.Environment)
	}
	server := takoconfig.ServerConfig{Host: opts.Server, User: opts.User, Port: opts.SSHPort}
	if opts.Password != "" {
		server.Password = configExportPasswordPlaceholder
	} else if opts.SSHKey != "" {
		server.SSHKey = opts.SSHKey
	}
	servers, mappingWarnings, err := configExportServerMapping(opts, docs, server)
	if err != nil {
		return nil, mappingWarnings, err
	}
	cfg, warnings, err := configmaterialize.BuildConfig(configmaterialize.Options{
		Desired:  docs.Desired,
		Actual:   docs.Actual,
		History:  docs.History,
		Servers:  servers,
		Validate: !opts.NoValidate,
	})
	warnings = append(mappingWarnings, warnings...)
	return cfg, warnings, err
}

func configExportServerMapping(opts configExportOptions, docs configExportStateDocs, server takoconfig.ServerConfig) (map[string]takoconfig.ServerConfig, []configmaterialize.Warning, error) {
	targetNodes := remoteConfigExportTargetNodes(docs)
	if len(targetNodes) == 0 {
		return map[string]takoconfig.ServerConfig{opts.ServerName: server}, nil, nil
	}
	if len(targetNodes) == 1 {
		targetNode := targetNodes[0]
		var warnings []configmaterialize.Warning
		if targetNode != opts.ServerName {
			warnings = append(warnings, configmaterialize.Warning{
				Code:    "server_name_remapped",
				Server:  targetNode,
				Message: fmt.Sprintf("remote state targets server key %q; attached supplied connection details there instead of %q", targetNode, opts.ServerName),
			})
		}
		return map[string]takoconfig.ServerConfig{targetNode: server}, warnings, nil
	}
	for _, targetNode := range targetNodes {
		if targetNode == opts.ServerName {
			return map[string]takoconfig.ServerConfig{targetNode: server}, nil, nil
		}
	}
	return nil, nil, fmt.Errorf("--server-name %q does not match any remote target node (%s); pass --server-name with one of the remote target node keys", opts.ServerName, strings.Join(targetNodes, ", "))
}

func remoteConfigExportTargetNodes(docs configExportStateDocs) []string {
	if docs.Desired != nil {
		if nodes := cleanConfigExportTargetNodes(docs.Desired.TargetNodes); len(nodes) > 0 {
			return nodes
		}
	}
	if docs.Actual != nil {
		if nodes := cleanConfigExportTargetNodes(docs.Actual.TargetNodes); len(nodes) > 0 {
			return nodes
		}
	}
	return nil
}

func cleanConfigExportTargetNodes(values []string) []string {
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

func sanitizeConfigExportServerName(value string) string {
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

func currentUsername() string {
	if u, err := user.Current(); err == nil && u != nil {
		name := strings.TrimSpace(u.Username)
		if idx := strings.LastIndexAny(name, `\\/`); idx >= 0 {
			name = name[idx+1:]
		}
		return name
	}
	return strings.TrimSpace(os.Getenv("USER"))
}

func expandHome(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

var _ takodclient.RequestExecutor = (*pkgssh.Client)(nil)
