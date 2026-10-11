package takod

// Read-only OS probes for node status (add-server-status-history design D5).
// Probes only read cached package lists: they never refresh lists, install
// packages or reboot. Command execution and filesystem access are injectable
// for fixture tests.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

const (
	osProbeTimeout        = 120 * time.Second
	osProbeListLimit      = 20
	aptRebootRequiredPath = "/var/run/reboot-required"
	aptRebootPackagesPath = "/var/run/reboot-required.pkgs"
	aptUpdateStampPath    = "/var/lib/apt/periodic/update-success-stamp"
	aptListsPath          = "/var/lib/apt/lists"
)

// NodeOSStatus is the os block of GET /v1/node/status.
type NodeOSStatus struct {
	PackageManager      string     `json:"packageManager"`
	PendingUpdates      int        `json:"pendingUpdates"`
	SecurityUpdates     int        `json:"securityUpdates"`
	SecurityPackages    []string   `json:"securityPackages,omitempty"`
	ListsAgeSeconds     int64      `json:"listsAgeSeconds,omitempty"`
	RebootRequired      bool       `json:"rebootRequired"`
	RebootRequiredSince *time.Time `json:"rebootRequiredSince,omitempty"`
	RebootPackages      []string   `json:"rebootPackages,omitempty"`
	CheckedAt           time.Time  `json:"checkedAt,omitempty"`
	RebootCheckedAt     time.Time  `json:"rebootCheckedAt,omitempty"`
	Stale               bool       `json:"stale,omitempty"`
}

// commandResult is the outcome of a probe command. Err is set only when the
// command could not run (missing, timed out, killed); a non-zero exit is
// reported through ExitCode.
type commandResult struct {
	Stdout   string
	ExitCode int
	Err      error
}

type osProbeRunner func(ctx context.Context, env []string, name string, args ...string) commandResult

// osProbe gathers package update and reboot state.
type osProbe struct {
	run      osProbeRunner
	lookPath func(string) (string, error)
	stat     func(string) (os.FileInfo, error)
	readFile func(string) ([]byte, error)
	now      func() time.Time
	timeout  time.Duration
}

func newOSProbe() *osProbe {
	return &osProbe{
		run: runOSProbeCommand, lookPath: exec.LookPath, stat: os.Stat, readFile: os.ReadFile,
		now: func() time.Time { return time.Now().UTC() }, timeout: osProbeTimeout,
	}
}

func runOSProbeCommand(ctx context.Context, env []string, name string, args ...string) commandResult {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	output := newCappedOutputBuffer(defaultCommandOutputMaxBytes)
	cmd.Stdout = output
	cmd.Stderr = nil
	err := cmd.Run()
	result := commandResult{Stdout: output.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case ctx.Err() != nil:
		result.Err = ctx.Err()
	case errors.As(err, &exitErr):
		result.ExitCode = exitErr.ExitCode()
	default:
		result.Err = err
	}
	return result
}

// packageManager returns apt, dnf, yum or "" when unsupported.
func (p *osProbe) packageManager() string {
	for _, manager := range []string{"apt-get", "dnf", "yum"} {
		if _, err := p.lookPath(manager); err == nil {
			return strings.TrimSuffix(manager, "-get")
		}
	}
	return ""
}

// niceCommand wraps a command with nice and, when available, ionice idle
// class so a probe never competes with workloads.
func (p *osProbe) niceCommand(name string, args ...string) (string, []string) {
	wrapped := []string{"-n", "10"}
	if _, err := p.lookPath("ionice"); err == nil {
		wrapped = append(wrapped, "ionice", "-c3")
	}
	wrapped = append(wrapped, name)
	wrapped = append(wrapped, args...)
	if _, err := p.lookPath("nice"); err != nil {
		return wrapped[2], wrapped[3:]
	}
	return "nice", wrapped
}

func (p *osProbe) runNice(ctx context.Context, name string, args ...string) commandResult {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	command, commandArgs := p.niceCommand(name, args...)
	return p.run(ctx, []string{"LANG=C", "LC_ALL=C"}, command, commandArgs...)
}

// errUnsupportedPackageManager marks a node whose package manager Tako does
// not inspect.
var errUnsupportedPackageManager = errors.New("unsupported package manager")

// Updates counts pending and security updates from cached lists.
func (p *osProbe) Updates(ctx context.Context) (NodeOSStatus, error) {
	status := NodeOSStatus{PackageManager: p.packageManager(), CheckedAt: p.now()}
	switch status.PackageManager {
	case "apt":
		result := p.runNice(ctx, "apt-get", "-s", "-o", "Debug::NoLocking=1", "upgrade")
		if result.Err != nil {
			return status, fmt.Errorf("apt simulation: %w", result.Err)
		}
		if result.ExitCode != 0 {
			return status, fmt.Errorf("apt simulation exited %d", result.ExitCode)
		}
		pending, security, err := parseAptSimulation(result.Stdout)
		if err != nil {
			return status, err
		}
		status.PendingUpdates, status.SecurityUpdates = pending, len(security)
		status.SecurityPackages = capList(security)
		status.ListsAgeSeconds = p.aptListsAge()
		return status, nil
	case "dnf", "yum":
		manager := status.PackageManager
		result := p.runNice(ctx, manager, "--cacheonly", "-q", "check-update")
		if result.Err != nil {
			return status, fmt.Errorf("%s check-update: %w", manager, result.Err)
		}
		switch result.ExitCode {
		case 0:
		case 100:
			status.PendingUpdates = parseDnfCheckUpdate(result.Stdout)
		default:
			return status, fmt.Errorf("%s check-update exited %d", manager, result.ExitCode)
		}
		if status.PendingUpdates > 0 {
			security := p.runNice(ctx, manager, "--cacheonly", "-q", "updateinfo", "list", "--security")
			if security.Err != nil {
				return status, fmt.Errorf("%s updateinfo: %w", manager, security.Err)
			}
			if security.ExitCode != 0 {
				return status, fmt.Errorf("%s updateinfo exited %d", manager, security.ExitCode)
			}
			packages := parseDnfSecurity(security.Stdout)
			status.SecurityUpdates = len(packages)
			status.SecurityPackages = capList(packages)
		}
		return status, nil
	default:
		return status, errUnsupportedPackageManager
	}
}

