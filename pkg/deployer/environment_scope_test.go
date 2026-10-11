package deployer

import (
	"reflect"
	"sort"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func stagingProductionConfig() *config.Config {
	return &config.Config{
		Servers: map[string]config.ServerConfig{
			"app-prod": {Host: "203.0.113.20", User: "deploy"},
			"staging":  {Host: "203.0.113.30", User: "deploy"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"staging":    {Servers: []string{"staging"}},
			"production": {Servers: []string{"app-prod"}},
		},
	}
}

func TestStagingBuildNeverSelectsAnotherEnvironmentAsSource(t *testing.T) {
	// "app-prod" sorts before "staging"; a project-wide builder list would
	// pick it as sources[0] for the staging build.
	deploy := &Deployer{config: stagingProductionConfig(), environment: "staging"}
	builders, err := deploy.remoteBuilderServers()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(builders, []string{"staging"}) {
		t.Fatalf("staging builders = %v", builders)
	}
}

func TestRetainedImageLookupStaysInsideEnvironment(t *testing.T) {
	deploy := &Deployer{config: stagingProductionConfig(), environment: "staging"}
	servers, err := deploy.retainedImageSourceServers()
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(servers)
	if !reflect.DeepEqual(servers, []string{"staging"}) {
		t.Fatalf("retained-image lookup servers = %v", servers)
	}
}
