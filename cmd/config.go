package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	takoconfig "github.com/redentordev/tako-cli/pkg/config"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Inspect effective Tako configuration",
}

var configExplainCmd = &cobra.Command{
	Use:          "explain",
	Short:        "Show inferred runtime defaults and config sources",
	SilenceUsage: true,
	Long: `Show the effective configuration that Tako will use after environment
expansion, default resolution, and validation. This command does not contact
servers or write files.`,
	RunE: runConfigExplain,
}

func init() {
	rootCmd.AddCommand(configCmd)
	configCmd.AddCommand(configExplainCmd)
}

func runConfigExplain(cmd *cobra.Command, args []string) error {
	configPath := resolveDeployConfigPath(cfgFile)
	cfg, err := loadDeployConfig(cfgFile)
	if err != nil {
		return err
	}
	if err := ensureDeployRuntimeSupported(cfg); err != nil {
		return formatDeployConfigError(configPath, fmt.Errorf("invalid config: %w", err))
	}

	rawCfg, err := loadRawConfigForExplain(configPath)
	if err != nil {
		return formatDeployConfigError(configPath, err)
	}

	envName := getEnvironmentName(cfg)
	servers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return formatDeployConfigError(configPath, fmt.Errorf("invalid config: %w", err))
	}
	services, err := cfg.GetServices(envName)
	if err != nil {
		return formatDeployConfigError(configPath, fmt.Errorf("invalid config: %w", err))
	}
	envRaw := rawCfg.Environments[envName]

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Effective config: %s\n", filepath.Clean(configPath))
	fmt.Fprintf(out, "Environment: %s\n\n", envName)

	fmt.Fprintln(out, "Runtime")
	fmt.Fprintf(out, "  mode: %s (%s)\n", cfg.GetRuntimeMode(), runtimeStringSource(rawCfg, rawCfg.Runtime != nil, rawCfgRuntimeMode(rawCfg)))
	fmt.Fprintf(out, "  proxy: %s (%s)\n", cfg.GetRuntimeProxy(), runtimeStringSource(rawCfg, rawCfg.Runtime != nil, rawCfgRuntimeProxy(rawCfg)))
	fmt.Fprintf(out, "  agent.enabled: %t (%s)\n", cfg.Runtime.Agent.Enabled != nil && *cfg.Runtime.Agent.Enabled, runtimeBoolSource(rawCfg.Runtime != nil && rawCfg.Runtime.Agent != nil && rawCfg.Runtime.Agent.Enabled != nil))
	fmt.Fprintf(out, "  agent.socket: %s (%s)\n", cfg.Runtime.Agent.Socket, runtimeStringSource(rawCfg, rawCfg.Runtime != nil && rawCfg.Runtime.Agent != nil, rawCfgRuntimeAgentSocket(rawCfg)))
	fmt.Fprintf(out, "  agent.dataDir: %s (%s)\n\n", cfg.Runtime.Agent.DataDir, runtimeStringSource(rawCfg, rawCfg.Runtime != nil && rawCfg.Runtime.Agent != nil, rawCfgRuntimeAgentDataDir(rawCfg)))

	fmt.Fprintln(out, "State")
	fmt.Fprintf(out, "  backend: %s (%s)\n", cfg.GetStateBackend(), runtimeStringSource(rawCfg, rawCfg.State != nil, rawCfgStateBackend(rawCfg)))
	fmt.Fprintf(out, "  deployConsistency: %s (%s)\n", cfg.GetDeployConsistency(), runtimeStringSource(rawCfg, rawCfg.State != nil, rawCfgStateDeployConsistency(rawCfg)))
	fmt.Fprintf(out, "  onUnreachableNode: %s (%s)\n", cfg.GetOnUnreachableNode(), runtimeStringSource(rawCfg, rawCfg.State != nil, rawCfgStateOnUnreachableNode(rawCfg)))
	fmt.Fprintf(out, "  remoteCacheEnabled: %t (%s)\n\n", cfg.IsRemoteCacheEnabled(), runtimeBoolSource(rawCfg.State != nil && rawCfg.State.RemoteCacheEnabled != nil))

	fmt.Fprintln(out, "Mesh")
	fmt.Fprintf(out, "  enabled: %t (%s)\n", cfg.IsMeshEnabled(), runtimeBoolSource(rawCfg.Mesh != nil && rawCfg.Mesh.Enabled != nil))
	fmt.Fprintf(out, "  networkCIDR: %s (%s)\n", cfg.Mesh.NetworkCIDR, runtimeStringSource(rawCfg, rawCfg.Mesh != nil, rawCfgMeshNetworkCIDR(rawCfg)))
	fmt.Fprintf(out, "  interface: %s (%s)\n", cfg.Mesh.Interface, runtimeStringSource(rawCfg, rawCfg.Mesh != nil, rawCfgMeshInterface(rawCfg)))
	fmt.Fprintf(out, "  listenPort: %d (%s)\n", cfg.Mesh.ListenPort, runtimeIntSource(rawCfg.Mesh != nil && rawCfg.Mesh.ListenPort != 0))
	fmt.Fprintf(out, "  subnetBits: %d (%s)\n", cfg.Mesh.SubnetBits, runtimeIntSource(rawCfg.Mesh != nil && rawCfg.Mesh.SubnetBits != 0))
	fmt.Fprintf(out, "  natTraversal: %t (%s)\n\n", cfg.Mesh.NATTraversal, runtimeBoolSource(rawCfg.Mesh != nil && rawCfg.Mesh.NATTraversal))

	fmt.Fprintln(out, "Servers")
	for _, serverName := range servers {
		server := cfg.Servers[serverName]
		rawServer := rawCfg.Servers[serverName]
		fmt.Fprintf(out, "  %s:\n", serverName)
		fmt.Fprintf(out, "    host: %s (%s)\n", server.Host, explainStringSource(rawServer.Host))
		fmt.Fprintf(out, "    user: %s (%s)\n", server.User, explainStringSource(rawServer.User))
		fmt.Fprintf(out, "    port: %d (%s)\n", server.Port, runtimeIntSource(rawServer.Port != 0))
		if server.SSHKey != "" {
			fmt.Fprintf(out, "    auth: sshKey (%s)\n", explainStringSource(rawServer.SSHKey))
		} else if server.Password != "" {
			fmt.Fprintf(out, "    auth: password (%s, redacted)\n", explainStringSource(rawServer.Password))
		}
	}
	fmt.Fprintln(out)

	fmt.Fprintln(out, "Services")
	serviceNames := make([]string, 0, len(services))
	for name := range services {
		serviceNames = append(serviceNames, name)
	}
	sort.Strings(serviceNames)
	for _, serviceName := range serviceNames {
		service := services[serviceName]
		rawService := envRaw.Services[serviceName]
		fmt.Fprintf(out, "  %s:\n", serviceName)
		fmt.Fprintf(out, "    type: %s\n", service.GetServiceType())
		fmt.Fprintf(out, "    source: %s\n", explainServiceSource(service, rawService))
		fmt.Fprintf(out, "    replicas: %d (%s)\n", effectiveReplicas(service), runtimeIntSource(rawService.Replicas != 0))
		fmt.Fprintf(out, "    deploy.strategy: %s (%s)\n", service.Deploy.Strategy, runtimeStringSource(rawCfg, rawService.Deploy.Strategy != "", rawService.Deploy.Strategy))
		if service.Proxy != nil {
			if service.Proxy.Domain != "" {
				fmt.Fprintf(out, "    proxy.domain: %s (%s)\n", service.Proxy.Domain, explainStringSource(rawServiceProxyDomain(rawService)))
			}
			if service.Proxy.DynamicDomains != nil && service.Proxy.DynamicDomains.IsEnabled() {
				fmt.Fprintf(out, "    proxy.dynamicDomains.ask: %s (%s)\n", service.Proxy.DynamicDomains.Ask, explainStringSource(rawServiceProxyDynamicAsk(rawService)))
			}
		}
	}

	return nil
}

