package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/monitoring"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
)

const monitorNodeTimeout = 10 * time.Second

var (
	monitorService  string
	monitorOnce     bool
	monitorLocal    bool
	monitorWatch    time.Duration
	monitorWatchSet bool
)

var monitorCmd = &cobra.Command{
	Use:   "monitor",
	Short: "Show node-side health monitoring status",
	Long: `Show the health monitoring status that takod reports on every server of the
environment: service checks, host thresholds, container OOM/crash loops,
certificate expiry and their alert state.

Node-side monitoring runs inside takod (monitoring.health-v1) and is
configured by deploy from tako.yaml (services.*.monitoring and
notifications.alerts). This command only reads it.

Exit codes: 0 all ok; 6 any subject alerting, flapping, stale (no fresh
observation for 3 check intervals, e.g. a wedged Docker daemon), unable to
reach Docker or not deployed, or a node could not be read; 4 every node
unreachable; 2 no node supports monitoring.health-v1 (run tako upgrade
servers, or use --local).

--local runs the legacy foreground checker on this machine instead. It probes
public URLs from outside (node "external"), honors per-service intervals,
damping and the schema v1 payload, and delivers alerts itself.

Examples:
  tako monitor                    # Status table for the environment
  tako monitor --service web      # One service only
  tako monitor --watch            # Refresh every 5s (human output only)
  tako monitor --output json      # MonitorResult document
  tako monitor --local            # Foreground outside checker
  tako monitor --local --once     # One outside check pass (cron)`,
	Args: cobra.NoArgs,
	RunE: runMonitor,
}

func init() {
	rootCmd.AddCommand(monitorCmd)
	monitorCmd.Flags().StringVarP(&monitorService, "service", "s", "", "Show (or check) one service only")
	monitorCmd.Flags().BoolVar(&monitorLocal, "local", false, "Run the foreground outside checker on this machine")
	monitorCmd.Flags().BoolVar(&monitorOnce, "once", false, "With --local, run one check pass and exit (alone: deprecated alias of --local --once)")
	monitorCmd.Flags().DurationVar(&monitorWatch, "watch", 5*time.Second, "Refresh the status table at this interval (human output only)")
	monitorCmd.Flags().Lookup("watch").NoOptDefVal = "5s"
	// --once alone is the deprecated alias of --local --once (design D9).
	_ = monitorCmd.Flags().MarkHidden("once")
}

func runMonitor(cmd *cobra.Command, args []string) error {
	monitorWatchSet = cmd.Flags().Changed("watch")
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return formatConfigLoadError(cfgFile, err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}
	envName := getEnvironmentName(cfg)
	services, err := cfg.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services: %w", err)
	}
	if monitorService != "" {
		if _, exists := services[monitorService]; !exists {
			return &engine.InvalidRequestError{Err: fmt.Errorf("service '%s' not found in environment %s", monitorService, envName)}
		}
	}
	if monitorOnce && !monitorLocal {
		fmt.Fprintln(os.Stderr, "warning: tako monitor --once is deprecated; use tako monitor --local --once (it runs one outside check pass that delivers alerts)")
		monitorLocal = true
	}
	if err := validateMonitorModes(machineOutputEnabled(), monitorLocal, monitorWatchSet, monitorWatch); err != nil {
		return err
	}
	if monitorLocal {
		return runLocalMonitor(cmd, cfg, envName, services)
	}
	if monitorWatchSet {
		return watchMonitorStatus(cmd, cfg, envName)
	}
	result, statusErr := collectMonitorResult(cmd.Context(), cfg, envName, monitorService)
	if statusErr != nil {
		result.Error = cliEngine().RedactSensitive(statusErr.Error())
	}
	if !machineOutputEnabled() {
		printMonitorResult(cmd.OutOrStdout(), result)
	}
	if emitErr := emitResultDocument(result); emitErr != nil && statusErr == nil {
		statusErr = emitErr
	}
	return statusErr
}

