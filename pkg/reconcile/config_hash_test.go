package reconcile

import (
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestSafeServiceConfigHashStableAcrossOrderOnlyFields(t *testing.T) {
	a := config.ServiceConfig{
		Image:     "nginx:1.27",
		Port:      8080,
		Volumes:   []string{"data:/data", "cache:/cache"},
		DependsOn: []string{"db", "redis"},
		Proxy:     &config.ProxyConfig{Domain: "example.com"},
	}
	b := a
	b.Volumes = []string{"cache:/cache", "data:/data"}
	b.DependsOn = []string{"redis", "db"}

	hashA, ok := SafeServiceConfigHash(a)
	if !ok {
		t.Fatal("expected safe service hash")
	}
	hashB, ok := SafeServiceConfigHash(b)
	if !ok {
		t.Fatal("expected safe service hash")
	}
	if hashA != hashB {
		t.Fatalf("hashes differ for order-only changes: %q != %q", hashA, hashB)
	}
}

func TestSafeServiceConfigHashRejectsEnvMaterial(t *testing.T) {
	tests := map[string]config.ServiceConfig{
		"env":               {Image: "nginx", Env: map[string]string{"TOKEN": "secret"}},
		"envFile":           {Image: "nginx", EnvFile: ".env"},
		"monitoringWebhook": {Image: "nginx", Monitoring: &config.MonitoringConfig{Webhook: "https://hooks.example.test/token"}},
		"secrets":           {Image: "nginx", Secrets: []string{"TOKEN"}},
	}
	for name, service := range tests {
		t.Run(name, func(t *testing.T) {
			if hash, ok := SafeServiceConfigHash(service); ok || hash != "" {
				t.Fatalf("SafeServiceConfigHash() = %q, %v; want rejected", hash, ok)
			}
		})
	}
}

func TestDetectChangesUsesMatchingSafeConfigHash(t *testing.T) {
	service := config.ServiceConfig{
		Image:   "nginx:1.27",
		Port:    8080,
		Proxy:   &config.ProxyConfig{Domain: "example.com"},
		Volumes: []string{"data:/data"},
	}
	hash, ok := SafeServiceConfigHash(service)
	if !ok {
		t.Fatal("expected safe service hash")
	}

	reasons := detectChanges(service, &ActualService{
		Name:       "web",
		Image:      "nginx:1.27",
		Replicas:   1,
		ConfigHash: hash,
		ConfigSnapshot: &config.ServiceConfig{
			Image: "nginx:1.27",
		},
	})
	if len(reasons) != 0 {
		t.Fatalf("detectChanges() reasons = %#v, want none", reasons)
	}
}

func TestDetectChangesDoesNotLetHashHideReplicaDrift(t *testing.T) {
	service := config.ServiceConfig{
		Image:    "nginx:1.27",
		Replicas: 2,
	}
	hash, ok := SafeServiceConfigHash(service)
	if !ok {
		t.Fatal("expected safe service hash")
	}

	reasons := detectChanges(service, &ActualService{
		Name:       "web",
		Image:      "nginx:1.27",
		Replicas:   1,
		ConfigHash: hash,
		ConfigSnapshot: &config.ServiceConfig{
			Image: "nginx:1.27",
		},
	})
	if len(reasons) == 0 {
		t.Fatal("detectChanges() should report replica drift")
	}
}
