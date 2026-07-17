package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

func takodSocketFromConfig(cfg *config.Config) string {
	return engine.TakodSocketFromConfig(cfg)
}

func requireTakodRuntime(cfg *config.Config) error {
	return engine.RequireTakodRuntime(cfg)
}

func cleanupViaTakod(client *ssh.Client, cfg *config.Config, request takod.CleanupRequest) (*takod.CleanupResponse, error) {
	return engine.CleanupViaTakod(client, cfg, request)
}

func actualStateViaTakod(client any, cfg *config.Config, environment string) (*takod.ActualStateResponse, error) {
	var response takod.ActualStateResponse
	output, err := takodclient.RequestJSON(
		client,
		takodSocketFromConfig(cfg),
		"GET",
		takodclient.ActualStateEndpoint(cfg.Project.Name, environment),
		nil,
	)
	if err != nil {
		return nil, err
	}
	if err := decodeTakodJSON(output, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

func decodeTakodJSON(output string, value any) error {
	if err := json.Unmarshal([]byte(output), value); err != nil {
		return fmt.Errorf("failed to parse takod response: %w", err)
	}
	return nil
}

func printCleanupWarnings(out io.Writer, response *takod.CleanupResponse) {
	if response == nil {
		return
	}
	for _, warning := range response.Warnings {
		fmt.Fprintf(out, "  Warning: %s\n", warning)
	}
}

func cleanupProxyFiles(project string, environment string, services map[string]config.ServiceConfig) []string {
	seen := make(map[string]bool)
	add := func(name string) {
		if name != "" {
			seen[name] = true
		}
	}
	add(runtimeProxyConfigFileName(project, environment))
	for serviceName, service := range services {
		if service.IsProxied() {
			add(maintenanceProxyConfigFileName(project, environment, serviceName))
		}
	}
	files := make([]string, 0, len(seen))
	for name := range seen {
		files = append(files, name)
	}
	sort.Strings(files)
	return files
}

func cleanupImageRepositories(cfg *config.Config, environment string, services map[string]config.ServiceConfig) []string {
	return engine.CleanupImageRepositories(cfg, environment, services)
}

func externalVolumeNamesForEnvironment(cfg *config.Config, environment string) []string {
	return engine.ExternalVolumeNamesForEnvironment(cfg, environment)
}

func imageRepositoryFromRef(ref string) string {
	return engine.ImageRepositoryFromRef(ref)
}

func runtimeProxyConfigFileName(project string, environment string) string {
	return runtimeid.ProxyConfigFileName(project, environment)
}
