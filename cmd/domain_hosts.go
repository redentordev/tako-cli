package cmd

import (
	"encoding/binary"
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/spf13/cobra"
)

type internalHostEntry struct {
	Service string
	Host    string
	Address string
	Server  string
	Source  string
}

func runDomainsHosts(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	envName := getEnvironmentName(cfg)
	env, err := cfg.GetEnvironment(envName)
	if err != nil {
		return err
	}

	entries, err := collectInternalHostEntries(cfg, envName, env.Services, domainsHostsService, domainsHostsAddress)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		if domainsHostsService != "" {
			return fmt.Errorf("service %s has no configured internal proxy hosts", domainsHostsService)
		}
		fmt.Fprintln(cmd.OutOrStdout(), "No configured internal proxy hosts found.")
		return nil
	}

	printInternalHostEntries(cmd, cfg.Project.Name, envName, entries)
	return nil
}

func collectInternalHostEntries(cfg *config.Config, envName string, services map[string]config.ServiceConfig, serviceFilter string, addressMode string) ([]internalHostEntry, error) {
	proxyServers, err := cfg.GetEnvironmentProxyServers(envName)
	if err != nil {
		return nil, err
	}
	targets, err := internalHostAddressTargets(cfg, envName, proxyServers, addressMode)
	if err != nil {
		return nil, err
	}

	serviceNames := sortedServiceNames(services)
	var entries []internalHostEntry
	for _, serviceName := range serviceNames {
		if serviceFilter != "" && serviceName != serviceFilter {
			continue
		}
		service := services[serviceName]
		if service.Proxy == nil || !service.Proxy.IsInternal() {
			continue
		}
		for _, host := range service.Proxy.GetAllHosts() {
			for _, target := range targets {
				entries = append(entries, internalHostEntry{
					Service: serviceName,
					Host:    host,
					Address: target.Address,
					Server:  target.Server,
					Source:  target.Source,
				})
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Host == entries[j].Host {
			return entries[i].Address < entries[j].Address
		}
		return entries[i].Host < entries[j].Host
	})
	return entries, nil
}

type internalHostAddressTarget struct {
	Server  string
	Address string
	Source  string
}

func internalHostAddressTargets(cfg *config.Config, envName string, proxyServers []string, addressMode string) ([]internalHostAddressTarget, error) {
	addressMode = strings.ToLower(strings.TrimSpace(addressMode))
	if addressMode == "" {
		addressMode = "auto"
	}
	switch addressMode {
	case "auto", "private", "mesh", "ssh":
	default:
		return nil, fmt.Errorf("--address must be auto, private, mesh, or ssh")
	}

	envServers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return nil, err
	}
	meshIPs, err := meshHostIPs(cfg, envServers)
	if err != nil {
		return nil, err
	}

	targets := make([]internalHostAddressTarget, 0, len(proxyServers))
	for _, serverName := range proxyServers {
		server, ok := cfg.Servers[serverName]
		if !ok {
			return nil, fmt.Errorf("proxy server %s is not defined in servers", serverName)
		}
		address := ""
		source := ""
		switch addressMode {
		case "private":
			address = server.PrivateHost
			source = "privateHost"
			if address == "" {
				return nil, fmt.Errorf("server %s has no privateHost configured", serverName)
			}
		case "mesh":
			address = meshIPs[serverName]
			source = "mesh"
		case "ssh":
			address = server.Host
			source = "host"
		default:
			if server.PrivateHost != "" {
				address = server.PrivateHost
				source = "privateHost"
			} else {
				address = meshIPs[serverName]
				source = "mesh"
			}
		}
		if address == "" {
			return nil, fmt.Errorf("could not resolve internal address for server %s", serverName)
		}
		targets = append(targets, internalHostAddressTarget{Server: serverName, Address: address, Source: source})
	}
	return targets, nil
}

func meshHostIPs(cfg *config.Config, envServers []string) (map[string]string, error) {
	_, ipNet, err := net.ParseCIDR(cfg.Mesh.NetworkCIDR)
	if err != nil {
		return nil, fmt.Errorf("invalid mesh CIDR: %w", err)
	}
	baseIP := ipNet.IP.To4()
	if baseIP == nil {
		return nil, fmt.Errorf("mesh.networkCIDR must be IPv4")
	}
	base := binary.BigEndian.Uint32(baseIP)
	ips := make(map[string]string, len(envServers))
	for index, serverName := range envServers {
		ip := make(net.IP, 4)
		binary.BigEndian.PutUint32(ip, base+uint32(index+1))
		ips[serverName] = ip.String()
	}
	return ips, nil
}

func printInternalHostEntries(cmd *cobra.Command, project string, envName string, entries []internalHostEntry) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "# Tako internal hosts for %s/%s\n", project, envName)
	fmt.Fprintln(out, "# Add these on clients that can reach the listed private or mesh addresses.")
	for _, entry := range entries {
		fmt.Fprintf(out, "%s %s # service=%s server=%s source=%s\n", entry.Address, entry.Host, entry.Service, entry.Server, entry.Source)
	}
}
