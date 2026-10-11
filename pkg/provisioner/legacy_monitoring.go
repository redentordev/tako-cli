package provisioner

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/takodclient"
)

// Legacy bash monitoring agent paths (tako-monitor.service). takod collects
// host metrics natively since node.metrics-history-v1, so setup removes the
// agent instead of installing it.
const (
	legacyMonitorUnitPath   = "/etc/systemd/system/tako-monitor.service"
	legacyMonitorScriptPath = "/usr/local/bin/tako-monitor.sh"
)

// legacyMonitorGuardAttempts and legacyMonitorGuardDelay give a just
// restarted takod time to answer /v1/status.
const (
	legacyMonitorGuardAttempts = 5
	legacyMonitorGuardDelay    = 2 * time.Second
)

// LegacyMonitoringOptions configures RemoveLegacyMonitoringAgent.
type LegacyMonitoringOptions struct {
	// Socket is the takod Unix socket on the node.
	Socket string
	// Warn reports why the agent was left in place.
	Warn func(string)
}

// RemoveLegacyMonitoringAgent disables and deletes the legacy
// tako-monitor agent once takod advertises native metrics collection
// (node.metrics-history-v1). Without that capability the agent stays in
// place and the step completes with a warning. Leftover files under
// /var/lib/tako/metrics are kept. Running it again is a no-op.
func (p *Provisioner) RemoveLegacyMonitoringAgent(opts LegacyMonitoringOptions) error {
	p.logf("→ Removing legacy monitoring agent...\n")
	guard := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), takodclient.JSONRequestTimeout)
		defer cancel()
		return takodclient.RequireCapability(ctx, p.client, opts.Socket, "this node", takodclient.CapabilityNodeMetricsHistoryV1, "native metrics collection")
	}
	return removeLegacyMonitoringAgent(p.client, guard, legacyMonitorGuardAttempts, legacyMonitorGuardDelay, time.Sleep, p.logf, opts.Warn)
}

func removeLegacyMonitoringAgent(exec commandExecutor, guard func() error, attempts int, delay time.Duration, sleep func(time.Duration), logf func(string, ...any), warn func(string)) error {
	output, err := exec.Execute(fmt.Sprintf("if [ -e %s ] || [ -e %s ]; then echo present; else echo absent; fi", legacyMonitorUnitPath, legacyMonitorScriptPath))
	if err != nil {
		return fmt.Errorf("failed to check for the legacy monitoring agent: %w", err)
	}
	if strings.TrimSpace(output) != "present" {
		logf("  ✓ No legacy monitoring agent installed\n")
		return nil
	}
	var guardErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			sleep(delay)
		}
		if guardErr = guard(); guardErr == nil {
			break
		}
	}
	if guardErr != nil {
		message := fmt.Sprintf("⚠ Leaving the legacy monitoring agent in place: takod does not confirm native metrics collection (%v)", guardErr)
		logf("  %s\n", message)
		if warn != nil {
			warn(message)
		}
		return nil
	}
	for _, command := range []string{
		"sudo systemctl disable --now tako-monitor.service 2>/dev/null || true",
		fmt.Sprintf("sudo rm -f %s %s", legacyMonitorUnitPath, legacyMonitorScriptPath),
		"sudo systemctl daemon-reload",
	} {
		if _, err := exec.Execute(command); err != nil {
			return fmt.Errorf("failed to remove the legacy monitoring agent: %w", err)
		}
	}
	logf("  ✓ Removed legacy monitoring agent (takod collects host metrics natively)\n")
	return nil
}
