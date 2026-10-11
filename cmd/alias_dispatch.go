package cmd

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Project command aliases (tako.yaml `aliases:`) are resolved here, before
// cobra dispatch: `tako <alias> [args...]` is rewritten to
// `tako <global flags> <alias args...> [args...]` so the expanded command
// line runs exactly like a typed one (flags, machine contract, exit codes).
// Built-in commands always win and never read the config.

const (
	aliasCommandsGroupID = "commands"
	aliasGroupID         = "aliases"

	// aliasPlaceholderAnnotation marks the runtime-only commands registered
	// so help and completion list project aliases.
	aliasPlaceholderAnnotation = "tako.aliasPlaceholder"
)

// cobraLazyCommandNames are commands cobra adds during execution, so they are
// absent from root.Commands() when aliases are resolved.
var cobraLazyCommandNames = []string{"help", "completion", cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd}

// aliasForbiddenTargets are visible commands an alias may not run.
var aliasForbiddenTargets = map[string]bool{
	"help":                          true,
	"completion":                    true,
	cobra.ShellCompRequestCmd:       true,
	cobra.ShellCompNoDescRequestCmd: true,
	"takod":                         true,
	"internal":                      true,
}

type aliasLoaderFunc func(configPath string) (*config.ProjectAliases, error)

// aliasDispatch is the outcome of resolveAliasArgs.
type aliasDispatch struct {
	// args is the argv handed to cobra: the input unchanged, or the
	// expansion when an alias was invoked.
	args []string
	// alias is the invoked alias name, empty when no alias was expanded.
	alias string
	// trace is the -v expansion line written to stderr, if any.
	trace string
	// placeholders holds aliases to list in help and completion.
	placeholders *config.ProjectAliases
}

// executeRoot resolves project aliases and runs the command tree.
func executeRoot(ctx context.Context, root *cobra.Command, args []string, load aliasLoaderFunc) error {
	dispatch, err := resolveAliasArgs(root, args, load)
	if err != nil {
		root.PrintErrln(root.ErrPrefix(), err.Error())
		if _, unknown := err.(*aliasUnknownCommandError); unknown {
			root.PrintErrf("Run '%v --help' for usage.\n", root.CommandPath())
		}
		return err
	}
	if dispatch.placeholders != nil {
		registerAliasPlaceholders(root, dispatch.placeholders)
	}
	if dispatch.trace != "" {
		fmt.Fprintln(root.ErrOrStderr(), dispatch.trace)
	}
	root.SetArgs(dispatch.args)
	return root.ExecuteContext(ctx)
}

// aliasUnknownCommandError keeps cobra's unknown-command wording when no
// config exists to resolve a non-built-in first argument.
type aliasUnknownCommandError struct {
	message string
}

func (e *aliasUnknownCommandError) Error() string { return e.message }

type globalFlagStop int

const (
	globalFlagStopEnd        globalFlagStop = iota // no positional argument
	globalFlagStopPositional                       // positional argument at index
	globalFlagStopTerminator                       // `--`
	globalFlagStopHelp                             // -h/--help
	globalFlagStopVersion                          // --version
	globalFlagStopUnknown                          // unknown flag or missing value
)

type globalFlagScan struct {
	stop       globalFlagStop
	index      int
	configPath string
}

// scanGlobalFlags consumes root persistent flags from args[start:] until the
// first positional argument. Which flags take a value is derived from the
// real flag set (NoOptDefVal == "" means the flag consumes a value), so the
// scan follows pflag's parsing for every current and future global flag.
func scanGlobalFlags(flags *pflag.FlagSet, args []string, start int) globalFlagScan {
	scan := globalFlagScan{}
	i := start
	for i < len(args) {
		token := args[i]
		switch {
		case token == "--":
			scan.stop, scan.index = globalFlagStopTerminator, i
			return scan
		case token == "-h" || token == "--help":
			scan.stop, scan.index = globalFlagStopHelp, i
			return scan
		case token == "--version":
			scan.stop, scan.index = globalFlagStopVersion, i
			return scan
		case strings.HasPrefix(token, "--"):
			name, value, hasValue := strings.Cut(token[2:], "=")
			flag := flags.Lookup(name)
			if flag == nil {
				scan.stop, scan.index = globalFlagStopUnknown, i
				return scan
			}
			if !hasValue && flag.NoOptDefVal == "" {
				if i+1 >= len(args) {
					scan.stop, scan.index = globalFlagStopUnknown, i
					return scan
				}
				i++
				value = args[i]
			}
			if flag.Name == "config" {
				scan.configPath = value
			}
			i++
		case strings.HasPrefix(token, "-") && len(token) > 1:
			shorthands := token[1:]
			for j := 0; j < len(shorthands); j++ {
				short := shorthands[j : j+1]
				if short == "h" {
					scan.stop, scan.index = globalFlagStopHelp, i
					return scan
				}
				flag := flags.ShorthandLookup(short)
				if flag == nil {
					scan.stop, scan.index = globalFlagStopUnknown, i
					return scan
				}
				if flag.NoOptDefVal != "" {
					continue
				}
				value := strings.TrimPrefix(shorthands[j+1:], "=")
				if shorthands[j+1:] == "" {
					if i+1 >= len(args) {
						scan.stop, scan.index = globalFlagStopUnknown, i
						return scan
					}
					i++
					value = args[i]
				}
				if flag.Name == "config" {
					scan.configPath = value
				}
				break
			}
			i++
		default:
			scan.stop, scan.index = globalFlagStopPositional, i
			return scan
		}
	}
	scan.stop, scan.index = globalFlagStopEnd, len(args)
	return scan
}

