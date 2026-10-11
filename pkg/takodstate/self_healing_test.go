package takodstate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takoapi"
)

// TestDesiredServiceCarriesReadiness: deploy.readiness reaches desired state
// when it defines a check, so self-healing on the node can probe exactly
// what the rollout gates on.
func TestDesiredServiceCarriesReadiness(t *testing.T) {
	ready := config.ServiceConfig{Image: "web:1", Port: 3000, HealthCheck: config.HealthCheckConfig{Path: "/health"}}
	ready.Deploy.Readiness = config.DeployReadinessConfig{Path: "/ready", Interval: "15s"}
	desired := sanitizeDesiredService("web", ready, "web:1")
	if desired.Readiness == nil || desired.Readiness.Path != "/ready" || desired.Readiness.Interval != "15s" {
		t.Fatalf("readiness = %+v", desired.Readiness)
	}
	encoded, _ := json.Marshal(desired)
	var document takoapi.DesiredServiceDocument
	if err := json.Unmarshal(encoded, &document); err != nil || !strings.Contains(string(document.Readiness), `"/ready"`) {
		t.Fatalf("takoapi readiness = %s %v", document.Readiness, err)
	}
	plain := config.ServiceConfig{Image: "web:1", Port: 3000}
	plain.Deploy.Readiness = config.DeployReadinessConfig{Interval: "15s"}
	if desired := sanitizeDesiredService("web", plain, "web:1"); desired.Readiness != nil {
		t.Fatalf("readiness without a check = %+v, want omitted", desired.Readiness)
	}
	encoded, _ = json.Marshal(sanitizeDesiredService("web", plain, "web:1"))
	if strings.Contains(string(encoded), "readiness") {
		t.Fatalf("readiness not omitted: %s", encoded)
	}
}

func TestBuildDesiredRevisionCarriesSelfHealingPolicy(t *testing.T) {
	enabled, disabled := true, false
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "demo"},
		Environments: map[string]config.EnvironmentConfig{
			"production": {SelfHealing: &config.SelfHealingConfig{Enabled: &enabled, UnhealthyThreshold: 4}},
		},
	}
	services := map[string]config.ServiceConfig{
		"web":     {Image: "web:1", Port: 3000},
		"quiet":   {Image: "quiet:1", SelfHealing: &config.SelfHealingConfig{Enabled: &disabled}},
		"db":      {Image: "postgres:16", Persistent: true},
		"cleanup": {Kind: config.ServiceKindJob, Image: "app:1"},
	}
	revision, err := BuildDesiredRevision(cfg, "production", "deploy", services, nil, nil, GitInfo{})
	if err != nil {
		t.Fatal(err)
	}
	web := revision.Services["web"].SelfHealing
	want := takoapi.SelfHealingPolicyDocument{Enabled: true, RestartUnhealthy: true, StartExited: true, UnhealthyThreshold: 4, MaxRestarts: 3, WindowSeconds: 1800}
	if web == nil || *web != want {
		t.Fatalf("web selfHealing = %+v, want %+v", web, want)
	}
	for _, name := range []string{"quiet", "db", "cleanup"} {
		if policy := revision.Services[name].SelfHealing; policy != nil {
			t.Fatalf("%s selfHealing = %+v, want omitted", name, policy)
		}
	}

	encoded, err := json.Marshal(revision)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(encoded), `"selfHealing"`) != 1 {
		t.Fatalf("selfHealing must be omitted when disabled: %s", encoded)
	}
	var document takoapi.DesiredStateDocument
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	if got := document.Services["web"].SelfHealing; got == nil || *got != want {
		t.Fatalf("round trip selfHealing = %+v", got)
	}
	var back DesiredRevision
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatal(err)
	}
	if got := back.Services["web"].SelfHealing; got == nil || *got != want {
		t.Fatalf("takodstate round trip selfHealing = %+v", got)
	}

	// Disabling healing changes only the desired document, never the services'
	// container identity fields.
	cfg.Environments["production"] = config.EnvironmentConfig{}
	plain, err := BuildDesiredRevision(cfg, "production", "deploy", services, nil, nil, GitInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Services["web"].SelfHealing != nil {
		t.Fatalf("disabled environment still carries policy")
	}
	withPolicy := revision.Services["web"]
	withPolicy.SelfHealing = nil
	withoutPolicy := plain.Services["web"]
	a, _ := json.Marshal(withPolicy)
	b, _ := json.Marshal(withoutPolicy)
	if string(a) != string(b) {
		t.Fatalf("selfHealing changed other desired service fields:\n%s\n%s", a, b)
	}
}
