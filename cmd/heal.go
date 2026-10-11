package cmd

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/selfheal"
	"github.com/spf13/cobra"
)

var (
	healService     string
	healServer      string
	healJournal     int
	healFor         time.Duration
	healUntilResume bool
)

var healCmd = &cobra.Command{
	Use:   "heal",
	Short: "Inspect and control local self-healing",
	Long: `Inspect and control node-local self-healing.

Self-healing is opt-in (selfHealing.enabled in tako.yaml). takod restarts an
unhealthy container or starts a crashed one on its own node, with backoff, a
give-up limit and a node circuit breaker. It never creates containers and
never acts during a deploy, restore or pause. See docs/SELF-HEALING.md.

Examples:
  tako heal status                       # Per-service state on every node
  tako heal status --service web         # One service with its journal
  tako heal pause                        # Pause the environment for 1h
  tako heal pause --service web --for 4h # Pause one service
  tako heal pause --until-resume         # Pause until tako heal resume
  tako heal resume                       # End pauses
  tako heal reset --service web          # Clear give-up and backoff`,
}

var healStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show self-healing state, pauses, breaker and journal",
	Long: `Show the self-healing state that takod reports on every server of the
environment: per-service state and skip reason, pauses, the node circuit
breaker and the recent decision journal.

Exit codes: 0 ok or disabled; 6 a service gave up, a breaker is open or a node
could not be read; 4 every node unreachable; 2 no node supports
node.self-healing-v1 (run tako upgrade servers).`,
	Args: cobra.NoArgs,
	RunE: runHealStatus,
}

var healPauseCmd = &cobra.Command{
	Use:   "pause",
	Short: "Pause self-healing for the environment or one service",
	Long: `Pause self-healing on every server of the environment (or --server). The
pause defaults to 1h; --for sets another duration (at most 7 days) and
--until-resume pauses until tako heal resume. A pause waits for an action
already running and is in effect when the command returns.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error { return runHealChange(cmd, "pause") },
}

var healResumeCmd = &cobra.Command{
	Use:   "resume",
	Short: "End self-healing pauses",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, args []string) error { return runHealChange(cmd, "resume") },
}

var healResetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Clear give-up, counters and cooldowns (and the node breaker)",
	Long: `Clear a service's give-up state, action counters and cooldowns. Without
--service every service of the environment is reset and an open node
circuit breaker is closed.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error { return runHealChange(cmd, "reset") },
}

func init() {
	rootCmd.AddCommand(healCmd)
	healCmd.AddCommand(healStatusCmd, healPauseCmd, healResumeCmd, healResetCmd)
	for _, command := range []*cobra.Command{healStatusCmd, healPauseCmd, healResumeCmd, healResetCmd} {
		command.Flags().StringVarP(&healService, "service", "s", "", "One service only")
		command.Flags().StringVar(&healServer, "server", "", "One server only")
	}
	healStatusCmd.Flags().IntVar(&healJournal, "journal", 20, "Recent journal entries per node (0-1000)")
	healPauseCmd.Flags().DurationVar(&healFor, "for", selfheal.DefaultPause, "Pause duration (at most 168h)")
	healPauseCmd.Flags().BoolVar(&healUntilResume, "until-resume", false, "Pause until tako heal resume")
}

func healTargets(cfg *config.Config) (string, []string, error) {
	envName := getEnvironmentName(cfg)
	if healService != "" {
		services, err := cfg.GetServices(envName)
		if err != nil {
			return "", nil, fmt.Errorf("failed to get services: %w", err)
		}
		if _, ok := services[healService]; !ok {
			return "", nil, &engine.InvalidRequestError{Err: fmt.Errorf("service '%s' not found in environment %s", healService, envName)}
		}
	}
	servers, err := engine.ResolveStatusTargetServerNames(cfg, envName, healServer)
	if err != nil {
		return "", nil, err
	}
	return envName, servers, nil
}

func runHealStatus(cmd *cobra.Command, args []string) error {
	if healJournal < 0 || healJournal > selfheal.MaxJournalEntries {
		return &engine.InvalidRequestError{Err: fmt.Errorf("--journal must be between 0 and %d", selfheal.MaxJournalEntries)}
	}
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return formatConfigLoadError(cfgFile, err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}
	envName, servers, err := healTargets(cfg)
	if err != nil {
		return err
	}
	result, statusErr := engine.ReadHealStatus(cmd.Context(), cfg, envName, servers, healService, healJournal)
	if statusErr != nil {
		result.Error = cliEngine().RedactSensitive(statusErr.Error())
	}
	redactHealNodeErrors(result.Nodes)
	if !machineOutputEnabled() {
		printHealStatus(cmd.OutOrStdout(), result)
	}
	if emitErr := emitResultDocument(result); emitErr != nil && statusErr == nil {
		statusErr = emitErr
	}
	return statusErr
}

// healChangeRequest validates the pause flags (exit 2 on misuse).
func healChangeRequest(action string, forSet bool, now time.Time) (engine.HealChangeRequest, error) {
	request := engine.HealChangeRequest{Action: action, Service: healService}
	if action != "pause" {
		return request, nil
	}
	if healUntilResume && forSet {
		return request, &engine.InvalidRequestError{Err: fmt.Errorf("--for and --until-resume cannot be combined")}
	}
	if healUntilResume {
		request.Indefinite = true
		return request, nil
	}
	if healFor <= 0 || healFor > selfheal.MaxPause {
		return request, &engine.InvalidRequestError{Err: fmt.Errorf("--for must be between 1s and %s", selfheal.MaxPause)}
	}
	until := now.Add(healFor).UTC().Truncate(time.Second)
	request.Until = &until
	return request, nil
}

