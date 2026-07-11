package config

import (
	"strings"
	"testing"
)

func multiDomainValidationConfig(mutate func(*ProxyConfig)) *Config {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Proxy = &EnvironmentProxyConfig{
		Placement: &PlacementConfig{Strategy: "pinned", Servers: []string{"node-a"}},
	}
	web := production.Services["web"]
	web.Proxy = &ProxyConfig{
		Domain:  "example.com",
		Domains: []string{"app.example.com", "example.app"},
	}
	if mutate != nil {
		mutate(web.Proxy)
	}
	production.Services["web"] = web
	cfg.Environments["production"] = production
	return cfg
}

func TestValidateConfigAcceptsAdditionalServingDomains(t *testing.T) {
	cfg := multiDomainValidationConfig(nil)
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
	proxy := cfg.Environments["production"].Services["web"].Proxy
	got := proxy.GetAllDomains()
	want := []string{"example.com", "app.example.com", "example.app"}
	if len(got) != len(want) {
		t.Fatalf("GetAllDomains = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("GetAllDomains = %v, want %v (primary first)", got, want)
		}
	}
	hosts := proxy.GetAllHosts()
	if len(hosts) != 3 {
		t.Fatalf("GetAllHosts = %v, want all serving domains", hosts)
	}
}

func TestValidateConfigNormalizesAndValidatesCDN(t *testing.T) {
	cfg := multiDomainValidationConfig(func(p *ProxyConfig) { p.CDN = " Cloudflare " })
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
	if got := cfg.Environments["production"].Services["web"].Proxy.CDN; got != ProxyCDNCloudflare {
		t.Fatalf("proxy.cdn = %q, want cloudflare", got)
	}

	cfg = multiDomainValidationConfig(func(p *ProxyConfig) { p.CDN = "mystery-edge" })
	if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "invalid proxy.cdn") {
		t.Fatalf("invalid proxy.cdn error = %v", err)
	}
}

func TestValidateConfigRejectsCDNOnInternalProxy(t *testing.T) {
	cfg := multiDomainValidationConfig(func(p *ProxyConfig) {
		p.Visibility = ProxyVisibilityInternal
		p.Domain = ""
		p.Domains = nil
		p.CDN = ProxyCDNGeneric
	})
	if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "proxy.cdn requires public") {
		t.Fatalf("internal proxy.cdn error = %v", err)
	}
}

func TestValidateConfigWarnsForDynamicDomainsBehindCDN(t *testing.T) {
	cfg := multiDomainValidationConfig(func(p *ProxyConfig) {
		p.CDN = ProxyCDNCloudflare
		p.DynamicDomains = &DynamicDomainsConfig{Ask: "web:/api/domains/authorize"}
	})
	production := cfg.Environments["production"]
	web := production.Services["web"]
	web.Port = 3000
	production.Services["web"] = web
	cfg.Environments["production"] = production
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
	warnings := ValidationWarnings(cfg)
	if len(warnings) != 1 || warnings[0].Field != "proxy.dynamicDomains" || !strings.Contains(warnings[0].Message, "dynamicDomains behind proxy.cdn=cloudflare") {
		t.Fatalf("warnings = %#v", warnings)
	}
}

func TestValidateConfigClearsBlankCDNOnInternalProxy(t *testing.T) {
	cfg := multiDomainValidationConfig(func(p *ProxyConfig) {
		p.Visibility = ProxyVisibilityInternal
		p.Domain = ""
		p.Domains = nil
		p.CDN = "   "
	})
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
	if got := cfg.Environments["production"].Services["web"].Proxy.CDN; got != "" {
		t.Fatalf("internal blank proxy.cdn = %q, want empty", got)
	}
}

func TestValidateConfigNormalizesAdditionalDomains(t *testing.T) {
	cfg := multiDomainValidationConfig(func(p *ProxyConfig) {
		p.Domains = []string{" app.example.com "}
	})
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
	proxy := cfg.Environments["production"].Services["web"].Proxy
	if proxy.Domains[0] != "app.example.com" {
		t.Fatalf("additional domain = %q, want trimmed", proxy.Domains[0])
	}
}

func TestValidateConfigRejectsInvalidAdditionalDomains(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ProxyConfig)
		want   string
	}{
		{"wildcard", func(p *ProxyConfig) { p.Domains = []string{"*.example.com"} }, "wildcard proxy domain"},
		{"duplicate of primary", func(p *ProxyConfig) { p.Domains = []string{"Example.com"} }, "duplicate serving domain"},
		{"duplicate within domains", func(p *ProxyConfig) { p.Domains = []string{"app.example.com", "app.example.com"} }, "duplicate serving domain"},
		{"redirect collides with extra", func(p *ProxyConfig) { p.RedirectFrom = []string{"app.example.com"} }, "already the serving domain"},
		{"without primary", func(p *ProxyConfig) {
			p.Domain = ""
			p.DynamicDomains = &DynamicDomainsConfig{Ask: "web:/ask"}
		}, "requires a primary proxy domain"},
		{"injection", func(p *ProxyConfig) { p.Domains = []string{"app.example.com`) || PathPrefix(`/"} }, "invalid additional domain"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := multiDomainValidationConfig(tt.mutate)
			err := ValidateConfig(cfg)
			if err == nil {
				t.Fatal("ValidateConfig should reject the config")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want %q", err, tt.want)
			}
		})
	}
}

func TestValidateConfigRejectsInternalProxyWithDomains(t *testing.T) {
	cfg := multiDomainValidationConfig(func(p *ProxyConfig) {
		p.Visibility = ProxyVisibilityInternal
		p.Domain = ""
	})
	err := ValidateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "requires public proxy visibility") {
		t.Fatalf("error = %v, want public-visibility rejection", err)
	}
}

func TestValidateConfigRejectsCrossServiceDomainConflictViaDomains(t *testing.T) {
	cfg := multiDomainValidationConfig(nil)
	production := cfg.Environments["production"]
	production.Services["api"] = ServiceConfig{
		Image: "nginx:alpine",
		Proxy: &ProxyConfig{Domain: "app.example.com"},
	}
	cfg.Environments["production"] = production
	err := ValidateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "domain conflict") {
		t.Fatalf("error = %v, want cross-service domain conflict", err)
	}
}
