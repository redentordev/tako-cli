package takod

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const ubuntuAptSimulation = `NOTE: This is only a simulation!
      apt-get needs root privileges for real execution.
Reading package lists...
Building dependency tree...
Calculating upgrade...
The following packages will be upgraded:
  libssl3 openssl curl libcurl4 tzdata
12 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.
Inst libssl3 [3.0.2-0ubuntu1.10] (3.0.2-0ubuntu1.12 Ubuntu:22.04/jammy-updates, Ubuntu:22.04/jammy-security [amd64])
Inst openssl [3.0.2-0ubuntu1.10] (3.0.2-0ubuntu1.12 Ubuntu:22.04/jammy-updates, Ubuntu:22.04/jammy-security [amd64])
Inst curl [7.81.0-1ubuntu1.13] (7.81.0-1ubuntu1.14 Ubuntu:22.04/jammy-security [amd64])
Inst tzdata [2023c-0ubuntu0.22.04.2] (2024a-0ubuntu0.22.04 Ubuntu:22.04/jammy-updates [all])
Inst python3-apt [2.4.0] (2.4.0ubuntu2 Ubuntu:22.04/jammy-updates [amd64])
Inst snapd [2.58] (2.61 Ubuntu:22.04/jammy-updates [amd64])
Inst git [1:2.34.1-1ubuntu1.9] (1:2.34.1-1ubuntu1.10 Ubuntu:22.04/jammy-updates [amd64])
Inst vim [2:8.2.3995-1ubuntu2.12] (2:8.2.3995-1ubuntu2.13 Ubuntu:22.04/jammy-updates [amd64])
Inst vim-common [2:8.2.3995-1ubuntu2.12] (2:8.2.3995-1ubuntu2.13 Ubuntu:22.04/jammy-updates [all])
Inst htop [3.0.5-7build2] (3.0.5-7build3 Ubuntu:22.04/jammy-updates [amd64])
Inst less [590-1ubuntu0.22.04.1] (590-1ubuntu0.22.04.2 Ubuntu:22.04/jammy-updates [amd64])
Inst rsync [3.2.7-0ubuntu0.22.04.2] (3.2.7-0ubuntu0.22.04.3 Ubuntu:22.04/jammy-updates [amd64])
Conf libssl3 (3.0.2-0ubuntu1.12 Ubuntu:22.04/jammy-updates, Ubuntu:22.04/jammy-security [amd64])
`

const debianAptSimulation = `Reading package lists...
Building dependency tree...
Reading state information...
Calculating upgrade...
The following packages will be upgraded:
  openssl libssl3
2 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.
Inst libssl3 [3.0.11-1~deb12u1] (3.0.11-1~deb12u2 Debian-Security:12/stable-security [amd64])
Inst openssl [3.0.11-1~deb12u1] (3.0.11-1~deb12u2 Debian:12.5/stable [amd64])
`

const aptNothingToDo = `Reading package lists...
Calculating upgrade...
0 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.
`

const dnfCheckUpdate = `
Last metadata expiration check: 1:02:03 ago on Mon 12 Oct 2026 08:00:00 AM UTC.

openssl-libs.x86_64                1:3.0.7-27.el9          baseos
kernel.x86_64                      5.14.0-427.el9          baseos
vim-minimal.x86_64                 2:8.2.2637-20.el9_1     baseos
Obsoleting Packages
grub2-tools.x86_64                 1:2.06-77.el9           baseos
`

const dnfSecurity = `RLSA-2024:1234 Important/Sec. openssl-libs-1:3.0.7-27.el9.x86_64
RLSA-2024:2345 Important/Sec. kernel-5.14.0-427.el9.x86_64
RLSA-2024:2345 Important/Sec. kernel-5.14.0-427.el9.x86_64
RLBA-2024:3456 bugfix        vim-minimal-2:8.2.2637-20.el9_1.x86_64
`

type fakeFileInfo struct {
	name    string
	modTime time.Time
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() fs.FileMode  { return 0o644 }
func (f fakeFileInfo) ModTime() time.Time { return f.modTime }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return nil }

type fakeOSHost struct {
	mu       sync.Mutex
	tools    map[string]bool
	outputs  map[string]commandResult
	files    map[string]time.Time
	contents map[string]string
	commands [][]string
}

