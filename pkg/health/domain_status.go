package health

import (
	"context"
	"net"
	"net/url"
	"sort"
	"strings"
)

type DomainState string

const (
	DomainStateActive     DomainState = "active"
	DomainStatePendingDNS DomainState = "pending_dns"
	DomainStateWrongDNS   DomainState = "wrong_dns"
	DomainStatePendingTLS DomainState = "pending_tls"
	DomainStateUnknown    DomainState = "unknown"
)

type DomainDNSState string

const (
	DomainDNSOK        DomainDNSState = "ok"
	DomainDNSPending   DomainDNSState = "pending"
	DomainDNSWrong     DomainDNSState = "wrong_target"
	DomainDNSProxied   DomainDNSState = "proxied"
	DomainDNSUnchecked DomainDNSState = "unchecked"
)

type DomainTLSState string

const (
	DomainTLSActive  DomainTLSState = "active"
	DomainTLSPending DomainTLSState = "pending"
	DomainTLSSkipped DomainTLSState = "skipped"
)

type DomainStatus struct {
	Service         string
	Domain          string
	State           DomainState
	DNS             DomainDNSState
	TLS             DomainTLSState
	ResolvedIPs     []string
	CNAME           string
	ExpectedTargets []string
	ExpectedIPs     []string
	Message         string
	Warning         string
	DNSError        string
	TLSError        string
	SSL             *SSLInfo
}

func (s DomainStatus) Ready() bool {
	return s.State == DomainStateActive
}

func (s DomainStatus) Pending() bool {
	return s.State != DomainStateActive
}

func (h *HealthChecker) CheckDomain(ctx context.Context, serviceName string, domain string, expectedTargets []string) DomainStatus {
	domain = normalizeDNSName(domain)
	status := DomainStatus{
		Service:         serviceName,
		Domain:          domain,
		State:           DomainStateUnknown,
		DNS:             DomainDNSUnchecked,
		TLS:             DomainTLSSkipped,
		ExpectedTargets: normalizeTargetList(expectedTargets),
	}

	if domain == "" {
		status.State = DomainStateUnknown
		status.Message = "domain is empty"
		return status
	}

	expectedNames, expectedIPs := h.resolveExpectedTargets(ctx, status.ExpectedTargets)
	status.ExpectedIPs = sortedKeys(expectedIPs)

	ips, err := h.lookupHost(ctx, domain)
	if err != nil {
		status.DNSError = err.Error()
	}
	status.ResolvedIPs = normalizeTargetList(ips)
	status.CNAME = h.lookupCNAME(ctx, domain)

	targetMatched := domainMatchesExpectedTarget(status.ResolvedIPs, status.CNAME, expectedNames, expectedIPs)
	if len(status.ResolvedIPs) == 0 {
		status.State = DomainStatePendingDNS
		status.DNS = DomainDNSPending
		status.TLS = DomainTLSSkipped
		status.Message = "DNS has no A or AAAA records yet"
		return status
	}

	sslInfo := h.CheckSSL(ctx, domain)
	status.SSL = sslInfo
	if sslInfo != nil && sslInfo.Valid {
		status.State = DomainStateActive
		status.TLS = DomainTLSActive
		switch {
		case len(status.ExpectedTargets) == 0 || targetMatched:
			status.DNS = DomainDNSOK
			status.Message = "DNS is routed and TLS is active"
		default:
			status.DNS = DomainDNSProxied
			status.Message = "TLS is active through an external proxy/CDN or alternate edge"
		}
		return status
	}
	if sslInfo != nil && sslInfo.Error != "" {
		status.TLSError = sslInfo.Error
	}
	status.TLS = DomainTLSPending

	if len(status.ExpectedTargets) > 0 && !targetMatched {
		status.State = DomainStateWrongDNS
		status.DNS = DomainDNSWrong
		status.Message = "DNS resolves, but not to the expected proxy target"
		return status
	}

	status.State = DomainStatePendingTLS
	status.DNS = DomainDNSOK
	status.Message = "DNS is routed; waiting for TLS certificate issuance"
	return status
}

func (h *HealthChecker) lookupHost(ctx context.Context, domain string) ([]string, error) {
	resolver := h.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return resolver.LookupHost(ctx, domain)
}

func (h *HealthChecker) lookupCNAME(ctx context.Context, domain string) string {
	resolver := h.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	cname, err := resolver.LookupCNAME(ctx, domain)
	if err != nil {
		return ""
	}
	cname = normalizeDNSName(cname)
	if cname == domain {
		return ""
	}
	return cname
}

func (h *HealthChecker) resolveExpectedTargets(ctx context.Context, targets []string) (map[string]bool, map[string]bool) {
	names := make(map[string]bool)
	ips := make(map[string]bool)
	for _, target := range targets {
		target = normalizeDNSName(target)
		if target == "" {
			continue
		}
		if parsed := net.ParseIP(target); parsed != nil {
			ips[parsed.String()] = true
			continue
		}
		names[target] = true
		resolved, err := h.lookupHost(ctx, target)
		if err != nil {
			continue
		}
		for _, ip := range resolved {
			if parsed := net.ParseIP(strings.TrimSpace(ip)); parsed != nil {
				ips[parsed.String()] = true
			}
		}
	}
	return names, ips
}

func domainMatchesExpectedTarget(resolvedIPs []string, cname string, expectedNames map[string]bool, expectedIPs map[string]bool) bool {
	for _, ip := range resolvedIPs {
		if parsed := net.ParseIP(strings.TrimSpace(ip)); parsed != nil && expectedIPs[parsed.String()] {
			return true
		}
	}
	if cname != "" && expectedNames[normalizeDNSName(cname)] {
		return true
	}
	return false
}

func normalizeDNSName(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return ""
	}
	if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
		value = parsed.Host
	}
	value = strings.Trim(value, "[]")
	if parsed := net.ParseIP(value); parsed != nil {
		return parsed.String()
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = strings.Trim(host, "[]")
		if parsed := net.ParseIP(value); parsed != nil {
			return parsed.String()
		}
	}
	if idx := strings.Index(value, "/"); idx >= 0 {
		value = value[:idx]
	}
	return strings.TrimSuffix(value, ".")
}

func normalizeTargetList(values []string) []string {
	seen := make(map[string]bool)
	var normalized []string
	for _, value := range values {
		value = normalizeDNSName(value)
		if value == "" || seen[value] {
			continue
		}
		normalized = append(normalized, value)
		seen[value] = true
	}
	sort.Strings(normalized)
	return normalized
}

func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
