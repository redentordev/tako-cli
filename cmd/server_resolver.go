package cmd

import (
	"fmt"

	"github.com/redentordev/tako-cli/pkg/config"
)

func resolveServer(cfg *config.Config, envName, serverFlag string) (string, config.ServerConfig, error) {
	serverNames, err := statePullServerNames(cfg, envName, serverFlag)
	if err != nil {
		return "", config.ServerConfig{}, err
	}
	if len(serverNames) == 0 {
		return "", config.ServerConfig{}, fmt.Errorf("no servers configured for environment %s", envName)
	}
	serverName := serverNames[0]
	server, ok := cfg.Servers[serverName]
	if !ok {
		return "", config.ServerConfig{}, fmt.Errorf("server %s not found in configuration", serverName)
	}
	return serverName, server, nil
}