func (h *fakeOSHost) probe(now time.Time) *osProbe {
	return &osProbe{
		run: func(ctx context.Context, env []string, name string, args ...string) commandResult {
			h.mu.Lock()
			defer h.mu.Unlock()
			argv := append([]string{name}, args...)
			h.commands = append(h.commands, argv)
			joined := strings.Join(argv, " ")
			for key, result := range h.outputs {
				if strings.Contains(joined, key) {
					return result
				}
			}
			return commandResult{Err: errors.New("unexpected command " + joined)}
		},
		lookPath: func(name string) (string, error) {
			if h.tools[name] {
				return "/usr/bin/" + name, nil
			}
			return "", errors.New("not found")
		},
		stat: func(path string) (os.FileInfo, error) {
			if modTime, ok := h.files[path]; ok {
				return fakeFileInfo{name: path, modTime: modTime}, nil
			}
			return nil, os.ErrNotExist
		},
		readFile: func(path string) ([]byte, error) {
			if content, ok := h.contents[path]; ok {
				return []byte(content), nil
			}
			return nil, os.ErrNotExist
		},
		now:     func() time.Time { return now },
		timeout: time.Second,
	}
}

func aptHost(simulation string) *fakeOSHost {
	return &fakeOSHost{
		tools:   map[string]bool{"apt-get": true, "nice": true, "ionice": true, "timedatectl": true},
		outputs: map[string]commandResult{"apt-get -s": {Stdout: simulation}, "timedatectl": {Stdout: "yes\n"}},
		files:   map[string]time.Time{},
	}
}

var statusNow = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

func TestOSProbeUbuntuSecurityUpdates(t *testing.T) {
	host := aptHost(ubuntuAptSimulation)
	host.files[aptUpdateStampPath] = statusNow.Add(-5 * time.Hour)
	status, err := host.probe(statusNow).Updates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.PackageManager != "apt" || status.PendingUpdates != 12 || status.SecurityUpdates != 3 || status.ListsAgeSeconds != 5*3600 {
		t.Fatalf("status = %+v", status)
	}
	if strings.Join(status.SecurityPackages, ",") != "curl,libssl3,openssl" {
		t.Fatalf("security packages = %v", status.SecurityPackages)
	}
	command := strings.Join(host.commands[0], " ")
	if command != "nice -n 10 ionice -c3 apt-get -s -o Debug::NoLocking=1 dist-upgrade" {
		t.Fatalf("command = %s", command)
	}
}

func TestOSProbeDebianAndNothingPending(t *testing.T) {
	host := aptHost(debianAptSimulation)
	host.files[aptListsPath] = statusNow.Add(-time.Hour)
	status, err := host.probe(statusNow).Updates(context.Background())
	if err != nil || status.PendingUpdates != 2 || status.SecurityUpdates != 1 || status.ListsAgeSeconds != 3600 {
		t.Fatalf("status = %+v err = %v", status, err)
	}
	empty, err := aptHost(aptNothingToDo).probe(statusNow).Updates(context.Background())
	if err != nil || empty.PendingUpdates != 0 || empty.SecurityUpdates != 0 {
		t.Fatalf("empty = %+v err = %v", empty, err)
	}
	if _, err := aptHost("E: Could not open lock file\n").probe(statusNow).Updates(context.Background()); err == nil {
		t.Fatal("unrecognized output must fail the probe")
	}
}

func TestOSProbeFedoraRockyCacheOnly(t *testing.T) {
	host := &fakeOSHost{
		tools: map[string]bool{"dnf": true, "nice": true, "needs-restarting": true},
		outputs: map[string]commandResult{
			"check-update":     {Stdout: dnfCheckUpdate, ExitCode: 100},
			"updateinfo":       {Stdout: dnfSecurity},
			"needs-restarting": {Stdout: "Core libraries or services have been updated since boot-up:\n  * kernel\n  * systemd\n", ExitCode: 1},
		},
	}
	probe := host.probe(statusNow)
	status, err := probe.Updates(context.Background())
	if err != nil || status.PackageManager != "dnf" || status.PendingUpdates != 3 || status.SecurityUpdates != 2 {
		t.Fatalf("status = %+v err = %v", status, err)
	}
	if command := strings.Join(host.commands[0], " "); command != "nice -n 10 dnf --cacheonly -q check-update" {
		t.Fatalf("command = %s", command)
	}
	reboot, err := probe.Reboot(context.Background())
	if err != nil || !reboot.Required || strings.Join(reboot.Packages, ",") != "kernel,systemd" {
		t.Fatalf("reboot = %+v err = %v", reboot, err)
	}
}