// builtinCommandName reports the built-in top-level command a name
// dispatches to, including cobra command aliases and the commands cobra adds
// lazily (help, completion, __complete, __completeNoDesc).
func builtinCommandName(root *cobra.Command, name string) (string, bool) {
	for _, lazy := range cobraLazyCommandNames {
		if name == lazy {
			return lazy, true
		}
	}
	for _, child := range root.Commands() {
		if isAliasPlaceholder(child) {
			continue
		}
		if child.Name() == name || child.HasAlias(name) {
			return child.Name(), true
		}
	}
	return "", false
}

func isAliasPlaceholder(command *cobra.Command) bool {
	return command.Annotations[aliasPlaceholderAnnotation] == "true"
}

// resolveAliasArgs decides how argv is dispatched. Built-in invocations are
// returned unchanged without reading the config. A non-built-in first
// positional argument is looked up in the project aliases and expanded to
// prefix + alias args + suffix. Help, no-argument, and completion requests
// load aliases best-effort so they can be listed.
func resolveAliasArgs(root *cobra.Command, args []string, load aliasLoaderFunc) (aliasDispatch, error) {
	dispatch := aliasDispatch{args: append([]string{}, args...)}
	scan := scanGlobalFlags(root.PersistentFlags(), args, 0)

	switch scan.stop {
	case globalFlagStopHelp, globalFlagStopEnd:
		dispatch.placeholders = loadAliasesQuietly(load, scan.configPath)
		return dispatch, nil
	case globalFlagStopPositional:
	default:
		return dispatch, nil
	}

	candidate := args[scan.index]
	switch candidate {
	case "help":
		dispatch.placeholders = loadAliasesQuietly(load, scan.configPath)
		return dispatch, nil
	case cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
		configPath := scan.configPath
		if configPath == "" {
			configPath = scanGlobalFlags(root.PersistentFlags(), args, scan.index+1).configPath
		}
		dispatch.placeholders = loadAliasesQuietly(load, configPath)
		return dispatch, nil
	}
	if _, builtin := builtinCommandName(root, candidate); builtin {
		return dispatch, nil
	}

	loaded, err := load(scan.configPath)
	if err != nil {
		if config.IsConfigNotFound(err) {
			return dispatch, newAliasUnknownCommandError(root, candidate, scan.configPath)
		}
		return dispatch, &engine.InvalidRequestError{Err: fmt.Errorf("cannot resolve project alias %q: %w", candidate, err)}
	}
	value, ok := loaded.Aliases[candidate]
	if !ok {
		// Not an alias either: cobra reports the unknown command as before.
		return dispatch, nil
	}
	aliasArgs, err := config.AliasArguments(value)
	if err != nil {
		return dispatch, &engine.InvalidRequestError{Err: fmt.Errorf("aliases.%s: %w", candidate, err)}
	}
	if problem := aliasTargetProblem(root, loaded.Aliases, candidate, aliasArgs); problem != "" {
		return dispatch, &engine.InvalidRequestError{Err: fmt.Errorf("%s", problem)}
	}

	prefix := args[:scan.index]
	suffix := args[scan.index+1:]
	if err := checkAliasEnvPin(candidate, prefix, aliasArgs, suffix); err != nil {
		return dispatch, err
	}

	expanded := make([]string, 0, len(prefix)+len(aliasArgs)+len(suffix))
	expanded = append(expanded, prefix...)
	expanded = append(expanded, aliasArgs...)
	expanded = append(expanded, suffix...)
	dispatch.args = expanded
	dispatch.alias = candidate
	if verboseRequested(expanded) {
		dispatch.trace = fmt.Sprintf("alias %s -> tako %s", candidate, formatAliasArgs(expanded))
	}
	return dispatch, nil
}

