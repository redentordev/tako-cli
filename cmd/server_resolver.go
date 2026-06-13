package cmd

import (
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
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

type serverConnectFunc func(serverName string, server config.ServerConfig) (*ssh.Client, error)

func connectResolvedServer(cfg *config.Config, envName, serverFlag string) (string, config.ServerConfig, *ssh.Client, error) {
	return connectResolvedServerWith(cfg, envName, serverFlag, func(serverName string, server config.ServerConfig) (*ssh.Client, error) {
		client, err := ssh.NewClientFromConfig(ssh.ServerConfig{
			Host:     server.Host,
			Port:     server.Port,
			User:     server.User,
			SSHKey:   server.SSHKey,
			Password: server.Password,
		})
		if err != nil {
			return nil, err
		}
		if err := client.Connect(); err != nil {
			_ = client.Close()
			return nil, err
		}
		return client, nil
	})
}

func connectResolvedServerWith(cfg *config.Config, envName, serverFlag string, connect serverConnectFunc) (string, config.ServerConfig, *ssh.Client, error) {
	serverNames, err := statePullServerNames(cfg, envName, serverFlag)
	if err != nil {
		return "", config.ServerConfig{}, nil, err
	}
	if len(serverNames) == 0 {
		return "", config.ServerConfig{}, nil, fmt.Errorf("no servers configured for environment %s", envName)
	}

	var errors []string
	for _, serverName := range serverNames {
		server, ok := cfg.Servers[serverName]
		if !ok {
			return "", config.ServerConfig{}, nil, fmt.Errorf("server %s not found in configuration", serverName)
		}
		client, err := connect(serverName, server)
		if err == nil {
			return serverName, server, client, nil
		}
		errors = append(errors, fmt.Sprintf("%s: %v", serverName, err))
		if serverFlag != "" {
			return "", config.ServerConfig{}, nil, fmt.Errorf("failed to connect to node %s: %w", serverName, err)
		}
	}

	return "", config.ServerConfig{}, nil, fmt.Errorf("no reachable nodes for environment %s: %s", envName, strings.Join(errors, "; "))
}
