package examples_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	t.Setenv("TAKO_SERVER_HOST", "203.0.113.10")
	t.Setenv("TAKO_SSH_KEY", sshKey)
	t.Setenv("LETSENCRYPT_EMAIL", "admin@example.com")
	t.Setenv("POSTGRES_PASSWORD", "example-password")

	patterns := map[string]func(t *testing.T, cfg *config.Config){
		"00-prebuilt-image":        assertPrebuiltImagePattern,
		"01-static-site":           assertStaticSitePattern,
		"02-node-api":              assertNodeAPIPattern,
		"03-volume-sqlite":         assertVolumePattern,
		"04-postgres-app":          assertPostgresAppPattern,
		"05-workers-redis":         assertWorkersPattern,
		"06-cron-runner":           assertCronRunnerPattern,
		"07-python-fastapi":        assertFastAPIPattern,
		"08-go-web":                assertGoWebPattern,
		"09-monorepo-web-api":      assertMonorepoPattern,
		"10-stages-shared-node":    assertStagesPattern,
		"11-websocket-node":        assertWebSocketPattern,
		"12-github-actions-deploy": assertGitHubActionsPattern,
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
			assertBuildContextsExist(t, filepath.Dir(configPath), cfg)
			check(t, cfg)
		})
	}
}

func TestDeploymentPatternDocsAndValidatorCoverCatalog(t *testing.T) {
	patterns := []string{
		"00-prebuilt-image",
		"01-static-site",
		"02-node-api",
		"03-volume-sqlite",
		"04-postgres-app",
		"05-workers-redis",
		"06-cron-runner",
		"07-python-fastapi",
		"08-go-web",
		"09-monorepo-web-api",
		"10-stages-shared-node",
		"11-websocket-node",
		"12-github-actions-deploy",
	}

	readme := readExampleFile(t, filepath.Join("deployment-patterns", "README.md"))
	validateScript := readExampleFile(t, filepath.Join("deployment-patterns", "validate.sh"))
	envExample := readExampleFile(t, filepath.Join("deployment-patterns", ".env.example"))

	for _, pattern := range patterns {
		if !strings.Contains(readme, "`"+pattern+"`") {
			t.Fatalf("deployment pattern README missing %s", pattern)
		}
	}

	for _, expected := range []string{
		"SERVER_HOST",
		"TAKO_SERVER_HOST",
		"SSH_KEY",
		"TAKO_SSH_KEY",
		"LETSENCRYPT_EMAIL",
		"POSTGRES_PASSWORD",
	} {
		if !strings.Contains(envExample, expected) {
			t.Fatalf("deployment pattern .env.example missing %s", expected)
		}
	}

	for _, expected := range []string{
		"*/tako.yaml",
		"go build -o \"$TMP_DIR/tako\" \"$ROOT\"",
		"\"$TMP_DIR/tako\" --config tako.yaml --env production validate --quiet",
		"go test ./examples",
	} {
		if !strings.Contains(validateScript, expected) {
			t.Fatalf("deployment pattern validator missing %q", expected)
		}
	}
}

