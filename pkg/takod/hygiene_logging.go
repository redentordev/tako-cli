package takod

// Default container log rotation (design D1 of add-disk-hygiene-defaults).
// When a service has no `logging:` block, takod starts its containers with a
// bounded json-file policy. The default is applied only to `docker run`
// arguments: it is never part of the desired document or the CLI-computed
// tako.configHash label, so upgrading takod recreates nothing and a
// container keeps its options until it is next recreated. Operator policy
// always wins: the default is skipped when the daemon's default driver is not
// json-file, when a daemon.json (/etc/docker/daemon.json, the snap package's
// /var/snap/docker/current/config/daemon.json, or dockerd's --config-file)
// sets log-driver or any log-opts key or cannot be read or parsed, or when
// dockerd runs with --log-driver or --log-opt flags.
//
// A failed or slow `docker info` probe keeps the last good result; until
// one succeeds the probe is retried every loggingPolicyRetryInterval rather
// than at the next hourly check.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
)

const (
	DefaultContainerLogMaxSize  = "20m"
	DefaultContainerLogMaxFile  = 5
	defaultDockerDaemonJSONPath = "/etc/docker/daemon.json"
	snapDockerDaemonJSONPath    = "/var/snap/docker/current/config/daemon.json"
	loggingPolicyProbeTimeout   = 15 * time.Second
	loggingPolicyRetryInterval  = time.Minute
	daemonJSONMaxBytes          = 1 << 20
	dockerdCmdlineMaxBytes      = 64 << 10
)

// HygieneLoggingStatus reports whether the node default is applied and why.
type HygieneLoggingStatus struct {
	Active    bool      `json:"active"`
	Reason    string    `json:"reason"`
	Driver    string    `json:"driver,omitempty"`
	MaxSize   string    `json:"maxSize,omitempty"`
	MaxFile   int       `json:"maxFile,omitempty"`
	CheckedAt time.Time `json:"checkedAt,omitempty"`
}

type containerLoggingPolicy struct {
	disabled       bool
	maxSize        string
	maxFile        int
	daemonJSONPath string
	// extraDaemonJSONPaths are further daemon.json locations (snap Docker).
	extraDaemonJSONPaths []string
	loggingDriver        func(context.Context) (string, error)
	// dockerdArgs returns the running dockerd's arguments (nil when
	// unknown); tests replace it.
	dockerdArgs func(context.Context) []string
	now         func() time.Time
	// current is the active default, or nil when it is skipped or disabled.
	current atomic.Pointer[config.ServiceLoggingConfig]

	mu     sync.Mutex
	status HygieneLoggingStatus
	// lastGood is the last evaluation whose docker probe succeeded (or
	// that needed no probe).
	lastGood *HygieneLoggingStatus
	// probeFailed is set while the latest docker probe failed.
	probeFailed bool
}

func newContainerLoggingPolicy(disabled bool, maxSize string, maxFile int) *containerLoggingPolicy {
	if strings.TrimSpace(maxSize) == "" {
		maxSize = DefaultContainerLogMaxSize
	}
	if maxFile <= 0 {
		maxFile = DefaultContainerLogMaxFile
	}
	return &containerLoggingPolicy{
		disabled:             disabled,
		maxSize:              maxSize,
		maxFile:              maxFile,
		daemonJSONPath:       defaultDockerDaemonJSONPath,
		extraDaemonJSONPaths: []string{snapDockerDaemonJSONPath},
		loggingDriver:        dockerDefaultLoggingDriver,
		dockerdArgs:          runningDockerdArgs,
		now:                  func() time.Time { return time.Now().UTC() },
		status:               HygieneLoggingStatus{Reason: "not checked yet"},
	}
}

// ValidateDefaultContainerLogging checks the takod default log flags with the
// same bounds as a service `logging:` block.
func ValidateDefaultContainerLogging(mode string, maxSize string, maxFile int) error {
	switch strings.TrimSpace(mode) {
	case "", "on", "off":
	default:
		return fmt.Errorf("--default-container-logging must be on or off")
	}
	if err := validateDockerLogging(&config.ServiceLoggingConfig{Driver: "json-file", MaxSize: maxSize, MaxFiles: maxFile}); err != nil {
		return fmt.Errorf("invalid default container log rotation (max-size %q, max-file %d): %w", maxSize, maxFile, err)
	}
	return nil
}

