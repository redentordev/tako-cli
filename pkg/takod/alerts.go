package takod

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/alerttarget"
	"github.com/redentordev/tako-cli/pkg/recovery"
)

const schedulerAlertsDirName = "scheduler-alerts"
const alertDeliveryMaxAttempts = 3
const maxAlertDeliveries = 1024

type AlertPolicy struct {
	JobConsecutiveFailures int   `json:"jobConsecutiveFailures,omitempty"`
	JobConsecutiveTimeouts int   `json:"jobConsecutiveTimeouts,omitempty"`
	JobConsecutiveSkipped  int   `json:"jobConsecutiveSkipped,omitempty"`
	JobNoSuccessSeconds    int64 `json:"jobNoSuccessSeconds,omitempty"`
	BackupMaxAgeSeconds    int64 `json:"backupMaxAgeSeconds,omitempty"`
}

type AlertTargets struct {
	Slack   string `json:"slack,omitempty"`
	Discord string `json:"discord,omitempty"`
	Webhook string `json:"webhook,omitempty"`
}

type AlertConfigRequest struct {
	Project     string       `json:"project"`
	Environment string       `json:"environment"`
	Policy      AlertPolicy  `json:"policy"`
	Targets     AlertTargets `json:"targets"`

	// Additive node-side health monitoring fields (monitoring.health-v1).
	// They are handled by SourceAlertManager and never stored in the
	// scheduler-alerts documents.
	Monitors     []MonitorSpec           `json:"monitors,omitempty"`
	Host         *HostAlertPolicy        `json:"host,omitempty"`
	Containers   *ContainerAlertPolicy   `json:"containers,omitempty"`
	Certificates *CertificateAlertPolicy `json:"certificates,omitempty"`
	// ServiceTargets maps a monitored service to its monitoring.webhook.
	ServiceTargets map[string]string `json:"serviceTargets,omitempty"`
}

// legacyPart returns the job and backup part of a request.
func (request AlertConfigRequest) legacyPart() AlertConfigRequest {
	return AlertConfigRequest{Project: request.Project, Environment: request.Environment, Policy: request.Policy, Targets: request.Targets}
}

type AlertAttemptFact struct {
	ID          string    `json:"id"`
	Project     string    `json:"project"`
	Environment string    `json:"environment"`
	Kind        string    `json:"kind"`
	Service     string    `json:"service"`
	Status      string    `json:"status"`
	StartedAt   time.Time `json:"startedAt"`
	FinishedAt  time.Time `json:"finishedAt"`
}

type AlertSubjectStatus struct {
	Kind                string          `json:"kind"`
	Service             string          `json:"service"`
	ConfiguredAt        time.Time       `json:"configuredAt"`
	LastAttemptAt       time.Time       `json:"lastAttemptAt,omitempty"`
	LastSuccessAt       time.Time       `json:"lastSuccessAt,omitempty"`
	LastStatus          string          `json:"lastStatus,omitempty"`
	LastAttemptID       string          `json:"lastAttemptId,omitempty"`
	ConsecutiveFailures int             `json:"consecutiveFailures,omitempty"`
	ConsecutiveTimeouts int             `json:"consecutiveTimeouts,omitempty"`
	ConsecutiveSkipped  int             `json:"consecutiveSkipped,omitempty"`
	Active              bool            `json:"active"`
	Reasons             []string        `json:"reasons,omitempty"`
	EpisodeID           string          `json:"episodeId,omitempty"`
	OpenedAt            time.Time       `json:"openedAt,omitempty"`
	OpenedProviders     map[string]bool `json:"openedProviders,omitempty"`
	OpenedGeneration    string          `json:"openedCredentialGeneration,omitempty"`
	Retiring            bool            `json:"retiring,omitempty"`
}

type AlertDeliveryStatus struct {
	ID                   string    `json:"id"`
	EpisodeID            string    `json:"episodeId"`
	Kind                 string    `json:"kind"`
	Service              string    `json:"service"`
	Transition           string    `json:"transition"`
	Provider             string    `json:"provider"`
	CredentialGeneration string    `json:"credentialGeneration,omitempty"`
	Reasons              []string  `json:"reasons,omitempty"`
	Attempts             int       `json:"attempts"`
	NextAttemptAt        time.Time `json:"nextAttemptAt,omitempty"`
	Exhausted            bool      `json:"exhausted,omitempty"`
	LastError            string    `json:"lastError,omitempty"`
	DispatchStarted      bool      `json:"dispatchStarted,omitempty"`
	inFlight             bool
}

type AlertStatusResponse struct {
	Project         string                `json:"project"`
	Environment     string                `json:"environment"`
	Configured      bool                  `json:"configured"`
	EvaluatedAt     time.Time             `json:"evaluatedAt,omitempty"`
	Subjects        []AlertSubjectStatus  `json:"subjects,omitempty"`
	Deliveries      []AlertDeliveryStatus `json:"deliveries,omitempty"`
	ActiveEpisodes  int                   `json:"activeEpisodes"`
	PendingDelivery int                   `json:"pendingDelivery"`
	Exhausted       int                   `json:"exhausted"`
	// Sources is the additive node-side monitoring status
	// (monitoring.health-v1). Targets are never included.
	Sources *SourceAlertStatus `json:"sources,omitempty"`
}

type alertScopeState struct {
	SchemaVersion int                            `json:"schemaVersion"`
	Project       string                         `json:"project"`
	Environment   string                         `json:"environment"`
	EvaluatedAt   time.Time                      `json:"evaluatedAt,omitempty"`
	Subjects      map[string]*AlertSubjectStatus `json:"subjects"`
	Deliveries    []AlertDeliveryStatus          `json:"deliveries,omitempty"`
	Retiring      bool                           `json:"retiring,omitempty"`
}

type alertPolicyDocument struct {
	SchemaVersion int         `json:"schemaVersion"`
	Project       string      `json:"project"`
	Environment   string      `json:"environment"`
	Policy        AlertPolicy `json:"policy"`
	Providers     []string    `json:"providers"`
	Generation    string      `json:"credentialGeneration"`
}

type alertCredentialsDocument struct {
	SchemaVersion int          `json:"schemaVersion"`
	Project       string       `json:"project"`
	Environment   string       `json:"environment"`
	Generation    string       `json:"generation"`
	Targets       AlertTargets `json:"targets"`
}

type AlertManager struct {
	dataDir            string
	client             *http.Client
	now                func() time.Time
	deliver            func(context.Context, string, string, any) alertDeliveryResult
	cleanupCredentials func(string, string, string) error
	node               string

	mu                   sync.Mutex
	configs              map[string]AlertConfigRequest
	states               map[string]*alertScopeState
	configGenerations    map[string]string
	protectedGenerations map[string]map[string]bool
}

func NewAlertManager(dataDir string) *AlertManager {
	manager := &AlertManager{
		dataDir:              dataDir,
		now:                  func() time.Time { return time.Now().UTC() },
		configs:              make(map[string]AlertConfigRequest),
		states:               make(map[string]*alertScopeState),
		configGenerations:    make(map[string]string),
		protectedGenerations: make(map[string]map[string]bool),
	}
	manager.client = newAlertHTTPClient()
	manager.deliver = manager.deliverHTTP
	manager.cleanupCredentials = manager.removeStaleCredentials
	return manager
}

func (m *AlertManager) Load() error {
	return m.LoadContext(context.Background())
}