func TestExamplesDoNotUseKnownDemoDatabasePasswords(t *testing.T) {
	forbidden := []string{
		"dbpassword123",
		"postgres:secret123",
		"POSTGRES_PASSWORD: secret123",
		"MYSQL_ROOT_PASSWORD: secret\n",
		"POSTGRES_PASSWORD: changeme",
		"POSTGRES_PASSWORD=changeme",
		":changeme@",
		"changeme123",
		"console.log(`Database: ${process.env.DATABASE_URL}`)",
	}

	err := filepath.WalkDir(".", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case "node_modules", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		shouldScan := entry.Name() == ".env.example"
		if !shouldScan {
			switch filepath.Ext(path) {
			case ".js", ".md", ".yaml", ".yml":
				shouldScan = true
			}
		}
		if !shouldScan {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		content := string(data)
		for _, pattern := range forbidden {
			if strings.Contains(content, pattern) {
				t.Fatalf("%s contains unsafe copyable demo credential %q", path, strings.TrimSpace(pattern))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to scan examples: %v", err)
	}
}

func readExampleFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %s: %v", path, err)
	}
	return string(data)
}

func TestGitHubActionsDeploymentPatternIncludesStateWorkflow(t *testing.T) {
	workflowPath := filepath.Join("deployment-patterns", "12-github-actions-deploy", ".github", "workflows", "deploy.yml")
	data, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("failed to read GitHub Actions workflow: %v", err)
	}
	content := string(data)
	for _, expected := range []string{
		"TAKO_NONINTERACTIVE: \"1\"",
		"TAKO_HOST_KEY_MODE: strict",
		"tako validate -e production",
		"tako upgrade servers -e production --dry-run",
		"tako env pull -e production --force",
		"tako state pull -e production",
		"tako state lease",
		"tako deploy -e production --yes",
	} {
		if !strings.Contains(content, expected) {
			t.Fatalf("GitHub Actions deployment template missing %q", expected)
		}
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

func assertMonorepoPattern(t *testing.T, cfg *config.Config) {
	services := productionServices(t, cfg)
	web := services["web"]
	api := services["api"]
	assertPublicService(t, web, 3000)
	if web.Build != "./web" || !slices.Contains(web.DependsOn, "api") || web.Env["API_URL"] != "http://api:4000" {
		t.Fatalf("monorepo web should use its own context and depend on internal api: %#v", web)
	}
	if api.Build != "./api" || api.Proxy != nil || api.Port != 4000 || api.Replicas != 2 {
		t.Fatalf("monorepo api should be internal and replicated: %#v", api)
	}
}

func assertStagesPattern(t *testing.T, cfg *config.Config) {
	if cfg.Project.Name != "pattern-stages" {
		t.Fatalf("stage template project name = %q", cfg.Project.Name)
	}
	preview, err := cfg.GetService("preview", "web")
	if err != nil {
		t.Fatalf("preview web service missing: %v", err)
	}
	production, err := cfg.GetService("production", "web")
	if err != nil {
		t.Fatalf("production web service missing: %v", err)
	}
	if preview.Proxy == nil || production.Proxy == nil {
		t.Fatalf("stage services should both be public: preview=%#v production=%#v", preview, production)
	}
	if preview.Proxy.Domain == production.Proxy.Domain {
		t.Fatalf("stage domains must differ: %q", preview.Proxy.Domain)
	}
	if preview.Env["APP_STAGE"] != "preview" || production.Env["APP_STAGE"] != "production" {
		t.Fatalf("stage env values should make runtime identity visible: preview=%#v production=%#v", preview.Env, production.Env)
	}
}

func assertWebSocketPattern(t *testing.T, cfg *config.Config) {
	realtime := productionServices(t, cfg)["realtime"]
	assertPublicService(t, realtime, 3000)
	if realtime.Replicas != 2 || realtime.LoadBalancer.Strategy != "sticky" {
		t.Fatalf("websocket template should use replicated sticky balancing: %#v", realtime)
	}
}

func assertGitHubActionsPattern(t *testing.T, cfg *config.Config) {
	web := productionServices(t, cfg)["web"]
	assertPublicService(t, web, 3000)
	if cfg.Servers["prod"].User != "deploy" || cfg.Servers["prod"].SSHKey == "" {
		t.Fatalf("ci template should use deploy user and CI-provided ssh key: %#v", cfg.Servers["prod"])
	}
	if web.Replicas != 2 || web.HealthCheck.Path != "/health" {
		t.Fatalf("ci template should be replicated with health check: %#v", web)
	}
}

func assertPublicService(t *testing.T, service config.ServiceConfig, port int) {
	t.Helper()
	if service.Port != port || service.Proxy == nil || service.Proxy.Domain == "" {
		t.Fatalf("service should be public on port %d: %#v", port, service)
	}
}

func assertBuildContextsExist(t *testing.T, patternDir string, cfg *config.Config) {
	t.Helper()
	checked := map[string]bool{}
	for envName, env := range cfg.Environments {
		for serviceName, service := range env.Services {
			if service.Build == "" {
				continue
			}
			buildPath := filepath.Clean(filepath.Join(patternDir, service.Build))
			if checked[buildPath] {
				continue
			}
			checked[buildPath] = true
			info, err := os.Stat(buildPath)
			if err != nil {
				t.Fatalf("%s/%s build context %s is not readable: %v", envName, serviceName, buildPath, err)
			}
			if !info.IsDir() {
				t.Fatalf("%s/%s build context %s is not a directory", envName, serviceName, buildPath)
			}
			if _, err := os.Stat(filepath.Join(buildPath, "Dockerfile")); err != nil {
				t.Fatalf("%s/%s build context %s has no Dockerfile: %v", envName, serviceName, buildPath, err)
			}
		}
	}
}
