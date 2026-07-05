package deployplan

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/reconcile"
)

func TestFilterActualStateForServicesScopesTargetedDeployPlan(t *testing.T) {
	webActual := &reconcile.ActualService{Name: "web", Image: "demo/web:old", Replicas: 1}
	actualState := map[string]*reconcile.ActualService{
		"web": webActual,
		"api": {Name: "api", Image: "demo/api:old", Replicas: 1},
	}
	services := map[string]config.ServiceConfig{
		"web": {Build: "."},
	}

	got := FilterActualStateForServices(actualState, services)
	if len(got) != 1 {
		t.Fatalf("filtered actual services = %d, want 1", len(got))
	}
	if got["web"] != webActual {
		t.Fatalf("filtered web actual = %#v, want original web actual", got["web"])
	}
	if _, ok := got["api"]; ok {
		t.Fatal("filtered actual state included unselected api service")
	}
}

func TestHasBuildServices(t *testing.T) {
	if HasBuildServices(map[string]config.ServiceConfig{
		"web": {Build: "."},
	}) != true {
		t.Fatal("HasBuildServices should detect build-backed services")
	}
	if HasBuildServices(map[string]config.ServiceConfig{
		"db": {Image: "postgres:16"},
	}) != false {
		t.Fatal("HasBuildServices should ignore image-only services")
	}
	if HasBuildServices(nil) {
		t.Fatal("HasBuildServices should reject empty service maps")
	}
}

func TestServicesToDeployForEmptyPlanIncludesOnlyBuildServices(t *testing.T) {
	services := map[string]config.ServiceConfig{
		"web": {Build: "."},
		"db":  {Image: "postgres:16"},
	}
	plan := &reconcile.ReconciliationPlan{}

	got := ServicesToDeployForPlan(plan, services, false, false)
	if len(got) != 1 {
		t.Fatalf("ServicesToDeployForPlan returned %d service(s), want 1: %#v", len(got), got)
	}
	if _, ok := got["web"]; !ok {
		t.Fatalf("build service missing from deploy set: %#v", got)
	}
	if _, ok := got["db"]; ok {
		t.Fatalf("image-only service should not be redeployed on empty plan: %#v", got)
	}
}

func TestServicesToDeployForPlanIncludesAddsAndUpdatesOnly(t *testing.T) {
	services := map[string]config.ServiceConfig{
		"web":    {Build: "."},
		"worker": {Image: "worker:1"},
		"old":    {Image: "old:1"},
	}
	plan := &reconcile.ReconciliationPlan{
		Summary: reconcile.ReconciliationSummary{Total: 4, Adds: 1, Updates: 1, Removes: 1, NoOps: 1},
		Changes: []reconcile.ServiceChange{
			{Type: reconcile.ChangeUpdate, ServiceName: "web"},
			{Type: reconcile.ChangeAdd, ServiceName: "worker"},
			{Type: reconcile.ChangeRemove, ServiceName: "old"},
			{Type: reconcile.ChangeNone, ServiceName: "noop"},
		},
	}

	got := ServicesToDeployForPlan(plan, services, false, false)
	if len(got) != 2 {
		t.Fatalf("ServicesToDeployForPlan returned %d service(s), want 2: %#v", len(got), got)
	}
	for _, want := range []string{"web", "worker"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("%s missing from deploy set: %#v", want, got)
		}
	}
	if _, ok := got["old"]; ok {
		t.Fatalf("removed service should not be in deploy set: %#v", got)
	}
}

func TestServicesToDeployForPlanAlwaysIncludesBuildServices(t *testing.T) {
	services := map[string]config.ServiceConfig{
		"web":    {Build: "."},
		"api":    {Build: "./api"},
		"worker": {Image: "worker:2"},
		"old":    {Image: "old:1"},
	}
	plan := &reconcile.ReconciliationPlan{
		Summary: reconcile.ReconciliationSummary{Total: 3, Updates: 1, Removes: 1, NoOps: 1},
		Changes: []reconcile.ServiceChange{
			{Type: reconcile.ChangeUpdate, ServiceName: "worker"},
			{Type: reconcile.ChangeRemove, ServiceName: "old"},
			{Type: reconcile.ChangeNone, ServiceName: "api"},
		},
	}

	got := ServicesToDeployForPlan(plan, services, false, false)
	for _, want := range []string{"web", "api", "worker"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("%s missing from deploy set: %#v", want, got)
		}
	}
	if _, ok := got["old"]; ok {
		t.Fatalf("removed service should not be in deploy set: %#v", got)
	}
	if len(got) != 3 {
		t.Fatalf("ServicesToDeployForPlan returned %d service(s), want 3: %#v", len(got), got)
	}
}

