package cmd

import (
	"fmt"

	"github.com/redentordev/tako-cli/pkg/config"
)

func resolveServer(cfg *config.Config, envName, serverFlag string) (string, config.ServerConfig, error) {
	if serverFlag != "" {
		server, ok := cfg.Servers[serverFlag]
		if !ok {
			return "", config.ServerConfig{}, fmt.Errorf("server '%s' not found in config", serverFlag)
		}
		return serverFlag, server, nil
	}

	envServers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return "", config.ServerConfig{}, fmt.Errorf("failed to get environment servers: %w", err)
	}
	if len(envServers) == 0 {
		return "", config.ServerConfig{}, fmt.Errorf("no servers configured for environment %s", envName)
	}
	if len(envServers) > 1 {
		primaryName, err := cfg.GetPrimaryServer(envName)
		if err != nil {
			return "", config.ServerConfig{}, fmt.Errorf("failed to get primary node: %w", err)
		}
		return primaryName, cfg.Servers[primaryName], nil
	}

	name := envServers[0]
	return name, cfg.Servers[name], nil
}
