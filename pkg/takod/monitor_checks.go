package takod

// Service health checks (design D5): http checks through the node's local
// proxy and container replica checks, reported as level observations of
// kind "service".

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redentordev/tako-cli/pkg/notification"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

const (
	maxConcurrentMonitorChecks = 4
	monitorBodyReadLimit       = 64 << 10
	minimumPostLeaseGrace      = 30 * time.Second
	localProxyAddress          = "127.0.0.1:443"
)

// Service subject hold reasons shown in status.
const (
	holdReasonOperation      = "operation"
	holdReasonGrace          = "grace"
	holdReasonRemoval        = "removal_pending"
	holdReasonMaintenance    = "maintenance"
	holdReasonNoLocalRoute   = "no_local_route"
	holdReasonNotDeployed    = "not_deployed"
	monitorErrorConnect      = "connect"
	monitorErrorTLS          = "tls"
	monitorErrorTimeout      = "timeout"
	monitorErrorStatus       = "status"
	serviceReasonUnreachable = "unreachable"
	serviceReasonStatus      = "unhealthy_status"
	serviceReasonTLS         = "tls_invalid"
	serviceReasonReplicas    = "replicas_missing"
	serviceReasonUnhealthy   = "unhealthy"
)

func serviceAlertSourceSpec() AlertSourceSpec {
	down, up := string(notification.EventServiceDown), string(notification.EventServiceUp)
	return AlertSourceSpec{
		Kind: AlertKindService,
		Reasons: map[string]Severity{
			serviceReasonUnreachable: SeverityCritical, serviceReasonStatus: SeverityCritical,
			serviceReasonTLS: SeverityCritical, serviceReasonReplicas: SeverityCritical, serviceReasonUnhealthy: SeverityWarning,
		},
		DetailKeys: []string{"checkType", "errorClass", "statusCode", "expected", "running"},
		Events: map[string]string{
			serviceReasonUnreachable: down, serviceReasonStatus: down, serviceReasonTLS: down, serviceReasonReplicas: down,
			serviceReasonUnhealthy: string(notification.EventHealthCheckFailed),
		},
		Recoveries: map[string]string{
			serviceReasonUnreachable: up, serviceReasonStatus: up, serviceReasonTLS: up, serviceReasonReplicas: up,
			serviceReasonUnhealthy: string(notification.EventHealthCheckRecovered),
		},
	}
}

// localDesiredRevision is the subset of takodstate's desired revision the
// checks read. takod does not import takodstate.
type localDesiredRevision struct {
	Services map[string]localDesiredService `json:"services"`
}

type localDesiredService struct {
	Replicas    int `json:"replicas"`
	Assignments []struct {
		Slot   int    `json:"slot"`
		NodeID string `json:"nodeId"`
		Node   string `json:"node"`
	} `json:"assignments"`
	Placement *struct {
		Strategy string `json:"strategy"`
	} `json:"placement"`
	HealthCheck struct {
		StartPeriod string `json:"startPeriod"`
	} `json:"healthCheck"`
	RemovalPending bool   `json:"removalPending"`
	WorkloadKind   string `json:"workloadKind"`
}