func TestBlueGreenPruneGracePeriodUsesMaxConfiguredGrace(t *testing.T) {
	services := map[string]config.ServiceConfig{
		"web": {
			Deploy: config.DeployConfig{
				Strategy:    config.DeployStrategyBlueGreen,
				GracePeriod: "2s",
			},
		},
		"api": {
			Deploy: config.DeployConfig{
				Strategy:    config.DeployStrategyBlueGreen,
				GracePeriod: "5s",
			},
		},
		"worker": {
			Deploy: config.DeployConfig{
				Strategy:    config.DeployStrategyRolling,
				GracePeriod: "10s",
			},
		},
	}
	keepRevisions := map[string]string{
		"api":    "rev-api",
		"web":    "rev-web",
		"worker": "rev-worker",
	}

	grace, names, err := BlueGreenPruneGracePeriod(services, keepRevisions)
	if err != nil {
		t.Fatalf("BlueGreenPruneGracePeriod returned error: %v", err)
	}
	if grace != 5*time.Second {
		t.Fatalf("grace = %s, want 5s", grace)
	}
	if strings.Join(names, ",") != "api,web" {
		t.Fatalf("names = %#v, want api and web sorted", names)
	}
}

func TestManualPromotionPendingServicesOnlyIncludesUpdatesWithCurrentBlue(t *testing.T) {
	servicesToDeploy := map[string]config.ServiceConfig{
		"web": {
			Deploy: config.DeployConfig{
				Strategy:  config.DeployStrategyBlueGreen,
				Promotion: config.DeployPromotionManual,
			},
		},
		"api": {
			Deploy: config.DeployConfig{
				Strategy: config.DeployStrategyBlueGreen,
			},
		},
		"new": {
			Deploy: config.DeployConfig{
				Strategy:  config.DeployStrategyBlueGreen,
				Promotion: config.DeployPromotionManual,
			},
		},
	}
	actualState := map[string]*reconcile.ActualService{
		"web": {CurrentRevision: "rev-blue"},
	}

	got := ManualPromotionPendingServices(servicesToDeploy, actualState)
	if len(got) != 1 || got[0] != "web" {
		t.Fatalf("pending manual services = %#v, want web only", got)
	}
}

func TestShouldWarmManualPromotionService(t *testing.T) {
	manual := config.ServiceConfig{Deploy: config.DeployConfig{Strategy: config.DeployStrategyBlueGreen, Promotion: config.DeployPromotionManual}}
	automatic := config.ServiceConfig{Deploy: config.DeployConfig{Strategy: config.DeployStrategyBlueGreen}}
	actualState := map[string]*reconcile.ActualService{"web": {CurrentRevision: "rev-blue"}}

	if !IsManualBlueGreenService(manual) {
		t.Fatal("IsManualBlueGreenService should detect manual blue-green service")
	}
	if !ShouldWarmManualPromotionService("web", manual, actualState) {
		t.Fatal("ShouldWarmManualPromotionService should warm manual service with current revision")
	}
	if ShouldWarmManualPromotionService("new", manual, actualState) {
		t.Fatal("ShouldWarmManualPromotionService should not warm service without current revision")
	}
	if ShouldWarmManualPromotionService("web", automatic, actualState) {
		t.Fatal("ShouldWarmManualPromotionService should not warm automatic service")
	}
}

func TestServicesToDeployForPlanForceIncludesAllSelectedServices(t *testing.T) {
	services := map[string]config.ServiceConfig{
		"web": {Build: "."},
		"db":  {Image: "postgres:16"},
	}
	plan := &reconcile.ReconciliationPlan{}

	got := ServicesToDeployForPlan(plan, services, true, false)
	for _, want := range []string{"web", "db"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("%s missing from forced deploy set: %#v", want, got)
		}
	}
	if len(got) != 2 {
		t.Fatalf("forced deploy set has %d service(s), want 2: %#v", len(got), got)
	}
}

func TestServicesToDeployForPlanBroadForceSkipsPersistentServices(t *testing.T) {
	services := map[string]config.ServiceConfig{
		"web": {Build: "."},
		"db":  {Image: "postgres:16", Persistent: true, Volumes: []string{"pgdata:/var/lib/postgresql/data"}},
	}
	plan := &reconcile.ReconciliationPlan{}

	got := ServicesToDeployForPlan(plan, services, true, false)
	if _, ok := got["web"]; !ok {
		t.Fatalf("web missing from broad forced deploy set: %#v", got)
	}
	if _, ok := got["db"]; ok {
		t.Fatalf("persistent db should be skipped by broad force: %#v", got)
	}
	if skipped := PersistentServicesSkippedByForce(services, got, true, false); !slices.Equal(skipped, []string{"db"}) {
		t.Fatalf("skipped = %#v, want db", skipped)
	}
}

func TestServicesToDeployForPlanTargetedForceIncludesPersistentService(t *testing.T) {
	services := map[string]config.ServiceConfig{
		"db": {Image: "postgres:16", Persistent: true, Volumes: []string{"pgdata:/var/lib/postgresql/data"}},
	}
	plan := &reconcile.ReconciliationPlan{}

	got := ServicesToDeployForPlan(plan, services, true, true)
	if _, ok := got["db"]; !ok {
		t.Fatalf("targeted force should include persistent db: %#v", got)
	}
	if skipped := PersistentServicesSkippedByForce(services, got, true, true); len(skipped) != 0 {
		t.Fatalf("targeted force skipped persistent service: %#v", skipped)
	}
}
