package reconcile

import (
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
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

func TestSafeServiceConfigHashRedactsEnvAndSecretValues(t *testing.T) {
	a := config.ServiceConfig{
		Image:      "nginx",
		Dockerfile: "Dockerfile.web",
		Env:        map[string]string{"TOKEN": "secret-one"},
		EnvFile:    ".env.production",
		Secrets:    []string{"DATABASE_URL", "API_TOKEN:API_TOKEN"},
		Monitoring: &config.MonitoringConfig{Webhook: "https://hooks.example.test/one"},
	}
	b := a
	b.Env = map[string]string{"TOKEN": "secret-two"}
	b.Monitoring = &config.MonitoringConfig{Webhook: "https://hooks.example.test/two"}

	hashA, ok := SafeServiceConfigHash(a)
	if !ok {
		t.Fatal("expected safe redacted service hash")
	}
	hashB, ok := SafeServiceConfigHash(b)
	if !ok {
		t.Fatal("expected safe redacted service hash")
	}
	if hashA != hashB {
		t.Fatalf("hashes should ignore raw env/webhook values: %q != %q", hashA, hashB)
	}

	c := a
	c.Env = map[string]string{"OTHER_TOKEN": "secret-one"}
	hashC, ok := SafeServiceConfigHash(c)
	if !ok {
		t.Fatal("expected safe redacted service hash")
	}
	if hashA == hashC {
		t.Fatal("hash should change when env keys change")
	}

	d := a
	d.Secrets = []string{"DATABASE_URL"}
	hashD, ok := SafeServiceConfigHash(d)
	if !ok {
		t.Fatal("expected safe redacted service hash")
	}
	if hashA == hashD {
		t.Fatal("hash should change when secret refs change")
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

	reasons := detectChanges("demo", "production", "web", service, &ActualService{
		Name:       "web",
		Image:      "nginx:1.27",
		Replicas:   1,
		ConfigHash: hash,
		RuntimeID:  runtimeid.ServiceIdentity("demo", "production", "web"),
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

	reasons := detectChanges("demo", "production", "web", service, &ActualService{
		Name:       "web",
		Image:      "nginx:1.27",
		Replicas:   1,
		ConfigHash: hash,
		RuntimeID:  runtimeid.ServiceIdentity("demo", "production", "web"),
		ConfigSnapshot: &config.ServiceConfig{
			Image: "nginx:1.27",
		},
	})
	if len(reasons) == 0 {
		t.Fatal("detectChanges() should report replica drift")
	}
}

func TestDetectChangesDoesNotLetHashHideRuntimeIdentityDrift(t *testing.T) {
	service := config.ServiceConfig{
		Image: "nginx:1.27",
		Port:  8080,
	}
	hash, ok := SafeServiceConfigHash(service)
	if !ok {
		t.Fatal("expected safe service hash")
	}

	reasons := detectChanges("demo", "production", "web", service, &ActualService{
		Name:       "web",
		Image:      "nginx:1.27",
		Replicas:   1,
		ConfigHash: hash,
		ConfigSnapshot: &config.ServiceConfig{
			Image: "nginx:1.27",
		},
	})
	if len(reasons) == 0 {
		t.Fatal("detectChanges() should report runtime identity drift")
	}
	if reasons[0] != "Runtime identity changed" {
		t.Fatalf("first reason = %q, want runtime identity drift", reasons[0])
	}
}