func loadRawConfigForExplain(configPath string) (*takoconfig.Config, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg takoconfig.Config
	if strings.EqualFold(filepath.Ext(configPath), ".json") {
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("failed to parse JSON config for explanation: %w", err)
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			if err == nil {
				return nil, fmt.Errorf("failed to parse JSON config for explanation: multiple JSON values")
			}
			return nil, fmt.Errorf("failed to parse JSON config for explanation: %w", err)
		}
		return &cfg, nil
	}

	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("failed to parse YAML config for explanation: %w", err)
	}
	return &cfg, nil
}

func runtimeStringSource(_ *takoconfig.Config, present bool, value string) string {
	if !present || strings.TrimSpace(value) == "" {
		return "default"
	}
	return explainStringSource(value)
}

func runtimeBoolSource(present bool) string {
	if !present {
		return "default"
	}
	return "config"
}

func runtimeIntSource(present bool) string {
	if !present {
		return "default"
	}
	return "config"
}

func explainStringSource(value string) string {
	if strings.Contains(value, "${") {
		return "env"
	}
	if strings.TrimSpace(value) == "" {
		return "default"
	}
	return "config"
}

func effectiveReplicas(service takoconfig.ServiceConfig) int {
	if service.Replicas <= 0 {
		return 1
	}
	return service.Replicas
}