func loadAliasesQuietly(load aliasLoaderFunc, configPath string) *config.ProjectAliases {
	loaded, err := load(configPath)
	if err != nil || loaded == nil || len(loaded.Aliases) == 0 {
		return nil
	}
	return loaded
}

func newAliasUnknownCommandError(root *cobra.Command, name, configPath string) error {
	where := "no tako.yaml or tako.json found for project aliases"
	if configPath != "" {
		where = fmt.Sprintf("no config file found at %s for project aliases", configPath)
	}
	message := fmt.Sprintf("unknown command %q for %q (%s)", name, root.CommandPath(), where)
	if !root.DisableSuggestions {
		if root.SuggestionsMinimumDistance <= 0 {
			root.SuggestionsMinimumDistance = 2
		}
		if suggestions := root.SuggestionsFor(name); len(suggestions) > 0 {
			var builder strings.Builder
			builder.WriteString(message)
			builder.WriteString("\n\nDid you mean this?\n")
			for _, suggestion := range suggestions {
				fmt.Fprintf(&builder, "\t%v\n", suggestion)
			}
			message = builder.String()
		}
	}
	return &aliasUnknownCommandError{message: message}
}

type aliasToken struct {
	value     string
	fromAlias bool
}

// checkAliasEnvPin rejects an invocation whose -e/--env differs from the
// environment the alias pins. Only tokens before the first `--` of the
// expanded command line count; later tokens are remote command data.
func checkAliasEnvPin(name string, prefix, aliasArgs, suffix []string) error {
	tokens := make([]aliasToken, 0, len(prefix)+len(aliasArgs)+len(suffix))
	for _, value := range prefix {
		tokens = append(tokens, aliasToken{value: value})
	}
	for _, value := range aliasArgs {
		tokens = append(tokens, aliasToken{value: value, fromAlias: true})
	}
	for _, value := range suffix {
		tokens = append(tokens, aliasToken{value: value})
	}

	pinned := ""
	var requested []string
	for i := 0; i < len(tokens); i++ {
		token := tokens[i].value
		if token == "--" {
			break
		}
		value, consumedNext, ok := envFlagValue(token, tokens, i)
		if !ok {
			continue
		}
		flagFromAlias := tokens[i].fromAlias
		if consumedNext {
			i++
		}
		if flagFromAlias {
			if pinned == "" {
				pinned = value
			}
			continue
		}
		requested = append(requested, value)
	}
	if pinned == "" {
		return nil
	}
	for _, value := range requested {
		if value != pinned {
			return &engine.InvalidRequestError{Err: fmt.Errorf("alias %s pins environment %s; remove -e %s or use the target command directly", name, pinned, value)}
		}
	}
	return nil
}

// envFlagValue extracts the value of a -e/--env flag token.
func envFlagValue(token string, tokens []aliasToken, index int) (value string, consumedNext bool, ok bool) {
	switch {
	case token == "-e" || token == "--env":
		if index+1 < len(tokens) {
			return tokens[index+1].value, true, true
		}
		return "", false, false
	case strings.HasPrefix(token, "--env="):
		return strings.TrimPrefix(token, "--env="), false, true
	case strings.HasPrefix(token, "-e") && !strings.HasPrefix(token, "--"):
		return strings.TrimPrefix(token[2:], "="), false, true
	}
	return "", false, false
}

// verboseRequested reports whether -v/--verbose appears before `--`.
func verboseRequested(args []string) bool {
	for _, token := range args {
		switch {
		case token == "--":
			return false
		case token == "-v" || token == "--verbose" || token == "--verbose=true":
			return true
		}
	}
	return false
}

func formatAliasArgs(args []string) string {
	formatted := make([]string, len(args))
	for i, arg := range args {
		if arg == "" || strings.ContainsAny(arg, " \t\n\"'\\") {
			formatted[i] = strconv.Quote(arg)
		} else {
			formatted[i] = arg
		}
	}
	return strings.Join(formatted, " ")
}

