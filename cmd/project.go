package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/projectbinding"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/spf13/cobra"
)

const projectMutationAnnotation = "tako.projectMutation"

var projectAttachCluster string

var projectCmd = &cobra.Command{
	Use:   "project",
	Short: "Manage the current project workspace",
}

var projectAttachCmd = &cobra.Command{
	Use:          "attach",
	Short:        "Attach this workspace to an existing enrolled platform cluster",
	SilenceUsage: true,
	RunE:         runProjectAttach,
}

type clusterAttachmentRequiredDocument struct {
	APIVersion              string `json:"apiVersion"`
	Kind                    string `json:"kind"`
	Status                  string `json:"status"`
	Project                 string `json:"project"`
	ClusterID               string `json:"clusterId"`
	LocalNodeID             string `json:"localNodeId,omitempty"`
	LocalNodeName           string `json:"localNodeName,omitempty"`
	PublishedControllerID   string `json:"publishedControllerNodeId,omitempty"`
	PublishedControllerName string `json:"publishedControllerNodeName,omitempty"`
	Acknowledgement         string `json:"acknowledgement"`
}

func init() {
	rootCmd.AddCommand(projectCmd)
	projectCmd.AddCommand(projectAttachCmd)
	projectAttachCmd.Flags().StringVar(&projectAttachCluster, "cluster", "", "Exact immutable platform cluster ID (required)")
	_ = projectAttachCmd.MarkFlagRequired("cluster")
}

func markProjectMutating(cmds ...*cobra.Command) {
	for _, command := range cmds {
		if command.Annotations == nil {
			command.Annotations = map[string]string{}
		}
		command.Annotations[projectMutationAnnotation] = "true"
	}
}

func requireProjectMutationAttachment(cmd *cobra.Command) error {
	if cmd == nil || cmd.Annotations[projectMutationAnnotation] != "true" {
		return nil
	}
	configPath := resolveDeployConfigPath(cfgFile)
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return &engine.InvalidRequestError{Err: fmt.Errorf("load project config for platform attachment preflight: %w", err)}
	}
	return requireProjectMutationAttachmentForConfig(cmd, cfg, configPath)
}

func requireProjectMutationAttachmentForConfig(_ *cobra.Command, cfg *config.Config, configPath string) error {
	context, err := config.ResolveProjectClusterContext(cfg)
	if err != nil {
		return &engine.InvalidRequestError{Err: err}
	}
	if context == nil {
		return nil
	}
	path, err := projectbinding.PathForConfig(configPath)
	if err != nil {
		return &engine.InvalidRequestError{Err: err}
	}
	binding, err := projectbinding.ReadOptional(path)
	if err != nil {
		return &engine.InvalidRequestError{Err: fmt.Errorf("read workspace platform attachment: %w", err)}
	}
	if binding == nil {
		if machineOutputEnabled() {
			if emitErr := emitResultDocument(newClusterAttachmentRequiredDocument(cfg.Project.Name, *context)); emitErr != nil {
				return emitErr
			}
		}
		return &engine.InvalidRequestError{Err: fmt.Errorf("workspace is not attached to platform cluster %s; run 'tako project attach --cluster %s' before this project mutation", context.ClusterID, context.ClusterID)}
	}
	if err := binding.Matches(cfg.Project.Name, *context); err != nil {
		return &engine.InvalidRequestError{Err: fmt.Errorf("refusing project mutation with conflicting workspace platform attachment: %w", err)}
	}
	return nil
}

func newClusterAttachmentRequiredDocument(project string, platform config.PlatformContext) clusterAttachmentRequiredDocument {
	return clusterAttachmentRequiredDocument{
		APIVersion: takoapi.APIVersionV1Alpha1, Kind: "ClusterAttachmentRequired", Status: "required",
		Project: project, ClusterID: platform.ClusterID,
		LocalNodeID: platform.LocalNodeID, LocalNodeName: platform.LocalNodeName,
		PublishedControllerID: platform.ControllerNodeID, PublishedControllerName: platform.ControllerNodeName,
		Acknowledgement: "tako project attach --cluster " + platform.ClusterID,
	}
}

func runProjectAttach(cmd *cobra.Command, _ []string) error {
	configPath := resolveDeployConfigPath(cfgFile)
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return &engine.InvalidRequestError{Err: fmt.Errorf("load project config: %w", err)}
	}
	context, err := config.ResolveProjectClusterContext(cfg)
	if err != nil {
		return &engine.InvalidRequestError{Err: err}
	}
	if context == nil {
		return &engine.InvalidRequestError{Err: fmt.Errorf("project attach requires an enrolled local platform or fully identified off-node SSH servers")}
	}
	accepted := strings.ToLower(strings.TrimSpace(projectAttachCluster))
	if !strings.EqualFold(accepted, context.ClusterID) {
		return &engine.InvalidRequestError{Err: fmt.Errorf("--cluster %s does not match detected platform cluster %s", accepted, context.ClusterID)}
	}
	path, err := projectbinding.PathForConfig(configPath)
	if err != nil {
		return &engine.InvalidRequestError{Err: err}
	}
	binding, err := projectbinding.ReadOptional(path)
	if err != nil {
		return &engine.InvalidRequestError{Err: err}
	}
	if binding == nil {
		binding, err = projectbinding.New(cfg.Project.Name, *context, time.Now())
		if err != nil {
			return &engine.InvalidRequestError{Err: err}
		}
		binding, err = projectbinding.Create(path, *binding)
		if err != nil {
			return &engine.InvalidRequestError{Err: err}
		}
	}
	if err := binding.Matches(cfg.Project.Name, *context); err != nil {
		return &engine.InvalidRequestError{Err: err}
	}
	if machineOutputEnabled() {
		return emitResultDocument(binding)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Attached project %s to platform cluster %s.\n", binding.Project, binding.ClusterID)
	if binding.ControllerNodeID != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "Published controller: %s (%s).\n", binding.ControllerNodeName, binding.ControllerNodeID)
	}
	return nil
}