func (m *AlertManager) LoadContext(ctx context.Context) error {
	unlock, err := recovery.AcquireMutationLockContext(ctx, m.dataDir)
	if err != nil {
		return err
	}
	defer unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := validateStorageTreeNoSymlink(filepath.Join(m.dataDir, schedulerAlertsDirName), "scheduler alert storage"); err != nil {
		return err
	}
	if err := m.loadConfigsLocked(); err != nil {
		return err
	}
	if err := m.cleanupOrphanCredentialsLocked(); err != nil {
		return err
	}
	if err := m.loadStatesLocked(); err != nil {
		return err
	}
	if err := m.reconcileLoadedStatesLocked(); err != nil {
		return err
	}
	for key := range m.configs {
		project, environment, _ := strings.Cut(key, "/")
		if err := m.removeStaleCredentials(project, environment, m.configGenerations[key]); err != nil {
			return err
		}
	}
	return nil
}

func (m *AlertManager) Run(ctx context.Context) {
	evaluateTicker := time.NewTicker(30 * time.Second)
	deliveryTicker := time.NewTicker(time.Second)
	defer evaluateTicker.Stop()
	defer deliveryTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-evaluateTicker.C:
			if err := m.EvaluateContext(ctx); err != nil && ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "takod scheduler alert evaluation failed: %v\n", err)
			}
		case <-deliveryTicker.C:
			m.DispatchDue(ctx)
		}
	}
}

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		project := r.URL.Query().Get("project")
		environment := r.URL.Query().Get("environment")
		response, err := s.alertManager.Status(project, environment)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		response.Sources = s.sourceStatusFor(project, environment)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	case http.MethodPut:
		defer r.Body.Close()
		var request AlertConfigRequest
		if err := decodeJSONRequest(w, r, &request); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		request = normalizeAlertConfigRequest(request)
		if err := validateAlertConfigRequest(request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !alertConfigEnabled(request) {
			if err := s.alertManager.RemoveConfig(request.Project, request.Environment); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if err := s.sourceAlerts.RemoveScope(request.Project, request.Environment); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(AlertStatusResponse{Project: request.Project, Environment: request.Environment})
			return
		}
		if !s.requireFreeDisk(w, s.dataDir) {
			return
		}
		// Node-side monitoring sections go to the source manager; an
		// absent section set clears the scope's source tree (last deploy
		// wins, also for older CLIs).
		if err := s.sourceAlerts.ApplyConfig(request); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !alertPolicyEnabled(request.Policy) {
			if err := s.alertManager.RemoveConfig(request.Project, request.Environment); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			response, _ := s.alertManager.Status(request.Project, request.Environment)
			response.Sources = s.sourceStatusFor(request.Project, request.Environment)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response)
			return
		}
		jobSpecs := s.jobScheduler.alertSpecs(request.Project, request.Environment)
		backupSchedules := s.backupScheduler.alertSchedules(request.Project, request.Environment)
		jobs := make([]string, 0, len(jobSpecs))
		for _, spec := range jobSpecs {
			jobs = append(jobs, spec.Name)
		}
		backups := make([]string, 0, len(backupSchedules))
		for _, schedule := range backupSchedules {
			backups = append(backups, schedule.Service)
		}
		if err := s.alertManager.ApplyConfigForSubjects(request, jobs, backups); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, spec := range jobSpecs {
			if err := s.jobScheduler.syncAlertSubject(spec); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		for _, schedule := range backupSchedules {
			if err := s.backupScheduler.syncAlertSubject(schedule); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		response, _ := s.alertManager.Status(request.Project, request.Environment)
		response.Sources = s.sourceStatusFor(request.Project, request.Environment)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (m *AlertManager) ApplyConfig(request AlertConfigRequest) error {
	unlock, err := recovery.AcquireMutationLockContext(context.Background(), m.dataDir)
	if err != nil {
		return err
	}
	defer unlock()
	request = normalizeAlertConfigRequest(request).legacyPart()
	if err := validateAlertConfigRequest(request); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := alertScopeKey(request.Project, request.Environment)
	state := m.states[key]
	if state == nil {
		state = newAlertScopeState(request.Project, request.Environment)
	}
	generation, err := m.persistConfigLocked(request)
	if err != nil {
		return err
	}
	m.configs[key] = request
	m.configGenerations[key] = generation
	m.states[key] = state
	state.Retiring = false
	m.resetCredentialStateLocked(state, generation)
	for _, subject := range state.Subjects {
		subject.Retiring = false
		m.reconcileSubjectLocked(request, state, subject, m.now())
	}
	if err := m.persistStateLocked(state); err != nil {
		return err
	}
	delete(m.protectedGenerations, key)
	return m.cleanupCredentials(request.Project, request.Environment, generation)
}

func (m *AlertManager) RemoveConfig(project, environment string) error {
	unlock, lockErr := recovery.AcquireMutationLockContext(context.Background(), m.dataDir)
	if lockErr != nil {
		return lockErr
	}
	defer unlock()
	if !isSafeProjectName(project) || !isSafeRuntimeName(environment) {
		return fmt.Errorf("invalid alert scope")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := alertScopeKey(project, environment)
	config, configured := m.configs[key]
	if !configured {
		return m.removeConfigFilesLocked(project, environment)
	}
	config.Policy = AlertPolicy{}
	generation, err := m.persistConfigLocked(config)
	if err != nil {
		return err
	}
	m.configs[key] = config
	m.configGenerations[key] = generation
	state := m.stateLocked(project, environment)
	state.Retiring = true
	for _, subject := range state.Subjects {
		m.retireSubjectLocked(state, subject)
	}
	if m.finalizeRetiringSubjectsLocked(state) {
		return m.removeConfigFilesLocked(project, environment)
	}
	return m.persistStateLocked(state)
}

func (m *AlertManager) RegisterSubject(project, environment, kind, service string) error {
	unlock, lockErr := recovery.AcquireMutationLockContext(context.Background(), m.dataDir)
	if lockErr != nil {
		return lockErr
	}
	defer unlock()
	if !validAlertSubject(project, environment, kind, service) {
		return fmt.Errorf("invalid alert subject")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := alertScopeKey(project, environment)
	if _, configured := m.configs[key]; !configured {
		return nil
	}
	state := m.stateLocked(project, environment)
	if state.Retiring {
		return nil
	}
	subjectKey := alertSubjectKey(kind, service)
	if _, exists := state.Subjects[subjectKey]; !exists {
		state.Subjects[subjectKey] = &AlertSubjectStatus{Kind: kind, Service: service, ConfiguredAt: m.now(), OpenedProviders: map[string]bool{}}
	}
	return m.persistStateLocked(state)
}

func (m *AlertManager) ReconcileSubjects(project, environment string, jobs, backups []string) error {
	unlock, lockErr := recovery.AcquireMutationLockContext(context.Background(), m.dataDir)
	if lockErr != nil {
		return lockErr
	}
	defer unlock()
	if !isSafeProjectName(project) || !isSafeRuntimeName(environment) {
		return fmt.Errorf("invalid alert scope")
	}
	desired, err := alertDesiredSubjects(jobs, backups)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := alertScopeKey(project, environment)
	if _, configured := m.configs[key]; !configured {
		return nil
	}
	state := m.stateLocked(project, environment)
	for key := range desired {
		if state.Subjects[key] != nil {
			continue
		}
		kind, service, _ := strings.Cut(key, "/")
		state.Subjects[key] = &AlertSubjectStatus{Kind: kind, Service: service, ConfiguredAt: m.now(), OpenedProviders: map[string]bool{}}
	}
	for key := range state.Subjects {
		if _, keep := desired[key]; keep {
			continue
		}
		m.retireSubjectLocked(state, state.Subjects[key])
	}
	m.finalizeRetiringSubjectsLocked(state)
	return m.persistStateLocked(state)
}

func (m *AlertManager) ApplyConfigForSubjects(request AlertConfigRequest, jobs, backups []string) error {
	unlock, lockErr := recovery.AcquireMutationLockContext(context.Background(), m.dataDir)
	if lockErr != nil {
		return lockErr
	}
	defer unlock()
	request = normalizeAlertConfigRequest(request).legacyPart()
	if err := validateAlertConfigRequest(request); err != nil {
		return err
	}
	desired, err := alertDesiredSubjects(jobs, backups)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := alertScopeKey(request.Project, request.Environment)
	state := m.states[key]
	if state == nil {
		state = newAlertScopeState(request.Project, request.Environment)
	}
	generation, err := m.persistConfigLocked(request)
	if err != nil {
		return err
	}
	m.configs[key] = request
	m.configGenerations[key] = generation
	m.states[key] = state
	state.Retiring = false
	m.resetCredentialStateLocked(state, generation)
	for subjectKey := range desired {
		if state.Subjects[subjectKey] == nil {
			kind, service, _ := strings.Cut(subjectKey, "/")
			state.Subjects[subjectKey] = &AlertSubjectStatus{Kind: kind, Service: service, ConfiguredAt: m.now(), OpenedProviders: map[string]bool{}}
		}
		state.Subjects[subjectKey].Retiring = false
	}
	for subjectKey := range state.Subjects {
		if _, keep := desired[subjectKey]; keep {
			continue
		}
		m.retireSubjectLocked(state, state.Subjects[subjectKey])
	}
	now := m.now()
	for _, subject := range state.Subjects {
		if subject.Retiring {
			m.retireSubjectLocked(state, subject)
		} else {
			m.reconcileSubjectLocked(request, state, subject, now)
		}
	}
	m.finalizeRetiringSubjectsLocked(state)
	state.EvaluatedAt = now
	if err := m.persistStateLocked(state); err != nil {
		return err
	}
	delete(m.protectedGenerations, key)
	return m.cleanupCredentials(request.Project, request.Environment, generation)
}

func alertDesiredSubjects(jobs, backups []string) (map[string]struct{}, error) {
	desired := make(map[string]struct{}, len(jobs)+len(backups))
	for _, service := range jobs {
		if !isSafeServiceName(service) {
			return nil, fmt.Errorf("invalid job alert subject")
		}
		desired[alertSubjectKey("job", service)] = struct{}{}
	}
	for _, service := range backups {
		if !isSafeServiceName(service) {
			return nil, fmt.Errorf("invalid backup alert subject")
		}
		desired[alertSubjectKey("backup", service)] = struct{}{}
	}
	return desired, nil
}

func (m *AlertManager) RemoveSubject(project, environment, kind, service string) error {
	unlock, lockErr := recovery.AcquireMutationLockContext(context.Background(), m.dataDir)
	if lockErr != nil {
		return lockErr
	}
	defer unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.states[alertScopeKey(project, environment)]
	if state == nil {
		return nil
	}
	subject := state.Subjects[alertSubjectKey(kind, service)]
	if subject == nil {
		return nil
	}
	m.retireSubjectLocked(state, subject)
	m.finalizeRetiringSubjectsLocked(state)
	return m.persistStateLocked(state)
}

func (m *AlertManager) Observe(fact AlertAttemptFact) error {
	unlock, lockErr := recovery.AcquireMutationLockContext(context.Background(), m.dataDir)
	if lockErr != nil {
		return lockErr
	}
	defer unlock()
	if !validAlertSubject(fact.Project, fact.Environment, fact.Kind, fact.Service) || fact.ID == "" || len(fact.ID) > 128 || hasControlChars(fact.ID) || fact.StartedAt.IsZero() || fact.FinishedAt.IsZero() || fact.StartedAt.After(fact.FinishedAt) || !validAlertAttemptStatus(fact.Status) {
		return fmt.Errorf("invalid alert attempt fact")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	config, configured := m.configs[alertScopeKey(fact.Project, fact.Environment)]
	if !configured {
		return nil
	}
	state := m.stateLocked(fact.Project, fact.Environment)
	key := alertSubjectKey(fact.Kind, fact.Service)
	subject := state.Subjects[key]
	if subject == nil || subject.Retiring || state.Retiring {
		return nil
	}
	if fact.ID == subject.LastAttemptID || fact.FinishedAt.Before(subject.LastAttemptAt) || (fact.FinishedAt.Equal(subject.LastAttemptAt) && subject.LastAttemptID != "" && fact.ID < subject.LastAttemptID) {
		return nil
	}
	subject.LastAttemptAt = fact.FinishedAt.UTC()
	subject.LastStatus = fact.Status
	subject.LastAttemptID = fact.ID
	if fact.Status == JobRunStatusSucceeded {
		subject.LastSuccessAt = fact.FinishedAt.UTC()
		subject.ConsecutiveFailures = 0
		subject.ConsecutiveTimeouts = 0
		subject.ConsecutiveSkipped = 0
	} else {
		switch fact.Status {
		case JobRunStatusFailed:
			subject.ConsecutiveFailures++
			subject.ConsecutiveTimeouts = 0
			subject.ConsecutiveSkipped = 0
		case JobRunStatusTimeout:
			subject.ConsecutiveTimeouts++
			subject.ConsecutiveFailures = 0
			subject.ConsecutiveSkipped = 0
		case JobRunStatusSkipped:
			subject.ConsecutiveSkipped++
			subject.ConsecutiveFailures = 0
			subject.ConsecutiveTimeouts = 0
		}
	}
	m.reconcileSubjectLocked(config, state, subject, fact.FinishedAt.UTC())
	state.EvaluatedAt = m.now()
	return m.persistStateLocked(state)
}

func (m *AlertManager) Evaluate() error {
	return m.EvaluateContext(context.Background())
}

func (m *AlertManager) EvaluateContext(ctx context.Context) error {
	unlock, err := recovery.AcquireMutationLockContext(ctx, m.dataDir)
	if err != nil {
		return err
	}
	defer unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for key, state := range m.states {
		config, ok := m.configs[key]
		if !ok {
			continue
		}
		for _, subject := range state.Subjects {
			if subject.Retiring || state.Retiring {
				m.retireSubjectLocked(state, subject)
			} else {
				m.reconcileSubjectLocked(config, state, subject, now)
			}
		}
		state.EvaluatedAt = now
		if m.finalizeRetiringSubjectsLocked(state) {
			if err := m.removeConfigFilesLocked(state.Project, state.Environment); err != nil {
				return err
			}
			continue
		}
		if err := m.persistStateLocked(state); err != nil {
			return err
		}
		if err := m.cleanupCredentials(state.Project, state.Environment, m.configGenerations[key]); err != nil {
			return err
		}
	}
	return nil
}

func (m *AlertManager) Status(project, environment string) (AlertStatusResponse, error) {
	if !isSafeProjectName(project) || !isSafeRuntimeName(environment) {
		return AlertStatusResponse{}, fmt.Errorf("invalid alert scope")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	response := AlertStatusResponse{Project: project, Environment: environment}
	_, response.Configured = m.configs[alertScopeKey(project, environment)]
	state := m.states[alertScopeKey(project, environment)]
	if state == nil {
		return response, nil
	}
	response.Configured = response.Configured && !state.Retiring
	response.EvaluatedAt = state.EvaluatedAt
	for _, subject := range state.Subjects {
		copy := *subject
		copy.Reasons = append([]string(nil), subject.Reasons...)
		copy.OpenedProviders = nil
		copy.OpenedGeneration = ""
		response.Subjects = append(response.Subjects, copy)
		if subject.Active {
			response.ActiveEpisodes++
		}
	}
	sort.Slice(response.Subjects, func(i, j int) bool {
		if response.Subjects[i].Kind == response.Subjects[j].Kind {
			return response.Subjects[i].Service < response.Subjects[j].Service
		}
		return response.Subjects[i].Kind < response.Subjects[j].Kind
	})
	response.Deliveries = append([]AlertDeliveryStatus(nil), state.Deliveries...)
	for index := range response.Deliveries {
		response.Deliveries[index].Reasons = append([]string(nil), response.Deliveries[index].Reasons...)
		response.Deliveries[index].CredentialGeneration = ""
	}
	for _, delivery := range response.Deliveries {
		if delivery.Exhausted {
			response.Exhausted++
		} else {
			response.PendingDelivery++
		}
	}
	return response, nil
}

func (m *AlertManager) StatusAll() []AlertStatusResponse {
	m.mu.Lock()
	keys := make([]string, 0, len(m.states))
	for key := range m.states {
		keys = append(keys, key)
	}
	m.mu.Unlock()
	sort.Strings(keys)
	responses := make([]AlertStatusResponse, 0, len(keys))
	for _, key := range keys {
		project, environment, ok := strings.Cut(key, "/")
		if !ok {
			continue
		}
		response, err := m.Status(project, environment)
		if err == nil {
			responses = append(responses, response)
		}
	}
	return responses
}

func (m *AlertManager) DispatchDue(ctx context.Context) {
	for {
		unlockMutation, err := recovery.AcquireMutationLockContext(ctx, m.dataDir)
		if err != nil {
			return
		}
		m.mu.Lock()
		now := m.now()
		var scope string
		var index = -1
		var delivery AlertDeliveryStatus
		var config AlertConfigRequest
		for key, state := range m.states {
			candidateConfig, ok := m.configs[key]
			if !ok {
				continue
			}
			for candidateIndex, candidate := range state.Deliveries {
				if !candidate.Exhausted && !candidate.inFlight && !candidate.NextAttemptAt.After(now) {
					scope, index, delivery, config = key, candidateIndex, candidate, candidateConfig
					break
				}
			}
			if index >= 0 {
				break
			}
		}
		if index < 0 {
			m.mu.Unlock()
			unlockMutation()
			return
		}
		state := m.states[scope]
		credentials, err := m.readCredentials(state.Project, state.Environment, delivery.CredentialGeneration)
		if err != nil {
			state.Deliveries[index].LastError = "credentials_unavailable"
			state.Deliveries[index].Exhausted = true
			_ = m.persistStateLocked(state)
			m.mu.Unlock()
			unlockMutation()
			continue
		}
		endpoint := alertTargetForProvider(credentials.Targets, delivery.Provider)
		payload := m.deliveryPayloadLocked(config, delivery)
		state.Deliveries[index].inFlight = true
		state.Deliveries[index].DispatchStarted = true
		if err := m.persistStateLocked(state); err != nil {
			state.Deliveries[index].inFlight = false
			fmt.Fprintf(os.Stderr, "takod scheduler alert dispatch marker persistence failed: %v\n", err)
			m.mu.Unlock()
			unlockMutation()
			return
		}
		m.mu.Unlock()
		unlockMutation()

		result := m.deliver(ctx, delivery.Provider, endpoint, payload)

		unlockMutation, err = recovery.AcquireMutationLockContext(ctx, m.dataDir)
		if err != nil {
			return
		}
		m.mu.Lock()
		state = m.states[scope]
		index = alertDeliveryIndex(state, delivery.ID)
		if index < 0 {
			m.mu.Unlock()
			unlockMutation()
			continue
		}
		current := &state.Deliveries[index]
		current.inFlight = false
		if result.Interrupted || ctx.Err() != nil {
			current.LastError = "interrupted"
			if err := m.persistStateLocked(state); err != nil {
				fmt.Fprintf(os.Stderr, "takod scheduler alert delivery state persistence failed: %v\n", err)
			}
			m.mu.Unlock()
			unlockMutation()
			return
		}
		current.Attempts++
		current.LastError = result.Error
		if result.Delivered {
			convertedToRecovery := false
			if current.Transition == "opening" {
				if subject := state.Subjects[alertSubjectKey(current.Kind, current.Service)]; subject != nil && subject.EpisodeID == current.EpisodeID && subject.Active && !subject.Retiring && current.CredentialGeneration == m.configGenerations[scope] {
					if subject.OpenedProviders == nil {
						subject.OpenedProviders = map[string]bool{}
					}
					subject.OpenedProviders[current.Provider] = true
					subject.OpenedGeneration = current.CredentialGeneration
				} else {
					*current = alertRecoveryDelivery(*current, m.now())
					convertedToRecovery = true
				}
			}
			if !convertedToRecovery {
				state.Deliveries = append(state.Deliveries[:index], state.Deliveries[index+1:]...)
			}
		} else if !result.Retryable || current.Attempts >= alertDeliveryMaxAttempts {
			current.Exhausted = true
		} else {
			delays := []time.Duration{time.Second, 5 * time.Second}
			current.NextAttemptAt = m.now().Add(delays[current.Attempts-1])
		}
		if m.finalizeRetiringSubjectsLocked(state) {
			if err := m.removeConfigFilesLocked(state.Project, state.Environment); err != nil {
				fmt.Fprintf(os.Stderr, "takod scheduler alert retirement cleanup failed: %v\n", err)
			}
			m.mu.Unlock()
			unlockMutation()
			continue
		}
		if err := m.persistStateLocked(state); err != nil {
			fmt.Fprintf(os.Stderr, "takod scheduler alert delivery state persistence failed: %v\n", err)
		} else if err := m.cleanupCredentials(state.Project, state.Environment, m.configGenerations[scope]); err != nil {
			fmt.Fprintf(os.Stderr, "takod scheduler alert credential cleanup failed: %v\n", err)
		}
		m.mu.Unlock()
		unlockMutation()
	}
}

func alertDeliveryIndex(state *alertScopeState, id string) int {
	if state == nil {
		return -1
	}
	for index := range state.Deliveries {
		if state.Deliveries[index].ID == id {
			return index
		}
	}
	return -1
}

func (m *AlertManager) reconcileSubjectLocked(config AlertConfigRequest, state *alertScopeState, subject *AlertSubjectStatus, now time.Time) {
	reasons := alertReasons(config.Policy, subject, now)
	if len(reasons) > 0 && !subject.Active {
		subject.Active = true
		subject.Reasons = reasons
		subject.EpisodeID = newAlertID()
		subject.OpenedAt = now
		subject.OpenedProviders = map[string]bool{}
		subject.OpenedGeneration = ""
		m.queueTransitionLocked(state, subject, "opening", configuredAlertProviders(config.Targets))
		return
	}
	if len(reasons) > 0 {
		subject.Reasons = reasons
		m.queueMissingOpeningsLocked(config, state, subject)
		return
	}
	if !subject.Active {
		return
	}
	currentGeneration := m.configGenerations[alertScopeKey(state.Project, state.Environment)]
	if subject.OpenedGeneration != "" && subject.OpenedGeneration != currentGeneration {
		if !m.queueOpenedGenerationRecoveryLocked(state, subject) {
			return
		}
		subject.OpenedProviders = map[string]bool{}
		subject.OpenedGeneration = ""
	}
	providers := make([]string, 0, len(subject.OpenedProviders))
	if subject.OpenedGeneration == currentGeneration {
		for provider := range subject.OpenedProviders {
			providers = append(providers, provider)
		}
	}
	filtered := state.Deliveries[:0]
	for _, delivery := range state.Deliveries {
		if delivery.EpisodeID != subject.EpisodeID || delivery.Transition != "opening" || delivery.inFlight {
			filtered = append(filtered, delivery)
		}
	}
	state.Deliveries = filtered
	if !m.queueTransitionLocked(state, subject, "recovery", providers) {
		return
	}
	subject.OpenedProviders = map[string]bool{}
	subject.OpenedGeneration = ""
	subject.Active = false
	subject.Reasons = nil
	subject.EpisodeID = ""
	subject.OpenedAt = time.Time{}
}

func alertRecoveryDelivery(opening AlertDeliveryStatus, now time.Time) AlertDeliveryStatus {
	return AlertDeliveryStatus{
		ID: newAlertID(), EpisodeID: opening.EpisodeID, Kind: opening.Kind, Service: opening.Service,
		Transition: "recovery", Provider: opening.Provider, CredentialGeneration: opening.CredentialGeneration,
		Reasons: append([]string(nil), opening.Reasons...), NextAttemptAt: now,
	}
}

func (m *AlertManager) queueTransitionLocked(state *alertScopeState, subject *AlertSubjectStatus, transition string, providers []string) bool {
	sort.Strings(providers)
	allQueued := true
	for _, provider := range providers {
		duplicate := false
		for _, delivery := range state.Deliveries {
			if delivery.EpisodeID == subject.EpisodeID && delivery.Transition == transition && delivery.Provider == provider {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		delivery := AlertDeliveryStatus{
			ID: newAlertID(), EpisodeID: subject.EpisodeID, Kind: subject.Kind, Service: subject.Service,
			Transition: transition, Provider: provider, CredentialGeneration: m.configGenerations[alertScopeKey(state.Project, state.Environment)], Reasons: append([]string(nil), subject.Reasons...), NextAttemptAt: m.now(),
		}
		if !appendAlertDelivery(state, delivery) {
			allQueued = false
		}
	}
	return allQueued
}

func appendAlertDelivery(state *alertScopeState, delivery AlertDeliveryStatus) bool {
	if len(state.Deliveries) >= maxAlertDeliveries {
		for index, existing := range state.Deliveries {
			if existing.Exhausted && !existing.inFlight {
				state.Deliveries = append(state.Deliveries[:index], state.Deliveries[index+1:]...)
				break
			}
		}
	}
	if len(state.Deliveries) >= maxAlertDeliveries {
		return false
	}
	state.Deliveries = append(state.Deliveries, delivery)
	return true
}

func (m *AlertManager) resetCredentialStateLocked(state *alertScopeState, generation string) {
	for _, subject := range state.Subjects {
		if subject.OpenedGeneration != "" && subject.OpenedGeneration != generation {
			if m.queueOpenedGenerationRecoveryLocked(state, subject) {
				subject.OpenedProviders = map[string]bool{}
				subject.OpenedGeneration = ""
			}
		}
	}
	filtered := state.Deliveries[:0]
	for _, delivery := range state.Deliveries {
		if delivery.CredentialGeneration != generation && delivery.Transition == "opening" && !delivery.DispatchStarted && !delivery.inFlight {
			continue
		}
		filtered = append(filtered, delivery)
	}
	state.Deliveries = filtered
}

func (m *AlertManager) queueOpenedGenerationRecoveryLocked(state *alertScopeState, subject *AlertSubjectStatus) bool {
	allQueued := true
	for provider := range subject.OpenedProviders {
		duplicate := false
		for _, delivery := range state.Deliveries {
			if delivery.EpisodeID == subject.EpisodeID && delivery.Transition == "recovery" && delivery.Provider == provider && delivery.CredentialGeneration == subject.OpenedGeneration {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		if !appendAlertDelivery(state, AlertDeliveryStatus{
			ID: newAlertID(), EpisodeID: subject.EpisodeID, Kind: subject.Kind, Service: subject.Service,
			Transition: "recovery", Provider: provider, CredentialGeneration: subject.OpenedGeneration,
			Reasons: append([]string(nil), subject.Reasons...), NextAttemptAt: m.now(),
		}) {
			allQueued = false
		}
	}
	return allQueued
}

func (m *AlertManager) queueMissingOpeningsLocked(config AlertConfigRequest, state *alertScopeState, subject *AlertSubjectStatus) {
	generation := m.configGenerations[alertScopeKey(state.Project, state.Environment)]
	if subject.OpenedGeneration != "" && subject.OpenedGeneration != generation {
		if !m.queueOpenedGenerationRecoveryLocked(state, subject) {
			return
		}
		subject.OpenedProviders = map[string]bool{}
		subject.OpenedGeneration = ""
	}
	pending := make(map[string]bool)
	for _, delivery := range state.Deliveries {
		if delivery.EpisodeID == subject.EpisodeID && delivery.Transition == "opening" && delivery.CredentialGeneration == generation {
			pending[delivery.Provider] = true
		}
	}
	var providers []string
	for _, provider := range configuredAlertProviders(config.Targets) {
		opened := subject.OpenedGeneration == generation && subject.OpenedProviders[provider]
		if !opened && !pending[provider] {
			providers = append(providers, provider)
		}
	}
	m.queueTransitionLocked(state, subject, "opening", providers)
}

func alertReasons(policy AlertPolicy, subject *AlertSubjectStatus, now time.Time) []string {
	var reasons []string
	if subject.Kind == "job" {
		if policy.JobConsecutiveFailures > 0 && subject.ConsecutiveFailures >= policy.JobConsecutiveFailures {
			reasons = append(reasons, "consecutive_failures")
		}
		if policy.JobConsecutiveTimeouts > 0 && subject.ConsecutiveTimeouts >= policy.JobConsecutiveTimeouts {
			reasons = append(reasons, "consecutive_timeouts")
		}
		if policy.JobConsecutiveSkipped > 0 && subject.ConsecutiveSkipped >= policy.JobConsecutiveSkipped {
			reasons = append(reasons, "consecutive_skipped")
		}
		if policy.JobNoSuccessSeconds > 0 {
			baseline := subject.LastSuccessAt
			if baseline.IsZero() {
				baseline = subject.ConfiguredAt
			}
			if !baseline.IsZero() && now.Sub(baseline) >= time.Duration(policy.JobNoSuccessSeconds)*time.Second {
				reasons = append(reasons, "no_success")
			}
		}
	} else if subject.Kind == "backup" && policy.BackupMaxAgeSeconds > 0 {
		baseline := subject.LastSuccessAt
		if baseline.IsZero() {
			baseline = subject.ConfiguredAt
		}
		if !baseline.IsZero() && now.Sub(baseline) >= time.Duration(policy.BackupMaxAgeSeconds)*time.Second {
			reasons = append(reasons, "backup_stale")
		}
	}
	sort.Strings(reasons)
	return reasons
}

// deliveryPayloadLocked builds the schema v1 payload for a job or backup
// delivery. The message text is unchanged from earlier releases; the v1 keys
// (schemaVersion, event, node, severity, subject, details, openedAt) are
// additive.
func (m *AlertManager) deliveryPayloadLocked(config AlertConfigRequest, delivery AlertDeliveryStatus) any {
	message := fmt.Sprintf("Tako alert %s for %s/%s %s %s", delivery.Transition, config.Project, config.Environment, delivery.Kind, delivery.Service)
	if len(delivery.Reasons) > 0 {
		message += ": " + strings.Join(delivery.Reasons, ", ")
	}
	var openedAt time.Time
	if state := m.states[alertScopeKey(config.Project, config.Environment)]; state != nil {
		if subject := state.Subjects[alertSubjectKey(delivery.Kind, delivery.Service)]; subject != nil && subject.EpisodeID == delivery.EpisodeID {
			openedAt = subject.OpenedAt
		}
	}
	return alertPayloadV1{
		EventID: delivery.ID, EpisodeID: delivery.EpisodeID, Transition: delivery.Transition,
		Event: schedulerAlertEvent(delivery.Kind), Project: config.Project, Environment: config.Environment,
		Node: m.node, Kind: delivery.Kind, Subject: delivery.Service, Severity: string(SeverityCritical),
		Reasons: append([]string(nil), delivery.Reasons...), OpenedAt: openedAt, Message: message, Timestamp: m.now(),
	}.Map()
}

// SetNodeName sets the node name carried in alert payloads.
func (m *AlertManager) SetNodeName(node string) {
	m.mu.Lock()
	m.node = node
	m.mu.Unlock()
}

type alertDeliveryResult struct {
	Delivered   bool
	Retryable   bool
	Interrupted bool
	Error       string
}

func (m *AlertManager) deliverHTTP(ctx context.Context, provider, endpoint string, payload any) alertDeliveryResult {
	body, ok := payload.(map[string]any)
	if !ok {
		return alertDeliveryResult{Error: "encode"}
	}
	return deliverAlertHTTP(ctx, m.client, provider, endpoint, body)
}

func newAlertHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := newAlertHTTPTransport(net.DefaultResolver.LookupIPAddr, dialer.DialContext)
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func newAlertHTTPTransport(lookup func(context.Context, string) ([]net.IPAddr, error), dial func(context.Context, string, string) (net.Conn, error)) *http.Transport {
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, fmt.Errorf("invalid alert target address")
			}
			addresses, err := lookup(ctx, host)
			if err != nil || len(addresses) == 0 {
				return nil, fmt.Errorf("resolve alert target")
			}
			for _, candidate := range addresses {
				if !isPublicAlertIP(candidate.IP) {
					return nil, fmt.Errorf("alert target resolved to a non-public address")
				}
			}
			var dialErr error
			for _, candidate := range addresses {
				connection, err := dial(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
				if err == nil {
					return connection, nil
				}
				dialErr = err
			}
			return nil, fmt.Errorf("dial alert target: %w", dialErr)
		},
	}
}

func isPublicAlertIP(ip net.IP) bool {
	return alerttarget.IsPublicIP(ip)
}

func validateAlertConfigRequest(request AlertConfigRequest) error {
	if !isSafeProjectName(request.Project) || !isSafeRuntimeName(request.Environment) {
		return fmt.Errorf("invalid alert scope")
	}
	if request.Policy.JobConsecutiveFailures < 0 || request.Policy.JobConsecutiveFailures > 1000 ||
		request.Policy.JobConsecutiveTimeouts < 0 || request.Policy.JobConsecutiveTimeouts > 1000 ||
		request.Policy.JobConsecutiveSkipped < 0 || request.Policy.JobConsecutiveSkipped > 1000 ||
		request.Policy.JobNoSuccessSeconds < 0 || request.Policy.JobNoSuccessSeconds > int64((365*24*time.Hour)/time.Second) ||
		request.Policy.BackupMaxAgeSeconds < 0 || request.Policy.BackupMaxAgeSeconds > int64((365*24*time.Hour)/time.Second) {
		return fmt.Errorf("invalid alert policy")
	}
	for _, duration := range []int64{request.Policy.JobNoSuccessSeconds, request.Policy.BackupMaxAgeSeconds} {
		if duration > 0 && duration < 60 {
			return fmt.Errorf("alert durations must be zero or at least 60 seconds")
		}
	}
	enabled := alertPolicyEnabled(request.Policy)
	providers := configuredAlertProviders(request.Targets)
	if enabled && len(providers) == 0 {
		return fmt.Errorf("enabled alert policy requires a target")
	}
	for _, provider := range providers {
		endpoint := alertTargetForProvider(request.Targets, provider)
		if normalized, err := alerttarget.NormalizeAndValidate(provider, endpoint); err != nil || normalized != endpoint {
			return fmt.Errorf("invalid %s alert target", provider)
		}
	}
	return validateSourceAlertRequest(request)
}

func normalizeAlertConfigRequest(request AlertConfigRequest) AlertConfigRequest {
	request.Targets.Slack = strings.TrimSpace(request.Targets.Slack)
	request.Targets.Discord = strings.TrimSpace(request.Targets.Discord)
	request.Targets.Webhook = strings.TrimSpace(request.Targets.Webhook)
	request.ServiceTargets = normalizeServiceTargets(request.ServiceTargets)
	return request
}

func alertPolicyEnabled(policy AlertPolicy) bool {
	return policy.JobConsecutiveFailures > 0 || policy.JobConsecutiveTimeouts > 0 || policy.JobConsecutiveSkipped > 0 || policy.JobNoSuccessSeconds > 0 || policy.BackupMaxAgeSeconds > 0
}

func validAlertSubject(project, environment, kind, service string) bool {
	return isSafeProjectName(project) && isSafeRuntimeName(environment) && (kind == "job" || kind == "backup") && isSafeRuntimeName(service)
}

func validAlertAttemptStatus(status string) bool {
	return status == JobRunStatusSucceeded || status == JobRunStatusFailed || status == JobRunStatusTimeout || status == JobRunStatusSkipped
}

func validAlertProvider(provider string) bool {
	return provider == "slack" || provider == "discord" || provider == "webhook"
}

func validAlertReason(reason string) bool {
	switch reason {
	case "consecutive_failures", "consecutive_timeouts", "consecutive_skipped", "no_success", "backup_stale":
		return true
	default:
		return false
	}
}

func (m *AlertManager) retireSubjectLocked(state *alertScopeState, subject *AlertSubjectStatus) {
	if subject == nil {
		return
	}
	subject.Retiring = true
	filtered := state.Deliveries[:0]
	for _, delivery := range state.Deliveries {
		if delivery.Kind == subject.Kind && delivery.Service == subject.Service && delivery.Transition == "opening" && !delivery.inFlight && !delivery.DispatchStarted {
			continue
		}
		filtered = append(filtered, delivery)
	}
	state.Deliveries = filtered
	if !subject.Active {
		return
	}
	currentGeneration := m.configGenerations[alertScopeKey(state.Project, state.Environment)]
	if subject.OpenedGeneration != "" && subject.OpenedGeneration != currentGeneration {
		if !m.queueOpenedGenerationRecoveryLocked(state, subject) {
			return
		}
		subject.OpenedProviders = map[string]bool{}
		subject.OpenedGeneration = ""
	}
	var providers []string
	if subject.OpenedGeneration == currentGeneration {
		for provider := range subject.OpenedProviders {
			providers = append(providers, provider)
		}
	}
	if !m.queueTransitionLocked(state, subject, "recovery", providers) {
		return
	}
	subject.OpenedProviders = map[string]bool{}
	subject.OpenedGeneration = ""
	subject.Active = false
	subject.EpisodeID = ""
	subject.OpenedAt = time.Time{}
}

func (m *AlertManager) finalizeRetiringSubjectsLocked(state *alertScopeState) bool {
	filtered := state.Deliveries[:0]
	for _, delivery := range state.Deliveries {
		subject := state.Subjects[alertSubjectKey(delivery.Kind, delivery.Service)]
		if subject != nil && subject.Retiring && delivery.Exhausted && !delivery.inFlight {
			continue
		}
		filtered = append(filtered, delivery)
	}
	state.Deliveries = filtered
	for key, subject := range state.Subjects {
		if subject == nil || !subject.Retiring || subject.Active {
			continue
		}
		pending := false
		for _, delivery := range state.Deliveries {
			if alertSubjectKey(delivery.Kind, delivery.Service) == key {
				pending = true
				break
			}
		}
		if !pending {
			delete(state.Subjects, key)
		}
	}
	return state.Retiring && len(state.Subjects) == 0 && len(state.Deliveries) == 0
}

func (m *AlertManager) removeConfigFilesLocked(project, environment string) error {
	for _, path := range []string{m.configPath(project, environment), m.statePath(project, environment)} {
		if err := validatePathParentsNoSymlink(m.dataDir, path, "scheduler alert path"); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := syncDirectory(filepath.Dir(path)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	credentialsDir := m.credentialsDir(project, environment)
	if err := validatePathParentsNoSymlink(m.dataDir, filepath.Join(credentialsDir, "credential.json"), "scheduler alert path"); err != nil {
		return err
	}
	if info, err := os.Lstat(credentialsDir); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("scheduler alert credentials path must be a directory")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.RemoveAll(credentialsDir); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := syncDirectory(filepath.Dir(credentialsDir)); err != nil && !os.IsNotExist(err) {
		return err
	}
	key := alertScopeKey(project, environment)
	delete(m.configs, key)
	delete(m.configGenerations, key)
	delete(m.protectedGenerations, key)
	delete(m.states, key)
	return nil
}

func configuredAlertProviders(targets AlertTargets) []string {
	var providers []string
	if targets.Slack != "" {
		providers = append(providers, "slack")
	}
	if targets.Discord != "" {
		providers = append(providers, "discord")
	}
	if targets.Webhook != "" {
		providers = append(providers, "webhook")
	}
	return providers
}

func alertTargetForProvider(targets AlertTargets, provider string) string {
	switch provider {
	case "slack":
		return targets.Slack
	case "discord":
		return targets.Discord
	case "webhook":
		return targets.Webhook
	default:
		return ""
	}
}

func newAlertScopeState(project, environment string) *alertScopeState {
	return &alertScopeState{SchemaVersion: 1, Project: project, Environment: environment, Subjects: map[string]*AlertSubjectStatus{}}
}

func (m *AlertManager) stateLocked(project, environment string) *alertScopeState {
	key := alertScopeKey(project, environment)
	state := m.states[key]
	if state == nil {
		state = newAlertScopeState(project, environment)
		m.states[key] = state
	}
	return state
}

func (m *AlertManager) persistConfigLocked(config AlertConfigRequest) (string, error) {
	key := alertScopeKey(config.Project, config.Environment)
	previousGeneration := m.configGenerations[key]
	generation := previousGeneration
	previous, exists := m.configs[key]
	targetsChanged := !exists || previous.Targets != config.Targets || !validAlertGeneration(generation)
	if !targetsChanged {
		credentials, err := m.readCredentials(config.Project, config.Environment, generation)
		targetsChanged = err != nil || credentials.Targets != config.Targets
	}
	if targetsChanged {
		generation = newAlertID()
		if !validAlertGeneration(generation) {
			return "", fmt.Errorf("generate scheduler alert credential identity")
		}
		credentials := alertCredentialsDocument{
			SchemaVersion: 1, Project: config.Project, Environment: config.Environment,
			Generation: generation, Targets: config.Targets,
		}
		credentialsPath := m.credentialsPath(config.Project, config.Environment, generation)
		if err := m.writeAlertDocument(credentialsPath, credentials); err != nil {
			return "", err
		}
	}
	policy := alertPolicyDocument{
		SchemaVersion: 1, Project: config.Project, Environment: config.Environment,
		Policy: config.Policy, Providers: configuredAlertProviders(config.Targets), Generation: generation,
	}
	if err := m.writeAlertDocument(m.configPath(config.Project, config.Environment), policy); err != nil {
		protected := m.protectedGenerations[key]
		if protected == nil {
			protected = map[string]bool{}
			m.protectedGenerations[key] = protected
		}
		if validAlertGeneration(previousGeneration) {
			protected[previousGeneration] = true
		}
		if validAlertGeneration(generation) {
			protected[generation] = true
		}
		return "", err
	}
	return generation, nil
}

func (m *AlertManager) persistStateLocked(state *alertScopeState) error {
	return m.writeAlertDocument(m.statePath(state.Project, state.Environment), state)
}

func (m *AlertManager) writeAlertDocument(path string, value any) error {
	if err := validatePathParentsNoSymlink(m.dataDir, path, "scheduler alert path"); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFileAtomic(path, data, 0600)
}

func validatePathParentsNoSymlink(dataDir, target, description string) error {
	base := filepath.Clean(dataDir)
	parent := filepath.Clean(filepath.Dir(target))
	relative, err := filepath.Rel(base, parent)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s escapes data directory", description)
	}
	current := base
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s component %s must be a directory", description, current)
		}
	}
	return nil
}

func validateStorageTreeNoSymlink(root, description string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if os.IsNotExist(walkErr) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s %s must not be a symlink", description, path)
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return fmt.Errorf("%s %s must be a regular file or directory", description, path)
		}
		return nil
	})
}

func (m *AlertManager) loadConfigsLocked() error {
	return loadAlertDocuments(filepath.Join(m.dataDir, schedulerAlertsDirName, "config"), func(path string, data []byte) error {
		var policy alertPolicyDocument
		if err := json.Unmarshal(data, &policy); err != nil || policy.SchemaVersion != 1 || !isSafeProjectName(policy.Project) || !isSafeRuntimeName(policy.Environment) || !validAlertGeneration(policy.Generation) || filepath.Clean(path) != filepath.Clean(m.configPath(policy.Project, policy.Environment)) {
			return fmt.Errorf("invalid scheduler alert config %s", path)
		}
		credentials, err := m.readCredentials(policy.Project, policy.Environment, policy.Generation)
		if err != nil {
			return fmt.Errorf("invalid scheduler alert credentials for %s: %w", path, err)
		}
		config := AlertConfigRequest{Project: policy.Project, Environment: policy.Environment, Policy: policy.Policy, Targets: credentials.Targets}
		providers := configuredAlertProviders(config.Targets)
		if validateAlertConfigRequest(config) != nil || strings.Join(policy.Providers, ",") != strings.Join(providers, ",") {
			return fmt.Errorf("invalid scheduler alert config %s", path)
		}
		key := alertScopeKey(config.Project, config.Environment)
		m.configs[key] = config
		m.configGenerations[key] = policy.Generation
		return nil
	})
}

func (m *AlertManager) removeStaleCredentials(project, environment, keepGeneration string) error {
	dir := m.credentialsDir(project, environment)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	needed := map[string]bool{keepGeneration: true}
	for generation := range m.protectedGenerations[alertScopeKey(project, environment)] {
		needed[generation] = true
	}
	if state := m.states[alertScopeKey(project, environment)]; state != nil {
		for _, delivery := range state.Deliveries {
			if !delivery.Exhausted {
				needed[delivery.CredentialGeneration] = true
			}
		}
		for _, subject := range state.Subjects {
			if subject.OpenedGeneration != "" {
				needed[subject.OpenedGeneration] = true
			}
		}
	}
	removed := false
	for _, entry := range entries {
		generation := strings.TrimSuffix(entry.Name(), ".json")
		if entry.Name() == generation+".json" && needed[generation] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
		removed = true
	}
	if removed {
		return syncDirectory(dir)
	}
	return nil
}

func (m *AlertManager) cleanupOrphanCredentialsLocked() error {
	root := filepath.Join(m.dataDir, schedulerAlertsDirName, "credentials")
	projects, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, project := range projects {
		projectPath := filepath.Join(root, project.Name())
		if !project.IsDir() {
			if err := os.Remove(projectPath); err != nil {
				return err
			}
			continue
		}
		environments, err := os.ReadDir(projectPath)
		if err != nil {
			return err
		}
		for _, environment := range environments {
			if _, configured := m.configs[alertScopeKey(project.Name(), environment.Name())]; configured && environment.IsDir() {
				continue
			}
			if err := os.RemoveAll(filepath.Join(projectPath, environment.Name())); err != nil {
				return err
			}
		}
		_ = os.Remove(projectPath)
	}
	return syncDirectory(root)
}

func (m *AlertManager) reconcileLoadedStatesLocked() error {
	now := m.now()
	for key, state := range m.states {
		config, configured := m.configs[key]
		if !configured {
			continue
		}
		if !alertPolicyEnabled(config.Policy) {
			state.Retiring = true
		}
		m.resetCredentialStateLocked(state, m.configGenerations[key])
		for _, subject := range state.Subjects {
			if subject.Retiring || state.Retiring {
				m.retireSubjectLocked(state, subject)
			} else {
				m.reconcileSubjectLocked(config, state, subject, now)
			}
		}
		if m.finalizeRetiringSubjectsLocked(state) {
			if err := m.removeConfigFilesLocked(state.Project, state.Environment); err != nil {
				return err
			}
			continue
		}
		state.EvaluatedAt = now
		if err := m.persistStateLocked(state); err != nil {
			return err
		}
	}
	return nil
}

func (m *AlertManager) readCredentials(project, environment, generation string) (alertCredentialsDocument, error) {
	path := m.credentialsPath(project, environment, generation)
	info, err := os.Lstat(path)
	if err != nil {
		return alertCredentialsDocument{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return alertCredentialsDocument{}, fmt.Errorf("credential document must be a regular 0600 file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return alertCredentialsDocument{}, err
	}
	var credentials alertCredentialsDocument
	if err := json.Unmarshal(data, &credentials); err != nil || credentials.SchemaVersion != 1 || credentials.Project != project || credentials.Environment != environment || credentials.Generation != generation {
		return alertCredentialsDocument{}, fmt.Errorf("credential document identity mismatch")
	}
	return credentials, nil
}

func (m *AlertManager) loadStatesLocked() error {
	return loadAlertDocuments(filepath.Join(m.dataDir, schedulerAlertsDirName, "state"), func(path string, data []byte) error {
		var state alertScopeState
		if err := json.Unmarshal(data, &state); err != nil || state.SchemaVersion != 1 || !isSafeProjectName(state.Project) || !isSafeRuntimeName(state.Environment) || len(state.Deliveries) > maxAlertDeliveries || filepath.Clean(path) != filepath.Clean(m.statePath(state.Project, state.Environment)) {
			return fmt.Errorf("invalid scheduler alert state %s", path)
		}
		if state.Subjects == nil {
			state.Subjects = map[string]*AlertSubjectStatus{}
		}
		for key, subject := range state.Subjects {
			if subject == nil || key != alertSubjectKey(subject.Kind, subject.Service) || !validAlertSubject(state.Project, state.Environment, subject.Kind, subject.Service) || subject.ConfiguredAt.IsZero() || len(subject.LastAttemptID) > 128 || hasControlChars(subject.LastAttemptID) || (subject.OpenedGeneration != "" && !validAlertGeneration(subject.OpenedGeneration)) || subject.ConsecutiveFailures < 0 || subject.ConsecutiveTimeouts < 0 || subject.ConsecutiveSkipped < 0 || (subject.LastStatus != "" && !validAlertAttemptStatus(subject.LastStatus)) || (subject.Active && (subject.EpisodeID == "" || subject.OpenedAt.IsZero())) {
				return fmt.Errorf("invalid scheduler alert subject in %s", path)
			}
			for _, reason := range subject.Reasons {
				if !validAlertReason(reason) {
					return fmt.Errorf("invalid scheduler alert reason in %s", path)
				}
			}
			if subject.OpenedProviders == nil {
				subject.OpenedProviders = map[string]bool{}
			}
			for provider := range subject.OpenedProviders {
				if !validAlertProvider(provider) {
					return fmt.Errorf("invalid scheduler alert provider in %s", path)
				}
			}
			if subject.OpenedGeneration != "" {
				if _, err := m.readCredentials(state.Project, state.Environment, subject.OpenedGeneration); err != nil {
					return fmt.Errorf("invalid scheduler alert opened credential generation in %s", path)
				}
			}
		}
		deliveryIDs := make(map[string]bool, len(state.Deliveries))
		for index := range state.Deliveries {
			delivery := &state.Deliveries[index]
			subject := state.Subjects[alertSubjectKey(delivery.Kind, delivery.Service)]
			if delivery.ID == "" || deliveryIDs[delivery.ID] || delivery.EpisodeID == "" || !validAlertGeneration(delivery.CredentialGeneration) || subject == nil || !validAlertSubject(state.Project, state.Environment, delivery.Kind, delivery.Service) || (delivery.Transition != "opening" && delivery.Transition != "recovery") || !validAlertProvider(delivery.Provider) || delivery.Attempts < 0 || delivery.Attempts > alertDeliveryMaxAttempts || len(delivery.LastError) > 64 || hasControlChars(delivery.LastError) {
				return fmt.Errorf("invalid scheduler alert delivery in %s", path)
			}
			if !delivery.Exhausted {
				if _, err := m.readCredentials(state.Project, state.Environment, delivery.CredentialGeneration); err != nil {
					return fmt.Errorf("invalid scheduler alert delivery credential generation in %s", path)
				}
			}
			deliveryIDs[delivery.ID] = true
			for _, reason := range delivery.Reasons {
				if !validAlertReason(reason) {
					return fmt.Errorf("invalid scheduler alert delivery reason in %s", path)
				}
			}
		}
		key := alertScopeKey(state.Project, state.Environment)
		if _, configured := m.configs[key]; !configured {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			return syncDirectory(filepath.Dir(path))
		}
		m.states[key] = &state
		return nil
	})
}

func loadAlertDocuments(root string, consume func(string, []byte) error) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if os.IsNotExist(walkErr) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("scheduler alert document %s must be a regular file", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return consume(path, data)
	})
}

func (m *AlertManager) configPath(project, environment string) string {
	return filepath.Join(m.dataDir, schedulerAlertsDirName, "config", project, environment+".json")
}

func (m *AlertManager) statePath(project, environment string) string {
	return filepath.Join(m.dataDir, schedulerAlertsDirName, "state", project, environment+".json")
}

func (m *AlertManager) credentialsDir(project, environment string) string {
	return filepath.Join(m.dataDir, schedulerAlertsDirName, "credentials", project, environment)
}

func (m *AlertManager) credentialsPath(project, environment, generation string) string {
	return filepath.Join(m.credentialsDir(project, environment), generation+".json")
}

func validAlertGeneration(generation string) bool {
	if len(generation) != 32 {
		return false
	}
	_, err := hex.DecodeString(generation)
	return err == nil
}

func alertScopeKey(project, environment string) string { return project + "/" + environment }
func alertSubjectKey(kind, service string) string      { return kind + "/" + service }

func newAlertID() string {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(data)
}