// ValidBuildCacheKeepStorage reports whether value is an accepted
// `docker builder prune --keep-storage` budget.
func ValidBuildCacheKeepStorage(value string) bool { return isSafeBuildCacheKeepStorage(value) }

func dockerDefaultLoggingDriver(ctx context.Context) (string, error) {
	output, err := runDocker(ctx, "info", "--format", "{{.LoggingDriver}}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

// Refresh re-reads the daemon logging driver and daemon.json and publishes
// the resulting default.
// A failed docker probe keeps the last good result (and its published
// default); NeedsRetry then reports true until a probe succeeds.
func (p *containerLoggingPolicy) Refresh(ctx context.Context) HygieneLoggingStatus {
	status, probeErr := p.evaluate(ctx)
	status.CheckedAt = p.now()
	p.mu.Lock()
	p.probeFailed = probeErr != nil
	if probeErr != nil && p.lastGood != nil {
		kept := *p.lastGood
		kept.Reason += " (kept: last docker probe failed: " + firstLine(probeErr.Error()) + ")"
		kept.CheckedAt = status.CheckedAt
		p.status = kept
		p.mu.Unlock()
		return kept
	}
	if probeErr == nil {
		good := status
		p.lastGood = &good
	}
	p.status = status
	p.mu.Unlock()
	if status.Active {
		p.current.Store(&config.ServiceLoggingConfig{Driver: "json-file", MaxSize: p.maxSize, MaxFiles: p.maxFile})
	} else {
		p.current.Store(nil)
	}
	return status
}

// NeedsRetry reports whether the latest docker probe failed, so the
// hygiene loop retries soon instead of at the next hourly check.
func (p *containerLoggingPolicy) NeedsRetry() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.probeFailed
}

// evaluate returns the policy status and, when the docker probe failed, its
// error (the status then describes the failure).
func (p *containerLoggingPolicy) evaluate(ctx context.Context) (HygieneLoggingStatus, error) {
	if p.disabled {
		return HygieneLoggingStatus{Reason: "disabled by --default-container-logging=off"}, nil
	}
	if err := validateDockerLogging(&config.ServiceLoggingConfig{Driver: "json-file", MaxSize: p.maxSize, MaxFiles: p.maxFile}); err != nil {
		return HygieneLoggingStatus{Reason: "invalid default log rotation flags"}, nil
	}
	var args []string
	if p.dockerdArgs != nil {
		args = p.dockerdArgs(ctx)
	}
	configFile, reason := dockerdLoggingFlags(args)
	if reason != "" {
		return HygieneLoggingStatus{Reason: reason}, nil
	}
	paths := []string{p.daemonJSONPath}
	if configFile != "" {
		paths = []string{configFile}
	}
	paths = append(paths, p.extraDaemonJSONPaths...)
	for _, path := range paths {
		if reason := daemonJSONLoggingPolicy(path); reason != "" {
			if path != defaultDockerDaemonJSONPath && path != p.daemonJSONPath {
				reason = strings.Replace(reason, "daemon.json", path, 1)
			}
			return HygieneLoggingStatus{Reason: reason}, nil
		}
	}
	probeCtx, cancel := context.WithTimeout(ctx, loggingPolicyProbeTimeout)
	defer cancel()
	driver, err := p.loggingDriver(probeCtx)
	if err != nil {
		return HygieneLoggingStatus{Reason: "docker logging driver unknown: " + firstLine(err.Error())}, err
	}
	if driver != "json-file" {
		return HygieneLoggingStatus{Reason: fmt.Sprintf("docker default logging driver is %q (operator-managed)", driver)}, nil
	}
	return HygieneLoggingStatus{Active: true, Reason: "active", Driver: "json-file", MaxSize: p.maxSize, MaxFile: p.maxFile}, nil
}

// dockerdLoggingFlags inspects dockerd's arguments: it returns the
// --config-file path, if any, or why the flags make logging
// operator-managed (--log-driver or --log-opt).
func dockerdLoggingFlags(args []string) (configFile string, reason string) {
	for index, arg := range args {
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--log-driver":
			return "", "dockerd runs with --log-driver (operator-managed)"
		case "--log-opt":
			return "", "dockerd runs with --log-opt (operator-managed)"
		case "--config-file":
			if !hasValue && index+1 < len(args) {
				value = args[index+1]
			}
			configFile = value
		}
	}
	return configFile, ""
}

