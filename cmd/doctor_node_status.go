package cmd

// Doctor checks backed by takod's GET /v1/node/status
// (add-server-status-history design D7). They report only; Tako never
// installs updates or reboots a node.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

// doctorSecurityUpdatesWarnAfter matches the node alert window for
// security_updates_pending.
const doctorSecurityUpdatesWarnAfter = 72 * time.Hour

// doctorNodeStatusInfo is one node's status probe result.
type doctorNodeStatusInfo struct {
	// Supported is false when takod lacks node.status-v1.
	Supported bool
	Status    *takod.NodeStatusResponse
	// LegacyAgent is true while tako-monitor.service is still enabled;
	// LegacyAgentErr is set when that could not be determined.
	LegacyAgent    bool
	LegacyAgentErr string
}

func checkNodeStatus(record func(checkResult), cfg *config.Config, clients map[string]*ssh.Client) {
	if len(clients) == 0 {
		return
	}
	clientNames := make([]string, 0, len(clients))
	for name := range clients {
		clientNames = append(clientNames, name)
	}
	sort.Strings(clientNames)
	checkNodeStatusWith(record, clientNames, time.Now(), func(clientName string) (*doctorNodeStatusInfo, error) {
		return detectDoctorNodeStatus(clients[clientName], cfg, clientName)
	})
}

func detectDoctorNodeStatus(client *ssh.Client, cfg *config.Config, serverName string) (*doctorNodeStatusInfo, error) {
	socket := takodSocketFromConfig(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), doctorNodeProbeTimeout)
	defer cancel()
	info := &doctorNodeStatusInfo{}
	if err := takodclient.RequireCapability(ctx, client, socket, serverName, takodclient.CapabilityNodeStatusV1, "node status"); err != nil {
		var required *takodclient.CapabilityRequiredError
		if errors.As(err, &required) {
			return info, nil
		}
		return nil, err
	}
	info.Supported = true
	output, err := takodclient.RequestJSONWithTimeout(client, socket, "GET", takodclient.NodeStatusEndpoint, nil, doctorNodeProbeTimeout)
	if err != nil {
		return nil, err
	}
	var status takod.NodeStatusResponse
	if err := decodeTakodJSON(output, &status); err != nil {
		return nil, err
	}
	info.Status = &status
	if enabled, err := client.Execute("systemctl is-enabled tako-monitor.service 2>/dev/null || true"); err != nil {
		info.LegacyAgentErr = err.Error()
	} else {
		state := strings.TrimSpace(enabled)
		info.LegacyAgent = state == "enabled" || state == "enabled-runtime"
	}
	return info, nil
}

func checkNodeStatusWith(record func(checkResult), clientNames []string, now time.Time, probe func(string) (*doctorNodeStatusInfo, error)) {
	for _, name := range clientNames {
		info, err := probe(name)
		if err != nil || info == nil {
			record(checkResult{"WARN", fmt.Sprintf("%s: Cannot read node status: %v", name, err), "Check 'systemctl status takod' on the node"})
			continue
		}
		if !info.Supported || info.Status == nil {
			record(checkResult{"SKIP", fmt.Sprintf("%s: OS updates, reboot, clock and metrics collector checks need takod with %s", name, takodclient.CapabilityNodeStatusV1), "tako upgrade servers"})
			continue
		}
		status := info.Status
		recordOSSecurityUpdates(record, name, status, now)
		recordRebootRequired(record, name, status)
		recordClockSync(record, name, status)
		recordMetricsCollector(record, name, status)
		switch {
		case info.LegacyAgentErr != "":
			record(checkResult{"SKIP", fmt.Sprintf("%s: Cannot check for the legacy tako-monitor agent: %s", name, info.LegacyAgentErr), ""})
		case info.LegacyAgent:
			record(checkResult{"PASS", fmt.Sprintf("%s: legacy tako-monitor agent is still installed (takod collects metrics natively; it is unused)", name), "Re-run 'tako setup' to remove it"})
		default:
			record(checkResult{"PASS", fmt.Sprintf("%s: legacy tako-monitor agent not installed", name), ""})
		}
	}
}

func conditionUnknown(condition takod.NodeCondition, ok bool) bool {
	return !ok || condition.Status == takod.NodeConditionUnknown
}

func staleSuffix(stale bool) string {
	if stale {
		return " (stale)"
	}
	return ""
}

