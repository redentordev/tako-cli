package config

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

const previewTestEnvironments = `volumes:
  pgdata: {}
environments:
  staging:
    servers: [node-a]
    services:
      web:
        image: app:1
        port: 3000
        replicas: 3
        proxy:
          domain: app.example.com
          domains: [www.app.example.com]
          redirectFrom: [old.app.example.com]
        deploy:
          strategy: blue_green
          promotion: manual
        env:
          FOO: bar
          PUBLIC_URL: https://app.example.com
      admin:
        image: admin:1
        port: 4000
        proxy:
          domain: admin.example.com
      db:
        image: postgres:16
        persistent: true
        volumes: ["pgdata:/var/lib/postgresql/data"]
        backup:
          schedule: "0 2 * * *"
          retain: 7
        monitoring:
          enabled: true
          webhook: https://hooks.example.com/alert
      agent:
        image: agent:1
        replicas: 4
        placement:
          strategy: global
      cleanup:
        kind: job
        image: app:1
        schedule: "@daily"
        command: ["cleanup"]
      migrate:
        kind: run
        image: app:1
        command: ["migrate"]
  preview:
    abstract: true
    extends: staging
    servers: [node-b]
    preview:
      domain: "{name}-{service}.preview.example.com"
      ttl: 72h
      env:
        PUBLIC_URL: "{url:web}"
      services:
        admin:
          env:
            BETTER_AUTH_URL: "{url:admin}"
            WEB_HOST: "{host:web}"
`

func loadPreviewTestConfig(t *testing.T, environments string, instance *PreviewInstance) (*Config, error) {
	t.Helper()
	return LoadConfigWithOptions(writeExtendsTestConfig(t, "tako.yaml", extendsTestHeader+environments), LoadOptions{Preview: instance})
}

func mustLoadPreviewTestConfig(t *testing.T, environments string, instance *PreviewInstance) *Config {
	t.Helper()
	cfg, err := loadPreviewTestConfig(t, environments, instance)
	if err != nil {
		t.Fatalf("LoadConfigWithOptions: %v", err)
	}
	return cfg
}

