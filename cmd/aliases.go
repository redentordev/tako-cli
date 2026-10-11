package cmd

import (
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/spf13/cobra"
)

var aliasesCmd = &cobra.Command{
	Use:          "aliases",
	Short:        "List project command aliases from tako.yaml",
	SilenceUsage: true,
	Args:         cobra.NoArgs,
	Long: `List the project command aliases defined under "aliases:" in tako.yaml or
tako.json, with the Tako command each one runs.

Only the aliases section is read: no servers are contacted and environment
variables referenced elsewhere in the config are not required. Aliases whose
name matches a built-in command are reported as shadowed, because built-in
commands always take precedence.`,
	Example: `  tako aliases
  tako aliases --output json
  tako --config ./deploy/tako.yaml aliases`,
	RunE: runAliases,
}

func init() {
	rootCmd.AddCommand(aliasesCmd)
}

func runAliases(cmd *cobra.Command, args []string) error {
	loaded, err := config.LoadAliases(cfgFile)
	if err != nil {
		return &engine.InvalidRequestError{Err: err}
	}
	configPath := filepath.Clean(loaded.ConfigPath)
	result := engine.AliasListResult{
		APIVersion: takoapi.APIVersionCurrent,
		Kind:       engine.KindAliasList,
		ConfigPath: configPath,
		Aliases:    []engine.AliasEntry{},
	}
	for _, name := range loaded.Aliases.Names() {
		value := loaded.Aliases[name]
		aliasArgs, err := config.AliasArguments(value)
		if err != nil {
			return &engine.InvalidRequestError{Err: fmt.Errorf("aliases.%s: %w", name, err)}
		}
		entry := engine.AliasEntry{
			Name:   name,
			Args:   aliasArgs,
			Form:   config.AliasForm(value),
			Target: aliasArgs[0],
		}
		if builtin, shadowed := builtinCommandName(rootCmd, name); shadowed {
			entry.Shadowed = true
			entry.ShadowedBy = builtin
		}
		result.Aliases = append(result.Aliases, entry)
	}

	var aliasErrors []string
	for _, finding := range checkAliasTargets(rootCmd, loaded.Aliases) {
		finding.Path = configPath
		result.Findings = append(result.Findings, finding)
		if finding.Severity == engine.ValidateSeverityError {
			aliasErrors = append(aliasErrors, finding.Message)
		}
	}

	out := humanOut()
	if len(result.Aliases) == 0 {
		fmt.Fprintf(out, "No aliases defined in %s\n", configPath)
	} else {
		writer := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(writer, "ALIAS\tRUNS")
		for _, entry := range result.Aliases {
			note := ""
			if entry.Shadowed {
				note = fmt.Sprintf("  (shadowed by built-in %q)", entry.ShadowedBy)
			}
			fmt.Fprintf(writer, "%s\ttako %s%s\n", entry.Name, formatAliasArgs(entry.Args), note)
		}
		if err := writer.Flush(); err != nil {
			return err
		}
	}
	for _, finding := range result.Findings {
		if finding.Severity == engine.ValidateSeverityWarn {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %s\n", finding.Message)
		}
	}

	if err := emitResultDocument(result); err != nil {
		return err
	}
	if len(aliasErrors) > 0 {
		return &engine.InvalidRequestError{Err: fmt.Errorf("invalid aliases in %s: %s", configPath, strings.Join(aliasErrors, "; "))}
	}
	return nil
}
