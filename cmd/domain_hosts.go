package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/mesh"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
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
		return formatConfigLoadError(cfgFile, err)
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

	result := engine.DomainsHostsResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindDomainsHostsResult,
		Project:     cfg.Project.Name,
		Environment: envName,
		Service:     domainsHostsService,
		AddressMode: domainsHostsAddress,
		Entries:     []engine.InternalHostEntry{},
	}
	for _, entry := range entries {
		result.Entries = append(result.Entries, engine.InternalHostEntry{
			Service: entry.Service,
			Host:    entry.Host,
			Address: entry.Address,
			Server:  entry.Server,
			Source:  entry.Source,
		})
	}

	// Machine modes reserve stdout for parseable output.
	var out io.Writer = cmd.OutOrStdout()
	if machineOutputEnabled() {
		out = os.Stderr
	}
	if len(entries) == 0 {
		if domainsHostsService != "" {
			return &engine.InvalidRequestError{Err: fmt.Errorf("service %s has no configured internal proxy hosts", domainsHostsService)}
		}
		fmt.Fprintln(out, "No configured internal proxy hosts found.")
		return emitResultDocument(result)
	}

	printInternalHostEntries(out, cfg.Project.Name, envName, entries)
	return emitResultDocument(result)
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

	targets := make([]internalHostAddressTarget, 0, len(proxyServers))
	for _, serverName := range proxyServers {
		server, ok := cfg.Servers[serverName]
		if !ok {
			return nil, fmt.Errorf("proxy server %s is not defined in servers", serverName)
		}
		address := ""
		source := ""
		var err error
		switch addressMode {
		case "private":
			address = server.PrivateHost
			source = "privateHost"
			if address == "" {
				return nil, fmt.Errorf("server %s has no privateHost configured", serverName)
			}
		case "mesh":
			address, err = recordedMeshHostIP(cfg, serverName)
			if err != nil {
				return nil, err
			}
			source = "mesh"
		case "ssh":
			address = server.Host
			source = "host"
		default:
			if server.PrivateHost != "" {
				address = server.PrivateHost
				source = "privateHost"
			} else {
				address, err = recordedMeshHostIP(cfg, serverName)
				if err != nil {
					return nil, err
				}
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

// readDomainsHostsNodeStatus reads one proxy node's GET /v1/status document.
// Tests replace it to avoid SSH.
var readDomainsHostsNodeStatus = func(cfg *config.Config, serverName string) (takod.Status, error) {
	pool := ssh.NewPool()
	defer pool.CloseAll()
	factory, err := nodeclient.NewFactory(cfg, pool, takodSocketFromConfig(cfg))
	if err != nil {
		return takod.Status{}, err
	}
	defer factory.CloseIdleConnections()
	client, _, err := factory.Client(context.Background(), serverName)
	if err != nil {
		return takod.Status{}, err
	}
	output, err := takodclient.RequestJSON(client, takodSocketFromConfig(cfg), "GET", "/v1/status", nil)
	if err != nil {
		return takod.Status{}, err
	}
	var status takod.Status
	if err := json.Unmarshal([]byte(output), &status); err != nil {
		return takod.Status{}, fmt.Errorf("takod returned invalid status: %w", err)
	}
	return status, nil
}

// recordedMeshHostIP returns the mesh address a proxy node actually uses:
// the enrolled platform address, or the address its takod recorded when the
// mesh was last applied. Mesh addresses are not derivable from the config, so
// an unreachable node or a node without a record is an error.
func recordedMeshHostIP(cfg *config.Config, serverName string) (string, error) {
	if platform := strings.TrimSpace(cfg.Servers[serverName].MeshIP); platform != "" {
		return hostIPOnly(platform), nil
	}
	status, err := readDomainsHostsNodeStatus(cfg, serverName)
	if err != nil {
		return "", fmt.Errorf("could not read the recorded mesh address of server %s (use --address private or --address ssh to skip mesh lookup): %w", serverName, err)
	}
	recorded := mesh.RecordedNodeAddress(status.Node)
	if recorded == "" {
		return "", fmt.Errorf("server %s has no recorded mesh address; deploy the environment first or use --address private or --address ssh", serverName)
	}
	return hostIPOnly(recorded), nil
}

func hostIPOnly(address string) string {
	address = strings.TrimSpace(address)
	if ip, _, err := net.ParseCIDR(address); err == nil {
		return ip.String()
	}
	return address
}
func printInternalHostEntries(out io.Writer, project string, envName string, entries []internalHostEntry) {
	fmt.Fprintf(out, "# Tako internal hosts for %s/%s\n", project, envName)
	fmt.Fprintln(out, "# Add these on clients that can reach the listed private or mesh addresses.")
	for _, entry := range entries {
		fmt.Fprintf(out, "%s %s # service=%s server=%s source=%s\n", entry.Address, entry.Host, entry.Service, entry.Server, entry.Source)
	}
}