func TestOSProbeMissingToolTimeoutAndUnsupported(t *testing.T) {
	unsupported := &fakeOSHost{tools: map[string]bool{}}
	if _, err := unsupported.probe(statusNow).Updates(context.Background()); !errors.Is(err, errUnsupportedPackageManager) {
		t.Fatalf("err = %v", err)
	}
	if _, err := unsupported.probe(statusNow).Reboot(context.Background()); !errors.Is(err, errUnsupportedPackageManager) {
		t.Fatalf("reboot err = %v", err)
	}
	if _, available, _ := unsupported.probe(statusNow).ClockSynchronized(context.Background()); available {
		t.Fatal("clock must be unavailable without timedatectl")
	}
	noNeedsRestarting := &fakeOSHost{tools: map[string]bool{"yum": true}}
	if _, err := noNeedsRestarting.probe(statusNow).Reboot(context.Background()); err == nil || errors.Is(err, errUnsupportedPackageManager) {
		t.Fatalf("missing needs-restarting err = %v", err)
	}
	timeout := aptHost("")
	timeout.outputs["apt-get -s"] = commandResult{Err: context.DeadlineExceeded}
	if _, err := timeout.probe(statusNow).Updates(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout err = %v", err)
	}
}

func TestOSProbeNeverMutatesTheNode(t *testing.T) {
	hosts := []*fakeOSHost{aptHost(ubuntuAptSimulation), {
		tools:   map[string]bool{"dnf": true, "nice": true, "ionice": true, "needs-restarting": true, "timedatectl": true},
		outputs: map[string]commandResult{"check-update": {Stdout: dnfCheckUpdate, ExitCode: 100}, "updateinfo": {Stdout: dnfSecurity}, "needs-restarting": {}, "timedatectl": {Stdout: "no\n"}},
	}}
	for _, host := range hosts {
		probe := host.probe(statusNow)
		_, _ = probe.Updates(context.Background())
		_, _ = probe.Reboot(context.Background())
		_, _, _ = probe.ClockSynchronized(context.Background())
		for _, argv := range host.commands {
			for _, arg := range argv {
				switch arg {
				case "update", "makecache", "install", "reboot", "shutdown", "upgrade", "full-upgrade", "autoremove":
					t.Fatalf("probe ran a mutating command: %v", argv)
				}
			}
			if argv[0] == "apt-get" || (len(argv) > 5 && argv[5] == "apt-get") {
				if !strings.Contains(strings.Join(argv, " "), "apt-get -s ") {
					t.Fatalf("apt-get without simulation: %v", argv)
				}
			}
		}
	}
}

func newTestStatusManager(t *testing.T, dir string, host *fakeOSHost, clock *fakeClock) *nodeStatusManager {
	t.Helper()
	manager := newNodeStatusManager(dir, func() string { return "app-1" }, nil, nil, 0)
	manager.probe = host.probe(statusNow)
	manager.probe.now = clock.Now
	manager.now = clock.Now
	manager.jitter = func(time.Duration) time.Duration { return 0 }
	manager.docker = func(context.Context) error { return nil }
	return manager
}

