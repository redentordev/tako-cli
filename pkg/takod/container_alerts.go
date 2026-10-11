package takod

// Container OOM and crash-loop detection (design D7). One shared sampler
// inspects every Tako container every 30s; its samples are exported to other
// in-process consumers (add-local-self-healing) through
// ContainerObservations.

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/notification"
)

const (
	containerSampleInterval     = 30 * time.Second
	containerInspectBatch       = 64
	containerRestartingSamples  = 3
	containerReasonCrashLoop    = "crash_loop"
	containerReasonOOM          = "oom"
	containerQuietWindowsToHeal = 2
	// containerDockerTimeout bounds each docker call of a sample so a
	// wedged dockerd cannot block the sampler; the container subjects then
	// go stale, which tako monitor reports as needing attention.
	containerDockerTimeout = 20 * time.Second
)

// ContainerSample is one container's inspected state.
type ContainerSample struct {
	ID            string
	Name          string
	Project       string
	Environment   string
	Service       string
	Status        string
	Restarting    bool
	RestartCount  int
	OOMKilled     bool
	ExitCode      int
	StartedAt     time.Time
	FinishedAt    time.Time
	Health        string
	RestartPolicy string
	MemoryLimitMB int64
}

func containerAlertSourceSpec() AlertSourceSpec {
	return AlertSourceSpec{
		Kind:       AlertKindContainer,
		Reasons:    map[string]Severity{containerReasonCrashLoop: SeverityCritical, containerReasonOOM: SeverityWarning},
		DetailKeys: []string{"container", "exitCode", "memoryLimitMb", "restarts", "window"},
		Events: map[string]string{
			containerReasonCrashLoop: string(notification.EventContainerCrashLoop),
			containerReasonOOM:       string(notification.EventContainerOOM),
		},
		Recoveries: map[string]string{containerReasonCrashLoop: string(notification.EventContainerRecovered)},
	}
}

type containerHistory struct {
	restarts         []time.Time
	lastRestartCount int
	lastStartedAt    time.Time
	lastOOMStartedAt time.Time
	restartingRun    int
	lastRestartAt    time.Time
	crashLooping     bool
	seen             bool
}

// containerAlertSampler samples containers and reports container alerts.
type containerAlertSampler struct {
	alerts *SourceAlertManager
	now    func() time.Time
	// docker runs a docker CLI command and returns stdout; tests replace it.
	docker func(ctx context.Context, args ...string) ([]byte, error)
	// dockerTimeout bounds each docker call (containerDockerTimeout).
	dockerTimeout time.Duration
	// wanted reports whether anything else (self-healing) needs samples.
	wanted func() bool

	mu        sync.Mutex
	samples   map[string]ContainerSample
	sampledAt time.Time
	history   map[string]*containerHistory
}

func newContainerAlertSampler(alerts *SourceAlertManager) *containerAlertSampler {
	return &containerAlertSampler{
		alerts: alerts,
		now:    func() time.Time { return time.Now().UTC() },
		docker: func(ctx context.Context, args ...string) ([]byte, error) {
			return actualDockerCommandContext(ctx, "docker", args...).Output()
		},
		dockerTimeout: containerDockerTimeout,
		samples:       map[string]ContainerSample{},
		history:       map[string]*containerHistory{},
	}
}

// dockerCall runs one docker command bounded by dockerTimeout.
func (s *containerAlertSampler) dockerCall(ctx context.Context, args ...string) ([]byte, error) {
	if s.dockerTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.dockerTimeout)
		defer cancel()
	}
	return s.docker(ctx, args...)
}

// ContainerObservations returns a copy of the latest samples keyed by
// container ID and the time they were taken. It is read-only for consumers.
func (s *containerAlertSampler) ContainerObservations() (map[string]ContainerSample, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := make(map[string]ContainerSample, len(s.samples))
	for id, sample := range s.samples {
		copied[id] = sample
	}
	return copied, s.sampledAt
}

func (s *containerAlertSampler) Run(ctx context.Context) {
	ticker := time.NewTicker(containerSampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.alerts.AnySubscribed(AlertKindContainer) || (s.wanted != nil && s.wanted()) {
				s.SampleOnce(ctx)
			}
		}
	}
}

