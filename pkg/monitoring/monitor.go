package monitoring

// Package monitoring implements `tako monitor --local`: a foreground checker
// that runs on the CLI host and probes public URLs from outside, so it is a
// genuine outside vantage point next to takod's node-side monitoring. It
// uses per-service intervals and timeouts, opening/recovery damping, the
// alert payload schema v1 and the SSRF-safe delivery client.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/alerttarget"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/notification"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

var stdout io.Writer = os.Stdout

// ExternalNode is the node name carried in payloads sent by the CLI-host
// checker.
const ExternalNode = "external"

// Monitor handles foreground service monitoring from the CLI host.
type Monitor struct {
	config         *config.Config
	sshPool        *ssh.Pool
	verbose        bool
	envName        string
	services       map[string]config.ServiceConfig
	filter         string
	runtimeFactory *nodeclient.Factory
	out            io.Writer
	now            func() time.Time
	httpClient     *http.Client
	alertClient    *http.Client
	deliver        func(ctx context.Context, provider, endpoint string, payload map[string]any) (bool, string)
	state          map[string]*serviceState
}

type serviceState struct {
	failing, passing int
	active           bool
	episodeID        string
	openedAt         time.Time
	reasons          []string
	nextCheck        time.Time
}

// checkFailure is a closed-vocabulary failure: a reason code plus
// errorClass/statusCode style details. Raw error text never reaches it.
type checkFailure struct {
	reason  string
	details map[string]string
}

func (f *checkFailure) Error() string {
	parts := []string{f.reason}
	keys := make([]string, 0, len(f.details))
	for key := range f.details {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		parts = append(parts, key+"="+f.details[key])
	}
	return strings.Join(parts, " ")
}

func (m *Monitor) SetServiceFilter(serviceName string) {
	m.filter = serviceName
}

// SetOutput redirects human output (default stdout).
func (m *Monitor) SetOutput(out io.Writer) { m.out = out }

// NewMonitor creates a new service monitor.
func NewMonitor(cfg *config.Config, sshPool *ssh.Pool, verbose bool) *Monitor {
	monitor := &Monitor{
		config:      cfg,
		sshPool:     sshPool,
		verbose:     verbose,
		now:         time.Now,
		alertClient: takod.NewAlertHTTPClient(),
		state:       map[string]*serviceState{},
	}
	monitor.httpClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	monitor.deliver = func(ctx context.Context, provider, endpoint string, payload map[string]any) (bool, string) {
		return takod.DeliverAlert(ctx, monitor.alertClient, provider, endpoint, payload)
	}
	return monitor
}

func (m *Monitor) printf(format string, args ...any) {
	out := m.out
	if out == nil {
		out = stdout
	}
	fmt.Fprintf(out, format, args...)
}

