package drift

import (
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestDetectorUsesActualServiceProvider(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "demo"},
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Services: map[string]config.ServiceConfig{
					"web": {
						Image:    "demo:web",
						Replicas: 2,
					},
				},
			},
		},
	}
	detector := NewDetectorWithActualProvider(cfg, "production", nil, false, func() (map[string]ActualService, error) {
		return map[string]ActualService{
			"web": {
				Name:     "web",
				Image:    "demo:web",
				Replicas: 2,
				Running:  2,
			},
		}, nil
	})

	state, err := detector.CheckOnce()
	if err != nil {
		t.Fatalf("CheckOnce returned error: %v", err)
	}
	if len(state.Drifts) != 0 {
		t.Fatalf("drifts = %#v, want none", state.Drifts)
	}
	if len(state.ServicesOK) != 1 || state.ServicesOK[0] != "web" {
		t.Fatalf("services ok = %#v, want web", state.ServicesOK)
	}
}

func TestDetectorRequiresClientOrActualServiceProvider(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "demo"},
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Services: map[string]config.ServiceConfig{},
			},
		},
	}
	detector := NewDetector(nil, cfg, "production", nil, false)

	if _, err := detector.CheckOnce(); err == nil {
		t.Fatal("CheckOnce should fail without a client or provider")
	}
}