// validateMonitorModes rejects the human-only modes in machine output and
// invalid flag combinations (exit 2).
func validateMonitorModes(machine, local, watchSet bool, watch time.Duration) error {
	switch {
	case local && machine:
		return &engine.InvalidRequestError{Err: fmt.Errorf("--local is interactive-only; use tako monitor --output json to read node-side monitoring status")}
	case local && watchSet:
		return &engine.InvalidRequestError{Err: fmt.Errorf("--watch cannot be combined with --local")}
	case watchSet && machine:
		return &engine.InvalidRequestError{Err: fmt.Errorf("--watch is interactive-only; run single-shot monitor reads in machine output modes")}
	case watchSet && watch < time.Second:
		return &engine.InvalidRequestError{Err: fmt.Errorf("--watch interval must be at least 1s")}
	}
	return nil
}

func runLocalMonitor(cmd *cobra.Command, cfg *config.Config, envName string, services map[string]config.ServiceConfig) error {
	hasMonitoring := false
	for name, service := range services {
		if monitorService != "" && name != monitorService {
			continue
		}
		if service.Monitoring != nil && service.Monitoring.Enabled {
			hasMonitoring = true
			break
		}
	}
	if !hasMonitoring {
		return &engine.InvalidRequestError{Err: fmt.Errorf("no services have monitoring enabled in environment %s", envName)}
	}
	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()
	monitor := monitoring.NewMonitor(cfg, sshPool, verbose)
	monitor.SetOutput(cmd.OutOrStdout())
	defer monitor.Close()
	if monitorService != "" {
		monitor.SetServiceFilter(monitorService)
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if monitorOnce {
		return monitor.CheckOnce(ctx, envName)
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	return monitor.Start(ctx, envName)
}

func watchMonitorStatus(cmd *cobra.Command, cfg *config.Config, envName string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	ticker := time.NewTicker(monitorWatch)
	defer ticker.Stop()
	for {
		result, err := collectMonitorResult(ctx, cfg, envName, monitorService)
		out := cmd.OutOrStdout()
		fmt.Fprint(out, "\033[H\033[2J")
		fmt.Fprintf(out, "tako monitor  %s/%s  every %s  %s\n\n", cfg.Project.Name, envName, monitorWatch, time.Now().Format("15:04:05"))
		printMonitorResult(out, result)
		var capability *takodclient.CapabilityRequiredError
		if errors.As(err, &capability) {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

type monitorNodeRead struct {
	node   engine.MonitorNode
	status *takod.AlertStatusResponse
	err    error
}

// collectMonitorResult reads GET /v1/alerts on every environment server in
// parallel and classifies the outcome into exit codes 0/2/4/6.
func collectMonitorResult(ctx context.Context, cfg *config.Config, envName, service string) (engine.MonitorResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	result := engine.MonitorResult{
		APIVersion: takoapi.APIVersionCurrent, Kind: engine.KindMonitorResult,
		Project: cfg.Project.Name, Environment: envName, Service: service, Nodes: []engine.MonitorNode{},
	}
	servers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return result, &engine.InvalidRequestError{Err: err}
	}
	sort.Strings(servers)
	pool := ssh.NewPool()
	defer pool.CloseAll()
	factory, err := nodeclient.NewFactory(cfg, pool, takodSocketFromConfig(cfg))
	if err != nil {
		return result, err
	}
	defer factory.CloseIdleConnections()
	reads := make([]monitorNodeRead, len(servers))
	var wg sync.WaitGroup
	for index, server := range servers {
		wg.Add(1)
		go func(index int, server string) {
			defer wg.Done()
			reads[index] = readMonitorNode(ctx, factory, cfg, envName, server)
		}(index, server)
	}
	wg.Wait()
	return classifyMonitorResult(result, reads, monitoredServices(cfg, envName, service))
}

func readMonitorNode(ctx context.Context, factory *nodeclient.Factory, cfg *config.Config, envName, server string) monitorNodeRead {
	read := monitorNodeRead{node: engine.MonitorNode{Node: server, Subjects: []engine.MonitorSubject{}}}
	nodeCtx, cancel := context.WithTimeout(ctx, monitorNodeTimeout)
	defer cancel()
	client, _, err := factory.Client(nodeCtx, server)
	if err != nil {
		read.err = &engine.ConnectivityError{Server: server, Err: fmt.Errorf("failed to connect to %s: %w", server, err)}
		read.node.Error = "unreachable"
		return read
	}
	read.node.Reachable = true
	socket := takodSocketFromConfig(cfg)
	if err := takodclient.RequireCapability(nodeCtx, client, socket, server, takod.CapabilityMonitoringHealthV1, "node-side health monitoring (or use tako monitor --local)"); err != nil {
		read.err = err
		var capability *takodclient.CapabilityRequiredError
		if errors.As(err, &capability) {
			read.node.Error = "unsupported"
		} else {
			read.node.Error = "status_unavailable"
		}
		return read
	}
	read.node.Supported = true
	output, err := takodclient.RequestJSONWithContext(nodeCtx, client, socket, "GET", takodclient.AlertsEndpoint(cfg.Project.Name, envName), nil)
	if err != nil {
		read.err = err
		read.node.Error = "status_unavailable"
		return read
	}
	var status takod.AlertStatusResponse
	if err := json.Unmarshal([]byte(output), &status); err != nil {
		read.err = err
		read.node.Error = "invalid_response"
		return read
	}
	read.status = &status
	return read
}

func monitoredServices(cfg *config.Config, envName, filter string) []string {
	services, err := cfg.GetServices(envName)
	if err != nil {
		return nil
	}
	var names []string
	for name, service := range services {
		if filter != "" && name != filter {
			continue
		}
		if service.Monitoring != nil && service.Monitoring.Enabled && !service.IsJob() && !service.IsRun() {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func monitorTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}

func classifyMonitorResult(result engine.MonitorResult, reads []monitorNodeRead, monitored []string) (engine.MonitorResult, error) {
	reachable, supported := 0, 0
	var firstConnectivity, firstCapability, firstFailure error
	reported := map[string]bool{}
	for _, read := range reads {
		node := read.node
		if node.Reachable {
			reachable++
		} else if firstConnectivity == nil {
			firstConnectivity = read.err
		}
		if node.Supported {
			supported++
		}
		var capability *takodclient.CapabilityRequiredError
		if errors.As(read.err, &capability) && firstCapability == nil {
			firstCapability = read.err
		}
		if read.err != nil && firstFailure == nil {
			firstFailure = read.err
		}
		if read.status != nil && read.status.Sources != nil {
			sources := read.status.Sources
			node.PendingDeliveries, node.ExhaustedDeliveries = sources.PendingDeliveries, sources.ExhaustedDeliveries
			for _, subject := range sources.Subjects {
				if result.Service != "" && subject.Subject != result.Service {
					continue
				}
				if subject.Kind == takod.AlertKindService {
					reported[subject.Subject] = true
				}
				node.Subjects = append(node.Subjects, engine.MonitorSubject{
					Kind: subject.Kind, Subject: subject.Subject, State: subject.State, Severity: subject.Severity,
					Reasons: subject.Reasons, Since: monitorTime(subject.Since), LastCheckedAt: monitorTime(subject.LastObservedAt), Details: subject.Details,
				})
				countMonitorState(&result.Counts, subject.State)
			}
		}
		result.Nodes = append(result.Nodes, node)
	}
	if supported > 0 {
		for _, service := range monitored {
			if !reported[service] {
				result.NotDeployed = append(result.NotDeployed, engine.MonitorSubject{Kind: takod.AlertKindService, Subject: service, State: takod.SubjectStateUnknown, Reasons: []string{"not_deployed"}})
				result.Counts.Unknown++
			}
		}
	}
	switch {
	case len(reads) > 0 && reachable == 0:
		result.Status = engine.MonitorStatusUnreachable
		return result, firstConnectivity
	case supported == 0:
		result.Status = engine.MonitorStatusUnsupported
		if firstCapability != nil {
			return result, firstCapability
		}
		if firstFailure != nil {
			return result, &engine.AttentionError{Err: fmt.Errorf("monitoring status could not be read: %w", firstFailure)}
		}
		return result, &engine.InvalidRequestError{Err: fmt.Errorf("environment %s has no servers", result.Environment)}
	}
	attention := firstFailure != nil || len(result.NotDeployed) > 0
	for _, node := range result.Nodes {
		for _, subject := range node.Subjects {
			if monitorSubjectNeedsAttention(subject) {
				attention = true
			}
		}
	}
	if attention {
		result.Status = engine.MonitorStatusAttention
		return result, &engine.AttentionError{Err: fmt.Errorf("monitoring needs attention in %s/%s", result.Project, result.Environment)}
	}
	result.Status = engine.MonitorStatusOK
	return result, nil
}

// monitorSubjectNeedsAttention reports subjects that make tako monitor exit
// 6: alerting or flapping subjects, stale subjects (takod stopped observing
// them, so they can no longer alert) and checks that could not reach Docker.
func monitorSubjectNeedsAttention(subject engine.MonitorSubject) bool {
	switch subject.State {
	case takod.SubjectStateAlerting, takod.SubjectStateFlapping, takod.SubjectStateStale:
		return true
	}
	return slices.Contains(subject.Reasons, monitorReasonDockerUnavailable)
}

// monitorReasonDockerUnavailable is the hold reason of a container check
// whose docker read failed or timed out.
const monitorReasonDockerUnavailable = "docker_unavailable"

func countMonitorState(counts *engine.MonitorCounts, state string) {
	switch state {
	case takod.SubjectStateOK:
		counts.OK++
	case takod.SubjectStateAlerting, takod.SubjectStateFlapping:
		counts.Alerting++
	case takod.SubjectStatePaused, takod.SubjectStateMaintenance:
		counts.Paused++
	default:
		counts.Unknown++
	}
}

func printMonitorResult(out io.Writer, result engine.MonitorResult) {
	writer := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "KIND\tSUBJECT\tNODE\tSTATE\tSEVERITY\tSINCE\tLAST CHECK\tREASONS")
	rows := 0
	formatTime := func(value *time.Time) string {
		if value == nil {
			return "-"
		}
		return value.Local().Format("2006-01-02 15:04:05")
	}
	dash := func(value string) string {
		if value == "" {
			return "-"
		}
		return value
	}
	pending, exhausted := 0, 0
	for _, node := range result.Nodes {
		pending += node.PendingDeliveries
		exhausted += node.ExhaustedDeliveries
		for _, subject := range node.Subjects {
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", subject.Kind, subject.Subject, node.Node, subject.State, dash(subject.Severity), formatTime(subject.Since), formatTime(subject.LastCheckedAt), dash(strings.Join(subject.Reasons, ",")))
			rows++
		}
	}
	for _, subject := range result.NotDeployed {
		fmt.Fprintf(writer, "%s\t%s\t-\t%s\t-\t-\t-\t%s\n", subject.Kind, subject.Subject, subject.State, strings.Join(subject.Reasons, ","))
		rows++
	}
	_ = writer.Flush()
	if rows == 0 {
		fmt.Fprintln(out, "No monitored subjects reported.")
	}
	for _, node := range result.Nodes {
		switch node.Error {
		case "":
		case "unsupported":
			fmt.Fprintf(out, "%s: takod does not support node-side monitoring; run tako upgrade servers, or use tako monitor --local\n", node.Node)
		default:
			fmt.Fprintf(out, "%s: %s\n", node.Node, strings.ReplaceAll(node.Error, "_", " "))
		}
	}
	fmt.Fprintf(out, "\nDeliveries: %d pending, %d exhausted\n", pending, exhausted)
}