func readLocalDesiredRevision(dataDir, project, environment string) (*localDesiredRevision, time.Time, error) {
	path := filepath.Join(dataDir, "desired", project, environment, "revision.json")
	info, err := os.Stat(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	var revision localDesiredRevision
	if err := json.Unmarshal(data, &revision); err != nil {
		return nil, time.Time{}, err
	}
	return &revision, info.ModTime().UTC(), nil
}

// expectedLocalReplicas is the number of replicas desired on node: its
// assignments, 1 for global placement, or the replica count when the
// revision has no assignments (single-node deploys).
func expectedLocalReplicas(service localDesiredService, node string, singleNode bool) int {
	if service.Placement != nil && service.Placement.Strategy == "global" {
		return 1
	}
	if len(service.Assignments) == 0 {
		if singleNode {
			return service.Replicas
		}
		return 0
	}
	count := 0
	for _, assignment := range service.Assignments {
		if assignment.Node == node {
			count++
		}
	}
	return count
}

// serviceMonitorRunner schedules every service monitor of every scope on
// this node with per-monitor intervals, up to 10% jitter, and at most 4
// concurrent checks.
type serviceMonitorRunner struct {
	alerts  *SourceAlertManager
	dataDir string
	node    func() string
	now     func() time.Time
	// dial connects to the local proxy; tests replace it.
	dial func(ctx context.Context, network, address string) (net.Conn, error)
	// leaseHeld and actual are injectable for tests.
	leaseHeld func(ctx context.Context, dataDir, project, environment string) (bool, error)
	actual    func(ctx context.Context, project, environment string) (*ActualStateResponse, error)
	routesDir func() string

	mu        sync.Mutex
	next      map[string]time.Time
	running   map[string]bool
	leaseSeen map[string]time.Time
	slots     chan struct{}
	wg        sync.WaitGroup
}

func newServiceMonitorRunner(alerts *SourceAlertManager, dataDir string, node func() string) *serviceMonitorRunner {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &serviceMonitorRunner{
		alerts: alerts, dataDir: dataDir, node: node,
		now:       func() time.Time { return time.Now().UTC() },
		dial:      dialer.DialContext,
		leaseHeld: stateOperationLeaseHeld,
		actual:    GatherActualState,
		routesDir: func() string { return proxyRoutesDir },
		next:      map[string]time.Time{},
		running:   map[string]bool{},
		leaseSeen: map[string]time.Time{},
		slots:     make(chan struct{}, maxConcurrentMonitorChecks),
	}
}

// Run ticks every second and starts due checks until ctx is done.
func (r *serviceMonitorRunner) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	defer r.wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

func (r *serviceMonitorRunner) tick(ctx context.Context) {
	now := r.now()
	active := map[string]bool{}
	for _, scope := range r.alerts.ScopePolicies() {
		for _, monitor := range scope.Policy.Monitors {
			key := scope.Project + "/" + scope.Environment + "/" + monitor.Service
			active[key] = true
			r.mu.Lock()
			due, scheduled := r.next[key]
			if !scheduled {
				// First run soon, spread by jitter.
				r.next[key] = now.Add(jitter(time.Duration(monitor.IntervalSeconds) * time.Second / 10))
				r.mu.Unlock()
				continue
			}
			if r.running[key] || now.Before(due) {
				r.mu.Unlock()
				continue
			}
			select {
			case r.slots <- struct{}{}:
			default:
				r.mu.Unlock()
				continue
			}
			r.running[key] = true
			interval := time.Duration(monitor.IntervalSeconds) * time.Second
			r.next[key] = now.Add(interval + jitter(interval/10))
			r.mu.Unlock()
			project, environment, spec := scope.Project, scope.Environment, monitor
			r.wg.Add(1)
			go func() {
				defer r.wg.Done()
				defer func() {
					<-r.slots
					r.mu.Lock()
					delete(r.running, key)
					r.mu.Unlock()
				}()
				r.CheckOnce(ctx, project, environment, spec)
			}()
		}
	}
	r.mu.Lock()
	for key := range r.next {
		if !active[key] && !r.running[key] {
			delete(r.next, key)
		}
	}
	r.mu.Unlock()
}

func jitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(max)))
}

// CheckOnce runs one check of monitor and reports it. Monitors this node
// does not own are retired.
func (r *serviceMonitorRunner) CheckOnce(ctx context.Context, project, environment string, monitor MonitorSpec) {
	node := r.node()
	retire := func() { _ = r.alerts.Retire(project, environment, AlertKindService, monitor.Service) }
	revision, revisionWritten, err := readLocalDesiredRevision(r.dataDir, project, environment)
	var desired localDesiredService
	found := false
	if err == nil {
		desired, found = revision.Services[monitor.Service]
	}
	observation := LevelObservation{
		Project: project, Environment: environment, Kind: AlertKindService, Subject: monitor.Service,
		Damping:  Damping{OpenAfter: monitor.AlertAfter, RecoverAfter: monitor.RecoverAfter},
		Interval: time.Duration(monitor.IntervalSeconds) * time.Second,
		Details:  map[string]string{"checkType": monitor.CheckType},
	}
	switch monitor.CheckType {
	case MonitorCheckHTTP:
		if monitor.CheckerNode != "" && monitor.CheckerNode != node {
			retire()
			return
		}
	case MonitorCheckContainer:
		if !found || expectedLocalReplicas(desired, node, len(revision.Services) > 0 && allUnassigned(revision)) == 0 {
			retire()
			return
		}
	default:
		return
	}
	hold := func(state, reason string) {
		observation.Hold, observation.HoldReason = state, reason
		_ = r.alerts.ReportLevel(observation)
	}
	if !found {
		hold(SubjectHoldUnknown, holdReasonNotDeployed)
		return
	}
	if desired.RemovalPending {
		hold(SubjectHoldPaused, holdReasonRemoval)
		return
	}
	scopeKey := project + "/" + environment
	held, leaseErr := r.leaseHeld(ctx, r.dataDir, project, environment)
	now := r.now()
	if leaseErr == nil && held {
		r.mu.Lock()
		r.leaseSeen[scopeKey] = now
		r.mu.Unlock()
		hold(SubjectHoldPaused, holdReasonOperation)
		return
	}
	grace := minimumPostLeaseGrace
	if startPeriod, parseErr := time.ParseDuration(desired.HealthCheck.StartPeriod); parseErr == nil && startPeriod > grace {
		grace = startPeriod
	}
	r.mu.Lock()
	lastOperation := r.leaseSeen[scopeKey]
	r.mu.Unlock()
	if revisionWritten.After(lastOperation) {
		lastOperation = revisionWritten
	}
	if now.Sub(lastOperation) < grace {
		hold(SubjectHoldPaused, holdReasonGrace)
		return
	}
	switch monitor.CheckType {
	case MonitorCheckHTTP:
		maintenance := filepath.Join(r.routesDir(), runtimeid.MaintenanceProxyConfigFileName(project, environment, monitor.Service))
		if _, statErr := os.Stat(maintenance); statErr == nil {
			hold(SubjectHoldMaintenance, holdReasonMaintenance)
			return
		}
		if !localRouteServes(r.routesDir(), project, environment, monitor.Service, monitor.Domain) {
			hold(SubjectHoldUnknown, holdReasonNoLocalRoute)
			return
		}
		reason, details := r.checkHTTP(ctx, monitor)
		for key, value := range details {
			observation.Details[key] = value
		}
		if reason != "" {
			observation.Failing = []string{reason}
		}
	case MonitorCheckContainer:
		expected := expectedLocalReplicas(desired, node, allUnassigned(revision))
		actual, actualErr := r.actual(ctx, project, environment)
		if actualErr != nil {
			if ctx.Err() != nil {
				return
			}
			hold(SubjectHoldUnknown, "docker_unavailable")
			return
		}
		running, health := 0, ""
		if service := actual.Services[monitor.Service]; service != nil {
			running = len(service.ActiveContainers)
			if running == 0 {
				running = len(service.Containers)
			}
			health = service.Health
		}
		observation.Details["expected"] = strconv.Itoa(expected)
		observation.Details["running"] = strconv.Itoa(running)
		if running < expected {
			observation.Failing = append(observation.Failing, serviceReasonReplicas)
		}
		if health == HealthStateUnhealthy {
			observation.Failing = append(observation.Failing, serviceReasonUnhealthy)
		}
	}
	observation.ObservedAt = r.now()
	_ = r.alerts.ReportLevel(observation)
}

