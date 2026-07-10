package config

import (
	"slices"
	"strings"
	"testing"
)

func TestValidateConfigAcceptsRunAndAddsImageFromDependency(t *testing.T) {
	cfg := minimalValidConfigWithService(ServiceConfig{
		Kind: ServiceKindService, Image: "getsentry/sentry:26.6.0",
	})
	env := cfg.Environments["production"]
	env.Services["migrate"] = ServiceConfig{
		Kind: ServiceKindRun, ImageFrom: "web",
		Command: ListValue("sentry", "upgrade", "--noinput"),
		Timeout: "30m",
	}
	cfg.Environments["production"] = env
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	run := cfg.Environments["production"].Services["migrate"]
	if !run.IsRun() || !slices.Contains(run.DependsOn, "web") {
		t.Fatalf("validated run = %#v", run)
	}
}

func TestValidateConfigRejectsInvalidRunServices(t *testing.T) {
	tests := []struct {
		name    string
		service ServiceConfig
		want    string
	}{
		{name: "scalar command", service: ServiceConfig{Kind: ServiceKindRun, Image: "busybox", Command: StringValue("echo no")}, want: "argv list form"},
		{name: "missing image", service: ServiceConfig{Kind: ServiceKindRun, Command: ListValue("true")}, want: "exactly one"},
		{name: "both image sources", service: ServiceConfig{Kind: ServiceKindRun, Image: "busybox", ImageFrom: "web", Command: ListValue("true")}, want: "exactly one"},
		{name: "build", service: ServiceConfig{Kind: ServiceKindRun, Build: ".", Command: ListValue("true")}, want: "cannot set build"},
		{name: "ports", service: ServiceConfig{Kind: ServiceKindRun, Image: "busybox", Command: ListValue("true"), Port: 80}, want: "cannot expose"},
		{name: "restart", service: ServiceConfig{Kind: ServiceKindRun, Image: "busybox", Command: ListValue("true"), Restart: "always"}, want: "cannot set restart"},
		{name: "global placement", service: ServiceConfig{Kind: ServiceKindRun, Image: "busybox", Command: ListValue("true"), Placement: &PlacementConfig{Strategy: "global"}}, want: "execute exactly once"},
		{name: "release control", service: ServiceConfig{Kind: ServiceKindRun, Image: "busybox", Command: ListValue("true"), Deploy: DeployConfig{Release: &ReleaseConfig{Command: []string{"true"}}}}, want: "cannot set deploy rollout controls"},
		{name: "health timing", service: ServiceConfig{Kind: ServiceKindRun, Image: "busybox", Command: ListValue("true"), HealthCheck: HealthCheckConfig{Interval: "10s"}}, want: "cannot set healthCheck"},
		{name: "imports", service: ServiceConfig{Kind: ServiceKindRun, Image: "busybox", Command: ListValue("true"), Imports: []string{"other.api"}}, want: "cannot set export or imports"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := minimalValidConfigWithService(tt.service)
			err := ValidateConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestValidateConfigRejectsUnknownRunImageFrom(t *testing.T) {
	cfg := minimalValidConfigWithService(ServiceConfig{Kind: ServiceKindRun, ImageFrom: "missing", Command: ListValue("true")})
	if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "references unknown service") {
		t.Fatalf("error = %v", err)
	}
}