func (p *osProbe) aptListsAge() int64 {
	for _, path := range []string{aptUpdateStampPath, aptListsPath} {
		if info, err := p.stat(path); err == nil {
			age := p.now().Sub(info.ModTime())
			if age < 0 {
				age = 0
			}
			return int64(age / time.Second)
		}
	}
	return 0
}

// rebootState is the result of the reboot-required probe.
type rebootState struct {
	Required bool
	Since    time.Time
	Packages []string
}

// Reboot reports whether the node needs a reboot. apt systems use
// /var/run/reboot-required; dnf/yum systems use `needs-restarting -r`.
func (p *osProbe) Reboot(ctx context.Context) (rebootState, error) {
	manager := p.packageManager()
	if manager == "apt" {
		info, err := p.stat(aptRebootRequiredPath)
		if errors.Is(err, os.ErrNotExist) {
			return rebootState{}, nil
		}
		if err != nil {
			return rebootState{}, err
		}
		state := rebootState{Required: true, Since: info.ModTime().UTC()}
		if data, readErr := p.readFile(aptRebootPackagesPath); readErr == nil {
			state.Packages = capList(uniqueLines(string(data)))
		}
		return state, nil
	}
	if _, err := p.lookPath("needs-restarting"); err != nil {
		if manager == "" {
			return rebootState{}, errUnsupportedPackageManager
		}
		return rebootState{}, fmt.Errorf("needs-restarting is not installed")
	}
	result := p.runNice(ctx, "needs-restarting", "-r")
	if result.Err != nil {
		return rebootState{}, result.Err
	}
	switch result.ExitCode {
	case 0:
		return rebootState{}, nil
	case 1:
		return rebootState{Required: true, Packages: capList(parseNeedsRestarting(result.Stdout))}, nil
	default:
		return rebootState{}, fmt.Errorf("needs-restarting exited %d", result.ExitCode)
	}
}

// ClockSynchronized runs timedatectl. ok is false when the tool is missing.
func (p *osProbe) ClockSynchronized(ctx context.Context) (synchronized bool, ok bool, err error) {
	if _, lookErr := p.lookPath("timedatectl"); lookErr != nil {
		return false, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result := p.run(ctx, []string{"LANG=C", "LC_ALL=C"}, "timedatectl", "show", "-p", "NTPSynchronized", "--value")
	if result.Err != nil {
		return false, true, result.Err
	}
	if result.ExitCode != 0 {
		return false, true, fmt.Errorf("timedatectl exited %d", result.ExitCode)
	}
	switch strings.TrimSpace(result.Stdout) {
	case "yes":
		return true, true, nil
	case "no":
		return false, true, nil
	default:
		return false, true, fmt.Errorf("unexpected timedatectl output %q", strings.TrimSpace(result.Stdout))
	}
}

// parseAptSimulation counts "Inst" lines of `apt-get -s upgrade`. A line is
// a security update when its origin contains "-security".
func parseAptSimulation(output string) (int, []string, error) {
	pending := 0
	var security []string
	sawSummary := false
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.Contains(line, "upgraded,") && strings.Contains(line, "newly installed") {
			sawSummary = true
		}
		if !strings.HasPrefix(line, "Inst ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pending++
		if strings.Contains(strings.ToLower(line), "-security") {
			security = append(security, fields[1])
		}
	}
	if !sawSummary && pending == 0 {
		return 0, nil, fmt.Errorf("unrecognized apt simulation output")
	}
	sort.Strings(security)
	return pending, security, nil
}

// parseDnfCheckUpdate counts package lines (name.arch version repo) before
// any "Obsoleting Packages" section.
func parseDnfCheckUpdate(output string) int {
	count := 0
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "Obsoleting") {
			break
		}
		fields := strings.Fields(line)
		if len(fields) == 3 && strings.Contains(fields[0], ".") && !strings.HasPrefix(line, "Last metadata") {
			count++
		}
	}
	return count
}

// parseDnfSecurity returns the unique package names of
// `updateinfo list --security` (advisory, type, package-version.arch).
func parseDnfSecurity(output string) []string {
	seen := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 || !strings.Contains(strings.ToLower(fields[1]), "sec") {
			continue
		}
		seen[fields[2]] = true
	}
	packages := make([]string, 0, len(seen))
	for name := range seen {
		packages = append(packages, name)
	}
	sort.Strings(packages)
	return packages
}

func parseNeedsRestarting(output string) []string {
	var packages []string
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "* ") {
			packages = append(packages, strings.TrimSpace(strings.TrimPrefix(line, "* ")))
		}
	}
	return packages
}

func uniqueLines(text string) []string {
	seen := map[string]bool{}
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		lines = append(lines, line)
	}
	sort.Strings(lines)
	return lines
}

func capList(values []string) []string {
	if len(values) > osProbeListLimit {
		values = values[:osProbeListLimit]
	}
	if len(values) == 0 {
		return nil
	}
	return append([]string(nil), values...)
}

// dockerPinger pings the Docker daemon.
type dockerPinger func(ctx context.Context) error

func pingDockerDaemon(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	output, err := runDocker(ctx, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace([]byte(output))) == 0 {
		return errors.New("docker daemon returned no server version")
	}
	return nil
}