func allUnassigned(revision *localDesiredRevision) bool {
	if revision == nil {
		return false
	}
	for _, service := range revision.Services {
		if len(service.Assignments) > 0 {
			return false
		}
	}
	return true
}

// localRouteServes reports whether a local route manifest of the scope
// routes domain to service. Unreadable manifests are ignored.
func localRouteServes(dir, project, environment, service, domain string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	maintenance := runtimeid.MaintenanceProxyConfigFileName(project, environment, service)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || entry.Name() == maintenance {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<20 {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		manifest, err := ParseProxyRouteManifest(string(data))
		if err != nil || manifest.Project != project || manifest.Environment != environment {
			continue
		}
		for _, route := range manifest.Routes {
			if route.Service != service {
				continue
			}
			for _, candidate := range route.Domains {
				if strings.EqualFold(candidate, domain) {
					return true
				}
			}
		}
	}
	return false
}

// checkHTTP performs one http check against the local proxy and returns a
// failing reason (empty on success) plus closed-vocabulary details. Error
// text never leaves this function.
func (r *serviceMonitorRunner) checkHTTP(ctx context.Context, monitor MonitorSpec) (string, map[string]string) {
	timeout := time.Duration(monitor.TimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var tlsInvalid atomic.Bool
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return r.dial(ctx, network, localProxyAddress)
		},
		TLSClientConfig: &tls.Config{
			ServerName: monitor.Domain,
			MinVersion: tls.VersionTLS12,
			// The chain is deliberately not verified: pushed origin
			// certificates are not publicly trusted. Hostname coverage
			// and validity are verified below; expiry alerting is the
			// certificate kind's job.
			InsecureSkipVerify: true, //nolint:gosec
			VerifyConnection: func(state tls.ConnectionState) error {
				if len(state.PeerCertificates) == 0 {
					tlsInvalid.Store(true)
					return errors.New("no certificate")
				}
				leaf := state.PeerCertificates[0]
				now := r.now()
				if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) || leaf.VerifyHostname(monitor.Domain) != nil {
					tlsInvalid.Store(true)
					return errors.New("certificate invalid")
				}
				return nil
			},
		},
		DisableKeepAlives:     true,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+monitor.Domain+monitor.HealthPath, nil)
	if err != nil {
		return serviceReasonUnreachable, map[string]string{"errorClass": monitorErrorConnect}
	}
	request.Header.Set("User-Agent", "takod-monitor/1")
	response, err := client.Do(request)
	if err != nil {
		switch {
		case tlsInvalid.Load():
			return serviceReasonTLS, map[string]string{"errorClass": monitorErrorTLS}
		case errors.Is(err, context.DeadlineExceeded) || isTimeoutError(err):
			return serviceReasonUnreachable, map[string]string{"errorClass": monitorErrorTimeout}
		case isTLSError(err):
			return serviceReasonUnreachable, map[string]string{"errorClass": monitorErrorTLS}
		default:
			return serviceReasonUnreachable, map[string]string{"errorClass": monitorErrorConnect}
		}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, monitorBodyReadLimit))
	response.Body.Close()
	details := map[string]string{"statusCode": strconv.Itoa(response.StatusCode)}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return "", details
	}
	if response.StatusCode == http.StatusUnauthorized && monitor.BasicAuth {
		return "", details
	}
	details["errorClass"] = monitorErrorStatus
	return serviceReasonStatus, details
}

func isTimeoutError(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func isTLSError(err error) bool {
	var recordErr tls.RecordHeaderError
	var alertErr tls.AlertError
	return errors.As(err, &recordErr) || errors.As(err, &alertErr) || strings.Contains(err.Error(), "tls:")
}