func TestNodeConditionsTransitionsAndRestartPersistence(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{now: statusNow}
	host := aptHost(ubuntuAptSimulation)
	rebootAt := time.Date(2026, 10, 9, 3, 12, 0, 0, time.UTC)
	host.files[aptRebootRequiredPath] = rebootAt
	host.contents = map[string]string{aptRebootPackagesPath: "linux-image-6.8.0-45-generic\nlibc6\nlibc6\n"}
	manager := newTestStatusManager(t, dir, host, clock)
	manager.RunOSUpdates(context.Background())
	manager.RunReboot(context.Background())
	manager.RunDocker(context.Background())
	status, ok := manager.Status()
	if !ok {
		t.Fatal("status not evaluated")
	}
	osUpdates, _ := status.Condition(NodeConditionOSUpdates)
	reboot, _ := status.Condition(NodeConditionRebootRequired)
	if osUpdates.Status != NodeConditionWarn || !osUpdates.Since.Equal(statusNow) {
		t.Fatalf("os-updates = %+v", osUpdates)
	}
	if reboot.Status != NodeConditionWarn || !reboot.Since.Equal(rebootAt) || !status.OS.RebootRequired || !status.OS.RebootRequiredSince.Equal(rebootAt) {
		t.Fatalf("reboot = %+v os = %+v", reboot, status.OS)
	}
	if strings.Join(status.OS.RebootPackages, ",") != "libc6,linux-image-6.8.0-45-generic" || status.OS.PendingUpdates != 12 {
		t.Fatalf("os = %+v", status.OS)
	}
	// Re-evaluating keeps since while the status is unchanged.
	clock.Advance(6 * time.Hour)
	manager.RunOSUpdates(context.Background())
	osUpdates = manager.doc.Conditions[NodeConditionOSUpdates]
	if !osUpdates.Since.Equal(statusNow) || !osUpdates.CheckedAt.Equal(clock.Now()) {
		t.Fatalf("since not preserved: %+v", osUpdates)
	}
	// Restart: a new manager loads the persisted since.
	restarted := newTestStatusManager(t, dir, host, clock)
	restarted.Load()
	restartedStatus, _ := restarted.Status()
	if condition, _ := restartedStatus.Condition(NodeConditionRebootRequired); !condition.Since.Equal(rebootAt) {
		t.Fatalf("since after restart = %v", condition.Since)
	}
	// The reboot happened: the condition returns to ok with a new since.
	delete(host.files, aptRebootRequiredPath)
	clock.Advance(time.Minute)
	restarted.RunReboot(context.Background())
	if condition := restarted.doc.Conditions[NodeConditionRebootRequired]; condition.Status != NodeConditionOK || !condition.Since.Equal(clock.Now()) || restarted.doc.OS.RebootRequiredSince != nil {
		t.Fatalf("after reboot = %+v", condition)
	}
	data, err := os.ReadFile(restarted.path)
	if err != nil || !strings.Contains(string(data), `"schemaVersion": 1`) {
		t.Fatalf("persisted = %s err = %v", data, err)
	}
}

func TestNodeConditionStaleOnProbeFailure(t *testing.T) {
	clock := &fakeClock{now: statusNow}
	host := aptHost(ubuntuAptSimulation)
	manager := newTestStatusManager(t, t.TempDir(), host, clock)
	manager.RunOSUpdates(context.Background())
	host.outputs["apt-get -s"] = commandResult{Stdout: "E: Problem parsing list\n", ExitCode: 100}
	clock.Advance(6 * time.Hour)
	manager.RunOSUpdates(context.Background())
	condition := manager.doc.Conditions[NodeConditionOSUpdates]
	if condition.Status != NodeConditionWarn || !condition.Stale || !manager.doc.OS.Stale || manager.doc.OS.PendingUpdates != 12 {
		t.Fatalf("condition = %+v os = %+v", condition, manager.doc.OS)
	}
	fresh := newTestStatusManager(t, t.TempDir(), host, clock)
	fresh.RunOSUpdates(context.Background())
	if condition := fresh.doc.Conditions[NodeConditionOSUpdates]; condition.Status != NodeConditionUnknown {
		t.Fatalf("first failing probe = %+v", condition)
	}
}

func TestClockSyncFailsAfterTwoConsecutiveChecks(t *testing.T) {
	clock := &fakeClock{now: statusNow}
	host := aptHost(aptNothingToDo)
	manager := newTestStatusManager(t, t.TempDir(), host, clock)
	manager.RunClock(context.Background())
	if manager.doc.Conditions[NodeConditionClockSync].Status != NodeConditionOK {
		t.Fatal("expected ok")
	}
	host.outputs["timedatectl"] = commandResult{Stdout: "no\n"}
	clock.Advance(15 * time.Minute)
	firstNo := clock.Now()
	manager.RunClock(context.Background())
	if manager.doc.Conditions[NodeConditionClockSync].Status != NodeConditionOK {
		t.Fatal("one failed check must not fail the condition")
	}
	clock.Advance(15 * time.Minute)
	manager.RunClock(context.Background())
	condition := manager.doc.Conditions[NodeConditionClockSync]
	if condition.Status != NodeConditionFail || !condition.Since.Equal(firstNo) {
		t.Fatalf("condition = %+v", condition)
	}
}