func TestNormalizePreviewName(t *testing.T) {
	long := "Feature/" + strings.Repeat("abcdefghij", 5)
	digest := sha256.Sum256([]byte(long))
	cutAtDash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	cutDigest := sha256.Sum256([]byte(cutAtDash))
	for _, tc := range []struct {
		in, want string
	}{
		{"pr-42", "pr-42"},
		{"Feature/Login_V2", "feature-login-v2"},
		{"--a  b..c--", "a-b-c"},
		{"PR 7", "pr-7"},
		{long, ("feature-" + strings.Repeat("abcdefghij", 3))[:33] + "-" + hex.EncodeToString(digest[:])[:6]},
		{cutAtDash, strings.Repeat("a", 32) + "-" + hex.EncodeToString(cutDigest[:])[:6]},
	} {
		got, err := NormalizePreviewName(tc.in)
		if err != nil || got != tc.want {
			t.Fatalf("NormalizePreviewName(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
		if len(got) > 40 {
			t.Fatalf("normalized name %q is longer than 40", got)
		}
		if identity := PreviewIdentity(got); len(identity) > 48 || !isValidRuntimeIdentifier(identity) {
			t.Fatalf("identity %q is not a valid environment name", identity)
		}
	}
	for _, empty := range []string{"", "///", "---"} {
		if _, err := NormalizePreviewName(empty); err == nil {
			t.Fatalf("NormalizePreviewName(%q) succeeded", empty)
		}
	}
}

func TestResolvePreviewBase(t *testing.T) {
	declared := []string{"preview", "preview-mobile", "qa", "staging"}
	if base, err := ResolvePreviewBase([]string{"preview"}, declared, ""); err != nil || base != "preview" {
		t.Fatalf("single base = %q, %v", base, err)
	}
	if _, err := ResolvePreviewBase([]string{"preview", "preview-mobile"}, declared, ""); err == nil || !strings.Contains(err.Error(), "preview, preview-mobile") || !strings.Contains(err.Error(), "-e") {
		t.Fatalf("ambiguous err = %v", err)
	}
	if base, err := ResolvePreviewBase([]string{"preview", "preview-mobile"}, declared, "preview-mobile"); err != nil || base != "preview-mobile" {
		t.Fatalf("explicit base = %q, %v", base, err)
	}
	if _, err := ResolvePreviewBase([]string{"preview"}, declared, "qa"); err == nil || !strings.Contains(err.Error(), "qa declares no preview: block") {
		t.Fatalf("explicit non-preview err = %v", err)
	}
	if _, err := ResolvePreviewBase(nil, declared, ""); err == nil || !strings.Contains(err.Error(), "docs/PREVIEW-ENVIRONMENTS.md") {
		t.Fatalf("none err = %v", err)
	}
}

func TestPreviewBlockUnknownKeyRejected(t *testing.T) {
	_, err := loadPreviewTestConfig(t, `environments:
  staging:
    servers: [node-a]
    preview:
      maxPreviews: 3
    services:
      web:
        image: app:1
`, nil)
	if err == nil || !strings.Contains(err.Error(), "environment staging") || !strings.Contains(err.Error(), "maxPreviews") {
		t.Fatalf("YAML err = %v, want unknown field naming staging", err)
	}
	_, err = LoadConfig(writeExtendsTestConfig(t, "tako.json", `{
  "project": {"name": "demo", "version": "1.0.0"},
  "servers": {"node-a": {"host": "192.0.2.10", "user": "root", "password": "test-password"}},
  "environments": {"staging": {"servers": ["node-a"], "preview": {"maxPreviews": 3}, "services": {"web": {"image": "nginx:alpine"}}}}
}`))
	if err == nil || !strings.Contains(err.Error(), "environment staging") || !strings.Contains(err.Error(), "maxPreviews") {
		t.Fatalf("JSON err = %v, want unknown field naming staging", err)
	}
}

func TestPreviewTemplateIsNotUnusedAndNotInherited(t *testing.T) {
	cfg := mustLoadPreviewTestConfig(t, previewTestEnvironments+`  qa:
    extends: preview
`, nil)
	for _, warning := range ValidationWarnings(cfg) {
		if warning.Field == "abstract" && warning.Environment == "preview" {
			t.Fatalf("preview template reported unused: %+v", warning)
		}
	}
	if bases := cfg.PreviewBases(); len(bases) != 1 || bases[0] != "preview" {
		t.Fatalf("PreviewBases = %v, want [preview] (qa must not inherit preview:)", bases)
	}
	if cfg.Environments["qa"].Preview != nil {
		t.Fatal("resolved qa carries a preview block")
	}
	_, err := loadPreviewTestConfig(t, previewTestEnvironments+`  qa:
    extends: preview
`, &PreviewInstance{Name: "pr-1", Base: "qa"})
	if err == nil || !strings.Contains(err.Error(), "qa declares no preview: block") {
		t.Fatalf("preview of qa err = %v", err)
	}
}

func TestPreviewTemplateWithoutChildrenHasNoUnusedWarning(t *testing.T) {
	cfg := mustLoadPreviewTestConfig(t, previewTestEnvironments, nil)
	for _, warning := range ValidationWarnings(cfg) {
		if warning.Field == "abstract" {
			t.Fatalf("unexpected unused-abstract warning %+v", warning)
		}
	}
}

func TestPreviewConcreteBaseIsStrippedAndFound(t *testing.T) {
	environments := `environments:
  staging:
    servers: [node-a]
    preview: {}
    services:
      web:
        image: app:1
        port: 3000
        proxy:
          domain: app.example.com
`
	cfg := mustLoadPreviewTestConfig(t, environments, nil)
	if cfg.Environments["staging"].Preview != nil {
		t.Fatal("staging keeps its preview block after resolution")
	}
	if bases := cfg.PreviewBases(); len(bases) != 1 || bases[0] != "staging" {
		t.Fatalf("PreviewBases = %v", bases)
	}
	preview := mustLoadPreviewTestConfig(t, environments, &PreviewInstance{Name: "PR 42"})
	info := preview.PreviewInstance()
	if info == nil || info.Base != "staging" || info.Identity != "preview-pr-42" {
		t.Fatalf("PreviewInstance = %+v", info)
	}
	if host := preview.Environments["preview-pr-42"].Services["web"].Proxy.Domain; host != "pr-42.app.example.com" {
		t.Fatalf("default host = %q", host)
	}
	if preview.Environments["staging"].Services["web"].Proxy.Domain != "app.example.com" {
		t.Fatal("preview policy changed the base environment")
	}
}

func TestPreviewInstanceFromAbstractTemplate(t *testing.T) {
	cfg := mustLoadPreviewTestConfig(t, previewTestEnvironments, &PreviewInstance{Name: "pr-42"})
	env, ok := cfg.Environments["preview-pr-42"]
	if !ok {
		t.Fatalf("environments = %v", cfg.DeclaredEnvironmentNames())
	}
	if chain := cfg.EnvironmentExtendsChain("preview-pr-42"); len(chain) != 2 || chain[0] != "preview" || chain[1] != "staging" {
		t.Fatalf("chain = %v", chain)
	}
	if len(env.Servers) != 1 || env.Servers[0] != "node-b" {
		t.Fatalf("servers = %v, want template servers", env.Servers)
	}
	if _, ok := env.Services["cleanup"]; ok {
		t.Fatal("job kept without keepJobs")
	}
	if _, ok := env.Services["migrate"]; !ok {
		t.Fatal("kind: run service dropped")
	}
	web := env.Services["web"]
	if web.Replicas != 1 {
		t.Fatalf("web replicas = %d, want 1", web.Replicas)
	}
	if env.Services["agent"].Placement.Strategy != "global" {
		t.Fatal("global placement changed")
	}
	if web.Deploy.Promotion != DeployPromotionAutomatic {
		t.Fatalf("promotion = %q", web.Deploy.Promotion)
	}
	db := env.Services["db"]
	if db.Backup != nil || db.Monitoring != nil {
		t.Fatalf("db backup/monitoring kept: %+v %+v", db.Backup, db.Monitoring)
	}
	if got := cfg.GetVolumeName("pgdata", "preview-pr-42"); got == cfg.GetVolumeName("pgdata", "staging") {
		t.Fatalf("preview volume %q is shared with staging", got)
	}
	if web.Proxy.Domain != "pr-42-web.preview.example.com" || len(web.Proxy.Domains) != 0 || len(web.Proxy.RedirectFrom) != 0 {
		t.Fatalf("web proxy = %+v", web.Proxy)
	}
	admin := env.Services["admin"]
	if admin.Proxy.Domain != "pr-42-admin.preview.example.com" {
		t.Fatalf("admin host = %q", admin.Proxy.Domain)
	}
	if admin.Env["BETTER_AUTH_URL"] != "https://pr-42-admin.preview.example.com" || admin.Env["WEB_HOST"] != "pr-42-web.preview.example.com" {
		t.Fatalf("admin env = %v", admin.Env)
	}
	if web.Env["PUBLIC_URL"] != "https://pr-42-web.preview.example.com" || web.Env["FOO"] != "bar" {
		t.Fatalf("web env = %v (preview env must win over the template)", web.Env)
	}
	if web.Env["TAKO_PREVIEW"] != "true" || web.Env["TAKO_PREVIEW_NAME"] != "pr-42" || web.Env["TAKO_PREVIEW_URL"] != "https://pr-42-web.preview.example.com" {
		t.Fatalf("web preview env = %v", web.Env)
	}
	if _, ok := db.Env["TAKO_PREVIEW_URL"]; ok || db.Env["TAKO_PREVIEW"] != "true" {
		t.Fatalf("db env = %v", db.Env)
	}
	staging := cfg.Environments["staging"]
	if staging.Services["web"].Replicas != 3 || staging.Services["db"].Backup == nil || staging.Services["web"].Deploy.Promotion != DeployPromotionManual {
		t.Fatal("preview policy changed staging")
	}
	if cfg.SecretsEnvironment("preview-pr-42") != "preview" || cfg.SecretsEnvironment("staging") != "staging" {
		t.Fatalf("SecretsEnvironment = %q/%q", cfg.SecretsEnvironment("preview-pr-42"), cfg.SecretsEnvironment("staging"))
	}
	info := cfg.PreviewInstance()
	if info.Hosts["web"] != "pr-42-web.preview.example.com" || info.Policy.EffectiveTTL() != "72h" {
		t.Fatalf("info = %+v", info)
	}
}

func TestPreviewInheritsRequestHeadersExceptBaseHostsAndTrustedProxies(t *testing.T) {
	environments := strings.Replace(previewTestEnvironments, "          redirectFrom: [old.app.example.com]\n", "          redirectFrom: [old.app.example.com]\n          trustedProxies: [cloudfront, 192.0.2.0/24]\n          requestHeaders:\n            X-Forwarded-Host: www.app.example.com\n            X-Forwarded-Proto: https\n", 1)
	environments = strings.Replace(environments, "          domain: admin.example.com\n", "          domain: admin.example.com\n          requestHeaders:\n            X-Origin-Host: App.Example.com\n", 1)
	base := mustLoadPreviewTestConfig(t, environments, nil)
	var fields []string
	for _, warning := range ValidationWarnings(base) {
		if strings.HasPrefix(warning.Field, "proxy.requestHeaders.") {
			fields = append(fields, warning.Service+":"+warning.Field)
		}
	}
	if strings.Join(fields, ",") != "admin:proxy.requestHeaders.X-Origin-Host,web:proxy.requestHeaders.X-Forwarded-Host" {
		t.Fatalf("pinned request header warnings = %v", fields)
	}

	cfg := mustLoadPreviewTestConfig(t, environments, &PreviewInstance{Name: "pr-7"})
	env := cfg.Environments["preview-pr-7"]
	web := env.Services["web"].Proxy
	if len(web.RequestHeaders) != 1 || web.RequestHeaders["X-Forwarded-Proto"] != "https" {
		t.Fatalf("web preview requestHeaders = %v, want only X-Forwarded-Proto", web.RequestHeaders)
	}
	if strings.Join(web.TrustedProxies, ",") != strings.Join(cfg.Environments["staging"].Services["web"].Proxy.TrustedProxies, ",") || len(web.TrustedProxies) != 2 {
		t.Fatalf("web preview trustedProxies = %v", web.TrustedProxies)
	}
	if admin := env.Services["admin"].Proxy; len(admin.RequestHeaders) != 0 {
		t.Fatalf("admin preview requestHeaders = %v, want the pinned header dropped", admin.RequestHeaders)
	}
	staging := cfg.Environments["staging"].Services["web"].Proxy
	if staging.RequestHeaders["X-Forwarded-Host"] != "www.app.example.com" {
		t.Fatalf("preview policy changed staging requestHeaders: %v", staging.RequestHeaders)
	}
}

func TestPreviewKeepJobs(t *testing.T) {
	environments := strings.Replace(previewTestEnvironments, "      ttl: 72h\n", "      ttl: 72h\n      keepJobs: true\n      maxReplicas: 2\n", 1)
	cfg := mustLoadPreviewTestConfig(t, environments, &PreviewInstance{Name: "pr-1"})
	env := cfg.Environments["preview-pr-1"]
	if _, ok := env.Services["cleanup"]; !ok {
		t.Fatal("keepJobs dropped the job")
	}
	if env.Services["web"].Replicas != 2 {
		t.Fatalf("replicas = %d, want maxReplicas 2", env.Services["web"].Replicas)
	}
}

func TestPreviewIsolationChecks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		service string
		volumes string
		want    []string
	}{
		{"host ports", `        ports: ["5432:5432"]` + "\n", "", []string{"service db publishes host ports", "ports: null"}},
		{"imports", `        imports: ["other.api"]` + "\n", "", []string{"service db imports"}},
		{"bind mount", `        volumes: ["/srv/uploads:/app/uploads"]` + "\n", "", []string{"service db bind-mounts host path /srv/uploads"}},
		{"named volume", `        volumes: ["uploads:/app/uploads"]` + "\n", "  uploads:\n    name: shared_uploads\n", []string{"service db", "uploads", "shared across environments"}},
		{"external volume", `        volumes: ["uploads:/app/uploads"]` + "\n", "  uploads:\n    external: true\n", []string{"uploads", "shared across environments"}},
		{"driver opts", `        volumes: ["media:/app/media"]` + "\n", "  media:\n    driver_opts: {type: none, o: bind, device: /srv/media}\n", []string{"service db", "media", "shared across environments"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			environments := "volumes:\n  pgdata: {}\n" + tc.volumes + `environments:
  staging:
    servers: [node-a]
    preview: {}
    services:
      db:
        image: postgres:16
` + tc.service
			cfg, err := loadPreviewTestConfig(t, environments, nil)
			if err != nil {
				t.Fatalf("base config rejected: %v", err)
			}
			found := false
			for _, warning := range ValidationWarnings(cfg) {
				if warning.Field == "preview" && warning.Environment == "staging" {
					found = true
				}
			}
			if !found {
				t.Fatalf("no preview isolation warning for %s", tc.name)
			}
			_, err = loadPreviewTestConfig(t, environments, &PreviewInstance{Name: "pr-1"})
			if err == nil {
				t.Fatalf("preview with %s was accepted", tc.name)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("err = %v, want %q", err, want)
				}
			}
		})
	}
}

