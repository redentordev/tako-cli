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
	t.Setenv("APP_SERVER_HOST", "203.0.113.20")
	t.Setenv("APP_SSH_KEY", sshKey)
	t.Setenv("EDGE_SERVER_HOST", "203.0.113.30")
	t.Setenv("EDGE_SSH_KEY", sshKey)
	t.Setenv("LETSENCRYPT_EMAIL", "admin@example.com")
	t.Setenv("POSTGRES_PASSWORD", "example-password")
	t.Setenv("JARDIN_ADMIN_HOST", "admin.example.com")
	t.Setenv("JARDIN_SITE_HOST", "sites.example.com")
	t.Setenv("JARDIN_ENV_FILE", ".env.example")

	patterns := map[string]func(t *testing.T, cfg *config.Config){
		"00-prebuilt-image":              assertPrebuiltImagePattern,
		"01-static-site":                 assertStaticSitePattern,
		"02-node-api":                    assertNodeAPIPattern,
		"03-volume-sqlite":               assertVolumePattern,
		"04-postgres-app":                assertPostgresAppPattern,
		"05-workers-redis":               assertWorkersPattern,
		"06-cron-runner":                 assertCronRunnerPattern,
		"07-python-fastapi":              assertFastAPIPattern,
		"08-go-web":                      assertGoWebPattern,
		"09-monorepo-web-api":            assertMonorepoPattern,
		"10-stages-shared-node":          assertStagesPattern,
		"11-websocket-node":              assertWebSocketPattern,
		"12-github-actions-deploy":       assertGitHubActionsPattern,
		"cms-dynamic-domains/app":        assertCMSDynamicDomainsAppPattern,
		"cms-dynamic-domains/edge":       assertCMSDynamicDomainsEdgePattern,
		"next-admin-renderer-mongo/app":  assertNextAdminRendererMongoAppPattern,
		"next-admin-renderer-mongo/edge": assertNextAdminRendererMongoEdgePattern,
	}

	matches, err := deploymentPatternConfigPaths()
	if err != nil {
		t.Fatalf("failed to scan deployment pattern templates: %v", err)
	}
	if len(matches) != len(patterns) {
		t.Fatalf("template count = %d, want %d: %#v", len(matches), len(patterns), matches)
	}

	for _, configPath := range matches {
		patternName := deploymentPatternID(configPath)
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
		"cms-dynamic-domains",
		"next-admin-renderer-mongo",
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
		"APP_SERVER_HOST",
		"APP_SSH_KEY",
		"EDGE_SERVER_HOST",
		"EDGE_SSH_KEY",
		"LETSENCRYPT_EMAIL",
		"POSTGRES_PASSWORD",
		"JARDIN_ADMIN_HOST",
		"JARDIN_SITE_HOST",
		"JARDIN_ENV_FILE",
	} {
		if !strings.Contains(envExample, expected) {
			t.Fatalf("deployment pattern .env.example missing %s", expected)
		}
	}

	for _, expected := range []string{
		"find \"$PATTERNS_DIR\"",
		"go build -o \"$TMP_DIR/test-config\" \"$ROOT/cmd/test-config\"",
		"\"$TMP_DIR/test-config\" tako.yaml",
		"go test ./examples",
	} {
		if !strings.Contains(validateScript, expected) {
			t.Fatalf("deployment pattern validator missing %q", expected)
		}
	}
}

func TestNextAdminRendererMongoDocsCoverEnvAndCIFlow(t *testing.T) {
	readme := readExampleFile(t, filepath.Join("deployment-patterns", "next-admin-renderer-mongo", "README.md"))
	for _, expected := range []string{
		"JARDIN_ENV_FILE=.env.production",
		"tako env push production --from-file .env.production",
		"tako env pull production --force",
		"tako state pull -e production",
		"tako deploy -e production --yes",
		"TAKO_NONINTERACTIVE=1 tako setup -e production --dedicated-edge",
		"TAKO_NONINTERACTIVE=1 tako deploy -e production --yes",
		"clean checkout",
	} {
		if !strings.Contains(readme, expected) {
			t.Fatalf("next-admin-renderer-mongo README missing %q", expected)
		}
	}
}