// runningDockerdArgs returns the arguments of the running dockerd from
// /proc, else from systemd's ExecStart of docker.service, else nil.
func runningDockerdArgs(ctx context.Context) []string {
	if args := dockerdArgsFromProc("/proc"); args != nil {
		return args
	}
	ctx, cancel := context.WithTimeout(ctx, loggingPolicyProbeTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, "systemctl", "show", "--property=ExecStart", "--value", "docker.service").Output()
	if err != nil {
		return nil
	}
	return parseSystemdExecStartArgs(string(output))
}

// dockerdArgsFromProc finds a process named dockerd under procRoot and
// returns its NUL-separated command line, or nil.
func dockerdArgsFromProc(procRoot string) []string {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "" || name[0] < '0' || name[0] > '9' {
			continue
		}
		comm, err := os.ReadFile(filepath.Join(procRoot, name, "comm"))
		if err != nil || strings.TrimSpace(string(comm)) != "dockerd" {
			continue
		}
		file, err := os.Open(filepath.Join(procRoot, name, "cmdline"))
		if err != nil {
			continue
		}
		buffer := make([]byte, dockerdCmdlineMaxBytes)
		count, _ := file.Read(buffer)
		file.Close()
		var args []string
		for _, part := range strings.Split(string(buffer[:count]), "\x00") {
			if part != "" {
				args = append(args, part)
			}
		}
		if len(args) > 0 {
			return args
		}
	}
	return nil
}

// parseSystemdExecStartArgs extracts argv from `systemctl show
// --property=ExecStart --value` output ("{ path=... ; argv[]=... ; ... }").
func parseSystemdExecStartArgs(output string) []string {
	_, rest, ok := strings.Cut(output, "argv[]=")
	if !ok {
		return nil
	}
	if end := strings.Index(rest, " ;"); end >= 0 {
		rest = rest[:end]
	}
	return strings.Fields(rest)
}

// daemonJSONLoggingPolicy returns why daemon.json makes logging
// operator-managed, or "" when it is absent or sets no logging keys.
func daemonJSONLoggingPolicy(path string) string {
	if path == "" {
		return ""
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		return "daemon.json unreadable (operator-managed)"
	}
	if info.Size() > daemonJSONMaxBytes {
		return "daemon.json too large (operator-managed)"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "daemon.json unreadable (operator-managed)"
	}
	if strings.TrimSpace(string(data)) == "" {
		return ""
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		return "daemon.json is not valid JSON (operator-managed)"
	}
	if _, ok := document["log-driver"]; ok {
		return "daemon.json sets log-driver (operator-managed)"
	}
	if _, ok := document["log-opts"]; ok {
		return "daemon.json sets log-opts (operator-managed)"
	}
	return ""
}

// Status returns the last evaluation.
func (p *containerLoggingPolicy) Status() HygieneLoggingStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

func firstLine(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		value = value[:index]
	}
	if len(value) > 512 {
		value = value[:512]
	}
	return value
}

// Current returns the active default, or nil.
func (p *containerLoggingPolicy) Current() *config.ServiceLoggingConfig {
	if p == nil {
		return nil
	}
	return p.current.Load()
}

// defaultContainerLogging is the node default for new service containers
// without a `logging:` block, or nil.
func (h *hygieneManager) defaultContainerLogging() *config.ServiceLoggingConfig {
	if h == nil {
		return nil
	}
	return h.logging.Current()
}

// effectiveContainerLogging is the logging policy of a new service
// container: an explicit `logging:` block, else the node default (if any).
func effectiveContainerLogging(explicit, nodeDefault *config.ServiceLoggingConfig) *config.ServiceLoggingConfig {
	if explicit != nil {
		return explicit
	}
	return nodeDefault
}
