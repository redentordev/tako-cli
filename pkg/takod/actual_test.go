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

func TestParseActualStateReadsPersistentLabel(t *testing.T) {
	output := `
demo_production_postgres_1|postgres:16-alpine|container-a|hash-db||demo|production|postgres|true
`

	actual := ParseActualState("demo", "production", output)
	postgres := actual.Services["postgres"]
	if postgres == nil {
		t.Fatalf("postgres service missing: %#v", actual.Services)
	}
	if !postgres.Persistent {
		t.Fatalf("postgres persistent = false, want true")
	}
}

func TestParseActualStateKeepsPersistentWhenAnyReplicaIsLabeled(t *testing.T) {
	output := `
demo_production_db_1|postgres:16-alpine|container-a|hash-db||demo|production|db|false
demo_production_db_2|postgres:16-alpine|container-b|hash-db||demo|production|db|true
`

	actual := ParseActualState("demo", "production", output)
	db := actual.Services["db"]
	if db == nil {
		t.Fatalf("db service missing: %#v", actual.Services)
	}
	if !db.Persistent {
		t.Fatalf("mixed persistent labels should preserve true")
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