// checkAliasTargets reports the alias problems that need the command tree:
// an error when an alias does not start with a visible, runnable top-level
// command, and a warning when a built-in with the same name shadows the
// alias (built-ins always win, so the alias can never run). Shadowing is
// never an error so a future Tako release that adds a command does not
// invalidate an existing tako.yaml.
func checkAliasTargets(root *cobra.Command, aliases config.AliasMap) []engine.ValidateFinding {
	var findings []engine.ValidateFinding
	for _, name := range aliases.Names() {
		field := "aliases." + name
		if builtin, shadowed := builtinCommandName(root, name); shadowed {
			findings = append(findings, engine.ValidateFinding{
				Severity: engine.ValidateSeverityWarn,
				Field:    field,
				Message:  fmt.Sprintf("alias %q is shadowed by built-in command %q and can never run; rename the alias", name, builtin),
			})
		}
		args, err := config.AliasArguments(aliases[name])
		if err != nil {
			findings = append(findings, engine.ValidateFinding{Severity: engine.ValidateSeverityError, Field: field, Message: fmt.Sprintf("%s: %v", field, err)})
			continue
		}
		if message := aliasTargetProblem(root, aliases, name, args); message != "" {
			findings = append(findings, engine.ValidateFinding{Severity: engine.ValidateSeverityError, Field: field, Message: message})
		}
	}
	return findings
}

// aliasTargetProblem returns why an alias cannot run, or "" when its first
// argument names a visible, runnable built-in command. Expansion is single
// level: a first argument that names another alias (and no built-in) is an
// error, so alias cycles are impossible.
func aliasTargetProblem(root *cobra.Command, aliases config.AliasMap, name string, args []string) string {
	target := args[0]
	if aliasForbiddenTargets[target] {
		return fmt.Sprintf("alias %q targets %q, which aliases cannot run", name, target)
	}
	var command *cobra.Command
	for _, child := range root.Commands() {
		if isAliasPlaceholder(child) {
			continue
		}
		if child.Name() == target || child.HasAlias(target) {
			command = child
			break
		}
	}
	if command == nil {
		if _, isAlias := aliases[target]; isAlias {
			return fmt.Sprintf("alias %q: aliases cannot reference other aliases (%q is an alias); repeat its arguments instead", name, target)
		}
		return fmt.Sprintf("alias %q targets unknown command %q", name, target)
	}
	if aliasForbiddenTargets[command.Name()] || command.Hidden {
		return fmt.Sprintf("alias %q targets %q, which aliases cannot run", name, target)
	}
	if found, _, err := root.Find(args); err == nil && found != nil {
		for current := found; current != nil && current != root; current = current.Parent() {
			if current.Hidden {
				return fmt.Sprintf("alias %q targets hidden command %q, which aliases cannot run", name, current.CommandPath())
			}
		}
	}
	return ""
}

// registerAliasPlaceholders adds one runtime-only command per reachable
// alias so `tako --help` lists them under "Project Aliases:" and cobra's
// dynamic completion offers them. Placeholders are never dispatched (argv
// is rewritten before cobra runs) and are only registered from Execute, so
// generated man pages, completion scripts, and coverage tests never see
// them.
func registerAliasPlaceholders(root *cobra.Command, loaded *config.ProjectAliases) {
	type placeholder struct {
		name string
		args []string
	}
	var reachable []placeholder
	for _, name := range loaded.Aliases.Names() {
		if _, shadowed := builtinCommandName(root, name); shadowed {
			continue
		}
		args, err := config.AliasArguments(loaded.Aliases[name])
		if err != nil {
			continue
		}
		reachable = append(reachable, placeholder{name: name, args: args})
	}
	if len(reachable) == 0 {
		return
	}

	if !root.ContainsGroup(aliasCommandsGroupID) {
		root.AddGroup(&cobra.Group{ID: aliasCommandsGroupID, Title: "Available Commands:"})
	}
	if !root.ContainsGroup(aliasGroupID) {
		root.AddGroup(&cobra.Group{ID: aliasGroupID, Title: "Project Aliases:"})
	}
	for _, child := range root.Commands() {
		if child.GroupID == "" {
			child.GroupID = aliasCommandsGroupID
		}
	}
	root.SetHelpCommandGroupID(aliasCommandsGroupID)
	root.SetCompletionCommandGroupID(aliasCommandsGroupID)

	for _, alias := range reachable {
		expansion := "tako " + formatAliasArgs(alias.args)
		name := alias.name
		root.AddCommand(&cobra.Command{
			Use:   name + " [args...]",
			Short: "alias for: " + expansion,
			Long: fmt.Sprintf(`Project alias defined in %s.

Runs: %s [args...]

Arguments after the alias name are appended to the expansion. Global flags
such as --config, -e and -v go before the alias name.`, loaded.ConfigPath, expansion),
			GroupID:               aliasGroupID,
			DisableFlagParsing:    true,
			DisableFlagsInUseLine: true,
			Annotations:           map[string]string{aliasPlaceholderAnnotation: "true"},
			RunE: func(*cobra.Command, []string) error {
				return fmt.Errorf("internal error: project alias %q was not expanded before dispatch", name)
			},
		})
	}
}
