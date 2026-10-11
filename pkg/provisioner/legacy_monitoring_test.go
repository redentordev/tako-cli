package provisioner

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// legacyAgentHost fakes a node: the agent files exist until rm runs.
type legacyAgentHost struct {
	present  bool
	commands []string
}

func (h *legacyAgentHost) Execute(command string) (string, error) {
	h.commands = append(h.commands, command)
	switch {
	case strings.Contains(command, "echo present"):
		if h.present {
			return "present\n", nil
		}
		return "absent\n", nil
	case strings.HasPrefix(command, "sudo rm -f"):
		h.present = false
	}
	return "", nil
}

func TestRemoveLegacyMonitoringAgentRemovesOnceCapable(t *testing.T) {
	host := &legacyAgentHost{present: true}
	calls, sleeps := 0, 0
	guard := func() error {
		calls++
		if calls < 3 {
			return errors.New("connection refused")
		}
		return nil
	}
	nolog := func(string, ...any) {}
	if err := removeLegacyMonitoringAgent(host, guard, 5, time.Second, func(time.Duration) { sleeps++ }, nolog, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 3 || sleeps != 2 {
		t.Fatalf("guard calls = %d, sleeps = %d", calls, sleeps)
	}
	want := []string{
		"sudo systemctl disable --now tako-monitor.service 2>/dev/null || true",
		"sudo rm -f /etc/systemd/system/tako-monitor.service /usr/local/bin/tako-monitor.sh",
		"sudo systemctl daemon-reload",
	}
	if got := strings.Join(host.commands[1:], "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("commands =\n%s", got)
	}
	for _, command := range host.commands {
		if strings.Contains(command, "/var/lib/tako/metrics") || strings.Contains(command, "bc") && strings.Contains(command, "install") {
			t.Fatalf("unexpected command %q", command)
		}
	}

	// Second run: nothing left to remove, no guard call, no mutation.
	host.commands = nil
	if err := removeLegacyMonitoringAgent(host, guard, 5, time.Second, func(time.Duration) {}, nolog, nil); err != nil {
		t.Fatal(err)
	}
	if len(host.commands) != 1 || calls != 3 {
		t.Fatalf("second run commands = %v, guard calls = %d", host.commands, calls)
	}
}

func TestRemoveLegacyMonitoringAgentGuardNotMetLeavesAgent(t *testing.T) {
	host := &legacyAgentHost{present: true}
	var warnings []string
	err := removeLegacyMonitoringAgent(host, func() error { return errors.New("takod on this node does not support native metrics collection") }, 3, time.Second, func(time.Duration) {}, func(string, ...any) {}, func(message string) { warnings = append(warnings, message) })
	if err != nil {
		t.Fatalf("guard not met must complete, got %v", err)
	}
	if !host.present || len(host.commands) != 1 {
		t.Fatalf("agent was touched: present=%v commands=%v", host.present, host.commands)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "Leaving the legacy monitoring agent in place") {
		t.Fatalf("warnings = %v", warnings)
	}
}
