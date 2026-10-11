package takod

// Certificate expiry alerts (design D8). The certificate checked for a
// domain is the one the proxy serves, chosen by ProxyCertificateStatus
// (takod store first, then Caddy's storage).

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/notification"
)

const (
	certificateScanInterval   = 6 * time.Hour
	certificateFirstScanDelay = 2 * time.Minute
	certReasonExpiring        = "cert_expiring"
	certReasonExpired         = "cert_expired"
)

func certificateAlertSourceSpec() AlertSourceSpec {
	renewed := string(notification.EventSSLRenewed)
	return AlertSourceSpec{
		Kind:       AlertKindCertificate,
		Reasons:    map[string]Severity{certReasonExpiring: SeverityWarning, certReasonExpired: SeverityCritical},
		DetailKeys: []string{"domain", "expiresAt", "daysLeft", "source"},
		Events: map[string]string{
			certReasonExpiring: string(notification.EventSSLExpiringSoon),
			certReasonExpired:  string(notification.EventSSLExpired),
		},
		Recoveries: map[string]string{certReasonExpiring: renewed, certReasonExpired: renewed},
	}
}

type certificateExpiryScanner struct {
	alerts    *SourceAlertManager
	now       func() time.Time
	routesDir func() string
	status    func(ctx context.Context, domain string) (*ProxyCertificateStatusResponse, error)
	// node returns this node's name; nil or "" disables reporter election.
	node func() string
}

// certificateReporter elects the node that reports certificate expiry for
// a scope: the http checker node of its monitors (the lowest name when
// they differ), so a domain routed on every proxy node alerts once instead
// of once per node. "" means no election (every node reports).
func certificateReporter(policy SourceAlertPolicy) string {
	reporter := ""
	for _, monitor := range policy.Monitors {
		if monitor.CheckType != MonitorCheckHTTP || monitor.CheckerNode == "" {
			continue
		}
		if reporter == "" || monitor.CheckerNode < reporter {
			reporter = monitor.CheckerNode
		}
	}
	return reporter
}

func newCertificateExpiryScanner(alerts *SourceAlertManager) *certificateExpiryScanner {
	return &certificateExpiryScanner{
		alerts:    alerts,
		now:       func() time.Time { return time.Now().UTC() },
		routesDir: func() string { return proxyRoutesDir },
		status:    ProxyCertificateStatus,
	}
}

func (c *certificateExpiryScanner) Run(ctx context.Context) {
	timer := time.NewTimer(certificateFirstScanDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			c.ScanOnce(ctx)
			timer.Reset(certificateScanInterval)
		}
	}
}

// routedDomains returns, per scope, the concrete domains of its local route
// manifests. Unreadable manifests are skipped.
func routedDomains(dir string) map[string][]string {
	result := map[string][]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return result
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
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
		if err != nil {
			continue
		}
		key := alertScopeKey(manifest.Project, manifest.Environment)
		for _, route := range manifest.Routes {
			for _, domain := range route.Domains {
				if normalized, err := normalizeProxyCertificateDomain(domain); err == nil && !strings.HasPrefix(normalized, "*.") {
					result[key] = append(result[key], normalized)
				}
			}
		}
	}
	for key, domains := range result {
		result[key] = sortedUniqueStrings(domains)
	}
	return result
}

// ScanOnce checks every routed domain of every subscribed scope. Domains no
// longer routed are retired.
func (c *certificateExpiryScanner) ScanOnce(ctx context.Context) {
	if !c.alerts.AnySubscribed(AlertKindCertificate) {
		return
	}
	domains := routedDomains(c.routesDir())
	now := c.now()
	for _, scope := range c.alerts.ScopePolicies() {
		policy := scope.Policy.Certificates
		if policy == nil || !c.alerts.Subscribed(scope.Project, scope.Environment, AlertKindCertificate) {
			continue
		}
		current := map[string]bool{}
		scopeDomains := domains[alertScopeKey(scope.Project, scope.Environment)]
		if reporter := certificateReporter(scope.Policy); reporter != "" && c.node != nil && c.node() != "" && c.node() != reporter {
			// Another node reports this scope's certificates.
			scopeDomains = nil
		}
		for _, domain := range scopeDomains {
			if ctx.Err() != nil {
				return
			}
			current[domain] = true
			observation := LevelObservation{
				Project: scope.Project, Environment: scope.Environment, Kind: AlertKindCertificate, Subject: domain,
				Interval: certificateScanInterval, ObservedAt: now, Details: map[string]string{"domain": domain},
			}
			status, err := c.status(ctx, domain)
			if err != nil || status == nil || !status.Present {
				observation.Hold, observation.HoldReason = SubjectHoldUnknown, "no_certificate"
				_ = c.alerts.ReportLevel(observation)
				continue
			}
			left := status.NotAfter.Sub(now)
			observation.Details["expiresAt"] = status.NotAfter.UTC().Format(time.RFC3339)
			days := int64(left / (24 * time.Hour))
			if left < 0 {
				days = 0
			}
			observation.Details["daysLeft"] = strconv.FormatInt(days, 10)
			if status.Source != "" && alertDetailValuePattern.MatchString(status.Source) {
				observation.Details["source"] = status.Source
			}
			switch {
			case left <= 0:
				observation.Failing = []string{certReasonExpired}
			case left <= time.Duration(policy.WarnBeforeSeconds)*time.Second:
				observation.Failing = []string{certReasonExpiring}
				if left <= time.Duration(policy.CriticalBeforeSeconds)*time.Second {
					observation.Severities = map[string]Severity{certReasonExpiring: SeverityCritical}
				}
			}
			_ = c.alerts.ReportLevel(observation)
		}
		subjects := c.alerts.Subjects(scope.Project, scope.Environment, AlertKindCertificate)
		sort.Strings(subjects)
		for _, subject := range subjects {
			if !current[subject] {
				_ = c.alerts.Retire(scope.Project, scope.Environment, AlertKindCertificate, subject)
			}
		}
	}
}
