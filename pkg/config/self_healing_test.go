package config

import (
	"strings"
	"testing"
	"time"
)

func selfHealingBool(value bool) *bool { return &value }

func TestEffectiveSelfHealing(t *testing.T) {
	on := selfHealingBool(true)
	off := selfHealingBool(false)
	for _, tc := range []struct {
		name    string
		env     *EnvironmentConfig
		service ServiceConfig
		want    SelfHealingPolicy
	}{
		{
			name: "no config is off",
			want: SelfHealingPolicy{RestartUnhealthy: true, StartExited: true, UnhealthyThreshold: 3, MaxRestarts: 3, Window: 30 * time.Minute},
		},
		{
			name: "environment enables with defaults",
			env:  &EnvironmentConfig{SelfHealing: &SelfHealingConfig{Enabled: on}},
			want: SelfHealingPolicy{Enabled: true, RestartUnhealthy: true, StartExited: true, UnhealthyThreshold: 3, MaxRestarts: 3, Window: 30 * time.Minute},
		},
		{
			name:    "service overrides keys",
			env:     &EnvironmentConfig{SelfHealing: &SelfHealingConfig{Enabled: on, UnhealthyThreshold: 5, Window: "1h"}},
			service: ServiceConfig{SelfHealing: &SelfHealingConfig{RestartUnhealthy: off, MaxRestarts: 7}},
			want:    SelfHealingPolicy{Enabled: true, StartExited: true, UnhealthyThreshold: 5, MaxRestarts: 7, Window: time.Hour},
		},
		{
			name:    "service disables",
			env:     &EnvironmentConfig{SelfHealing: &SelfHealingConfig{Enabled: on}},
			service: ServiceConfig{SelfHealing: &SelfHealingConfig{Enabled: off}},
			want:    SelfHealingPolicy{RestartUnhealthy: true, StartExited: true, UnhealthyThreshold: 3, MaxRestarts: 3, Window: 30 * time.Minute},
		},
		{
			name:    "persistent ignores environment enabled",
			env:     &EnvironmentConfig{SelfHealing: &SelfHealingConfig{Enabled: on, MaxRestarts: 2}},
			service: ServiceConfig{Persistent: true},
			want:    SelfHealingPolicy{RestartUnhealthy: true, StartExited: true, UnhealthyThreshold: 3, MaxRestarts: 2, Window: 30 * time.Minute},
		},
		{
			name:    "persistent explicit opt-in",
			env:     &EnvironmentConfig{SelfHealing: &SelfHealingConfig{Enabled: on}},
			service: ServiceConfig{Persistent: true, SelfHealing: &SelfHealingConfig{Enabled: on, RestartUnhealthy: off}},
			want:    SelfHealingPolicy{Enabled: true, StartExited: true, UnhealthyThreshold: 3, MaxRestarts: 3, Window: 30 * time.Minute},
		},
		{
			name:    "job never heals",
			env:     &EnvironmentConfig{SelfHealing: &SelfHealingConfig{Enabled: on}},
			service: ServiceConfig{Kind: ServiceKindJob},
			want:    SelfHealingPolicy{RestartUnhealthy: true, StartExited: true, UnhealthyThreshold: 3, MaxRestarts: 3, Window: 30 * time.Minute},
		},
		{
			name:    "run never heals",
			env:     &EnvironmentConfig{SelfHealing: &SelfHealingConfig{Enabled: on}},
			service: ServiceConfig{Kind: ServiceKindRun},
			want:    SelfHealingPolicy{RestartUnhealthy: true, StartExited: true, UnhealthyThreshold: 3, MaxRestarts: 3, Window: 30 * time.Minute},
		},
		{
			name:    "both actions off disables",
			env:     &EnvironmentConfig{SelfHealing: &SelfHealingConfig{Enabled: on, RestartUnhealthy: off, StartExited: off}},
			service: ServiceConfig{},
			want:    SelfHealingPolicy{UnhealthyThreshold: 3, MaxRestarts: 3, Window: 30 * time.Minute},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveSelfHealing(tc.env, tc.service); got != tc.want {
				t.Fatalf("EffectiveSelfHealing = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestValidateSelfHealingConfig(t *testing.T) {
	valid := []*SelfHealingConfig{
		nil,
		{},
		{Enabled: selfHealingBool(true), UnhealthyThreshold: 1, MaxRestarts: 20, Window: "5m"},
		{UnhealthyThreshold: 20, MaxRestarts: 1, Window: "24h"},
	}
	for _, block := range valid {
		if err := validateSelfHealingConfig("environment production", block); err != nil {
			t.Fatalf("valid block %+v rejected: %v", block, err)
		}
	}
	invalid := map[string]*SelfHealingConfig{
		"threshold too high":    {UnhealthyThreshold: 21},
		"threshold negative":    {UnhealthyThreshold: -1},
		"max restarts too high": {MaxRestarts: 21},
		"max restarts negative": {MaxRestarts: -2},
		"window too short":      {Window: "4m59s"},
		"window too long":       {Window: "25h"},
		"window garbage":        {Window: "soon"},
		"window blank":          {Window: "  "},
	}
	for name, block := range invalid {
		if err := validateSelfHealingConfig("environment production", block); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSelfHealingConfigLoadAndInheritance(t *testing.T) {
	cfg := mustLoadExtendsTestConfig(t, `environments:
  base:
    abstract: true
    servers: [node-a]
    selfHealing:
      enabled: true
      unhealthyThreshold: 4
    services:
      web:
        image: app:1
        port: 3000
        healthCheck:
          path: /health
      db:
        image: postgres:16
        persistent: true
        volumes: ["pgdata:/var/lib/postgresql/data"]
  production:
    extends: base
    services:
      db:
        selfHealing:
          enabled: true
          restartUnhealthy: false
`)
	env := cfg.Environments["production"]
	if env.SelfHealing == nil || env.SelfHealing.Enabled == nil || !*env.SelfHealing.Enabled || env.SelfHealing.UnhealthyThreshold != 4 {
		t.Fatalf("environment selfHealing not inherited: %+v", env.SelfHealing)
	}
	web := EffectiveSelfHealing(&env, env.Services["web"])
	if !web.Enabled || web.UnhealthyThreshold != 4 {
		t.Fatalf("web policy = %+v", web)
	}
	db := EffectiveSelfHealing(&env, env.Services["db"])
	if !db.Enabled || db.RestartUnhealthy || !db.StartExited {
		t.Fatalf("db policy = %+v", db)
	}
	if !cfg.SelfHealingEnabled("production") {
		t.Fatalf("SelfHealingEnabled(production) = false")
	}
}

func TestSelfHealingPersistentNeedsOwnOptIn(t *testing.T) {
	cfg := mustLoadExtendsTestConfig(t, `environments:
  production:
    servers: [node-a]
    selfHealing:
      enabled: true
    services:
      db:
        image: postgres:16
        persistent: true
        volumes: ["pgdata:/var/lib/postgresql/data"]
`)
	env := cfg.Environments["production"]
	if EffectiveSelfHealing(&env, env.Services["db"]).Enabled {
		t.Fatalf("persistent service inherited environment enabled")
	}
	if cfg.SelfHealingEnabled("production") {
		t.Fatalf("SelfHealingEnabled reported true for persistent-only environment")
	}
}

func TestSelfHealingInheritedByPreview(t *testing.T) {
	cfg := mustLoadPreviewTestConfig(t, `environments:
  staging:
    servers: [node-a]
    selfHealing:
      enabled: true
    services:
      web:
        image: app:1
        port: 3000
    preview:
      domain: "{name}-{service}.preview.example.com"
`, &PreviewInstance{Name: "pr-7"})
	preview := cfg.PreviewInstance()
	if preview == nil {
		t.Fatalf("preview not resolved")
	}
	env := cfg.Environments[preview.Identity]
	if !EffectiveSelfHealing(&env, env.Services["web"]).Enabled {
		t.Fatalf("preview did not inherit selfHealing: %+v", env.SelfHealing)
	}
}

func TestSelfHealingStrictDecodeAndKindValidation(t *testing.T) {
	_, err := loadExtendsTestConfig(t, `environments:
  production:
    servers: [node-a]
    selfHealing:
      enabled: true
      restartEverything: true
    services:
      web:
        image: app:1
`)
	if err == nil || !strings.Contains(err.Error(), "restartEverything") {
		t.Fatalf("unknown YAML selfHealing key err = %v", err)
	}
	_, err = LoadConfig(writeExtendsTestConfig(t, "tako.json", `{
  "project": {"name": "demo", "version": "1.0.0"},
  "servers": {"node-a": {"host": "192.0.2.10", "user": "root", "password": "test-password"}},
  "environments": {"production": {"servers": ["node-a"], "services": {"web": {"image": "app:1", "selfHealing": {"enabled": true, "bogus": 1}}}}}
}`))
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("unknown JSON selfHealing key err = %v", err)
	}
	_, err = loadExtendsTestConfig(t, `environments:
  production:
    servers: [node-a]
    services:
      cleanup:
        kind: job
        image: app:1
        schedule: "@daily"
        command: ["cleanup"]
        selfHealing:
          enabled: true
`)
	if err == nil || !strings.Contains(err.Error(), "cannot set selfHealing") {
		t.Fatalf("job selfHealing err = %v", err)
	}
	_, err = loadExtendsTestConfig(t, `environments:
  production:
    servers: [node-a]
    services:
      migrate:
        kind: run
        image: app:1
        command: ["migrate"]
        selfHealing:
          enabled: true
`)
	if err == nil || !strings.Contains(err.Error(), "cannot set selfHealing") {
		t.Fatalf("run selfHealing err = %v", err)
	}
	_, err = loadExtendsTestConfig(t, `environments:
  production:
    servers: [node-a]
    selfHealing:
      window: 1m
    services:
      web:
        image: app:1
`)
	if err == nil || !strings.Contains(err.Error(), "selfHealing.window") {
		t.Fatalf("short window err = %v", err)
	}
}