func recordOSSecurityUpdates(record func(checkResult), name string, status *takod.NodeStatusResponse, now time.Time) {
	condition, ok := status.Condition(takod.NodeConditionOSUpdates)
	osStatus := status.OS
	if conditionUnknown(condition, ok) || osStatus == nil {
		record(checkResult{"SKIP", fmt.Sprintf("%s: OS security updates unknown%s", name, conditionReason(condition)), ""})
		return
	}
	stale := staleSuffix(osStatus.Stale || condition.Stale)
	if osStatus.SecurityUpdates == 0 {
		record(checkResult{"PASS", fmt.Sprintf("%s: no security updates pending (%d update(s) total)%s", name, osStatus.PendingUpdates, stale), ""})
		return
	}
	since := condition.Since
	message := fmt.Sprintf("%s: %d security update(s) pending (%d total)", name, osStatus.SecurityUpdates, osStatus.PendingUpdates)
	if !since.IsZero() {
		message += " since " + since.UTC().Format(time.RFC3339)
	}
	message += stale
	if !since.IsZero() && now.Sub(since) >= doctorSecurityUpdatesWarnAfter {
		record(checkResult{"WARN", message, "Apply OS updates on the node (Tako reports updates but never installs them)"})
		return
	}
	record(checkResult{"PASS", message + "; warns after 72h", ""})
}

func recordRebootRequired(record func(checkResult), name string, status *takod.NodeStatusResponse) {
	condition, ok := status.Condition(takod.NodeConditionRebootRequired)
	if conditionUnknown(condition, ok) {
		record(checkResult{"SKIP", fmt.Sprintf("%s: Reboot required unknown%s", name, conditionReason(condition)), ""})
		return
	}
	if condition.Status == takod.NodeConditionOK {
		record(checkResult{"PASS", fmt.Sprintf("%s: no reboot required", name), ""})
		return
	}
	message := fmt.Sprintf("%s: Reboot required", name)
	since := condition.Since
	if status.OS != nil && status.OS.RebootRequiredSince != nil {
		since = *status.OS.RebootRequiredSince
	}
	if !since.IsZero() {
		message += " since " + since.UTC().Format(time.RFC3339)
	}
	if status.OS != nil && len(status.OS.RebootPackages) > 0 {
		message += " (packages: " + strings.Join(status.OS.RebootPackages, ", ") + ")"
	}
	record(checkResult{"WARN", message + staleSuffix(condition.Stale), "Reboot the node in a maintenance window (Tako never reboots nodes)"})
}

func recordClockSync(record func(checkResult), name string, status *takod.NodeStatusResponse) {
	condition, ok := status.Condition(takod.NodeConditionClockSync)
	switch {
	case conditionUnknown(condition, ok):
		record(checkResult{"SKIP", fmt.Sprintf("%s: Clock synchronization unknown%s", name, conditionReason(condition)), ""})
	case condition.Status == takod.NodeConditionOK:
		record(checkResult{"PASS", fmt.Sprintf("%s: clock synchronized", name), ""})
	case condition.Status == takod.NodeConditionFail:
		record(checkResult{"FAIL", fmt.Sprintf("%s: clock not synchronized since %s", name, condition.Since.UTC().Format(time.RFC3339)), "Enable NTP on the node (timedatectl set-ntp true) and check systemd-timesyncd or chrony"})
	default:
		record(checkResult{"WARN", fmt.Sprintf("%s: clock synchronization %s%s", name, condition.Status, conditionReason(condition)), "Check NTP on the node (timedatectl status)"})
	}
}

func recordMetricsCollector(record func(checkResult), name string, status *takod.NodeStatusResponse) {
	condition, ok := status.Condition(takod.NodeConditionMetricsCollector)
	switch {
	case !ok:
		record(checkResult{"WARN", fmt.Sprintf("%s: native metrics collection is disabled (--metrics-interval 0)", name), "Remove --metrics-interval 0 from the takod unit to enable history"})
	case condition.Status == takod.NodeConditionOK:
		record(checkResult{"PASS", fmt.Sprintf("%s: metrics collector sampling", name), ""})
	case condition.Status == takod.NodeConditionUnknown:
		record(checkResult{"SKIP", fmt.Sprintf("%s: metrics collector%s", name, conditionReason(condition)), ""})
	case condition.Status == takod.NodeConditionFail:
		record(checkResult{"FAIL", fmt.Sprintf("%s: metrics collector stalled%s", name, conditionReason(condition)), "Check 'journalctl -u takod' on the node"})
	default:
		record(checkResult{"WARN", fmt.Sprintf("%s: metrics collector degraded%s", name, conditionReason(condition)), "Check 'journalctl -u takod' on the node"})
	}
}

func conditionReason(condition takod.NodeCondition) string {
	parts := []string{}
	if condition.Reason != "" {
		parts = append(parts, condition.Reason)
	}
	if condition.Detail != "" {
		parts = append(parts, condition.Detail)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ": ") + ")"
}