func deploymentPatternConfigPaths() ([]string, error) {
	var matches []string
	err := filepath.WalkDir("deployment-patterns", func(path string, entry os.DirEntry, err error) error {
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
		if entry.Name() == "tako.yaml" {
			matches = append(matches, filepath.ToSlash(path))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(matches)
	return matches, nil
}

func deploymentPatternID(configPath string) string {
	path := filepath.ToSlash(configPath)
	path = strings.TrimPrefix(path, "deployment-patterns/")
	path = strings.TrimSuffix(path, "/tako.yaml")
	return path
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
		"sk_live_",
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

func TestExamplesDoNotContainGeneratedRuntimeArtifacts(t *testing.T) {
	forbiddenNames := map[string]bool{
		"test-prometheus.txt": true,
	}
	forbiddenExtensions := map[string]bool{
		".db":      true,
		".sqlite":  true,
		".sqlite3": true,
		".log":     true,
		".out":     true,
		".zip":     true,
	}

	err := filepath.WalkDir(".", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == "21-nfs-shared-storage" && entry.IsDir() {
			t.Fatalf("%s is deprecated; NFS shared storage support was removed", path)
		}
		if entry.IsDir() {
			switch entry.Name() {
			case "node_modules", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if forbiddenNames[entry.Name()] {
			t.Fatalf("%s is generated runtime output and should not be committed", path)
		}
		if strings.HasSuffix(entry.Name(), ".tar.gz") || forbiddenExtensions[filepath.Ext(entry.Name())] {
			t.Fatalf("%s is a generated/binary artifact and should not be committed", path)
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
		"tako env pull --force",
		"tako state pull",
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
	if len(api.Ports) != 2 || api.Ports[1].Name != "metrics" || api.Ports[1].Mode != "internal" || api.Ports[1].Target != 9090 {
		t.Fatalf("node api should expose explicit http and internal metrics ports: %#v", api.Ports)
	}
	if api.Replicas != 2 || api.HealthCheck.Path != "/health" {
		t.Fatalf("node api should be replicated with health check: %#v", api)
	}
	if api.Deploy.Strategy != "rolling" || api.Deploy.Order != "start-first" || api.Deploy.MaxUnavailable != 0 {
		t.Fatalf("node api should use start-first rolling deploys: %#v", api.Deploy)
	}
}

func assertVolumePattern(t *testing.T, cfg *config.Config) {
	web := productionServices(t, cfg)["web"]
	assertPublicService(t, web, 3000)
	if !slices.Contains(web.Volumes, "app_data:/app/data") {
		t.Fatalf("volume template missing app_data mount: %#v", web.Volumes)
	}
	if cfg.Volumes["app_data"].Driver != "local" {
		t.Fatalf("volume template should define app_data as a local named volume: %#v", cfg.Volumes)
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
	if realtime.Replicas != 2 || realtime.LoadBalancer.Strategy != "ip_hash" {
		t.Fatalf("websocket template should use replicated ip_hash balancing: %#v", realtime)
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

func assertCMSDynamicDomainsAppPattern(t *testing.T, cfg *config.Config) {
	if cfg.Project.Name != "pattern-cms-app" {
		t.Fatalf("cms app project name = %q", cfg.Project.Name)
	}
	services := productionServices(t, cfg)
	for _, serviceName := range []string{"admin", "renderer"} {
		service := services[serviceName]
		if service.Export == nil || service.Export.Ports["web"] != 80 {
			t.Fatalf("%s should export web:80: %#v", serviceName, service.Export)
		}
		if service.Port != 80 {
			t.Fatalf("%s should expose internal port 80: %#v", serviceName, service)
		}
	}
	mongo := services["mongo"]
	if !mongo.Persistent || !slices.Contains(mongo.Volumes, "mongo_data:/data/db") {
		t.Fatalf("mongo should be persistent with mongo_data volume: %#v", mongo)
	}
}

func assertCMSDynamicDomainsEdgePattern(t *testing.T, cfg *config.Config) {
	if cfg.Project.Name != "pattern-cms-edge" {
		t.Fatalf("cms edge project name = %q", cfg.Project.Name)
	}
	if cfg.Imports["jardin_admin"].Project != "pattern-cms-app" || cfg.Imports["jardin_admin"].Service != "admin" {
		t.Fatalf("edge missing admin import: %#v", cfg.Imports["jardin_admin"])
	}
	if cfg.Imports["jardin_renderer"].Project != "pattern-cms-app" || cfg.Imports["jardin_renderer"].Service != "renderer" {
		t.Fatalf("edge missing renderer import: %#v", cfg.Imports["jardin_renderer"])
	}
	edge := productionServices(t, cfg)["edge"]
	if len(edge.Ports) != 2 {
		t.Fatalf("edge should declare http and https host ports: %#v", edge.Ports)
	}
	wantPorts := map[string]int{"http": 80, "https": 443}
	for _, port := range edge.Ports {
		if port.Mode != "host" || port.Published != wantPorts[port.Name] || port.Target != wantPorts[port.Name] {
			t.Fatalf("edge port should bind public host port directly: %#v", port)
		}
	}
	if len(edge.Configs) != 1 || edge.Configs[0].Source != "caddyfile" || edge.Configs[0].Target != "/etc/caddy/Caddyfile" {
		t.Fatalf("edge should mount managed Caddyfile: %#v", edge.Configs)
	}
	caddyConfig, ok := cfg.Configs["caddyfile"]
	if !ok || caddyConfig.Generate == nil || caddyConfig.Generate.Caddy == nil {
		t.Fatalf("edge should generate managed Caddyfile: %#v", cfg.Configs)
	}
	caddy := caddyConfig.Generate.Caddy
	if caddy.AdminImport != "jardin_admin" || caddy.RendererImport != "jardin_renderer" || !caddy.OnDemandTLS {
		t.Fatalf("edge generated Caddy config should consume imports: %#v", caddy)
	}
	for _, mount := range []string{"caddy_data:/data", "caddy_config:/config"} {
		if !slices.Contains(edge.Volumes, mount) {
			t.Fatalf("edge missing Caddy volume %s: %#v", mount, edge.Volumes)
		}
	}
}

func assertNextAdminRendererMongoAppPattern(t *testing.T, cfg *config.Config) {
	if cfg.Project.Name != "pattern-next-cms-app" {
		t.Fatalf("next cms app project name = %q", cfg.Project.Name)
	}
	caddyConfig, ok := cfg.Configs["colocated_caddyfile"]
	if !ok || caddyConfig.Source != "ops/caddy/Caddyfile" {
		t.Fatalf("next cms app should use checked-in colocated Caddyfile: %#v", cfg.Configs)
	}

	services := productionServices(t, cfg)
	mongo := services["mongo"]
	if !mongo.Persistent || !slices.Contains(mongo.Volumes, "mongo_data:/data/db") {
		t.Fatalf("mongo should be persistent with mongo_data volume: %#v", mongo)
	}
	if mongo.Placement == nil || mongo.Placement.Strategy != "pinned" {
		t.Fatalf("mongo should be pinned for node-local data: %#v", mongo.Placement)
	}

	admin := services["admin"]
	if admin.Build != "." || admin.Dockerfile != "Dockerfile" || admin.Port != 3000 {
		t.Fatalf("admin should build from root Dockerfile on port 3000: %#v", admin)
	}
	if admin.Export == nil || admin.Export.Ports["web"] != 3000 {
		t.Fatalf("admin should export web:3000: %#v", admin.Export)
	}
	if !slices.Contains(admin.DependsOn, "mongo") || admin.Env["MONGO_URL"] == "" || admin.EnvFile != ".env.example" {
		t.Fatalf("admin should depend on mongo and receive runtime env file: %#v", admin)
	}
	if admin.HealthCheck.Path != "/api/health" || admin.Deploy.Strategy != "rolling" || admin.Deploy.Order != "start-first" {
		t.Fatalf("admin should be health-gated rolling deploy: health=%#v deploy=%#v", admin.HealthCheck, admin.Deploy)
	}

	renderer := services["renderer"]
	if renderer.Build != "." || renderer.Dockerfile != "Dockerfile.renderer" || renderer.Replicas != 2 {
		t.Fatalf("renderer should build from Dockerfile.renderer with replicas: %#v", renderer)
	}
	if renderer.Export == nil || renderer.Export.Ports["web"] != 3000 {
		t.Fatalf("renderer should export web:3000: %#v", renderer.Export)
	}
	if !slices.Contains(renderer.DependsOn, "admin") || renderer.Env["ADMIN_URL"] != "http://admin:3000" || renderer.EnvFile != ".env.example" {
		t.Fatalf("renderer should depend on admin and receive runtime env file: %#v", renderer)
	}
	if renderer.HealthCheck.Path != "/api/health" || renderer.Deploy.Strategy != "rolling" || renderer.Deploy.Order != "start-first" {
		t.Fatalf("renderer should be health-gated rolling deploy: health=%#v deploy=%#v", renderer.HealthCheck, renderer.Deploy)
	}

	edge := services["edge-colocated"]
	assertPublicService(t, edge, 80)
	if len(edge.Configs) != 1 || edge.Configs[0].Source != "colocated_caddyfile" || edge.Configs[0].Target != "/etc/caddy/Caddyfile" {
		t.Fatalf("colocated edge should mount checked-in Caddyfile: %#v", edge.Configs)
	}
	for _, mount := range []string{"caddy_colocated_data:/data", "caddy_colocated_config:/config"} {
		if !slices.Contains(edge.Volumes, mount) {
			t.Fatalf("colocated edge missing Caddy volume %s: %#v", mount, edge.Volumes)
		}
	}
}

func assertNextAdminRendererMongoEdgePattern(t *testing.T, cfg *config.Config) {
	if cfg.Project.Name != "pattern-next-cms-edge" {
		t.Fatalf("next cms edge project name = %q", cfg.Project.Name)
	}
	if cfg.Imports["jardin_admin"].Project != "pattern-next-cms-app" || cfg.Imports["jardin_admin"].Service != "admin" {
		t.Fatalf("edge missing admin import: %#v", cfg.Imports["jardin_admin"])
	}
	if cfg.Imports["jardin_renderer"].Project != "pattern-next-cms-app" || cfg.Imports["jardin_renderer"].Service != "renderer" {
		t.Fatalf("edge missing renderer import: %#v", cfg.Imports["jardin_renderer"])
	}

	edge := productionServices(t, cfg)["edge"]
	if len(edge.Ports) != 2 {
		t.Fatalf("dedicated edge should declare http and https host ports: %#v", edge.Ports)
	}
	wantPorts := map[string]int{"http": 80, "https": 443}
	for _, port := range edge.Ports {
		if port.Mode != "host" || port.Published != wantPorts[port.Name] || port.Target != wantPorts[port.Name] {
			t.Fatalf("dedicated edge port should bind public host port directly: %#v", port)
		}
	}
	if len(edge.Configs) != 1 || edge.Configs[0].Source != "caddyfile" || edge.Configs[0].Target != "/etc/caddy/Caddyfile" {
		t.Fatalf("dedicated edge should mount managed generated Caddyfile: %#v", edge.Configs)
	}
	caddyConfig, ok := cfg.Configs["caddyfile"]
	if !ok || caddyConfig.Generate == nil || caddyConfig.Generate.Caddy == nil {
		t.Fatalf("dedicated edge should generate managed Caddyfile: %#v", cfg.Configs)
	}
	caddy := caddyConfig.Generate.Caddy
	if caddy.AdminImport != "jardin_admin" || caddy.RendererImport != "jardin_renderer" || caddy.AskImport != "jardin_admin" || !caddy.OnDemandTLS {
		t.Fatalf("dedicated edge generated Caddy config should consume app imports: %#v", caddy)
	}
	for _, mount := range []string{"caddy_data:/data", "caddy_config:/config"} {
		if !slices.Contains(edge.Volumes, mount) {
			t.Fatalf("dedicated edge missing Caddy volume %s: %#v", mount, edge.Volumes)
		}
	}
}

func assertPublicService(t *testing.T, service config.ServiceConfig, port int) {
	t.Helper()
	for _, servicePort := range service.EffectivePorts() {
		if servicePort.Target == port && servicePort.Mode == "proxy" && servicePort.Proxy != nil && servicePort.Proxy.Domain != "" {
			return
		}
	}
	t.Fatalf("service should be public on port %d: %#v", port, service)
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
			if service.Dockerfile != "" {
				dockerfilePath := filepath.Clean(filepath.Join(buildPath, service.Dockerfile))
				if _, err := os.Stat(dockerfilePath); err != nil {
					t.Fatalf("%s/%s build context %s has no service Dockerfile %s: %v", envName, serviceName, buildPath, service.Dockerfile, err)
				}
			}
		}
	}
}
