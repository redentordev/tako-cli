package takod

// Default container log rotation (design D1 of add-disk-hygiene-defaults).
// When a service has no `logging:` block, takod starts its containers with a
// bounded json-file policy. The default is applied only to `docker run`
// arguments: it is never part of the desired document or the CLI-computed
// tako.configHash label, so upgrading takod recreates nothing and a
// container keeps its options until it is next recreated. Operator policy
// always wins: the default is skipped when the daemon's default driver is not
// json-file, when /etc/docker/daemon.json sets log-driver or any log-opts
// key, or when that file cannot be read or parsed.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
	loggingPolicyProbeTimeout   = 15 * time.Second
	daemonJSONMaxBytes          = 1 << 20
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
	loggingDriver  func(context.Context) (string, error)
	now            func() time.Time
	// current is the active default, or nil when it is skipped or disabled.
	current atomic.Pointer[config.ServiceLoggingConfig]

	mu     sync.Mutex
	status HygieneLoggingStatus
}

func newContainerLoggingPolicy(disabled bool, maxSize string, maxFile int) *containerLoggingPolicy {
	if strings.TrimSpace(maxSize) == "" {
		maxSize = DefaultContainerLogMaxSize
	}
	if maxFile <= 0 {
		maxFile = DefaultContainerLogMaxFile
	}
	return &containerLoggingPolicy{
		disabled:       disabled,
		maxSize:        maxSize,
		maxFile:        maxFile,
		daemonJSONPath: defaultDockerDaemonJSONPath,
		loggingDriver:  dockerDefaultLoggingDriver,
		now:            func() time.Time { return time.Now().UTC() },
		status:         HygieneLoggingStatus{Reason: "not checked yet"},
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
func (p *containerLoggingPolicy) Refresh(ctx context.Context) HygieneLoggingStatus {
	status := p.evaluate(ctx)
	status.CheckedAt = p.now()
	p.mu.Lock()
	p.status = status
	p.mu.Unlock()
	if status.Active {
		p.current.Store(&config.ServiceLoggingConfig{Driver: "json-file", MaxSize: p.maxSize, MaxFiles: p.maxFile})
	} else {
		p.current.Store(nil)
	}
	return status
}

func (p *containerLoggingPolicy) evaluate(ctx context.Context) HygieneLoggingStatus {
	if p.disabled {
		return HygieneLoggingStatus{Reason: "disabled by --default-container-logging=off"}
	}
	if err := validateDockerLogging(&config.ServiceLoggingConfig{Driver: "json-file", MaxSize: p.maxSize, MaxFiles: p.maxFile}); err != nil {
		return HygieneLoggingStatus{Reason: "invalid default log rotation flags"}
	}
	if reason := daemonJSONLoggingPolicy(p.daemonJSONPath); reason != "" {
		return HygieneLoggingStatus{Reason: reason}
	}
	probeCtx, cancel := context.WithTimeout(ctx, loggingPolicyProbeTimeout)
	defer cancel()
	driver, err := p.loggingDriver(probeCtx)
	if err != nil {
		return HygieneLoggingStatus{Reason: "docker logging driver unknown: " + firstLine(err.Error())}
	}
	if driver != "json-file" {
		return HygieneLoggingStatus{Reason: fmt.Sprintf("docker default logging driver is %q (operator-managed)", driver)}
	}
	return HygieneLoggingStatus{Active: true, Reason: "active", Driver: "json-file", MaxSize: p.maxSize, MaxFile: p.maxFile}
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
