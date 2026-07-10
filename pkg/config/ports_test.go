package config

import (
	"strings"
	"testing"
)

func TestParsePortPublish(t *testing.T) {
	cases := []struct {
		entry string
		want  string
	}{
		{"3000", "3000:3000/tcp"},
		{" 3000:8080 ", "3000:8080/tcp"},
		{"25565/udp", "25565:25565/udp"},
		{"3000:8080/TCP", "3000:8080/tcp"},
		{"127.0.0.1:9000:3000", "127.0.0.1:9000:3000/tcp"},
		{"[::1]:9000:3000/udp", "[::1]:9000:3000/udp"},
	}
	for _, tc := range cases {
		publish, err := ParsePortPublish(tc.entry)
		if err != nil {
			t.Fatalf("ParsePortPublish(%q) returned error: %v", tc.entry, err)
		}
		if publish.String() != tc.want {
			t.Fatalf("ParsePortPublish(%q) = %q, want %q", tc.entry, publish.String(), tc.want)
		}
	}
}

func TestParsePortPublishRejectsInvalidEntries(t *testing.T) {
	for _, entry := range []string{
		"",
		"   ",
		"0:3000",
		"70000",
		"3000:0",
		"abc",
		"3000:8080/sctp",
		"1.2.3.4.5:80:80",
		"a:b:c:d",
		"[::1]:9000",
	} {
		if _, err := ParsePortPublish(entry); err == nil {
			t.Fatalf("ParsePortPublish(%q) should fail", entry)
		}
	}
}

func portsValidationConfig(mutate func(*EnvironmentConfig, *ServiceConfig)) *Config {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	production.Servers = []string{"node-a"}
	web := production.Services["web"]
	web.Ports = []string{"3000:3000"}
	if mutate != nil {
		mutate(&production, &web)
	}
	production.Services["web"] = web
	cfg.Environments["production"] = production
	return cfg
}

func TestValidateServicePortsNormalizesEntries(t *testing.T) {
	cfg := portsValidationConfig(func(_ *EnvironmentConfig, web *ServiceConfig) {
		web.Ports = []string{" 25565 ", "9000:3000/udp"}
	})
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
	got := cfg.Environments["production"].Services["web"].Ports
	if got[0] != "25565:25565/tcp" || got[1] != "9000:3000/udp" {
		t.Fatalf("ports not normalized: %#v", got)
	}
}

func TestValidateServicePortsRejections(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*EnvironmentConfig, *ServiceConfig)
		wantErr string
	}{
		{
			"rolling strategy",
			func(_ *EnvironmentConfig, web *ServiceConfig) {
				web.Deploy.Strategy = DeployStrategyRolling
			},
			"requires deploy.strategy recreate",
		},
		{
			"multiple replicas",
			func(_ *EnvironmentConfig, web *ServiceConfig) {
				web.Replicas = 2
			},
			"at most one replica",
		},
		{
			"duplicate host port in service",
			func(_ *EnvironmentConfig, web *ServiceConfig) {
				web.Ports = []string{"3000:3000", "3000:8080"}
			},
			"duplicate ports host port 3000/tcp",
		},
		{
			"job with ports",
			func(env *EnvironmentConfig, _ *ServiceConfig) {
				env.Services["worker"] = ServiceConfig{
					Kind:     ServiceKindJob,
					Image:    "alpine",
					Schedule: "@hourly",
					Command:  StringValue("true"),
					Ports:    []string{"9000"},
				}
			},
			"kind: job cannot publish ports",
		},
		{
			"invalid entry",
			func(_ *EnvironmentConfig, web *ServiceConfig) {
				web.Ports = []string{"70000"}
			},
			"must be between 1 and 65535",
		},
		{
			"proxy port collision",
			func(env *EnvironmentConfig, web *ServiceConfig) {
				configureValidationWebProxy(env, web)
				web.Ports = []string{"443:8443"}
			},
			"reserved by tako-proxy",
		},
		{
			"cross-service duplicate",
			func(env *EnvironmentConfig, _ *ServiceConfig) {
				env.Services["game"] = ServiceConfig{Image: "alpine", Ports: []string{"3000/tcp"}}
			},
			"both publish host port 3000/tcp",
		},
		{
			"multi-node without placement",
			func(env *EnvironmentConfig, _ *ServiceConfig) {
				env.Servers = []string{"node-a", "node-b"}
			},
			"placement.strategy pinned or global",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := portsValidationConfig(tc.mutate)
			err := ValidateConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateServicePortsMultiNodePinnedAllowed(t *testing.T) {
	cfg := portsValidationConfig(func(env *EnvironmentConfig, web *ServiceConfig) {
		env.Servers = []string{"node-a", "node-b"}
		web.Placement = &PlacementConfig{Strategy: "pinned", Servers: []string{"node-a"}}
	})
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
}

func TestValidateServicePortsUDPDoesNotCollideWithTCP(t *testing.T) {
	cfg := portsValidationConfig(func(_ *EnvironmentConfig, web *ServiceConfig) {
		web.Ports = []string{"3000:3000", "3000:3000/udp"}
	})
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
}
