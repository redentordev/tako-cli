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

func TestParseActualStateReadsRevisionStrategyAndActiveState(t *testing.T) {
	output := `
demo_production_web_1|demo/web:1|container-a|hash-web||demo|production|web|false|rev-new|rolling|1|true
demo_production_web_2|demo/web:2|container-b|hash-web||demo|production|web|false|rev-old|rolling|2|false
`

	actual := ParseActualState("demo", "production", output)
	web := actual.Services["web"]
	if web == nil {
		t.Fatalf("web service missing: %#v", actual.Services)
	}
	if web.CurrentRevision != "rev-new" {
		t.Fatalf("current revision = %q, want rev-new", web.CurrentRevision)
	}
	if web.PreviousRevision != "rev-old" {
		t.Fatalf("previous revision = %q, want rev-old", web.PreviousRevision)
	}
	if web.DeployStrategy != "rolling" {
		t.Fatalf("deploy strategy = %q, want rolling", web.DeployStrategy)
	}
	if len(web.ActiveContainers) != 1 || web.ActiveContainers[0] != "container-a" {
		t.Fatalf("active containers = %#v, want container-a", web.ActiveContainers)
	}
	if len(web.WarmingContainers) != 1 || web.WarmingContainers[0] != "container-b" {
		t.Fatalf("warming containers = %#v, want container-b", web.WarmingContainers)
	}
	if len(web.WarmingRevisions) != 1 || web.WarmingRevisions[0] != "rev-old" {
		t.Fatalf("warming revisions = %#v, want rev-old", web.WarmingRevisions)
	}
	if web.RevisionImages["rev-new"] != "demo/web:1" || web.RevisionImages["rev-old"] != "demo/web:2" {
		t.Fatalf("revision images = %#v, want images keyed by revision", web.RevisionImages)
	}
}

func TestParseContainerHealth(t *testing.T) {
	cases := []struct {
		status string
		want   string
	}{
		{"Up 5 minutes (healthy)", HealthStateHealthy},
		{"Up 2 seconds (health: starting)", HealthStateStarting},
		{"Up About an hour (unhealthy)", HealthStateUnhealthy},
		{"Up 5 minutes", ""},
		{"Restarting (1) 5 seconds ago", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := ParseContainerHealth(tc.status); got != tc.want {
			t.Errorf("ParseContainerHealth(%q) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestMergeHealthStatesWorstWins(t *testing.T) {
	cases := []struct {
		existing string
		incoming string
		want     string
	}{
		{"", HealthStateHealthy, HealthStateHealthy},
		{HealthStateHealthy, "", HealthStateHealthy},
		{HealthStateHealthy, HealthStateStarting, HealthStateStarting},
		{HealthStateUnhealthy, HealthStateHealthy, HealthStateUnhealthy},
		{HealthStateStarting, HealthStateUnhealthy, HealthStateUnhealthy},
		{"", "", ""},
	}
	for _, tc := range cases {
		if got := MergeHealthStates(tc.existing, tc.incoming); got != tc.want {
			t.Errorf("MergeHealthStates(%q, %q) = %q, want %q", tc.existing, tc.incoming, got, tc.want)
		}
	}
}

func TestParseActualStateAggregatesActiveContainerHealth(t *testing.T) {
	output := `
demo_production_web_1|demo/web:1|container-a|hash-web||demo|production|web|false|rev-new|rolling|1|true|Up 5 minutes (healthy)
demo_production_web_2|demo/web:1|container-b|hash-web||demo|production|web|false|rev-new|rolling|2|true|Up 10 seconds (unhealthy)
demo_production_web_3|demo/web:2|container-c|hash-web||demo|production|web|false|rev-old|rolling|3|false|Up 2 seconds (health: starting)
demo_production_api_1|demo/api:1|container-d|hash-api||demo|production|api|false|rev-1|rolling|1|true|Up 3 hours
`

	actual := ParseActualState("demo", "production", output)
	web := actual.Services["web"]
	if web == nil {
		t.Fatalf("web service missing: %#v", actual.Services)
	}
	if web.Health != HealthStateUnhealthy {
		t.Fatalf("web health = %q, want unhealthy (worst active container wins; warming excluded)", web.Health)
	}
	api := actual.Services["api"]
	if api == nil {
		t.Fatalf("api service missing: %#v", actual.Services)
	}
	if api.Health != "" {
		t.Fatalf("api health = %q, want empty for containers without a health check", api.Health)
	}
}

// TestParseActualStateWithoutStatusColumn pins version-skew behavior: output
// from an older docker ps format (no status column) parses with empty health.
func TestParseActualStateWithoutStatusColumn(t *testing.T) {
	output := `
demo_production_web_1|demo/web:1|container-a|hash-web||demo|production|web|false|rev-new|rolling|1|true
`

	actual := ParseActualState("demo", "production", output)
	web := actual.Services["web"]
	if web == nil {
		t.Fatalf("web service missing: %#v", actual.Services)
	}
	if web.Health != "" {
		t.Fatalf("web health = %q, want empty without a status column", web.Health)
	}
}

func TestParseActualStateTreatsOnlyRemainingInactiveRevisionAsCurrent(t *testing.T) {
	output := `
demo_production_web_1|demo/web:2|container-a|hash-web||demo|production|web|false|rev-green|blue_green|1|false
`

	actual := ParseActualState("demo", "production", output)
	web := actual.Services["web"]
	if web == nil {
		t.Fatalf("web service missing: %#v", actual.Services)
	}
	if web.CurrentRevision != "rev-green" {
		t.Fatalf("current revision = %q, want rev-green", web.CurrentRevision)
	}
	if web.PreviousRevision != "" {
		t.Fatalf("previous revision = %q, want empty", web.PreviousRevision)
	}
	if len(web.ActiveContainers) != 1 || web.ActiveContainers[0] != "container-a" {
		t.Fatalf("active containers = %#v, want container-a", web.ActiveContainers)
	}
	if len(web.WarmingContainers) != 0 {
		t.Fatalf("warming containers = %#v, want empty after fallback", web.WarmingContainers)
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
