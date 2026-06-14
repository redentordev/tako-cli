package examples_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestDeploymentPatternTemplatesLoadAndCoverCommonUseCases(t *testing.T) {
	sshKey := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(sshKey, []byte("test-key"), 0600); err != nil {
		t.Fatalf("failed to write test ssh key: %v", err)
	}
	t.Setenv("SERVER_HOST", "203.0.113.10")
	t.Setenv("SSH_KEY", sshKey)
	t.Setenv("LETSENCRYPT_EMAIL", "admin@example.com")
	t.Setenv("POSTGRES_PASSWORD", "example-password")

	patterns := map[string]func(t *testing.T, cfg *config.Config){
		"00-prebuilt-image": assertPrebuiltImagePattern,
		"01-static-site":    assertStaticSitePattern,
		"02-node-api":       assertNodeAPIPattern,
		"03-volume-sqlite":  assertVolumePattern,
		"04-postgres-app":   assertPostgresAppPattern,
		"05-workers-redis":  assertWorkersPattern,
		"06-cron-runner":    assertCronRunnerPattern,
		"07-python-fastapi": assertFastAPIPattern,
		"08-go-web":         assertGoWebPattern,
	}

	matches, err := filepath.Glob(filepath.Join("deployment-patterns", "*", "tako.yaml"))
	if err != nil {
		t.Fatalf("failed to scan deployment pattern templates: %v", err)
	}
	if len(matches) != len(patterns) {
		t.Fatalf("template count = %d, want %d: %#v", len(matches), len(patterns), matches)
	}

	for _, configPath := range matches {
		patternName := filepath.Base(filepath.Dir(configPath))
		check, ok := patterns[patternName]
		if !ok {
			t.Fatalf("deployment pattern %s is not covered by assertions", patternName)
		}
		t.Run(patternName, func(t *testing.T) {
			cfg := loadPatternConfig(t, configPath)
			check(t, cfg)
		})
	}
}

func loadPatternConfig(t *testing.T, configPath string) *config.Config {
	t.Helper()
	absPath, err := filepath.Abs(configPath)
	if err != nil {
		t.Fatalf("failed to resolve %s: %v", configPath, err)
	}
	dir := filepath.Dir(absPath)
	name := filepath.Base(absPath)
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to read cwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("failed to chdir to %s: %v", dir, err)
	}
	defer func() {
		if err := os.Chdir(oldDir); err != nil {
			t.Fatalf("failed to restore cwd: %v", err)
		}
	}()
	cfg, err := config.LoadConfig(name)
	if err != nil {
		t.Fatalf("LoadConfig(%s) returned error: %v", configPath, err)
	}
	return cfg
}

func productionServices(t *testing.T, cfg *config.Config) map[string]config.ServiceConfig {
	t.Helper()
	services, err := cfg.GetServices("production")
	if err != nil {
		t.Fatalf("GetServices(production) returned error: %v", err)
	}
	return services
}

func assertPrebuiltImagePattern(t *testing.T, cfg *config.Config) {
	services := productionServices(t, cfg)
	web := services["web"]
	if web.Image == "" || web.Build != "" {
		t.Fatalf("prebuilt web should use image only: %#v", web)
	}
	assertPublicService(t, web, 80)
	if web.Replicas != 2 {
		t.Fatalf("prebuilt web replicas = %d, want 2", web.Replicas)
	}
}

func assertStaticSitePattern(t *testing.T, cfg *config.Config) {
	services := productionServices(t, cfg)
	web := services["web"]
	if web.Build != "." || web.Image != "" {
		t.Fatalf("static web should build local nginx image: %#v", web)
	}
	assertPublicService(t, web, 80)
}

func assertNodeAPIPattern(t *testing.T, cfg *config.Config) {
	api := productionServices(t, cfg)["api"]
	assertPublicService(t, api, 3000)
	if api.Replicas != 2 || api.HealthCheck.Path != "/health" {
		t.Fatalf("node api should be replicated with health check: %#v", api)
	}
}

func assertVolumePattern(t *testing.T, cfg *config.Config) {
	web := productionServices(t, cfg)["web"]
	assertPublicService(t, web, 3000)
	if !slices.Contains(web.Volumes, "app_data:/app/data") {
		t.Fatalf("volume template missing app_data mount: %#v", web.Volumes)
	}
	if web.Backup == nil || web.Backup.Schedule == "" || web.Backup.Retain != 14 {
		t.Fatalf("volume template missing backup schedule: %#v", web.Backup)
	}
}

func assertPostgresAppPattern(t *testing.T, cfg *config.Config) {
	services := productionServices(t, cfg)
	web := services["web"]
	postgres := services["postgres"]
	assertPublicService(t, web, 3000)
	if !slices.Contains(web.DependsOn, "postgres") {
		t.Fatalf("web should depend on postgres: %#v", web.DependsOn)
	}
	if postgres.Image == "" || postgres.Port != 0 || !postgres.Persistent || !slices.Contains(postgres.Volumes, "postgres_data:/var/lib/postgresql/data") {
		t.Fatalf("postgres service should be persistent image-backed database: %#v", postgres)
	}
	if postgres.Placement == nil || postgres.Placement.Strategy != "pinned" {
		t.Fatalf("postgres should be pinned for node-local data: %#v", postgres.Placement)
	}
}

func assertWorkersPattern(t *testing.T, cfg *config.Config) {
	services := productionServices(t, cfg)
	assertPublicService(t, services["api"], 3000)
	worker := services["worker"]
	if worker.Port != 0 || worker.Command == "" || worker.Replicas != 3 {
		t.Fatalf("worker should be a replicated background command: %#v", worker)
	}
	redis := services["redis"]
	if redis.Port != 0 || !redis.Persistent || !slices.Contains(redis.Volumes, "redis_data:/data") {
		t.Fatalf("redis should be persistent queue storage: %#v", redis)
	}
}

func assertCronRunnerPattern(t *testing.T, cfg *config.Config) {
	jobs := productionServices(t, cfg)["jobs"]
	if jobs.Port != 0 || jobs.Proxy != nil || jobs.Command == "" {
		t.Fatalf("cron runner should be an internal command-only service: %#v", jobs)
	}
	if !slices.Contains(jobs.Volumes, "cron_logs:/var/log/tako-cron") {
		t.Fatalf("cron runner should keep logs in a named volume: %#v", jobs.Volumes)
	}
}

func assertFastAPIPattern(t *testing.T, cfg *config.Config) {
	api := productionServices(t, cfg)["api"]
	assertPublicService(t, api, 8000)
	if api.Build != "." || api.HealthCheck.Path != "/health" {
		t.Fatalf("fastapi template should build local app with health check: %#v", api)
	}
}

func assertGoWebPattern(t *testing.T, cfg *config.Config) {
	web := productionServices(t, cfg)["web"]
	assertPublicService(t, web, 8080)
	if web.Replicas != 2 || web.LoadBalancer.Strategy != "round_robin" {
		t.Fatalf("go web template should be replicated with round robin load balancing: %#v", web)
	}
}

func assertPublicService(t *testing.T, service config.ServiceConfig, port int) {
	t.Helper()
	if service.Port != port || service.Proxy == nil || service.Proxy.Domain == "" {
		t.Fatalf("service should be public on port %d: %#v", port, service)
	}
}