func TestPreviewHostValidation(t *testing.T) {
	base := `environments:
  production:
    servers: [node-a]
    services:
      web:
        image: app:1
        port: 3000
        proxy:
          domain: app.example.com
  staging:
    servers: [node-a]
    preview:
      domain: "%s"
    services:
      web:
        image: app:1
        port: 3000
        proxy:
          domain: staging.example.com
      admin:
        image: app:1
        port: 3000
        proxy:
          domain: admin.staging.example.com
`
	for _, tc := range []struct {
		template string
		want     string
	}{
		{"app.example.com", "already served by environment production"},
		{"{name}.preview.example.com", "both render host"},
		{"{name}-{branch}.example.com", "unknown placeholder {branch}"},
		{"{name}-" + strings.Repeat("x", 70) + ".example.com", "longer than 63"},
	} {
		_, err := loadPreviewTestConfig(t, strings.Replace(base, "%s", tc.template, 1), &PreviewInstance{Name: "pr-1"})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("template %q err = %v, want %q", tc.template, err, tc.want)
		}
	}
}

func TestPreviewStaticValidation(t *testing.T) {
	for _, tc := range []struct {
		name, block, want string
	}{
		{"unknown env placeholder", "      env:\n        BRANCH: \"{branch}\"\n", "{branch}"},
		{"missing url target", "      env:\n        ADMIN: \"{url:admin}\"\n", "admin"},
		{"bad ttl", "      ttl: soon\n", "ttl"},
		{"negative max", "      max: -1\n", "max"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadPreviewTestConfig(t, `environments:
  staging:
    servers: [node-a]
    preview:
`+tc.block+`    services:
      web:
        image: app:1
        port: 3000
        proxy:
          domain: app.example.com
`, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "staging") {
				t.Fatalf("err = %v, want %q naming staging", err, tc.want)
			}
		})
	}
	cfg := mustLoadPreviewTestConfig(t, `environments:
  staging:
    servers: [node-a]
    preview:
      ttl: none
    services:
      web:
        image: app:1
`, nil)
	if cfg.PreviewPolicy("staging").EffectiveTTL() != "none" {
		t.Fatal("ttl none not kept")
	}
}

