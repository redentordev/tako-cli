package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/health"
	"github.com/redentordev/tako-cli/pkg/takod"
)

type originTestResolver struct{ hosts map[string][]string }

func (r originTestResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	if values, ok := r.hosts[host]; ok {
		return values, nil
	}
	return nil, errors.New("no such host")
}

func (originTestResolver) LookupCNAME(context.Context, string) (string, error) {
	return "", errors.New("no cname")
}

func unreachableOriginTLS(context.Context, string) *health.SSLInfo {
	return &health.SSLInfo{Valid: false, Error: "connection timed out"}
}

func originTestChecker() *health.HealthChecker {
	return health.NewHealthCheckerWithProbes(originTestResolver{hosts: map[string][]string{"origin.example.com": {"203.0.113.10"}}}, unreachableOriginTLS)
}

type fakeNodeCerts struct {
	capabilities map[string][]string
	statuses     map[string]takod.ProxyCertificateStatusResponse
	statusCalls  int
	certCalls    int
}

func (f *fakeNodeCerts) request(_ context.Context, server string, endpoint string) (string, error) {
	if endpoint == "/v1/status" {
		f.statusCalls++
		data, _ := json.Marshal(map[string]any{"capabilities": f.capabilities[server]})
		return string(data), nil
	}
	f.certCalls++
	if !strings.HasPrefix(endpoint, "/v1/certs/status?domain=") {
		return "", errors.New("unexpected endpoint " + endpoint)
	}
	data, _ := json.Marshal(f.statuses[server])
	return string(data), nil
}

func validNodeCert(domain string) takod.ProxyCertificateStatusResponse {
	return takod.ProxyCertificateStatusResponse{Domain: domain, Present: true, Valid: true, Source: takod.CertificateSourceCaddy, Issuer: "Test CA", NotAfter: time.Now().Add(60 * 24 * time.Hour).UTC()}
}

func monitorOriginDomain(t *testing.T, spec DomainStatusSpec, checker NodeCertificateChecker, timeout time.Duration) (health.DomainStatus, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	statuses, err := New(Options{}).MonitorDomainStatuses(ctx, originTestChecker(), []DomainStatusSpec{spec}, DomainStatusOptions{
		Timeout:          timeout,
		Strict:           true,
		ExpectedTargets:  []string{"203.0.113.10"},
		NodeCertificates: checker,
	})
	elapsed := time.Since(start)
	if len(statuses) != 1 {
		t.Fatalf("statuses = %#v err=%v", statuses, err)
	}
	if statuses[0].Ready() && err != nil {
		t.Fatalf("ready status returned strict error: %v", err)
	}
	if !statuses[0].Ready() && err == nil {
		t.Fatalf("pending status returned no strict error: %#v", statuses[0])
	}
	return statuses[0], elapsed
}

func TestCDNOriginWithValidNodeCertificateIsActiveOriginRestrictedWithoutWaiting(t *testing.T) {
	fake := &fakeNodeCerts{
		capabilities: map[string][]string{"edge-a": {takod.CapabilityProxyCertStatusV1}, "edge-b": {takod.CapabilityProxyCertStatusV1}},
		statuses:     map[string]takod.ProxyCertificateStatusResponse{"edge-a": validNodeCert("origin.example.com"), "edge-b": validNodeCert("origin.example.com")},
	}
	checker := newNodeCertificateChecker([]string{"edge-b", "edge-a"}, fake.request)
	status, elapsed := monitorOriginDomain(t, DomainStatusSpec{Service: "web", Domain: "origin.example.com", Role: "serving", CDN: config.ProxyCDNGeneric}, checker, 2*time.Minute)

	if status.State != health.DomainStateActive || status.TLS != health.DomainTLSActive || status.DNS != health.DomainDNSOK || !status.OriginRestricted {
		t.Fatalf("status = %#v, want active origin-restricted", status)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("monitor waited %s; origin-restricted domains must resolve on the first attempt", elapsed)
	}
	if status.NodeCertificate == nil || status.NodeCertificate.Source != takod.CertificateSourceCaddy || strings.Join(status.NodeCertificate.Servers, ",") != "edge-a,edge-b" {
		t.Fatalf("node certificate = %#v", status.NodeCertificate)
	}
	if got := DomainStateLabel(status); got != "active (origin-restricted)" {
		t.Fatalf("label = %q", got)
	}
	entry := NewDomainStatusEntry(status, "serving")
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if entry.State != "active" || !strings.Contains(string(data), `"originRestricted":true`) || !strings.Contains(string(data), `"nodeCertificate"`) {
		t.Fatalf("entry JSON = %s", data)
	}
	if fake.statusCalls != 2 {
		t.Fatalf("capability probes = %d, want one per node", fake.statusCalls)
	}
}

