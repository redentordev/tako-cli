package cmd

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

func takodSocketFromConfig(cfg *config.Config) string {
	if cfg.Runtime != nil && cfg.Runtime.Agent != nil && cfg.Runtime.Agent.Socket != "" {
		return cfg.Runtime.Agent.Socket
	}
	return takodclient.DefaultSocket
}

func requireTakodRuntime(cfg *config.Config) error {
	if !cfg.IsTakodRuntime() {
		return fmt.Errorf("runtime.mode=%s is not supported; Tako now uses runtime.mode=takod", cfg.GetRuntimeMode())
	}
	return nil
}

func cleanupViaTakod(client *ssh.Client, cfg *config.Config, request takod.CleanupRequest) (*takod.CleanupResponse, error) {
	var response takod.CleanupResponse
	output, err := takodclient.RequestJSON(client, takodSocketFromConfig(cfg), "POST", "/v1/cleanup", request)
	if err != nil {
		return nil, err
	}
	if err := decodeTakodJSON(output, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

func actualStateViaTakod(client *ssh.Client, cfg *config.Config, environment string) (*takod.ActualStateResponse, error) {
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

func printCleanupWarnings(response *takod.CleanupResponse) {
	if response == nil {
		return
	}
	for _, warning := range response.Warnings {
		fmt.Printf("  Warning: %s\n", warning)
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
	add(runtimeid.LegacyProxyConfigFileName(project, environment))
	for serviceName, service := range services {
		if service.IsPublic() {
			add(maintenanceProxyConfigFileName(project, environment, serviceName))
			add(runtimeid.LegacyMaintenanceProxyConfigFileName(project, environment, serviceName))
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
	seen := make(map[string]bool)
	for serviceName, service := range services {
		if service.Build == "" && service.Image != "" {
			continue
		}
		repository := imageRepositoryFromRef(cfg.GetFullImageName(serviceName, environment))
		if repository != "" {
			seen[repository] = true
		}
	}
	repositories := make([]string, 0, len(seen))
	for repository := range seen {
		repositories = append(repositories, repository)
	}
	sort.Strings(repositories)
	return repositories
}

func imageRepositoryFromRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if digest := strings.Index(ref, "@"); digest >= 0 {
		ref = ref[:digest]
	}
	lastSlash := strings.LastIndex(ref, "/")
	lastColon := strings.LastIndex(ref, ":")
	if lastColon > lastSlash {
		ref = ref[:lastColon]
	}
	return ref
}

func runtimeProxyConfigFileName(project string, environment string) string {
	return runtimeid.ProxyConfigFileName(project, environment)
}
