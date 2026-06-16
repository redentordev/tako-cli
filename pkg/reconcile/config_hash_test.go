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
		DependsOn: []string{"db", "redis"},
		Proxy:     &config.ProxyConfig{Domain: "example.com"},
	}
	b := a
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
		"env":               {Image: "nginx", Env: map[string]config.EnvValue{"TOKEN": config.PlainEnvValue("secret")}},
		"envFile":           {Image: "nginx", EnvFile: ".env"},
		"monitoringWebhook": {Image: "nginx", Monitoring: &config.MonitoringConfig{Webhook: "https://hooks.example.test/token"}},
		"secrets":           {Image: "nginx", Secrets: []string{"TOKEN"}},
		"volumes":           {Image: "nginx", Volumes: []string{"data:/data"}},
		"hookEnv":           {Image: "nginx", Hooks: config.HooksConfig{PreDeploy: &config.HookConfig{Command: "echo ok", Env: map[string]string{"TOKEN": "secret"}}}},
		"hookSecrets":       {Image: "nginx", Hooks: config.HooksConfig{PreDeploy: &config.HookConfig{Command: "echo ok", Secrets: []string{"TOKEN"}}}},
	}
	for name, service := range tests {
		t.Run(name, func(t *testing.T) {
			if hash, ok := SafeServiceConfigHash(service); ok || hash != "" {
				t.Fatalf("SafeServiceConfigHash() = %q, %v; want rejected", hash, ok)
			}
		})
	}
}

func TestSafeServiceConfigHashIncludesHookCommands(t *testing.T) {
	base := config.ServiceConfig{
		Image: "nginx",
		Hooks: config.HooksConfig{
			PreDeploy: &config.HookConfig{Command: "echo one", Timeout: "5m"},
		},
	}
	changed := base
	changed.Hooks.PreDeploy = &config.HookConfig{Command: "echo two", Timeout: "5m"}

	hashA, ok := SafeServiceConfigHash(base)
	if !ok {
		t.Fatal("expected safe hash for hook without env material")
	}
	hashB, ok := SafeServiceConfigHash(changed)
	if !ok {
		t.Fatal("expected safe hash for changed hook without env material")
	}
	if hashA == hashB {
		t.Fatal("hook command change should change safe config hash")
	}
}

func TestSafeServiceConfigHashIncludesExplicitPorts(t *testing.T) {
	base := config.ServiceConfig{
		Image: "nginx:1.27",
		Ports: []config.PortConfig{
			{Name: "http", Target: 3000, Mode: "proxy", Protocol: "http", Proxy: &config.ProxyConfig{Domain: "example.com"}},
			{Name: "metrics", Target: 9090, Mode: "internal", Protocol: "tcp"},
		},
	}
	changed := base
	changed.Ports = append([]config.PortConfig(nil), base.Ports...)
	changed.Ports[1].Target = 9191

	hashA, ok := SafeServiceConfigHash(base)
	if !ok {
		t.Fatal("expected safe hash for explicit ports")
	}
	hashB, ok := SafeServiceConfigHash(changed)
	if !ok {
		t.Fatal("expected safe hash for changed explicit ports")
	}
	if hashA == hashB {
		t.Fatal("explicit port change should change safe config hash")
	}
}

func TestSafeServiceConfigHashIncludesPlatform(t *testing.T) {
	base := config.ServiceConfig{
		Build:    ".",
		Platform: "linux/amd64",
	}
	changed := base
	changed.Platform = "linux/arm64"

	hashA, ok := SafeServiceConfigHash(base)
	if !ok {
		t.Fatal("expected safe hash for platform config")
	}
	hashB, ok := SafeServiceConfigHash(changed)
	if !ok {
		t.Fatal("expected safe hash for changed platform config")
	}
	if hashA == hashB {
		t.Fatal("platform change should change safe config hash")
	}
}

func TestSafeServiceConfigHashIncludesDockerfile(t *testing.T) {
	base := config.ServiceConfig{
		Build:      ".",
		Dockerfile: "Dockerfile",
	}
	changed := base
	changed.Dockerfile = "Dockerfile.renderer"

	hashA, ok := SafeServiceConfigHash(base)
	if !ok {
		t.Fatal("expected safe hash for dockerfile config")
	}
	hashB, ok := SafeServiceConfigHash(changed)
	if !ok {
		t.Fatal("expected safe hash for changed dockerfile config")
	}
	if hashA == hashB {
		t.Fatal("dockerfile change should change safe config hash")
	}
}

func TestSafeServiceConfigHashIncludesConfigFileContentHash(t *testing.T) {
	base := config.ServiceConfig{
		Image: "caddy:2.9-alpine",
		Configs: []config.ServiceConfigFileMount{{
			Source:      "caddyfile",
			Target:      "/etc/caddy/Caddyfile",
			Mode:        "0444",
			ContentHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		}},
	}
	changed := base
	changed.Configs = append([]config.ServiceConfigFileMount(nil), base.Configs...)
	changed.Configs[0].ContentHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	hashA, ok := SafeServiceConfigHash(base)
	if !ok {
		t.Fatal("expected safe hash for config file mount")
	}
	hashB, ok := SafeServiceConfigHash(changed)
	if !ok {
		t.Fatal("expected safe hash for changed config file mount")
	}
	if hashA == hashB {
		t.Fatal("config file content hash change should change safe config hash")
	}
}

func TestDetectChangesUsesMatchingSafeConfigHash(t *testing.T) {
	service := config.ServiceConfig{
		Image: "nginx:1.27",
		Port:  8080,
		Proxy: &config.ProxyConfig{Domain: "example.com"},
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

func TestDetectChangesComparesConfigFilesWhenSafeHashUnavailable(t *testing.T) {
	desired := config.ServiceConfig{
		Image:   "caddy:2.9-alpine",
		Volumes: []string{"caddy_data:/data"},
		Configs: []config.ServiceConfigFileMount{{
			Source:      "caddyfile",
			Target:      "/etc/caddy/Caddyfile",
			Mode:        "0444",
			ContentHash: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		}},
	}
	old := desired
	old.Configs = append([]config.ServiceConfigFileMount(nil), desired.Configs...)
	old.Configs[0].ContentHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	reasons := detectChanges("demo", "production", "edge", desired, &ActualService{
		Name:           "edge",
		Image:          "caddy:2.9-alpine",
		Replicas:       1,
		RuntimeID:      runtimeid.ServiceIdentity("demo", "production", "edge"),
		ConfigSnapshot: &old,
	})
	if len(reasons) == 0 {
		t.Fatal("detectChanges() should report config file changes when safe hash is unavailable")
	}
}