func runHealChange(cmd *cobra.Command, action string) error {
	forSet := cmd.Flags().Lookup("for") != nil && cmd.Flags().Changed("for")
	request, err := healChangeRequest(action, forSet, time.Now())
	if err != nil {
		return err
	}
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return formatConfigLoadError(cfgFile, err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}
	envName, servers, err := healTargets(cfg)
	if err != nil {
		return err
	}
	result, changeErr := engine.ChangeHeal(cmd.Context(), cfg, envName, servers, request)
	if changeErr != nil {
		result.Error = cliEngine().RedactSensitive(changeErr.Error())
	}
	for i := range result.Nodes {
		result.Nodes[i].Error = cliEngine().RedactSensitive(result.Nodes[i].Error)
	}
	if !machineOutputEnabled() {
		printHealChange(cmd.OutOrStdout(), result)
	}
	if emitErr := emitResultDocument(result); emitErr != nil && changeErr == nil {
		changeErr = emitErr
	}
	return changeErr
}

func redactHealNodeErrors(nodes []engine.HealNode) {
	for i := range nodes {
		nodes[i].Error = cliEngine().RedactSensitive(nodes[i].Error)
	}
}

func healTime(value *time.Time) string {
	if value == nil || value.IsZero() {
		return "-"
	}
	return value.Local().Format("2006-01-02 15:04:05")
}

func healDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func printHealStatus(out io.Writer, result engine.HealStatusResult) {
	writer := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "SERVICE\tNODE\tSTATE\tSKIP\tACTIONS\tLAST ACTION\tPAUSED UNTIL")
	rows := 0
	for _, node := range result.Nodes {
		for _, service := range node.Services {
			last := "-"
			if service.LastAction != "" {
				last = service.LastAction
				if service.LastReason != "" {
					last += " (" + service.LastReason + ")"
				}
				last += " " + healTime(service.LastActionAt)
			}
			paused := "-"
			if service.State == selfheal.StatePaused {
				paused = "until resume"
				if service.PausedUntil != nil {
					paused = healTime(service.PausedUntil)
				}
			}
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", service.Service, node.Node, service.State, healDash(service.Skip), service.RecentActions, last, paused)
			rows++
		}
	}
	_ = writer.Flush()
	if rows == 0 {
		fmt.Fprintln(out, "No service has self-healing enabled.")
	}
	for _, node := range result.Nodes {
		switch node.Error {
		case "":
		case "unsupported":
			fmt.Fprintf(out, "%s: takod does not support self-healing; run tako upgrade servers\n", node.Node)
		default:
			fmt.Fprintf(out, "%s: %s\n", node.Node, strings.ReplaceAll(node.Error, "_", " "))
		}
		if node.Breaker != nil && node.Breaker.Open {
			fmt.Fprintf(out, "%s: circuit breaker open (%s) until %s; tako heal reset closes it\n", node.Node, healDash(node.Breaker.Reason), healTime(node.Breaker.Until))
		}
		for _, pause := range node.Pauses {
			target := "environment"
			if pause.Service != "" {
				target = pause.Service
			}
			until := "until resume"
			if pause.Until != nil {
				until = "until " + healTime(pause.Until)
			}
			fmt.Fprintf(out, "%s: paused %s %s\n", node.Node, target, until)
		}
	}
	journal := 0
	for _, node := range result.Nodes {
		journal += len(node.Journal)
	}
	if journal == 0 {
		return
	}
	fmt.Fprintln(out, "\nJournal:")
	writer = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "TIME\tNODE\tSERVICE\tCONTAINER\tDECISION\tREASON\tRESULT")
	for _, node := range result.Nodes {
		for _, entry := range node.Journal {
			reason := entry.Reason
			if entry.Code != 0 {
				reason = fmt.Sprintf("%s %d", healDash(reason), entry.Code)
			}
			at := entry.Time
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", healTime(&at), node.Node, healDash(entry.Service), healDash(entry.Container), entry.Decision, healDash(reason), healDash(entry.Result))
		}
	}
	_ = writer.Flush()
}

func printHealChange(out io.Writer, result engine.HealChangeResult) {
	target := "environment " + result.Environment
	if result.Service != "" {
		target = "service " + result.Service
	}
	for _, node := range result.Nodes {
		switch {
		case node.Error != "":
			fmt.Fprintf(out, "%s: ERROR %s\n", node.Node, node.Error)
		case !node.Changed:
			fmt.Fprintf(out, "%s: %s: nothing to %s\n", node.Node, target, result.Action)
		case result.Action == "pause" && node.PausedUntil != nil:
			fmt.Fprintf(out, "%s: paused %s until %s\n", node.Node, target, healTime(node.PausedUntil))
		case result.Action == "pause":
			fmt.Fprintf(out, "%s: paused %s until tako heal resume\n", node.Node, target)
		case result.Action == "resume":
			fmt.Fprintf(out, "%s: resumed %s\n", node.Node, target)
		default:
			fmt.Fprintf(out, "%s: reset %s\n", node.Node, target)
		}
	}
}