func TestMetricsCollectorConditionStalls(t *testing.T) {
	clock := &fakeClock{now: statusNow}
	latest := statusNow
	manager := newNodeStatusManager(t.TempDir(), nil, nil, func() (time.Time, bool, error, error, []string) {
		return latest, true, nil, nil, nil
	}, time.Minute)
	manager.now = clock.Now
	manager.startedAt = statusNow
	manager.UpdateCollector()
	if manager.doc.Conditions[NodeConditionMetricsCollector].Status != NodeConditionOK {
		t.Fatal("expected ok")
	}
	clock.Advance(3*time.Minute + time.Second)
	manager.UpdateCollector()
	if condition := manager.doc.Conditions[NodeConditionMetricsCollector]; condition.Status != NodeConditionFail || condition.Reason != "no-recent-sample" {
		t.Fatalf("condition = %+v", condition)
	}
	disabled := newNodeStatusManager(t.TempDir(), nil, nil, nil, 0)
	disabled.UpdateCollector()
	if _, ok := disabled.doc.Conditions[NodeConditionMetricsCollector]; ok {
		t.Fatal("metrics-collector must be absent with native collection disabled")
	}
	// Restarted with --metrics-interval 0: the persisted failing condition
	// is dropped, not kept failing forever.
	manager.mu.Lock()
	manager.persistLocked()
	manager.mu.Unlock()
	restarted := newNodeStatusManager(strings.TrimSuffix(manager.path, nodeStatusFileName), nil, nil, nil, 0)
	restarted.Load()
	if _, ok := restarted.doc.Conditions[NodeConditionMetricsCollector]; !ok {
		t.Fatal("persisted condition not loaded")
	}
	restarted.UpdateCollector()
	if _, ok := restarted.doc.Conditions[NodeConditionMetricsCollector]; ok {
		t.Fatal("stale metrics-collector condition kept with native collection disabled")
	}
	if data, err := os.ReadFile(restarted.path); err != nil || strings.Contains(string(data), NodeConditionMetricsCollector) {
		t.Fatalf("persisted after disable = %s err = %v", data, err)
	}
}

func TestNodeStatusRunStartIsRaceFree(t *testing.T) {
	manager := newNodeStatusManager(t.TempDir(), nil, nil, func() (time.Time, bool, error, error, []string) {
		return time.Time{}, false, nil, nil, nil
	}, time.Minute)
	manager.docker = func(context.Context) error { return nil }
	manager.probe = aptHost(aptNothingToDo).probe(statusNow)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		manager.Run(ctx)
	}()
	for index := 0; index < 50; index++ {
		manager.Status()
	}
	cancel()
	<-done
}

func TestRunDueSchedulesProbes(t *testing.T) {
	clock := &fakeClock{now: statusNow}
	host := aptHost(aptNothingToDo)
	manager := newTestStatusManager(t, t.TempDir(), host, clock)
	pings := 0
	manager.docker = func(context.Context) error { pings++; return nil }
	manager.jitter = func(max time.Duration) time.Duration { return max }
	manager.schedule(clock.Now())
	manager.RunDue(context.Background())
	if pings != 0 || len(host.commands) != 0 {
		t.Fatalf("probes ran before their first due time: pings=%d commands=%v", pings, host.commands)
	}
	clock.Advance(nodeProbeJitter)
	manager.RunDue(context.Background())
	if pings != 1 || manager.doc.Conditions[NodeConditionClockSync].Status != NodeConditionOK {
		t.Fatalf("pings = %d conditions = %+v", pings, manager.doc.Conditions)
	}
	if _, ran := manager.doc.Conditions[NodeConditionOSUpdates]; ran {
		t.Fatal("os-updates must wait 2-5 minutes after start")
	}
	clock.Advance(osUpdatesFirstMax)
	manager.RunDue(context.Background())
	if _, ran := manager.doc.Conditions[NodeConditionOSUpdates]; !ran {
		t.Fatal("os-updates did not run after 5 minutes")
	}
}

