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

func TestSafeServiceConfigHashTracksPublishedPorts(t *testing.T) {
	base := config.ServiceConfig{Image: "itzg/minecraft-server", Port: 0}
	baseHash, ok := SafeServiceConfigHash(base)
	if !ok {
		t.Fatal("expected safe service hash")
	}

	published := base
	published.Ports = []string{"25565:25565/tcp"}
	publishedHash, ok := SafeServiceConfigHash(published)
	if !ok {
		t.Fatal("expected safe service hash")
	}
	if publishedHash == baseHash {
		t.Fatal("adding ports must change the config hash so redeploys rebind")
	}
}

func TestSafeServiceConfigHashTracksOperatorFileContentWithoutLocalSourcePath(t *testing.T) {
	first := config.ServiceConfig{
		Image: "nginx:1.27", Files: []config.ServiceFileConfig{{Source: "/checkout-a/nginx.conf", Target: "/etc/nginx/nginx.conf", Secret: true}},
		FilesContentHash: "sha256:first",
	}
	second := first
	second.Files = []config.ServiceFileConfig{{Source: "/checkout-b/nginx.conf", Target: "/etc/nginx/nginx.conf", Secret: true}}
	firstHash, _ := SafeServiceConfigHash(first)
	secondHash, _ := SafeServiceConfigHash(second)
	if firstHash != secondHash {
		t.Fatalf("local source paths changed service hash: %q != %q", firstHash, secondHash)
	}
	second.FilesContentHash = "sha256:second"
	changedHash, _ := SafeServiceConfigHash(second)
	if changedHash == firstHash {
		t.Fatal("operator file content hash did not change service hash")
	}
}

func TestSafeServiceConfigHashTracksContainerGapAFields(t *testing.T) {
	base := config.ServiceConfig{Image: "busybox:latest"}
	baseHash, ok := SafeServiceConfigHash(base)
	if !ok {
		t.Fatal("expected safe service hash")
	}
	variants := []config.ServiceConfig{
		func() config.ServiceConfig {
			value := base
			value.Command = config.ListValue("echo", "ok")
			return value
		}(),
		func() config.ServiceConfig {
			value := base
			value.Entrypoint = config.ListValue("/bin/sh", "-e")
			return value
		}(),
		func() config.ServiceConfig {
			value := base
			value.Labels = map[string]string{"com.example.role": "worker"}
			return value
		}(),
		func() config.ServiceConfig { value := base; value.HealthCheck.Command = "true"; return value }(),
	}
	for i, variant := range variants {
		hash, ok := SafeServiceConfigHash(variant)
		if !ok {
			t.Fatalf("variant %d did not hash", i)
		}
		if hash == baseHash {
			t.Fatalf("variant %d did not change config hash", i)
		}
	}
}

