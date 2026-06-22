package health

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type fakeResolver struct {
	hosts  map[string][]string
	cnames map[string]string
}

func (r fakeResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	if values, ok := r.hosts[host]; ok {
		return values, nil
	}
	return nil, errors.New("no such host")
}

func (r fakeResolver) LookupCNAME(ctx context.Context, host string) (string, error) {
	if value, ok := r.cnames[host]; ok {
		return value, nil
	}
	return "", errors.New("no cname")
}

func TestCheckDomainActiveDirectTarget(t *testing.T) {
	checker := testDomainChecker(fakeResolver{
		hosts: map[string][]string{
			"app.example.com":  {"203.0.113.10"},
			"edge.example.com": {"203.0.113.10"},
		},
	}, validTLS)

	status := checker.CheckDomain(context.Background(), "web", "app.example.com", []string{"edge.example.com"})

	if status.State != DomainStateActive || status.DNS != DomainDNSOK || status.TLS != DomainTLSActive {
		t.Fatalf("status = %#v, want active direct target", status)
	}
	if !reflect.DeepEqual(status.ExpectedIPs, []string{"203.0.113.10"}) {
		t.Fatalf("expected IPs = %#v", status.ExpectedIPs)
	}
}

func TestCheckDomainActiveThroughExternalProxy(t *testing.T) {
	checker := testDomainChecker(fakeResolver{
		hosts: map[string][]string{
			"app.example.com":  {"198.51.100.55"},
			"edge.example.com": {"203.0.113.10"},
		},
	}, validTLS)

	status := checker.CheckDomain(context.Background(), "web", "app.example.com", []string{"edge.example.com"})

	if status.State != DomainStateActive || status.DNS != DomainDNSProxied || status.TLS != DomainTLSActive {
		t.Fatalf("status = %#v, want active proxied", status)
	}
}

func TestCheckDomainPendingDNS(t *testing.T) {
	checker := testDomainChecker(fakeResolver{}, validTLS)

	status := checker.CheckDomain(context.Background(), "web", "missing.example.com", []string{"203.0.113.10"})

	if status.State != DomainStatePendingDNS || status.DNS != DomainDNSPending || status.TLS != DomainTLSSkipped {
		t.Fatalf("status = %#v, want pending DNS", status)
	}
}

func TestCheckDomainWrongTargetWhenTLSPending(t *testing.T) {
	checker := testDomainChecker(fakeResolver{
		hosts: map[string][]string{
			"app.example.com": {"198.51.100.55"},
		},
	}, pendingTLS)

	status := checker.CheckDomain(context.Background(), "web", "app.example.com", []string{"203.0.113.10"})

	if status.State != DomainStateWrongDNS || status.DNS != DomainDNSWrong || status.TLS != DomainTLSPending {
		t.Fatalf("status = %#v, want wrong DNS with pending TLS", status)
	}
}

func TestCheckDomainPendingTLSWhenDNSMatches(t *testing.T) {
	checker := testDomainChecker(fakeResolver{
		hosts: map[string][]string{
			"app.example.com": {"203.0.113.10"},
		},
	}, pendingTLS)

	status := checker.CheckDomain(context.Background(), "web", "app.example.com", []string{"203.0.113.10"})

	if status.State != DomainStatePendingTLS || status.DNS != DomainDNSOK || status.TLS != DomainTLSPending {
		t.Fatalf("status = %#v, want pending TLS", status)
	}
}

func testDomainChecker(resolver fakeResolver, tlsChecker func(context.Context, string) *SSLInfo) *HealthChecker {
	return &HealthChecker{
		timeout:    time.Second,
		resolver:   resolver,
		sslChecker: tlsChecker,
	}
}

func validTLS(context.Context, string) *SSLInfo {
	return &SSLInfo{Valid: true, Issuer: "test", Expiry: time.Now().Add(24 * time.Hour)}
}

func pendingTLS(context.Context, string) *SSLInfo {
	return &SSLInfo{Valid: false, Error: "certificate is not ready"}
}