type nodeAlertHarness struct {
	*sourceTestHarness
	manager *nodeStatusManager
	clock   *fakeClock
}

func newNodeAlertHarness(t *testing.T, dir string) *nodeAlertHarness {
	t.Helper()
	h := newSourceTestHarness(t, dir)
	if err := h.manager.RegisterSource(nodeAlertSourceSpec()); err != nil {
		t.Fatal(err)
	}
	request := AlertConfigRequest{Project: "shop", Environment: "production", Targets: AlertTargets{Slack: testSlackTarget}, Host: &HostAlertPolicy{DiskPercent: 90, Conditions: true}}
	if err := h.manager.ApplyConfig(request); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: h.now}
	manager := newNodeStatusManager(dir, func() string { return "app-1" }, h.manager, nil, 0)
	manager.now = clock.Now
	return &nodeAlertHarness{sourceTestHarness: h, manager: manager, clock: clock}
}

func (h *nodeAlertHarness) set(conditionType, status string, since time.Time) {
	h.manager.mu.Lock()
	h.manager.setConditionLocked(conditionType, status, "", "", h.clock.Now(), since)
	h.manager.persistLocked()
	h.manager.mu.Unlock()
}

func (h *nodeAlertHarness) publish() []capturedDelivery {
	h.sourceTestHarness.now = h.clock.Now()
	h.manager.PublishAlerts()
	return h.dispatch()
}

func deliveryEvents(deliveries []capturedDelivery) []string {
	var events []string
	for _, delivery := range deliveries {
		events = append(events, delivery.Payload["subject"].(string)+":"+delivery.Payload["event"].(string))
	}
	return events
}

func TestNodeAlertRebootOpensAfter24hAndRecovers(t *testing.T) {
	h := newNodeAlertHarness(t, t.TempDir())
	h.set(NodeConditionRebootRequired, NodeConditionWarn, h.clock.Now())
	if sent := h.publish(); len(sent) != 0 {
		t.Fatalf("opened inside the window: %v", deliveryEvents(sent))
	}
	h.clock.Advance(25 * time.Hour)
	h.set(NodeConditionRebootRequired, NodeConditionWarn, time.Time{})
	sent := h.publish()
	if events := deliveryEvents(sent); len(events) != 1 || events[0] != "reboot-required:node_reboot_required" {
		t.Fatalf("events = %v", events)
	}
	if sent[0].Payload["kind"] != AlertKindNode || sent[0].Payload["severity"] != "warning" {
		t.Fatalf("payload = %v", sent[0].Payload)
	}
	h.clock.Advance(time.Hour)
	h.set(NodeConditionRebootRequired, NodeConditionOK, time.Time{})
	if events := deliveryEvents(h.publish()); len(events) != 1 || events[0] != "reboot-required:node_condition_cleared" {
		t.Fatalf("recovery events = %v", events)
	}
}

func TestNodeAlertSecurityWindowSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	h := newNodeAlertHarness(t, dir)
	h.set(NodeConditionOSUpdates, NodeConditionWarn, h.clock.Now())
	h.clock.Advance(70 * time.Hour)
	if sent := h.publish(); len(sent) != 0 {
		t.Fatalf("opened before 72h: %v", deliveryEvents(sent))
	}
	// takod restarts at 70h: the persisted since is reloaded.
	restarted := newNodeStatusManager(dir, func() string { return "app-1" }, h.sourceTestHarness.manager, nil, 0)
	restarted.now = h.clock.Now
	restarted.Load()
	h.manager = restarted
	h.clock.Advance(time.Hour)
	if sent := h.publish(); len(sent) != 0 {
		t.Fatalf("opened at 71h: %v", deliveryEvents(sent))
	}
	h.clock.Advance(time.Hour)
	if events := deliveryEvents(h.publish()); len(events) != 1 || events[0] != "os-updates:node_security_updates" {
		t.Fatalf("events at 72h = %v", events)
	}
}

