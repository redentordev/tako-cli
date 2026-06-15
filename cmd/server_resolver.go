package cmd

import (
	"fmt"

	"github.com/redentordev/tako-cli/pkg/config"
)

func serverConfigByName(cfg *config.Config, serverName string) (config.ServerConfig, error) {
	server, ok := cfg.Servers[serverName]
	if !ok {
		return config.ServerConfig{}, fmt.Errorf("server %s not found in configuration", serverName)
	}
	return server, nil
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