func TestPreviewIdentityCollision(t *testing.T) {
	_, err := loadPreviewTestConfig(t, previewTestEnvironments+`  preview-pr-42:
    extends: staging
`, &PreviewInstance{Name: "pr-42"})
	if err == nil || !strings.Contains(err.Error(), "preview-pr-42") || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("err = %v, want identity collision", err)
	}
}

func TestPreviewNoBaseAndAmbiguousBase(t *testing.T) {
	_, err := loadPreviewTestConfig(t, `environments:
  staging:
    servers: [node-a]
    services:
      web:
        image: app:1
`, &PreviewInstance{Name: "pr-1"})
	if err == nil || !strings.Contains(err.Error(), "no environment declares a preview: block") {
		t.Fatalf("err = %v", err)
	}
	environments := previewTestEnvironments + `  preview-mobile:
    abstract: true
    extends: staging
    preview: {}
`
	_, err = loadPreviewTestConfig(t, environments, &PreviewInstance{Name: "pr-1"})
	if err == nil || !strings.Contains(err.Error(), "preview, preview-mobile") {
		t.Fatalf("ambiguous err = %v", err)
	}
	cfg := mustLoadPreviewTestConfig(t, environments, &PreviewInstance{Name: "pr-1", Base: "preview-mobile"})
	if cfg.PreviewInstance().Base != "preview-mobile" {
		t.Fatalf("base = %q", cfg.PreviewInstance().Base)
	}
}

func TestPreviewJSONInstance(t *testing.T) {
	cfg, err := LoadConfigWithOptions(writeExtendsTestConfig(t, "tako.json", `{
  "project": {"name": "demo", "version": "1.0.0"},
  "servers": {"node-a": {"host": "192.0.2.10", "user": "root", "password": "test-password"}},
  "environments": {
    "staging": {"servers": ["node-a"], "preview": {"ttl": "24h"}, "services": {"web": {"image": "nginx:alpine", "port": 80, "proxy": {"domain": "app.example.com"}}}}
  }
}`), LoadOptions{Preview: &PreviewInstance{Name: "pr-9"}})
	if err != nil {
		t.Fatalf("LoadConfigWithOptions: %v", err)
	}
	if got := cfg.Environments["preview-pr-9"].Services["web"].Proxy.Domain; got != "pr-9.app.example.com" {
		t.Fatalf("host = %q", got)
	}
}
