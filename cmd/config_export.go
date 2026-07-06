package cmd

import (
	"context"
	"fmt"
	"strings"

	takoconfig "github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/configmaterialize"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/spf13/cobra"
)

const configExportPasswordPlaceholder = engine.ConfigExportPasswordPlaceholder

type configExportOptions struct {
	Project      string
	Environment  string
	Server       string
	ServerName   string
	User         string
	SSHPort      int
	SSHKey       string
	Password     string
	Socket       string
	File         string
	LegacyOutput string
	NoValidate   bool
}

type configExportStateReader = engine.ConfigExportStateReader
type configExportStateDocs = engine.ConfigExportStateDocs

var configExportCmd = newConfigExportCommand("export")
var configPullCmd = newConfigExportCommand("pull")

var runEngineExportConfig = func(ctx context.Context, req engine.ConfigExportRequest) (*engine.ConfigExportResult, error) {
	return cliEngine().ExportConfig(ctx, req)
}

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
arbitrary non-Tako containers.

Use --file/-o to write the generated YAML/JSON config to a file. The deprecated
local --output FILE form is still accepted for file paths, while --output text
and --output json select the global machine-output format for compatibility.`,
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
	flags.StringVarP(&opts.File, "file", "o", opts.File, "Write generated config to this file instead of stdout")
	flags.StringVar(&opts.LegacyOutput, "output", opts.LegacyOutput, "Legacy config file path; use --file/-o. Values text/json select machine output format")
	flags.BoolVar(&opts.NoValidate, "no-validate", opts.NoValidate, "Skip validation of the generated config")
}

func runConfigExport(cmd *cobra.Command, opts configExportOptions) error {
	outputPath, err := resolveConfigExportOutput(cmd, &opts)
	if err != nil {
		return err
	}
	req := opts.toEngineRequest()
	if err := engine.NormalizeConfigExportRequest(&req); err != nil {
		return err
	}

	result, err := runEngineExportConfig(cmd.Context(), req)
	if result != nil {
		renderConfigExportWarnings(cmd, result)
	}
	if err != nil {
		return err
	}
	return renderConfigExportResult(cmd, result, outputPath)
}

func (opts configExportOptions) toEngineRequest() engine.ConfigExportRequest {
	user := strings.TrimSpace(opts.User)
	if user == "" {
		user = currentUsername()
	}
	sshKey := expandHome(strings.TrimSpace(opts.SSHKey))
	return engine.ConfigExportRequest{
		Project:     opts.Project,
		Environment: opts.Environment,
		Server:      opts.Server,
		ServerName:  opts.ServerName,
		User:        user,
		SSHPort:     opts.SSHPort,
		SSHKey:      sshKey,
		Password:    opts.Password,
		Socket:      opts.Socket,
		NoValidate:  opts.NoValidate,
	}
}

func resolveConfigExportOutput(cmd *cobra.Command, opts *configExportOptions) (string, error) {
	filePath := strings.TrimSpace(opts.File)
	legacyOutput := strings.TrimSpace(opts.LegacyOutput)
	if cmd != nil && cmd.Flags().Changed("output") {
		switch legacyOutput {
		case outputFormatText, outputFormatJSON:
			outputFormatFlag = legacyOutput
		case "":
		default:
			if filePath != "" {
				return "", &engine.InvalidRequestError{Err: fmt.Errorf("use only one of --file/-o or legacy --output FILE")}
			}
			filePath = legacyOutput
		}
	}
	return filePath, nil
}

func renderConfigExportResult(cmd *cobra.Command, result *engine.ConfigExportResult, outputPath string) error {
	if result == nil {
		return nil
	}
	if outputPath != "" {
		if err := takoconfig.SaveConfig(outputPath, result.Config); err != nil {
			return err
		}
		result.OutputPath = outputPath
		result.YAML = ""
	} else if !machineOutputEnabled() {
		_, err := cmd.OutOrStdout().Write([]byte(result.YAML))
		return err
	}
	if machineOutputEnabled() {
		return emitResultDocument(result)
	}
	return nil
}

func renderConfigExportWarnings(cmd *cobra.Command, result *engine.ConfigExportResult) {
	for _, warning := range result.Warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warning.Message)
	}
}

func normalizeConfigExportOptions(opts *configExportOptions) error {
	req := opts.toEngineRequest()
	if err := engine.NormalizeConfigExportRequest(&req); err != nil {
		return err
	}
	opts.Project = req.Project
	opts.Environment = req.Environment
	opts.Server = req.Server
	opts.ServerName = req.ServerName
	opts.User = req.User
	opts.SSHPort = req.SSHPort
	opts.SSHKey = req.SSHKey
	opts.Socket = req.Socket
	opts.File = strings.TrimSpace(opts.File)
	opts.LegacyOutput = strings.TrimSpace(opts.LegacyOutput)
	return nil
}

func readConfigExportState(reader configExportStateReader, project, environment string) (configExportStateDocs, error) {
	return engine.ReadConfigExportState(reader, project, environment)
}

func materializeConfigExport(opts configExportOptions, docs configExportStateDocs) (*takoconfig.Config, []configmaterialize.Warning, error) {
	return engine.MaterializeConfigExport(opts.toEngineRequest(), docs)
}

func sanitizeConfigExportServerName(value string) string {
	return engine.SanitizeConfigExportServerName(value)
}

func remoteConfigExportTargetNodes(docs configExportStateDocs) []string {
	return engine.RemoteConfigExportTargetNodes(docs)
}

func cleanConfigExportTargetNodes(values []string) []string {
	return engine.CleanConfigExportTargetNodes(values)
}
