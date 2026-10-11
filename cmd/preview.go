package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/spf13/cobra"
)

var (
	previewDestroyYes bool
	previewPruneYes   bool
	previewPruneDry   bool
)

var previewCmd = &cobra.Command{
	Use:   "preview",
	Short: "List, destroy and prune preview environments",
	Long: `Manage preview environments: short-lived copies of a template environment,
one per pull request or branch, created with 'tako deploy --preview <name>'.

See docs/PREVIEW-ENVIRONMENTS.md for template setup, DNS and limits.`,
}

var previewListCmd = &cobra.Command{
	Use:          "list",
	Short:        "List the project's previews and their expiry",
	SilenceUsage: true,
	Args:         cobra.NoArgs,
	RunE:         runPreviewList,
}

var previewDestroyCmd = &cobra.Command{
	Use:   "destroy <name>",
	Short: "Destroy a preview and everything it owns",
	Long: `Destroy one preview: its proxy routes, containers, networks, volumes,
deployment history, service files and state, on every server it was deployed
to. Images and project-wide deploy files are shared with the base
environment and are never removed. Destroying a preview that does not exist
succeeds with status not_found.`,
	SilenceUsage: true,
	Args:         cobra.ExactArgs(1),
	RunE:         runPreviewDestroy,
}

var previewPruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Destroy every expired preview",
	Long: `Destroy every preview whose expiry has passed, one at a time. A preview whose
lease is held by another operation is skipped and reported. The command
exits 6 when any expired preview could not be destroyed.`,
	SilenceUsage: true,
	Args:         cobra.NoArgs,
	RunE:         runPreviewPrune,
}

func init() {
	rootCmd.AddCommand(previewCmd)
	previewCmd.AddCommand(previewListCmd, previewDestroyCmd, previewPruneCmd)
	previewDestroyCmd.Flags().BoolVarP(&previewDestroyYes, "yes", "y", false, "Approve destruction without the confirmation prompt (required in non-interactive and machine modes)")
	previewPruneCmd.Flags().BoolVarP(&previewPruneYes, "yes", "y", false, "Approve destruction without the confirmation prompt (required in non-interactive and machine modes)")
	previewPruneCmd.Flags().BoolVar(&previewPruneDry, "dry-run", false, "List the expired previews prune would destroy without changing anything")
	markProjectMutating(previewDestroyCmd, previewPruneCmd)
}

// previewEngine is the slice of the engine the preview commands use; a seam
// for tests.
type previewEngine interface {
	ListPreviews(ctx context.Context, cfg *config.Config) (*engine.PreviewListResult, error)
	DestroyPreview(ctx context.Context, req engine.PreviewDestroyRequest) (*engine.PreviewDestroyResult, error)
	PrunePreviews(ctx context.Context, req engine.PreviewPruneRequest) (*engine.PreviewPruneResult, error)
}

var newPreviewEngine = func() previewEngine { return cliEngine() }

func runPreviewList(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return formatConfigLoadError(cfgFile, err)
	}
	result, err := newPreviewEngine().ListPreviews(cmd.Context(), cfg)
	if result != nil {
		if !machineOutputEnabled() {
			renderPreviewList(cmd.OutOrStdout(), result, time.Now())
		}
		if emitErr := emitResultDocument(result); emitErr != nil && err == nil {
			err = emitErr
		}
	}
	return err
}

func renderPreviewList(out io.Writer, result *engine.PreviewListResult, now time.Time) {
	if len(result.Items) == 0 {
		fmt.Fprintf(out, "No previews for %s.\n", result.Project)
		return
	}
	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "NAME\tBASE\tSTATUS\tEXPIRES\tUPDATED\tSOURCE\tNODES")
	for _, item := range result.Items {
		expires := "never"
		if item.ExpiresAt != nil {
			expires = item.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC")
		}
		source := "-"
		if item.Source != nil {
			parts := []string{}
			if item.Source.Branch != "" {
				parts = append(parts, item.Source.Branch)
			}
			if item.Source.Commit != "" {
				parts = append(parts, item.Source.Commit)
			}
			if len(parts) > 0 {
				source = strings.Join(parts, "@")
			}
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", item.Name, item.Base, item.Status, expires, summaryAge(now.Sub(item.UpdatedAt)), source, strings.Join(item.Nodes, ","))
	}
	table.Flush()
}

func summaryAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func runPreviewDestroy(cmd *cobra.Command, args []string) error {
	name, err := config.NormalizePreviewName(args[0])
	if err != nil {
		return &engine.InvalidRequestError{Err: err}
	}
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return formatConfigLoadError(cfgFile, err)
	}
	if !previewDestroyYes {
		reason := fmt.Sprintf("preview destroy removes preview %s with its containers, volumes and history", name)
		proceed, err := confirmPreviewAction(reason, "preview destroy", cfg.Project.Name, config.PreviewIdentity(name), fmt.Sprintf("\nDestroy preview %s? (y/N): ", name))
		if err != nil || !proceed {
			return err
		}
	}
	result, err := newPreviewEngine().DestroyPreview(cmd.Context(), engine.PreviewDestroyRequest{
		Config:       cfg,
		Name:         name,
		LoadInstance: previewInstanceLoader(cfgFile),
		Verbose:      verbose,
	})
	if result != nil {
		if emitErr := emitResultDocument(result); emitErr != nil && err == nil {
			err = emitErr
		}
	}
	return err
}

func runPreviewPrune(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return formatConfigLoadError(cfgFile, err)
	}
	if !previewPruneYes && !previewPruneDry {
		reason := "preview prune destroys every expired preview with its containers, volumes and history"
		proceed, err := confirmPreviewAction(reason, "preview prune", cfg.Project.Name, "", "\nDestroy every expired preview? (y/N): ")
		if err != nil || !proceed {
			return err
		}
	}
	result, err := newPreviewEngine().PrunePreviews(cmd.Context(), engine.PreviewPruneRequest{
		Config:       cfg,
		DryRun:       previewPruneDry,
		LoadInstance: previewInstanceLoader(cfgFile),
	})
	if result != nil {
		if !machineOutputEnabled() {
			renderPreviewPrune(cmd.OutOrStdout(), result)
		}
		if emitErr := emitResultDocument(result); emitErr != nil && err == nil {
			err = emitErr
		}
	}
	return err
}

func renderPreviewPrune(out io.Writer, result *engine.PreviewPruneResult) {
	verb := "Destroyed"
	if result.DryRun {
		verb = "Would destroy"
	}
	if len(result.Destroyed) == 0 && len(result.Skipped) == 0 && len(result.Failed) == 0 {
		fmt.Fprintln(out, "No expired previews.")
		return
	}
	for _, entry := range result.Destroyed {
		fmt.Fprintf(out, "%s %s (from %s)\n", verb, entry.Name, entry.Base)
	}
	for _, entry := range result.Skipped {
		fmt.Fprintf(out, "Skipped %s: %s\n", entry.Name, entry.Reason)
	}
	for _, entry := range result.Failed {
		fmt.Fprintf(out, "Failed %s: %s\n", entry.Name, entry.Reason)
	}
}

// confirmPreviewAction asks for approval of a destructive preview command.
// Machine and non-interactive modes require --yes (exit 2).
func confirmPreviewAction(reason string, operation string, project string, environment string, prompt string) (bool, error) {
	if machineOutputEnabled() {
		if err := emitResultDocument(newOperationConfirmationRequiredDocument(reason, operation, project, environment, nil)); err != nil {
			return false, err
		}
		return false, &engine.ConfirmationRequiredError{Reason: reason + "; rerun with --yes"}
	}
	if err := requireDeployPromptAllowed(reason); err != nil {
		return false, &engine.ConfirmationRequiredError{Reason: err.Error()}
	}
	confirmed, err := confirmDeployAction(prompt, reason)
	if err != nil {
		return false, err
	}
	if !confirmed {
		fmt.Println("Cancelled; nothing was changed.")
	}
	return confirmed, nil
}
