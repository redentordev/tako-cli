package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/health"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

func TestDomainStatusStrictErrorRequiresDeclaredCDNForProxiedDNS(t *testing.T) {
	active := []health.DomainStatus{{Domain: "app.example.com", State: health.DomainStateActive}}
	if err := domainStatusStrictError(active, true); err != nil {
		t.Fatalf("active strict status returned error: %v", err)
	}

	pending := []health.DomainStatus{{Domain: "app.example.com", State: health.DomainStatePendingDNS}}
	err := domainStatusStrictError(pending, true)
	if err == nil {
		t.Fatal("pending strict status returned nil")
	}
	if !strings.Contains(err.Error(), "app.example.com=pending_dns") {
		t.Fatalf("error = %q", err)
	}
	if Classify(err) != ClassAttention {
		t.Fatalf("Classify(%v) = %d, want ClassAttention", err, Classify(err))
	}

	proxiedDeclared := []health.DomainStatus{{Domain: "app.example.com", State: health.DomainStateActive, DNS: health.DomainDNSProxied, CDN: config.ProxyCDNCloudflare}}
	if err := domainStatusStrictError(proxiedDeclared, true); err != nil {
		t.Fatalf("declared CDN strict status returned error: %v", err)
	}
	proxiedUnknown := []health.DomainStatus{{Domain: "app.example.com", State: health.DomainStateActive, DNS: health.DomainDNSProxied, CDN: "mystery-edge"}}
	if err := domainStatusStrictError(proxiedUnknown, true); err == nil || Classify(err) != ClassAttention {
		t.Fatalf("unknown CDN strict error = %v, want exit class 6", err)
	}

	proxiedHeuristic := []health.DomainStatus{{Domain: "app.example.com", State: health.DomainStateActive, DNS: health.DomainDNSProxied}}
	applyDomainCDNPolicy(&proxiedHeuristic[0], DomainStatusSpec{})
	err = domainStatusStrictError(proxiedHeuristic, true)
	if err == nil || !strings.Contains(err.Error(), "app.example.com=wrong_dns") {
		t.Fatalf("heuristic CDN strict error = %v", err)
	}
	if Classify(err) != ClassAttention {
		t.Fatalf("Classify(%v) = %d, want exit class 6", err, Classify(err))
	}

	unpointedDeclared := []health.DomainStatus{{Domain: "app.example.com", State: health.DomainStateWrongDNS, DNS: health.DomainDNSWrong, CDN: config.ProxyCDNCloudflare}}
	err = domainStatusStrictError(unpointedDeclared, true)
	if err == nil || Classify(err) != ClassAttention {
		t.Fatalf("unpointed declared CDN strict error = %v, class = %d", err, Classify(err))
	}
}

func TestCapabilityRequiredErrorClassifiesAsInvalid(t *testing.T) {
	err := fmt.Errorf("proxy preflight: %w", &takodclient.CapabilityRequiredError{Server: "node-a", Capability: "proxy.trusted-proxies-v1", Feature: "proxy trusted proxies"})
	if Classify(err) != ClassInvalid {
		t.Fatalf("Classify(%v) = %d, want ClassInvalid", err, Classify(err))
	}
}

func TestCollectConfiguredDomainSpecsMarksUntrustedAccessControls(t *testing.T) {
	services := map[string]config.ServiceConfig{
		"direct":  {Proxy: &config.ProxyConfig{Domain: "direct.example.com", CDN: config.ProxyCDNCloudflare, AllowIps: []string{"198.51.100.0/24"}}},
		"trusted": {Proxy: &config.ProxyConfig{Domain: "trusted.example.com", BasicAuth: &config.ProxyBasicAuthConfig{Username: "admin"}, TrustedProxies: []string{"203.0.113.0/24"}}},
	}
	specs := CollectConfiguredDomainSpecs(services, "")
	if len(specs) != 2 {
		t.Fatalf("specs = %#v, want 2", specs)
	}
	if !specs[0].WarnUntrustedAccessControls {
		t.Fatalf("direct spec = %#v, want warning enabled", specs[0])
	}
	if specs[0].CDN != config.ProxyCDNCloudflare {
		t.Fatalf("direct spec CDN = %q, want cloudflare", specs[0].CDN)
	}
	if specs[1].WarnUntrustedAccessControls {
		t.Fatalf("trusted spec = %#v, want warning disabled", specs[1])
	}
}

func TestApplyDomainCDNPolicyKeepsHeuristicSeparateFromDeclaration(t *testing.T) {
	declared := health.DomainStatus{DNS: health.DomainDNSProxied}
	applyDomainCDNPolicy(&declared, DomainStatusSpec{CDN: config.ProxyCDNGeneric})
	if declared.CDN != config.ProxyCDNGeneric || declared.Warning != "" {
		t.Fatalf("declared status = %#v", declared)
	}

	heuristic := health.DomainStatus{DNS: health.DomainDNSProxied}
	applyDomainCDNPolicy(&heuristic, DomainStatusSpec{})
	if heuristic.CDN != "" || heuristic.State != health.DomainStateWrongDNS || heuristic.DNS != health.DomainDNSWrong || !strings.Contains(heuristic.Warning, "proxy.cdn") {
		t.Fatalf("heuristic status = %#v", heuristic)
	}
	unknown := health.DomainStatus{State: health.DomainStateActive, DNS: health.DomainDNSProxied}
	applyDomainCDNPolicy(&unknown, DomainStatusSpec{CDN: "mystery-edge"})
	if unknown.CDN != "" || unknown.State != health.DomainStateWrongDNS || unknown.DNS != health.DomainDNSWrong {
		t.Fatalf("unknown declaration status = %#v", unknown)
	}
}

func TestApplyDomainAccessControlWarningOnlyForSuspectedCDN(t *testing.T) {
	spec := DomainStatusSpec{WarnUntrustedAccessControls: true}
	proxied := health.DomainStatus{DNS: health.DomainDNSProxied}
	applyDomainAccessControlWarning(&proxied, spec)
	if !strings.Contains(proxied.Warning, "proxy.trustedProxies") {
		t.Fatalf("warning = %q", proxied.Warning)
	}
	direct := health.DomainStatus{DNS: health.DomainDNSOK}
	applyDomainAccessControlWarning(&direct, spec)
	if direct.Warning != "" {
		t.Fatalf("direct warning = %q, want empty", direct.Warning)
	}
	wrong := health.DomainStatus{DNS: health.DomainDNSWrong}
	applyDomainAccessControlWarning(&wrong, spec)
	if !strings.Contains(wrong.Warning, "proxy.trustedProxies") {
		t.Fatalf("wrong-target warning = %q", wrong.Warning)
	}
}