func TestNodeAlertClockDockerUnknownAndCollector(t *testing.T) {
	h := newNodeAlertHarness(t, t.TempDir())
	h.set(NodeConditionClockSync, NodeConditionFail, h.clock.Now())
	h.set(NodeConditionDockerDaemon, NodeConditionFail, h.clock.Now())
	h.set(NodeConditionMetricsCollector, NodeConditionFail, h.clock.Now())
	h.set(NodeConditionRebootRequired, NodeConditionUnknown, h.clock.Now().Add(-48*time.Hour))
	if events := deliveryEvents(h.publish()); len(events) != 1 || events[0] != "clock-sync:node_clock_unsynchronized" {
		t.Fatalf("events = %v", events)
	}
	// A one-minute docker blip recovers before the 3-minute window.
	h.clock.Advance(time.Minute)
	h.set(NodeConditionDockerDaemon, NodeConditionOK, time.Time{})
	if sent := h.publish(); len(sent) != 0 {
		t.Fatalf("docker blip delivered: %v", deliveryEvents(sent))
	}
	h.set(NodeConditionDockerDaemon, NodeConditionFail, time.Time{})
	h.clock.Advance(2 * time.Minute)
	if sent := h.publish(); len(sent) != 0 {
		t.Fatalf("docker opened after 2m: %v", deliveryEvents(sent))
	}
	h.clock.Advance(time.Minute)
	if events := deliveryEvents(h.publish()); len(events) != 1 || events[0] != "docker-daemon:node_docker_unreachable" {
		t.Fatalf("docker events = %v", events)
	}
	// unknown and stale neither open nor recover.
	h.set(NodeConditionClockSync, NodeConditionUnknown, time.Time{})
	if sent := h.publish(); len(sent) != 0 {
		t.Fatalf("unknown delivered: %v", deliveryEvents(sent))
	}
	status, _ := h.sourceTestHarness.manager.Status("shop", "production")
	for _, subject := range status.Subjects {
		if subject.Subject == NodeConditionMetricsCollector || subject.Subject == NodeConditionRebootRequired {
			t.Fatalf("subject %s must not be published", subject.Subject)
		}
	}
}

func TestHandleNodeStatus(t *testing.T) {
	server := NewServerWithOptions("/tmp/takod-test.sock", t.TempDir(), "test", ServerOptions{NodeName: "app-1"})
	recorder := httptest.NewRecorder()
	server.handleNodeStatus(recorder, httptest.NewRequest(http.MethodGet, "/v1/node/status", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"conditions": []`) {
		t.Fatalf("empty status = %d %s", recorder.Code, recorder.Body.String())
	}
	sh := server.statusHistory
	sh.status.mu.Lock()
	sh.status.setConditionLocked(NodeConditionRebootRequired, NodeConditionWarn, "reboot-required", "reboot required", statusNow, statusNow)
	sh.status.doc.OS = &NodeOSStatus{PackageManager: "apt", PendingUpdates: 12, SecurityUpdates: 3, RebootRequired: true, RebootRequiredSince: &statusNow}
	sh.status.mu.Unlock()
	recorder = httptest.NewRecorder()
	server.handleNodeStatus(recorder, httptest.NewRequest(http.MethodGet, "/v1/node/status", nil))
	var response NodeStatusResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Node != "app-1" || response.OS == nil || !response.OS.RebootRequired || len(response.Conditions) != 1 {
		t.Fatalf("response = %+v", response)
	}
	condition := response.Conditions[0]
	data, _ := json.Marshal(condition)
	want := `{"type":"reboot-required","status":"warn","reason":"reboot-required","detail":"reboot required","since":"2026-10-12T12:00:00Z","checkedAt":"2026-10-12T12:00:00Z"}`
	if string(data) != want {
		t.Fatalf("condition json = %s", data)
	}
	restore := useTempMetricsFile(t, `{"cpu_percent":"1"}`)
	defer restore()
	metrics := httptest.NewRecorder()
	server.handleMetrics(metrics, httptest.NewRequest(http.MethodGet, "/v1/metrics", nil))
	var metricsResponse MetricsResponse
	if err := json.Unmarshal(metrics.Body.Bytes(), &metricsResponse); err != nil || metricsResponse.Status == nil || metricsResponse.Status.OS.PendingUpdates != 12 {
		t.Fatalf("metrics status = %+v err = %v", metricsResponse.Status, err)
	}
}
