package deployplan

import (
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/reconcile"
)

func TestProxyActiveRevisionsUsesDeployedAndExistingRevisions(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "demo", Version: "1.0.0"},
	}
	services := map[string]config.ServiceConfig{
		"web": {
			Build: ".",
			Deploy: config.DeployConfig{
				Strategy: config.DeployStrategyRolling,
			},
		},
		"api": {
			Image: "api:stable",
			Deploy: config.DeployConfig{
				Strategy: config.DeployStrategyBlueGreen,
			},
		},
		"worker": {
			Image: "worker:stable",
		},
	}
	servicesToDeploy := map[string]config.ServiceConfig{
		"web": services["web"],
	}
	imageRefs := map[string]string{
		"web": "demo/web:abcdef1234567890",
	}
	actualState := map[string]*reconcile.ActualService{
		"web": {CurrentRevision: "rev-web-old"},
		"api": {CurrentRevision: "rev-api-current"},
	}

	got := ProxyActiveRevisions(cfg, "production", services, servicesToDeploy, imageRefs, actualState)
	wantWeb := deployer.ServiceRevisionID(cfg.Project.Name, "production", "web", imageRefs["web"], services["web"])
	if got["web"] != wantWeb {
		t.Fatalf("web revision = %q, want deployed revision %q", got["web"], wantWeb)
	}
	if got["api"] != "rev-api-current" {
		t.Fatalf("api revision = %q, want current actual revision", got["api"])
	}
	if _, ok := got["worker"]; ok {
		t.Fatalf("recreate service should not get active revision: %#v", got)
	}
}

func TestProxyActiveRevisionsKeepsCurrentRevisionForManualBlueGreenDeploy(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "demo", Version: "1.0.0"},
	}
	services := map[string]config.ServiceConfig{
		"web": {
			Build: ".",
			Deploy: config.DeployConfig{
				Strategy:  config.DeployStrategyBlueGreen,
				Promotion: config.DeployPromotionManual,
			},
		},
	}
	servicesToDeploy := map[string]config.ServiceConfig{
		"web": services["web"],
	}
	imageRefs := map[string]string{
		"web": "demo/web:abcdef1234567890",
	}
	actualState := map[string]*reconcile.ActualService{
		"web": {CurrentRevision: "rev-web-blue"},
	}

	got := ProxyActiveRevisions(cfg, "production", services, servicesToDeploy, imageRefs, actualState)
	if got["web"] != "rev-web-blue" {
		t.Fatalf("web revision = %q, want current blue revision", got["web"])
	}
}

func TestProxyActiveRevisionsReturnsNilWithoutRevisions(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "demo", Version: "1.0.0"},
	}
	services := map[string]config.ServiceConfig{
		"worker": {Image: "worker:stable"},
	}

	if got := ProxyActiveRevisions(cfg, "production", services, nil, nil, nil); got != nil {
		t.Fatalf("ProxyActiveRevisions() = %#v, want nil", got)
	}
}

func TestDeployedProxyActiveRevisionsOnlyIncludesDeployedServices(t *testing.T) {
	servicesToDeploy := map[string]config.ServiceConfig{
		"web":    {Build: "."},
		"worker": {Image: "worker:stable"},
	}
	activeRevisions := map[string]string{
		"web": "rev-web-new",
		"api": "rev-api-current",
	}

	got := DeployedProxyActiveRevisions(servicesToDeploy, activeRevisions)
	if got["web"] != "rev-web-new" {
		t.Fatalf("web revision = %q, want deployed active revision", got["web"])
	}
	if _, ok := got["api"]; ok {
		t.Fatalf("unchanged service should not be pruned: %#v", got)
	}
	if _, ok := got["worker"]; ok {
		t.Fatalf("deployed service without active revision should not be pruned: %#v", got)
	}
	if len(got) != 1 {
		t.Fatalf("deployed active revisions = %#v, want only web", got)
	}
}

func TestDeployedProxyActiveRevisionsReturnsNilWithoutDeployedRevisions(t *testing.T) {
	servicesToDeploy := map[string]config.ServiceConfig{
		"worker": {Image: "worker:stable"},
	}
	activeRevisions := map[string]string{
		"api": "rev-api-current",
	}

	if got := DeployedProxyActiveRevisions(servicesToDeploy, activeRevisions); got != nil {
		t.Fatalf("DeployedProxyActiveRevisions() = %#v, want nil", got)
	}
}

func TestDeployedProxyActiveRevisionsSkipsManualBlueGreenWarmDeploys(t *testing.T) {
	servicesToDeploy := map[string]config.ServiceConfig{
		"web": {
			Build: ".",
			Deploy: config.DeployConfig{
				Strategy:  config.DeployStrategyBlueGreen,
				Promotion: config.DeployPromotionManual,
			},
		},
	}
	activeRevisions := map[string]string{
		"web": "rev-web-blue",
	}

	got := DeployedProxyActiveRevisions(servicesToDeploy, activeRevisions)
	if len(got) != 0 {
		t.Fatalf("manual warm deploy should not prune revisions: %#v", got)
	}
}