func TestCDNOriginWithoutNodeCertificateStaysPendingTLS(t *testing.T) {
	missing := takod.ProxyCertificateStatusResponse{Domain: "origin.example.com"}
	fake := &fakeNodeCerts{
		capabilities: map[string][]string{"edge-a": {takod.CapabilityProxyCertStatusV1}, "edge-b": {takod.CapabilityProxyCertStatusV1}},
		statuses:     map[string]takod.ProxyCertificateStatusResponse{"edge-a": validNodeCert("origin.example.com"), "edge-b": missing},
	}
	checker := newNodeCertificateChecker([]string{"edge-a", "edge-b"}, fake.request)
	status, _ := monitorOriginDomain(t, DomainStatusSpec{Service: "web", Domain: "origin.example.com", CDN: config.ProxyCDNCloudflare}, checker, 0)

	if status.State != health.DomainStatePendingTLS || status.OriginRestricted || status.NodeCertificate != nil {
		t.Fatalf("status = %#v, want pending_tls", status)
	}
	if !strings.Contains(status.Message, "edge-b: no certificate yet") {
		t.Fatalf("message = %q", status.Message)
	}
}

func TestCDNOriginExpiredNodeCertificateStaysPending(t *testing.T) {
	expired := validNodeCert("origin.example.com")
	expired.Valid, expired.Error = false, "certificate expired at 2026-01-01T00:00:00Z"
	fake := &fakeNodeCerts{
		capabilities: map[string][]string{"edge-a": {takod.CapabilityProxyCertStatusV1}},
		statuses:     map[string]takod.ProxyCertificateStatusResponse{"edge-a": expired},
	}
	status, _ := monitorOriginDomain(t, DomainStatusSpec{Domain: "origin.example.com", CDN: config.ProxyCDNGeneric}, newNodeCertificateChecker([]string{"edge-a"}, fake.request), 0)
	if status.State != health.DomainStatePendingTLS || !strings.Contains(status.Message, "expired") {
		t.Fatalf("status = %#v", status)
	}
}

func TestNonCDNOriginIgnoresNodeCertificates(t *testing.T) {
	fake := &fakeNodeCerts{
		capabilities: map[string][]string{"edge-a": {takod.CapabilityProxyCertStatusV1}},
		statuses:     map[string]takod.ProxyCertificateStatusResponse{"edge-a": validNodeCert("origin.example.com")},
	}
	status, _ := monitorOriginDomain(t, DomainStatusSpec{Domain: "origin.example.com"}, newNodeCertificateChecker([]string{"edge-a"}, fake.request), 0)
	if status.State != health.DomainStatePendingTLS || status.OriginRestricted || status.Message != "DNS is routed; waiting for TLS certificate issuance" {
		t.Fatalf("status = %#v, want unchanged pending_tls", status)
	}
	if fake.statusCalls != 0 || fake.certCalls != 0 {
		t.Fatalf("non-CDN domain contacted nodes: status=%d cert=%d", fake.statusCalls, fake.certCalls)
	}
}

func TestCDNOriginOnOldTakodFallsBackToPublicProbe(t *testing.T) {
	fake := &fakeNodeCerts{
		capabilities: map[string][]string{"edge-a": {takod.CapabilityProxyCertStatusV1}, "edge-b": {takod.CapabilityProxyCertsV1}},
		statuses:     map[string]takod.ProxyCertificateStatusResponse{"edge-a": validNodeCert("origin.example.com")},
	}
	checker := newNodeCertificateChecker([]string{"edge-a", "edge-b"}, fake.request)
	status, _ := monitorOriginDomain(t, DomainStatusSpec{Domain: "origin.example.com", CDN: config.ProxyCDNGeneric}, checker, 0)
	if status.State != health.DomainStatePendingTLS || status.OriginRestricted {
		t.Fatalf("status = %#v, want pending_tls fallback", status)
	}
	for _, want := range []string{"proxy.cdn is declared", "connection timed out", takod.CapabilityProxyCertStatusV1, "tako upgrade servers"} {
		if !strings.Contains(status.Message, want) {
			t.Fatalf("message %q does not mention %q", status.Message, want)
		}
	}
	if fake.certCalls != 0 {
		t.Fatalf("old takod was asked for certificate status %d times", fake.certCalls)
	}
	// The capability answer is cached across polling attempts.
	checker(context.Background(), "origin.example.com")
	if fake.statusCalls != 2 {
		t.Fatalf("capability probes = %d, want 2 (cached)", fake.statusCalls)
	}
}

func TestCDNOriginWithoutCheckerExplainsProbeFailure(t *testing.T) {
	status, _ := monitorOriginDomain(t, DomainStatusSpec{Domain: "origin.example.com", CDN: config.ProxyCDNGeneric}, nil, 0)
	if status.State != health.DomainStatePendingTLS || !strings.Contains(status.Message, "CDN's address ranges") {
		t.Fatalf("status = %#v", status)
	}
}