func explainServiceSource(service, raw takoconfig.ServiceConfig) string {
	switch {
	case service.Build != "":
		return fmt.Sprintf("build %q (%s)", service.Build, explainStringSource(raw.Build))
	case service.Image != "":
		return fmt.Sprintf("image %q (%s)", service.Image, explainStringSource(raw.Image))
	default:
		return "worker"
	}
}

func rawServiceProxyDomain(service takoconfig.ServiceConfig) string {
	if service.Proxy == nil {
		return ""
	}
	return service.Proxy.Domain
}

func rawServiceProxyDynamicAsk(service takoconfig.ServiceConfig) string {
	if service.Proxy == nil || service.Proxy.DynamicDomains == nil {
		return ""
	}
	return service.Proxy.DynamicDomains.Ask
}

func rawCfgRuntimeMode(cfg *takoconfig.Config) string {
	if cfg.Runtime == nil {
		return ""
	}
	return cfg.Runtime.Mode
}

func rawCfgRuntimeProxy(cfg *takoconfig.Config) string {
	if cfg.Runtime == nil {
		return ""
	}
	return cfg.Runtime.Proxy
}

func rawCfgRuntimeAgentSocket(cfg *takoconfig.Config) string {
	if cfg.Runtime == nil || cfg.Runtime.Agent == nil {
		return ""
	}
	return cfg.Runtime.Agent.Socket
}

func rawCfgRuntimeAgentDataDir(cfg *takoconfig.Config) string {
	if cfg.Runtime == nil || cfg.Runtime.Agent == nil {
		return ""
	}
	return cfg.Runtime.Agent.DataDir
}

func rawCfgStateBackend(cfg *takoconfig.Config) string {
	if cfg.State == nil {
		return ""
	}
	return cfg.State.Backend
}

func rawCfgStateDeployConsistency(cfg *takoconfig.Config) string {
	if cfg.State == nil {
		return ""
	}
	return cfg.State.DeployConsistency
}

func rawCfgStateOnUnreachableNode(cfg *takoconfig.Config) string {
	if cfg.State == nil {
		return ""
	}
	return cfg.State.OnUnreachableNode
}

func rawCfgMeshNetworkCIDR(cfg *takoconfig.Config) string {
	if cfg.Mesh == nil {
		return ""
	}
	return cfg.Mesh.NetworkCIDR
}

func rawCfgMeshInterface(cfg *takoconfig.Config) string {
	if cfg.Mesh == nil {
		return ""
	}
	return cfg.Mesh.Interface
}