type dockerInspectDocument struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	RestartCount int `json:"RestartCount"`
	State        struct {
		Status     string `json:"Status"`
		Restarting bool   `json:"Restarting"`
		OOMKilled  bool   `json:"OOMKilled"`
		ExitCode   int    `json:"ExitCode"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
		Health     *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	HostConfig struct {
		Memory        int64 `json:"Memory"`
		RestartPolicy struct {
			Name string `json:"Name"`
		} `json:"RestartPolicy"`
	} `json:"HostConfig"`
}

// SampleOnce takes one sample and reports alerts for subscribed scopes.
func (s *containerAlertSampler) SampleOnce(ctx context.Context) {
	output, err := s.dockerCall(ctx, "ps", "-a", "--no-trunc", "--filter", "label=tako.project", "--format", "{{.ID}}")
	if err != nil {
		return
	}
	var ids []string
	for _, line := range strings.Split(string(output), "\n") {
		if id := strings.TrimSpace(line); id != "" && isHexIdentifier(id) {
			ids = append(ids, id)
		}
	}
	samples := map[string]ContainerSample{}
	for start := 0; start < len(ids); start += containerInspectBatch {
		end := start + containerInspectBatch
		if end > len(ids) {
			end = len(ids)
		}
		data, inspectErr := s.dockerCall(ctx, append([]string{"inspect"}, ids[start:end]...)...)
		if inspectErr != nil && len(data) == 0 {
			continue
		}
		var documents []dockerInspectDocument
		if json.Unmarshal(data, &documents) != nil {
			continue
		}
		for _, document := range documents {
			sample := ContainerSample{
				ID: document.ID, Name: strings.TrimPrefix(document.Name, "/"),
				Project: document.Config.Labels["tako.project"], Environment: document.Config.Labels["tako.environment"],
				Service: document.Config.Labels["tako.service"], Status: document.State.Status,
				Restarting: document.State.Restarting, RestartCount: document.RestartCount, OOMKilled: document.State.OOMKilled,
				ExitCode: document.State.ExitCode, RestartPolicy: document.HostConfig.RestartPolicy.Name,
				MemoryLimitMB: document.HostConfig.Memory / (1 << 20),
			}
			sample.StartedAt, _ = time.Parse(time.RFC3339Nano, document.State.StartedAt)
			sample.FinishedAt, _ = time.Parse(time.RFC3339Nano, document.State.FinishedAt)
			if document.State.Health != nil {
				sample.Health = document.State.Health.Status
			}
			if sample.ID != "" {
				samples[sample.ID] = sample
			}
		}
	}
	s.observe(samples, s.now())
}

func isHexIdentifier(value string) bool {
	if len(value) < 12 || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// observe updates history from a sample set and reports observations.
func (s *containerAlertSampler) observe(samples map[string]ContainerSample, now time.Time) {
	type serviceKey struct{ project, environment, service string }
	type serviceState struct {
		crashLoop bool
		restarts  int
		exitCode  int
		window    time.Duration
		container string
	}
	services := map[serviceKey]*serviceState{}
	var oomEvents []EventObservation

	s.mu.Lock()
	s.samples = samples
	s.sampledAt = now
	policies := map[string]*ContainerAlertPolicy{}
	for _, scope := range s.alerts.ScopePolicies() {
		if scope.Policy.Containers != nil {
			policies[scope.Project+"/"+scope.Environment] = scope.Policy.Containers
		}
	}
	for id := range s.history {
		if _, ok := samples[id]; !ok {
			delete(s.history, id)
		}
	}
	for id, sample := range samples {
		if !isSafeProjectName(sample.Project) || !isSafeRuntimeName(sample.Environment) || !isSafeServiceName(sample.Service) {
			continue
		}
		history := s.history[id]
		if history == nil {
			history = &containerHistory{}
			s.history[id] = history
		}
		policy := policies[sample.Project+"/"+sample.Environment]
		window := 10 * time.Minute
		threshold := 3
		if policy != nil {
			if policy.CrashLoopWindowSeconds > 0 {
				window = time.Duration(policy.CrashLoopWindowSeconds) * time.Second
			}
			if policy.CrashLoopRestarts > 0 {
				threshold = policy.CrashLoopRestarts
			}
		}
		if history.seen && sample.RestartCount > history.lastRestartCount {
			for i := 0; i < sample.RestartCount-history.lastRestartCount && i < 1000; i++ {
				history.restarts = append(history.restarts, now)
			}
			history.lastRestartAt = now
		}
		kept := history.restarts[:0]
		for _, at := range history.restarts {
			if now.Sub(at) <= window {
				kept = append(kept, at)
			}
		}
		history.restarts = kept
		if sample.Restarting || sample.Status == "restarting" {
			history.restartingRun++
		} else {
			history.restartingRun = 0
		}
		if len(history.restarts) >= threshold || history.restartingRun >= containerRestartingSamples {
			history.crashLooping = true
		} else if history.crashLooping && sample.Status == "running" && now.Sub(history.lastRestartAt) >= containerQuietWindowsToHeal*window {
			history.crashLooping = false
		}
		// OOM: only when the kill is new relative to the previous sample.
		if sample.OOMKilled && history.seen && (!sample.StartedAt.Equal(history.lastStartedAt) || sample.RestartCount != history.lastRestartCount) && !sample.StartedAt.Equal(history.lastOOMStartedAt) {
			history.lastOOMStartedAt = sample.StartedAt
			if policy != nil && policy.OOM {
				details := map[string]string{"container": safeContainerToken(sample.Name), "exitCode": strconv.Itoa(sample.ExitCode)}
				if sample.MemoryLimitMB > 0 {
					details["memoryLimitMb"] = strconv.FormatInt(sample.MemoryLimitMB, 10)
				}
				oomEvents = append(oomEvents, EventObservation{
					Project: sample.Project, Environment: sample.Environment, Kind: AlertKindContainer,
					Subject: sample.Service, Reason: containerReasonOOM, Details: details, ObservedAt: now,
				})
			}
		}
		history.seen = true
		history.lastRestartCount = sample.RestartCount
		history.lastStartedAt = sample.StartedAt
		key := serviceKey{sample.Project, sample.Environment, sample.Service}
		state := services[key]
		if state == nil {
			state = &serviceState{window: window}
			services[key] = state
		}
		if history.crashLooping {
			state.crashLoop = true
			if len(history.restarts) >= state.restarts {
				state.restarts, state.exitCode, state.container = len(history.restarts), sample.ExitCode, safeContainerToken(sample.Name)
			}
		}
	}
	s.mu.Unlock()

	for _, event := range oomEvents {
		_ = s.alerts.ReportEvent(event)
	}
	reported := map[string]bool{}
	for key, state := range services {
		if !s.alerts.Subscribed(key.project, key.environment, AlertKindContainer) {
			continue
		}
		observation := LevelObservation{
			Project: key.project, Environment: key.environment, Kind: AlertKindContainer, Subject: key.service,
			Interval: containerSampleInterval, ObservedAt: now,
			Details: map[string]string{"restarts": strconv.Itoa(state.restarts), "window": strconv.FormatInt(int64(state.window/time.Second), 10)},
		}
		if state.crashLoop {
			observation.Failing = []string{containerReasonCrashLoop}
			observation.Details["exitCode"] = strconv.Itoa(state.exitCode)
			if state.container != "" {
				observation.Details["container"] = state.container
			}
		}
		_ = s.alerts.ReportLevel(observation)
		reported[key.project+"/"+key.environment+"/"+key.service] = true
	}
	// Services whose containers are all gone are retired.
	for _, scope := range s.alerts.ScopePolicies() {
		for _, subject := range s.alerts.Subjects(scope.Project, scope.Environment, AlertKindContainer) {
			if !reported[scope.Project+"/"+scope.Environment+"/"+subject] {
				_ = s.alerts.Retire(scope.Project, scope.Environment, AlertKindContainer, subject)
			}
		}
	}
}

func safeContainerToken(name string) string {
	if alertDetailValuePattern.MatchString(name) {
		return name
	}
	return "unknown"
}
