package takod

import "context"

// CapabilityMonitoringHealthV1 means the node runs node-side health
// monitoring: service checks, host thresholds, container OOM/crash-loop and
// certificate expiry alerts through the alert source API, and accepts the
// additive monitors/host/containers/certificates/serviceTargets fields of
// PUT /v1/alerts.
const CapabilityMonitoringHealthV1 = "monitoring.health-v1"

// registerBuiltinAlertSources registers the kinds owned by this change.
// Sibling sources register theirs the same way before Load.
func registerBuiltinAlertSources(manager *SourceAlertManager) error {
	for _, spec := range []AlertSourceSpec{
		serviceAlertSourceSpec(),
		hostAlertSourceSpec(),
		containerAlertSourceSpec(),
		certificateAlertSourceSpec(),
	} {
		if err := manager.RegisterSource(spec); err != nil {
			return err
		}
	}
	return nil
}

// startHealthMonitoring starts the source dispatch loop and the built-in
// sources. Each loop does nothing while no scope subscribes to it.
func (s *Server) startHealthMonitoring(ctx context.Context, start func(func())) {
	monitors := newServiceMonitorRunner(s.sourceAlerts, s.dataDir, func() string { return s.nodeName })
	hosts := newHostAlertEvaluator(s.sourceAlerts, s.hostSamples)
	certificates := newCertificateExpiryScanner(s.sourceAlerts)
	start(func() { s.sourceAlerts.Run(ctx) })
	start(func() { monitors.Run(ctx) })
	start(func() { hosts.Run(ctx) })
	start(func() { s.containerSampler.Run(ctx) })
	start(func() { certificates.Run(ctx) })
}

// sourceStatusFor returns the sources object for a scope, or nil when the
// scope has no node-side monitoring.
func (s *Server) sourceStatusFor(project, environment string) *SourceAlertStatus {
	if s.sourceAlerts == nil || !s.sourceAlerts.Configured(project, environment) {
		return nil
	}
	status, err := s.sourceAlerts.Status(project, environment)
	if err != nil {
		return nil
	}
	return &status
}

// alertStatusWithSources is the /v1/metrics alerts list: every scope with
// job/backup alerts or node-side monitoring, with sources attached.
func (s *Server) alertStatusWithSources() []AlertStatusResponse {
	responses := s.alertManager.StatusAll()
	seen := map[string]bool{}
	for index := range responses {
		seen[alertScopeKey(responses[index].Project, responses[index].Environment)] = true
		responses[index].Sources = s.sourceStatusFor(responses[index].Project, responses[index].Environment)
	}
	for _, scope := range s.sourceAlerts.ScopePolicies() {
		if seen[alertScopeKey(scope.Project, scope.Environment)] {
			continue
		}
		responses = append(responses, AlertStatusResponse{
			Project: scope.Project, Environment: scope.Environment,
			Sources: s.sourceStatusFor(scope.Project, scope.Environment),
		})
	}
	return responses
}
