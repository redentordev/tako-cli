package config

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

func materializeDefaultPlatformInventory(cfg *Config) error {
	return materializePlatformInventoryFromFiles(cfg, nodeidentity.DefaultInventoryPath, nodeidentity.DefaultLocalBindingPath, os.Getenv("TAKO_PLATFORM_SSH_KEY"), os.Getenv("TAKO_SSH_PASSWORD"))
}

func materializePlatformInventoryFromFiles(cfg *Config, inventoryPath, bindingPath, sshKey, password string) error {
	if _, err := os.Lstat(inventoryPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect platform inventory: %w", err)
	}
	inventory, err := nodeidentity.ReadInventoryOptional(inventoryPath)
	if err != nil {
		return err
	}
	if inventory == nil || inventory.ControllerNodeID == "" || inventory.Generation == 0 {
		return nil
	}
	binding, err := nodeidentity.ReadLocalBinding(bindingPath)
	if err != nil {
		return fmt.Errorf("read public local node binding: %w", err)
	}
	if binding.ClusterID != inventory.ClusterID {
		return fmt.Errorf("authoritative inventory does not match the public local node binding")
	}
	return MaterializePlatformInventory(cfg, inventory, binding.NodeID, binding.WorkerUID, sshKey, password)
}

// MaterializePlatformInventory replaces application-supplied infrastructure
// details with the controller-published node set. Applications retain only
// workload selection by immutable node name or node ID.
func MaterializePlatformInventory(cfg *Config, inventory *nodeidentity.ClusterInventory, localNodeID string, localWorkerUID int, sshKey, password string) error {
	if cfg == nil || inventory == nil {
		return fmt.Errorf("config and platform inventory are required")
	}
	if err := inventory.Validate(); err != nil {
		return err
	}
	byName := make(map[string]nodeidentity.InventoryNode)
	byID := make(map[string]nodeidentity.InventoryNode)
	for _, node := range inventory.Nodes {
		if !containsInventoryRole(node.Roles, nodeidentity.RoleWorker) {
			continue
		}
		byName[node.NodeName] = node
		byID[node.NodeID] = node
	}
	if len(byName) == 0 {
		return fmt.Errorf("platform inventory has no worker nodes")
	}

	aliases := make(map[string]string)
	for name, server := range cfg.Servers {
		if node, ok := byName[name]; ok {
			aliases[name] = node.NodeName
			continue
		}
		if server.NodeID != "" {
			if node, ok := byID[strings.ToLower(strings.TrimSpace(server.NodeID))]; ok {
				aliases[name] = node.NodeName
			}
		}
	}
	resolveNodes := func(values []string, field string) ([]string, error) {
		resolved := make([]string, 0, len(values))
		seen := map[string]struct{}{}
		for _, requested := range values {
			requested = strings.TrimSpace(requested)
			canonical := requested
			if node, ok := byID[strings.ToLower(requested)]; ok {
				canonical = node.NodeName
			} else if alias, ok := aliases[requested]; ok {
				canonical = alias
			}
			if _, ok := byName[canonical]; !ok {
				return nil, fmt.Errorf("%s references node %s absent from active platform membership", field, requested)
			}
			if _, duplicate := seen[canonical]; duplicate {
				continue
			}
			resolved = append(resolved, canonical)
			seen[canonical] = struct{}{}
		}
		return resolved, nil
	}

	materialized := make(map[string]ServerConfig, len(byName))
	for name, node := range byName {
		host := node.SSHHost
		user := node.SSHUser
		port := node.SSHPort
		transport := "ssh"
		workerUID := 0
		if node.NodeID == localNodeID && localWorkerUID > 0 {
			transport = "local"
			workerUID = localWorkerUID
			if host == "" {
				host = node.MeshEndpoint
			}
			if user == "" {
				user = "root"
			}
			if port == 0 {
				port = 22
			}
		}
		labels := map[string]string{"tako.lifecycle": node.Lifecycle}
		for _, role := range node.Roles {
			labels["tako.role."+role] = "true"
		}
		materialized[name] = ServerConfig{
			Host: host, User: user, Port: port, SSHKey: strings.TrimSpace(sshKey), Password: password,
			Labels: labels, Transport: transport, ClusterID: inventory.ClusterID, NodeID: node.NodeID, WorkerUID: workerUID,
			MeshIP: node.MeshIP, Lifecycle: node.Lifecycle, Roles: append([]string(nil), node.Roles...),
			SSHHostKeyType: node.SSHHostKeyType, SSHHostKey: node.SSHHostKey, SSHHostKeyFingerprint: node.SSHHostKeyFingerprint,
		}
	}
	cfg.Servers = materialized
	if cfg.Mesh == nil {
		cfg.Mesh = &MeshConfig{}
	}
	cfg.Mesh.NetworkCIDR = inventory.MeshCIDR

	for envName, env := range cfg.Environments {
		if len(env.Servers) == 0 && env.ServerSelector == nil {
			for name := range byName {
				env.Servers = append(env.Servers, name)
			}
			sort.Strings(env.Servers)
		} else if len(env.Servers) > 0 {
			resolved, err := resolveNodes(env.Servers, "environment "+envName)
			if err != nil {
				return err
			}
			env.Servers = resolved
		}
		if env.Proxy != nil && env.Proxy.Placement != nil && len(env.Proxy.Placement.Servers) > 0 {
			resolved, err := resolveNodes(env.Proxy.Placement.Servers, "environment "+envName+" proxy placement")
			if err != nil {
				return err
			}
			env.Proxy.Placement.Servers = resolved
		}
		for serviceName, service := range env.Services {
			if service.Placement != nil && len(service.Placement.Servers) > 0 {
				resolved, err := resolveNodes(service.Placement.Servers, "service "+envName+"/"+serviceName+" placement")
				if err != nil {
					return err
				}
				service.Placement.Servers = resolved
				env.Services[serviceName] = service
			}
		}
		cfg.Environments[envName] = env
	}
	return nil
}

func containsInventoryRole(roles []string, wanted string) bool {
	for _, role := range roles {
		if role == wanted {
			return true
		}
	}
	return false
}