func (m *Monitor) monitored() []string {
	var names []string
	for name, service := range m.services {
		if m.filter != "" && name != m.filter {
			continue
		}
		if service.Monitoring == nil || !service.Monitoring.Enabled || service.IsJob() || service.IsRun() {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (m *Monitor) load(envName string) error {
	services, err := m.config.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services: %w", err)
	}
	m.envName = envName
	m.services = services
	return nil
}

// Start monitors every enabled service at its own interval until ctx ends.
func (m *Monitor) Start(ctx context.Context, envName string) error {
	if err := m.load(envName); err != nil {
		return err
	}
	names := m.monitored()
	if len(names) == 0 {
		return fmt.Errorf("no services have monitoring enabled")
	}
	m.printf("Starting local monitoring for environment %s from this host (node %q)\n\n", envName, ExternalNode)
	for _, name := range names {
		service := m.services[name]
		interval, timeout := config.MonitorTiming(service.Monitoring)
		alertAfter, recoverAfter := config.MonitorDamping(m.alertsConfig(), service.Monitoring)
		m.printf("  monitoring %s (%s check every %s, timeout %s, alert after %d, recover after %d)\n", name, checkType(&service), interval, timeout, alertAfter, recoverAfter)
	}
	m.printf("\nPress Ctrl+C to stop monitoring\n")
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		m.runDue(ctx, true)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// CheckOnce runs one check of every monitored service and delivers an alert
// for each failure (the historical `tako monitor --once` behavior, now with
// the schema v1 payload).
func (m *Monitor) CheckOnce(ctx context.Context, envName string) error {
	if err := m.load(envName); err != nil {
		return err
	}
	m.printf("Running single monitoring check for environment %s...\n\n", envName)
	m.runDue(ctx, false)
	m.printf("\nCheck complete\n")
	return nil
}

func (m *Monitor) alertsConfig() *config.SchedulerAlertsConfig {
	if m.config.Notifications == nil {
		return nil
	}
	return m.config.Notifications.Alerts
}

func checkType(service *config.ServiceConfig) string {
	if service.Monitoring != nil && service.Monitoring.CheckType != "" {
		return service.Monitoring.CheckType
	}
	return "container"
}

// runDue checks every due service. damped applies alertAfter/recoverAfter
// across passes; a single pass (damped=false) alerts on each failure.
func (m *Monitor) runDue(ctx context.Context, damped bool) {
	now := m.now()
	for _, name := range m.monitored() {
		service := m.services[name]
		state := m.state[name]
		if state == nil {
			state = &serviceState{}
			m.state[name] = state
		}
		if damped && now.Before(state.nextCheck) {
			continue
		}
		interval, timeout := config.MonitorTiming(service.Monitoring)
		state.nextCheck = now.Add(interval)
		checkCtx, cancel := context.WithTimeout(ctx, timeout)
		failure := m.checkService(checkCtx, name, &service)
		cancel()
		if ctx.Err() != nil {
			return
		}
		stamp := now.Format("15:04:05")
		if failure != nil {
			m.printf("[%s] %s: %s\n", stamp, name, failure.Error())
		} else if m.verbose {
			m.printf("[%s] %s: healthy\n", stamp, name)
		}
		if !damped {
			if failure != nil {
				m.alert(ctx, name, &service, takod.AlertTransitionOpening, takod.NewAlertID(), time.Time{}, failure)
			}
			continue
		}
		alertAfter, recoverAfter := config.MonitorDamping(m.alertsConfig(), service.Monitoring)
		if failure != nil {
			state.failing++
			state.passing = 0
			state.reasons = []string{failure.reason}
			if !state.active && state.failing >= alertAfter {
				state.active, state.episodeID, state.openedAt = true, takod.NewAlertID(), now
				m.alert(ctx, name, &service, takod.AlertTransitionOpening, state.episodeID, state.openedAt, failure)
			}
			continue
		}
		state.passing++
		state.failing = 0
		if state.active && state.passing >= recoverAfter {
			m.alert(ctx, name, &service, takod.AlertTransitionRecovery, state.episodeID, state.openedAt, &checkFailure{reason: firstOr(state.reasons, "unreachable")})
			state.active = false
		}
	}
}

func firstOr(values []string, fallback string) string {
	if len(values) > 0 {
		return values[0]
	}
	return fallback
}

// checkService checks a single service based on its monitoring config.
func (m *Monitor) checkService(ctx context.Context, serviceName string, service *config.ServiceConfig) *checkFailure {
	if checkType(service) == "http" {
		return m.checkPublicService(ctx, service)
	}
	return m.checkInternalService(ctx, serviceName, service)
}

// checkPublicService checks a public service via its public URL.
func (m *Monitor) checkPublicService(ctx context.Context, service *config.ServiceConfig) *checkFailure {
	if service.Proxy == nil || service.Proxy.GetPrimaryDomain() == "" {
		return &checkFailure{reason: "unreachable", details: map[string]string{"errorClass": "no_domain"}}
	}
	path := service.HealthCheck.Path
	if path == "" {
		path = "/"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+service.Proxy.GetPrimaryDomain()+path, nil)
	if err != nil {
		return &checkFailure{reason: "unreachable", details: map[string]string{"errorClass": "connect"}}
	}
	response, err := m.httpClient.Do(request)
	if err != nil {
		class := "connect"
		switch {
		case ctx.Err() != nil:
			class = "timeout"
		case strings.Contains(err.Error(), "tls:") || strings.Contains(err.Error(), "x509:"):
			return &checkFailure{reason: "tls_invalid", details: map[string]string{"errorClass": "tls"}}
		}
		return &checkFailure{reason: "unreachable", details: map[string]string{"errorClass": class}}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	if response.StatusCode == http.StatusUnauthorized && service.Proxy.BasicAuth != nil {
		return nil
	}
	return &checkFailure{reason: "unhealthy_status", details: map[string]string{"errorClass": "status", "statusCode": strconv.Itoa(response.StatusCode)}}
}

// checkInternalService compares running replicas reported by takod with the
// desired count.
func (m *Monitor) checkInternalService(ctx context.Context, serviceName string, service *config.ServiceConfig) *checkFailure {
	expected := expectedReplicas(service, m.config, m.envName)
	actual := 0
	serverNames, err := m.config.GetEnvironmentServers(m.envName)
	if err != nil {
		return &checkFailure{reason: "unreachable", details: map[string]string{"errorClass": "config"}}
	}
	for _, serverName := range serverNames {
		client, err := m.runtimeClient(ctx, serverName)
		if err != nil {
			return &checkFailure{reason: "unreachable", details: map[string]string{"errorClass": "connect"}}
		}
		output, err := takodclient.RequestJSON(client, m.takodSocket(), "GET", takodclient.ActualStateEndpoint(m.config.Project.Name, m.envName), nil)
		if err != nil {
			return &checkFailure{reason: "unreachable", details: map[string]string{"errorClass": "connect"}}
		}
		var response takod.ActualStateResponse
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			return &checkFailure{reason: "unreachable", details: map[string]string{"errorClass": "status"}}
		}
		if state := response.Services[serviceName]; state != nil {
			actual += state.Replicas
		}
	}
	if actual < expected {
		return &checkFailure{reason: "replicas_missing", details: map[string]string{"expected": strconv.Itoa(expected), "running": strconv.Itoa(actual)}}
	}
	return nil
}

func (m *Monitor) runtimeClient(ctx context.Context, serverName string) (*takodclient.AgentClient, error) {
	if m.runtimeFactory == nil {
		factory, err := nodeclient.NewFactory(m.config, m.sshPool, m.takodSocket())
		if err != nil {
			return nil, err
		}
		m.runtimeFactory = factory
	}
	client, _, err := m.runtimeFactory.Client(ctx, serverName)
	return client, err
}

func (m *Monitor) Close() {
	if m != nil && m.runtimeFactory != nil {
		m.runtimeFactory.CloseIdleConnections()
	}
}

func expectedReplicas(service *config.ServiceConfig, cfg *config.Config, envName string) int {
	replicas := service.Replicas
	if replicas <= 0 {
		replicas = 1
	}
	if service.Placement != nil && service.Placement.Strategy == "global" {
		if servers, err := cfg.GetEnvironmentServers(envName); err == nil && len(servers) > 0 {
			return len(servers)
		}
	}
	return replicas
}

func (m *Monitor) takodSocket() string {
	if m.config.Runtime != nil && m.config.Runtime.Agent != nil && m.config.Runtime.Agent.Socket != "" {
		return m.config.Runtime.Agent.Socket
	}
	return takodclient.DefaultSocket
}

type alertTarget struct{ provider, endpoint string }

// targets returns the validated delivery targets of a service: its
// monitoring.webhook when deliverable, otherwise the notification targets.
// Targets failing the alert target rules are never used.
func (m *Monitor) targets(service *config.ServiceConfig) []alertTarget {
	if webhook := service.Monitoring.DeliverableWebhook(); webhook != "" {
		return []alertTarget{{provider: "service-webhook", endpoint: webhook}}
	}
	var targets []alertTarget
	if n := m.config.Notifications; n != nil {
		for _, candidate := range []alertTarget{{"slack", n.Slack}, {"discord", n.Discord}, {"webhook", n.Webhook}} {
			if strings.TrimSpace(candidate.endpoint) == "" {
				continue
			}
			if normalized, err := alerttarget.NormalizeAndValidate(candidate.provider, candidate.endpoint); err == nil {
				targets = append(targets, alertTarget{provider: candidate.provider, endpoint: normalized})
			}
		}
	}
	return targets
}

func (m *Monitor) alert(ctx context.Context, serviceName string, service *config.ServiceConfig, transition, episodeID string, openedAt time.Time, failure *checkFailure) {
	targets := m.targets(service)
	if len(targets) == 0 {
		if m.verbose {
			m.printf("  no deliverable alert target for %s\n", serviceName)
		}
		return
	}
	event := string(notification.EventServiceDown)
	if transition == takod.AlertTransitionRecovery {
		event = string(notification.EventServiceUp)
	}
	reasons := []string{failure.reason}
	message := fmt.Sprintf("Tako alert %s for %s/%s service %s on %s: %s", transition, m.config.Project.Name, m.envName, serviceName, ExternalNode, failure.reason)
	payload := takod.AlertPayload{
		EventID: takod.NewAlertID(), EpisodeID: episodeID, Transition: transition, Event: event,
		Project: m.config.Project.Name, Environment: m.envName, Node: ExternalNode, Kind: "service", Subject: serviceName,
		Severity: "critical", Message: message, Reasons: reasons, Details: failure.details, OpenedAt: openedAt, Timestamp: m.now().UTC(),
	}.Map()
	for _, target := range targets {
		delivered, errorToken := m.deliver(ctx, target.provider, target.endpoint, payload)
		if delivered {
			m.printf("  %s alert sent (%s)\n", transition, target.provider)
		} else if m.verbose {
			m.printf("  %s alert not delivered (%s): %s\n", transition, target.provider, errorToken)
		}
	}
}
