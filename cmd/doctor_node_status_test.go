package cmd

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/takod"
)

func TestCheckNodeStatusWith(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	rebootSince := time.Date(2026, 10, 9, 3, 12, 0, 0, time.UTC)
	condition := func(kind, status string, since time.Time) takod.NodeCondition {
		return takod.NodeCondition{Type: kind, Status: status, Since: since, CheckedAt: now}
	}
	healthy := &takod.NodeStatusResponse{
		OS: &takod.NodeOSStatus{PackageManager: "apt", PendingUpdates: 2, CheckedAt: now},
		Conditions: []takod.NodeCondition{
			condition(takod.NodeConditionOSUpdates, takod.NodeConditionOK, now),
			condition(takod.NodeConditionRebootRequired, takod.NodeConditionOK, now),
			condition(takod.NodeConditionClockSync, takod.NodeConditionOK, now),
			condition(takod.NodeConditionMetricsCollector, takod.NodeConditionOK, now),
		},
	}
	attention := &takod.NodeStatusResponse{
		OS: &takod.NodeOSStatus{PackageManager: "apt", PendingUpdates: 12, SecurityUpdates: 3, CheckedAt: now, RebootRequired: true, RebootRequiredSince: &rebootSince, RebootPackages: []string{"linux-image-6.8.0-45-generic"}},
		Conditions: []takod.NodeCondition{
			condition(takod.NodeConditionOSUpdates, takod.NodeConditionWarn, now.Add(-73*time.Hour)),
			condition(takod.NodeConditionRebootRequired, takod.NodeConditionWarn, rebootSince),
			{Type: takod.NodeConditionClockSync, Status: takod.NodeConditionFail, Reason: "ntp-unsynchronized", Since: now.Add(-time.Hour)},
			{Type: takod.NodeConditionMetricsCollector, Status: takod.NodeConditionFail, Reason: "no-recent-sample"},
		},
	}
	fresh := &takod.NodeStatusResponse{
		OS: &takod.NodeOSStatus{PackageManager: "apt", PendingUpdates: 4, SecurityUpdates: 1, CheckedAt: now},
		Conditions: []takod.NodeCondition{
			condition(takod.NodeConditionOSUpdates, takod.NodeConditionWarn, now.Add(-2*time.Hour)),
			{Type: takod.NodeConditionRebootRequired, Status: takod.NodeConditionUnknown, Reason: "unsupported"},
			{Type: takod.NodeConditionClockSync, Status: takod.NodeConditionUnknown},
		},
	}
	probes := map[string]*doctorNodeStatusInfo{
		"healthy":   {Supported: true, Status: healthy},
		"attention": {Supported: true, Status: attention, LegacyAgent: true},
		"fresh":     {Supported: true, Status: fresh, LegacyAgentErr: "ssh: broken pipe"},
		"old":       {Supported: false},
	}
	var got []string
	record := func(result checkResult) {
		line := result.status + " " + result.message
		if result.fix != "" {
			line += " | " + result.fix
		}
		got = append(got, line)
	}
	checkNodeStatusWith(record, []string{"healthy", "attention", "fresh", "old", "down"}, now, func(name string) (*doctorNodeStatusInfo, error) {
		if info, ok := probes[name]; ok {
			return info, nil
		}
		return nil, fmt.Errorf("dial tcp: refused")
	})
	want := []string{
		"PASS healthy: no security updates pending (2 update(s) total)",
		"PASS healthy: no reboot required",
		"PASS healthy: clock synchronized",
		"PASS healthy: metrics collector sampling",
		"PASS healthy: legacy tako-monitor agent not installed",
		"WARN attention: 3 security update(s) pending (12 total) since 2026-10-09T11:00:00Z | Apply OS updates on the node (Tako reports updates but never installs them)",
		"WARN attention: Reboot required since 2026-10-09T03:12:00Z (packages: linux-image-6.8.0-45-generic) | Reboot the node in a maintenance window (Tako never reboots nodes)",
		"FAIL attention: clock not synchronized since 2026-10-12T11:00:00Z | Enable NTP on the node (timedatectl set-ntp true) and check systemd-timesyncd or chrony",
		"FAIL attention: metrics collector stalled (no-recent-sample) | Check 'journalctl -u takod' on the node",
		"PASS attention: legacy tako-monitor agent is still installed (takod collects metrics natively; it is unused) | Re-run 'tako setup' to remove it",
		"PASS fresh: 1 security update(s) pending (4 total) since 2026-10-12T10:00:00Z; warns after 72h",
		"SKIP fresh: Reboot required unknown (unsupported)",
		"SKIP fresh: Clock synchronization unknown",
		"WARN fresh: native metrics collection is disabled (--metrics-interval 0) | Remove --metrics-interval 0 from the takod unit to enable history",
		"SKIP fresh: Cannot check for the legacy tako-monitor agent: ssh: broken pipe",
		"SKIP old: OS updates, reboot, clock and metrics collector checks need takod with node.status-v1 | tako upgrade servers",
		"WARN down: Cannot read node status: dial tcp: refused | Check 'systemctl status takod' on the node",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("checks =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
