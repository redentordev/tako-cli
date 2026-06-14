package takod

import (
	"context"
	"strings"
	"testing"
)

func TestParseActualState(t *testing.T) {
	output := `
demo_production_web_1|registry.example.com/demo/web:abc|container-a
demo_production_web_2|registry.example.com/demo/web:abc|container-b
demo_production_api_v2_3|registry.example.com/demo/api:def|container-c|hash-api
other_production_web_1|ignored|container-d
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
demo_production_web_1|registry.example.com/demo/web:abc|container-a|hash-a
demo_production_web_2|registry.example.com/demo/web:abc|container-b|hash-b
`

	actual := ParseActualState("demo", "production", output)
	if got := actual.Services["web"].ConfigHash; got != "" {
		t.Fatalf("mixed config hash = %q, want empty", got)
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
