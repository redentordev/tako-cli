package takod

import (
	"context"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

func TestParseActualState(t *testing.T) {
	output := `
demo_production_web_1|registry.example.com/demo/web:abc|container-a|||demo|production|web
demo_production_web_2|registry.example.com/demo/web:abc|container-b|||demo|production|web
demo_production_api_v2_3|registry.example.com/demo/api:def|container-c|hash-api||demo|production|api_v2
other_production_web_1|ignored|container-d|||other|production|web
malformed
`

	actual := ParseActualState("demo", "production", output)
	if len(actual.Services) != 2 {
		t.Fatalf("expected two services, got %#v", actual.Services)
	}
	if got := actual.Services["web"].Replicas; got != 2 {
		t.Fatalf("expected web replicas to be 2, got %d", got)
	}
	if got := actual.Services["api_v2"].Replicas; got != 1 {
		t.Fatalf("expected api_v2 replicas to be 1, got %d", got)
	}
	if actual.Services["web"].Containers[1] != "container-b" {
		t.Fatalf("unexpected web containers: %#v", actual.Services["web"].Containers)
	}
	if got := actual.Services["api_v2"].ConfigHash; got != "hash-api" {
		t.Fatalf("api_v2 config hash = %q, want hash-api", got)
	}
}

func TestParseActualStateCapturesHealthCounts(t *testing.T) {
	output := `
demo_production_web_1|demo/web:1|container-a|||demo|production|web|Up 10 seconds (healthy)
demo_production_web_2|demo/web:1|container-b|||demo|production|web|Up 8 seconds (unhealthy)
demo_production_web_3|demo/web:1|container-c|||demo|production|web|Up 3 seconds (health: starting)
demo_production_worker_1|demo/worker:1|container-d|||demo|production|worker|Up 1 minute
`

	actual := ParseActualState("demo", "production", output)
	web := actual.Services["web"]
	if web == nil {
		t.Fatal("missing web service")
	}
	if web.HealthyReplicas != 1 || web.UnhealthyReplicas != 1 || web.StartingReplicas != 1 {
		t.Fatalf("web health counts = healthy:%d unhealthy:%d starting:%d", web.HealthyReplicas, web.UnhealthyReplicas, web.StartingReplicas)
	}
	if got := actual.Services["worker"].NoHealthcheckReplicas; got != 1 {
		t.Fatalf("worker no-healthcheck replicas = %d, want 1", got)
	}
}

func TestParseActualStateClearsMixedConfigHash(t *testing.T) {
	output := `
demo_production_web_1|registry.example.com/demo/web:abc|container-a|hash-a||demo|production|web
demo_production_web_2|registry.example.com/demo/web:abc|container-b|hash-b||demo|production|web
`

	actual := ParseActualState("demo", "production", output)
	if got := actual.Services["web"].ConfigHash; got != "" {
		t.Fatalf("mixed config hash = %q, want empty", got)
	}
}

func TestParseActualStateUsesRuntimeLabelsForHashedContainers(t *testing.T) {
	identity := runtimeid.ServiceIdentity("demo-app", "production", "web")
	output := strings.Join([]string{
		runtimeid.ContainerName("demo-app", "production", "web", 1) + "|demo/web:1|container-a|hash-web|" + identity + "|demo-app|production|web",
		runtimeid.ContainerName("other", "production", "web", 1) + "|other/web:1|container-b|hash-other|" + runtimeid.ServiceIdentity("other", "production", "web") + "|other|production|web",
	}, "\n")

	actual := ParseActualState("demo-app", "production", output)
	if len(actual.Services) != 1 {
		t.Fatalf("expected one service, got %#v", actual.Services)
	}
	if got := actual.Services["web"].RuntimeID; got != identity {
		t.Fatalf("runtime id = %q, want %q", got, identity)
	}
	if got := actual.Services["web"].ConfigHash; got != "hash-web" {
		t.Fatalf("config hash = %q, want hash-web", got)
	}
}

func TestParseActualStateClearsMixedRuntimeID(t *testing.T) {
	identity := runtimeid.ServiceIdentity("demo", "production", "web")
	output := `
demo_production_web_1|demo/web:1|container-a|hash-web|` + identity + `|demo|production|web
demo_production_web_2|demo/web:1|container-b|hash-web||demo|production|web
`

	actual := ParseActualState("demo", "production", output)
	if got := actual.Services["web"].RuntimeID; got != "" {
		t.Fatalf("mixed runtime id = %q, want empty", got)
	}
}

func TestParseActualStateIgnoresUnlabeledContainers(t *testing.T) {
	output := `
demo_production_web_1|registry.example.com/demo/web:abc|container-a
`

	actual := ParseActualState("demo", "production", output)
	if len(actual.Services) != 0 {
		t.Fatalf("expected unlabeled containers to be ignored, got %#v", actual.Services)
	}
}

func TestGatherActualStateRejectsInvalidIdentifiers(t *testing.T) {
	tests := []struct {
		name        string
		project     string
		environment string
		want        string
	}{
		{name: "project", project: "../demo", environment: "production", want: "invalid project name"},
		{name: "environment", project: "demo", environment: "prod\nbad", want: "invalid environment name"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := GatherActualState(context.Background(), tt.project, tt.environment)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected %q error, got %v", tt.want, err)
			}
		})
	}
}
