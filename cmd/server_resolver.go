package cmd

import (
	"fmt"
	"sort"

	"github.com/redentordev/tako-cli/pkg/config"
)

func serverConfigByName(cfg *config.Config, serverName string) (config.ServerConfig, error) {
	server, ok := cfg.Servers[serverName]
	if !ok {
		return config.ServerConfig{}, fmt.Errorf("server %s not found in configuration", serverName)
	}
	return server, nil
}

func schedulableMutationServerSet(cfg *config.Config, envName string, servers map[string]config.ServerConfig, explicit bool) (map[string]config.ServerConfig, []string, error) {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	targets, err := config.ResolveSchedulableMutationTargets(cfg.Servers, names, envName, explicit)
	if err != nil {
		return nil, nil, err
	}
	filtered := make(map[string]config.ServerConfig, len(targets))
	for _, name := range targets {
		filtered[name] = servers[name]
	}
	return filtered, targets, nil
}

func resolveEnvironmentServerSet(cfg *config.Config, envName, serverFlag string) (map[string]config.ServerConfig, error) {
	serverNames, err := statePullServerNames(cfg, envName, serverFlag)
	if err != nil {
		return nil, err
	}
	servers := make(map[string]config.ServerConfig, len(serverNames))
	for _, serverName := range serverNames {
		server, ok := cfg.Servers[serverName]
		if !ok {
			return nil, fmt.Errorf("server %s not found in config", serverName)
		}
		servers[serverName] = server
	}
	return servers, nil
}