func TestSafeServiceConfigHashTracksResourceLimits(t *testing.T) {
	base := config.ServiceConfig{Image: "nginx:1.27", Port: 8080}
	baseHash, ok := SafeServiceConfigHash(base)
	if !ok {
		t.Fatal("expected safe service hash")
	}

	// An empty resources block must not change existing hashes.
	empty := base
	empty.Resources = &config.ResourceLimitsConfig{}
	emptyHash, ok := SafeServiceConfigHash(empty)
	if !ok {
		t.Fatal("expected safe service hash")
	}
	if emptyHash != baseHash {
		t.Fatalf("empty resources changed the hash: %q != %q", emptyHash, baseHash)
	}

	limited := base
	limited.Resources = &config.ResourceLimitsConfig{Memory: "512m", CPUs: "1.5"}
	limitedHash, ok := SafeServiceConfigHash(limited)
	if !ok {
		t.Fatal("expected safe service hash")
	}
	if limitedHash == baseHash {
		t.Fatal("resource limits should change the hash so limit edits trigger UPDATE plans")
	}

	changed := base
	changed.Resources = &config.ResourceLimitsConfig{Memory: "512m", CPUs: "2"}
	changedHash, ok := SafeServiceConfigHash(changed)
	if !ok {
		t.Fatal("expected safe service hash")
	}
	if changedHash == limitedHash {
		t.Fatal("cpu limit change should change the hash")
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
		Backup: &config.BackupConfig{
			Schedule: "@daily",
			Storage: &config.BackupStorageConfig{
				Provider:        config.BackupStorageProviderS3,
				Bucket:          "backups",
				Region:          "us-east-1",
				AccessKeyID:     "access-one",
				SecretAccessKey: "secret-one",
			},
		},
	}
	b := a
	b.Env = map[string]string{"TOKEN": "secret-two"}
	b.Monitoring = &config.MonitoringConfig{Webhook: "https://hooks.example.test/two"}
	b.Backup = &config.BackupConfig{
		Schedule: "@daily",
		Storage: &config.BackupStorageConfig{
			Provider:        config.BackupStorageProviderS3,
			Bucket:          "backups",
			Region:          "us-east-1",
			AccessKeyID:     "access-two",
			SecretAccessKey: "secret-two",
		},
	}

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

func TestSafeServiceConfigHashIncludesBuildAndRuntimeControls(t *testing.T) {
	base := config.ServiceConfig{Image: "demo/web:latest"}
	baseHash, ok := SafeServiceConfigHash(base)
	if !ok {
		t.Fatal("expected base hash")
	}
	variants := []config.ServiceConfig{
		{Image: "demo/web:latest", BuildArgs: map[string]string{"BASE": "alpine"}},
		{Image: "demo/web:latest", BuildTarget: "runtime"},
		{Image: "demo/web:latest", EnvFiles: []string{".env.base", ".env.prod"}},
		{Image: "demo/web:latest", User: "1000"},
		{Image: "demo/web:latest", WorkingDir: "/work"},
		{Image: "demo/web:latest", StopGracePeriod: "60s"},
		{Image: "demo/web:latest", Init: true},
		{Image: "demo/web:latest", ExtraHosts: []string{"db:10.0.0.2"}},
		{Image: "demo/web:latest", Ulimits: map[string]config.UlimitConfig{"nofile": {Soft: 1, Hard: 1}}},
		{Image: "demo/web:latest", ShmSize: "256m"},
	}
	for index, variant := range variants {
		hash, ok := SafeServiceConfigHash(variant)
		if !ok || hash == baseHash {
			t.Fatalf("variant %d did not change hash", index)
		}
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

func TestDetectChangesDoesNotLetHashHidePersistenceMetadataDrift(t *testing.T) {
	service := config.ServiceConfig{
		Image:      "postgres:16-alpine",
		Persistent: true,
		Volumes:    []string{"pgdata:/var/lib/postgresql/data"},
	}
	hash, ok := SafeServiceConfigHash(service)
	if !ok {
		t.Fatal("expected safe service hash")
	}

	reasons := detectChanges("demo", "production", "postgres", service, &ActualService{
		Name:       "postgres",
		Image:      "postgres:16-alpine",
		Replicas:   1,
		ConfigHash: hash,
		RuntimeID:  runtimeid.ServiceIdentity("demo", "production", "postgres"),
		ConfigSnapshot: &config.ServiceConfig{
			Image: "postgres:16-alpine",
		},
	})
	if len(reasons) == 0 {
		t.Fatal("detectChanges() should report persistence metadata drift")
	}
	if reasons[0] != "Persistence metadata changed" {
		t.Fatalf("first reason = %q, want persistence metadata drift", reasons[0])
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

func TestSafeServiceConfigHashIncludesSharedBuildDefinition(t *testing.T) {
	service := config.ServiceConfig{ImageFrom: "application", SharedBuildHash: "first"}
	first, ok := SafeServiceConfigHash(service)
	if !ok {
		t.Fatal("first hash failed")
	}
	service.SharedBuildHash = "second"
	second, ok := SafeServiceConfigHash(service)
	if !ok || first == second {
		t.Fatalf("hashes = %q %q", first, second)
	}
}
