package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/health"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
)

// Kinds of serialized domains result documents.
const (
	KindDomainsResult      = "DomainsResult"
	KindDomainsHostsResult = "DomainsHostsResult"
)

// DomainStatusEntry is one public domain's DNS/TLS readiness.
type DomainStatusEntry struct {
	Service string `json:"service"`
	Domain  string `json:"domain"`
	// Role is "serving", "redirect", or "ad-hoc".
	Role        string   `json:"role"`
	State       string   `json:"state"`
	DNS         string   `json:"dns"`
	TLS         string   `json:"tls"`
	ResolvedIPs []string `json:"resolvedIps,omitempty"`
	CNAME       string   `json:"cname,omitempty"`
	Message     string   `json:"message,omitempty"`
	Warning     string   `json:"warning,omitempty"`
	DNSError    string   `json:"dnsError,omitempty"`
	TLSError    string   `json:"tlsError,omitempty"`
}

// DomainsResult is the serializable outcome of `tako domains status`.
// With --strict, pending domains exit 6 and still emit the document.
type DomainsResult struct {
	APIVersion  string `json:"apiVersion"`
	Kind        string `json:"kind"`
	Project     string `json:"project"`
	Environment string `json:"environment"`
	// Service is the --service filter when one was requested.
	Service         string              `json:"service,omitempty"`
	ExpectedTargets []string            `json:"expectedTargets,omitempty"`
	AllActive       bool                `json:"allActive"`
	Domains         []DomainStatusEntry `json:"domains"`
}

// InternalHostEntry is one /etc/hosts mapping for an internal proxy route.
type InternalHostEntry struct {
	Service string `json:"service"`
	Host    string `json:"host"`
	Address string `json:"address"`
	Server  string `json:"server,omitempty"`
	Source  string `json:"source,omitempty"`
}

// DomainsHostsResult is the serializable outcome of `tako domains hosts`.
type DomainsHostsResult struct {
	APIVersion  string `json:"apiVersion"`
	Kind        string `json:"kind"`
	Project     string `json:"project"`
	Environment string `json:"environment"`
	// Service is the --service filter; AddressMode mirrors --address.
	Service     string              `json:"service,omitempty"`
	AddressMode string              `json:"addressMode,omitempty"`
	Entries     []InternalHostEntry `json:"entries"`
}

// DomainStatusSpec identifies one public domain to check.
type DomainStatusSpec struct {
	Service                     string
	Domain                      string
	Role                        string
	WarnUntrustedAccessControls bool
}

// DomainStatusOptions controls domain readiness monitoring.
type DomainStatusOptions struct {
	Timeout         time.Duration
	Strict          bool
	ExpectedTargets []string
}

// CollectConfiguredDomainSpecs lists the public domains configured on
// services, optionally filtered to one service.
func CollectConfiguredDomainSpecs(services map[string]config.ServiceConfig, serviceFilter string) []DomainStatusSpec {
	serviceNames := sortedServiceNames(services)
	var specs []DomainStatusSpec
	for _, serviceName := range serviceNames {
		if serviceFilter != "" && serviceName != serviceFilter {
			continue
		}
		service := services[serviceName]
		if service.Proxy == nil || !service.IsPublic() {
			continue
		}
		warnUntrustedAccessControls := (service.Proxy.BasicAuth != nil || len(service.Proxy.AllowIps) > 0) && len(service.Proxy.TrustedProxies) == 0
		for _, domain := range service.Proxy.GetAllDomains() {
			specs = append(specs, DomainStatusSpec{Service: serviceName, Domain: domain, Role: "serving", WarnUntrustedAccessControls: warnUntrustedAccessControls})
		}
		for _, domain := range service.Proxy.GetRedirectDomains() {
			specs = append(specs, DomainStatusSpec{Service: serviceName, Domain: domain, Role: "redirect", WarnUntrustedAccessControls: warnUntrustedAccessControls})
		}
	}
	return specs
}

// DomainExpectedTargets resolves the DNS targets public domains must point at.
func DomainExpectedTargets(cfg *config.Config, envName string, overrides []string) ([]string, error) {
	if len(overrides) > 0 {
		return append([]string(nil), overrides...), nil
	}
	proxyServers, err := cfg.GetEnvironmentProxyServers(envName)
	if err != nil {
		return nil, err
	}
	targets := make([]string, 0, len(proxyServers))
	for _, serverName := range proxyServers {
		server, ok := cfg.Servers[serverName]
		if !ok {
			return nil, fmt.Errorf("proxy server %s is not defined in servers", serverName)
		}
		targets = append(targets, server.Host)
	}
	return targets, nil
}

// MonitorDomainStatuses polls DNS/TLS readiness for the given domains until
// all are active or the timeout elapses, emitting progress events.
func (e *Engine) MonitorDomainStatuses(ctx context.Context, checker *health.HealthChecker, specs []DomainStatusSpec, options DomainStatusOptions) ([]health.DomainStatus, error) {
	if len(specs) == 0 {
		return nil, nil
	}

	header := "\n🌐 Checking public domain DNS/TLS status...\n"
	if len(options.ExpectedTargets) > 0 {
		header += fmt.Sprintf("   Expected proxy target(s): %s\n", strings.Join(options.ExpectedTargets, ", "))
	}
	e.info(events.TypeDomainStatus, events.PhaseDomains, header)

	start := time.Now()
	attempt := 1
	var latest []health.DomainStatus
	check := func() []health.DomainStatus {
		results := make([]health.DomainStatus, 0, len(specs))
		allReady := true
		for _, spec := range specs {
			status := checker.CheckDomain(ctx, spec.Service, spec.Domain, options.ExpectedTargets)
			applyDomainAccessControlWarning(&status, spec)
			results = append(results, status)
			if status.Pending() {
				allReady = false
			}
		}
		e.emitDomainStatusAttempt(attempt, results)
		attempt++
		if allReady {
			e.info(events.TypeDomainStatus, events.PhaseDomains, fmt.Sprintf("   ✓ All public domains active in %s\n", time.Since(start).Round(time.Second)))
		}
		return results
	}

	latest = check()
	if allDomainStatusesReady(latest) || options.Timeout <= 0 {
		return latest, domainStatusStrictError(latest, options.Strict)
	}

	deadline := time.NewTimer(options.Timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return latest, ctx.Err()
		case <-deadline.C:
			e.emitDomainPendingSummary(latest)
			return latest, domainStatusStrictError(latest, options.Strict)
		case <-ticker.C:
			latest = check()
			if allDomainStatusesReady(latest) {
				return latest, nil
			}
		}
	}
}

func applyDomainAccessControlWarning(status *health.DomainStatus, spec DomainStatusSpec) {
	if status == nil || !spec.WarnUntrustedAccessControls || (status.DNS != health.DomainDNSProxied && status.DNS != health.DomainDNSWrong) {
		return
	}
	status.Warning = "access controls are configured behind a suspected proxy/CDN without proxy.trustedProxies; original client IPs cannot be trusted"
}

func (e *Engine) emitDomainStatusAttempt(attempt int, statuses []health.DomainStatus) {
	for _, status := range statuses {
		message := fmt.Sprintf("   [%d] %s (%s): %s", attempt, status.Domain, status.Service, status.State)
		if status.Message != "" {
			message += fmt.Sprintf(" - %s", status.Message)
		}
		if len(status.ResolvedIPs) > 0 {
			message += fmt.Sprintf(" [resolved: %s]", strings.Join(status.ResolvedIPs, ", "))
		}
		if status.CNAME != "" {
			message += fmt.Sprintf(" [cname: %s]", status.CNAME)
		}
		if status.Warning != "" {
			message += fmt.Sprintf(" [warning: %s]", status.Warning)
		}
		message += "\n"
		level := events.LevelInfo
		if status.Warning != "" {
			level = events.LevelWarn
		}
		e.emit(events.Event{
			Type:    events.TypeDomainStatus,
			Phase:   events.PhaseDomains,
			Level:   level,
			Service: status.Service,
			Message: message,
			Data: map[string]any{
				"domain":  status.Domain,
				"state":   string(status.State),
				"attempt": attempt,
			},
		})
	}
}

func (e *Engine) emitDomainPendingSummary(statuses []health.DomainStatus) {
	pending := pendingDomainStatuses(statuses)
	if len(pending) == 0 {
		return
	}
	message := "\n⚠️  Public domain provisioning is still pending.\n"
	for _, status := range pending {
		message += fmt.Sprintf("   - %s (%s): %s", status.Domain, status.Service, status.State)
		if status.Message != "" {
			message += fmt.Sprintf(" - %s", status.Message)
		}
		message += "\n"
	}
	message += "   Deployment already reconciled. Fix DNS or wait for propagation, then run `tako domains status`.\n"
	e.warn(events.PhaseDomains, message)
}

func allDomainStatusesReady(statuses []health.DomainStatus) bool {
	for _, status := range statuses {
		if status.Pending() {
			return false
		}
	}
	return true
}

func pendingDomainStatuses(statuses []health.DomainStatus) []health.DomainStatus {
	var pending []health.DomainStatus
	for _, status := range statuses {
		if status.Pending() {
			pending = append(pending, status)
		}
	}
	return pending
}

func domainStatusStrictError(statuses []health.DomainStatus, strict bool) error {
	if !strict {
		return nil
	}
	pending := pendingDomainStatuses(statuses)
	if len(pending) == 0 {
		return nil
	}
	parts := make([]string, 0, len(pending))
	for _, status := range pending {
		parts = append(parts, fmt.Sprintf("%s=%s", status.Domain, status.State))
	}
	return &AttentionError{Err: fmt.Errorf("public domain check failed in strict mode: %s", strings.Join(parts, ", "))}
}
