package cmd

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/serviceimport"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
)

var (
	discoveryServer     string
	discoveryPort       int
	discoveryRoundRobin bool
	discoveryImport     string
	discoveryFormat     string
)

type discoveryPortSpec = serviceimport.PortSpec
type discoveryRow = serviceimport.Row

var discoveryCmd = &cobra.Command{
	Use:   "discovery [SERVICE]",
	Short: "Show healthy private service endpoints",
	Long: `Show healthy service endpoints reported by takod on each environment node.

Discovery is scoped to the current project and environment. The command queries
each node directly and only prints running, healthy containers reachable on the
project Docker network.

Use --import <alias> to resolve a top-level cross-project import from the
current config. The command reads the exporting project's desired state from the
configured import server(s), validates the named exported port, then asks takod
for live healthy endpoints on that exported service.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runDiscovery,
}

func init() {
	rootCmd.AddCommand(discoveryCmd)
	discoveryCmd.Flags().StringVarP(&discoveryServer, "server", "s", "", "Node to query")
	discoveryCmd.Flags().IntVar(&discoveryPort, "port", 0, "Target port to inspect instead of configured service ports")
	discoveryCmd.Flags().BoolVar(&discoveryRoundRobin, "round-robin", false, "Rotate endpoint order per service")
	discoveryCmd.Flags().StringVar(&discoveryImport, "import", "", "Resolve a top-level cross-project import alias")
	discoveryCmd.Flags().StringVar(&discoveryFormat, "format", "table", "Output format: table or upstreams")
}

func runDiscovery(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}
	if discoveryPort < 0 || discoveryPort > 65535 {
		return fmt.Errorf("port must be between 0 and 65535")
	}
	discoveryImport = strings.TrimSpace(discoveryImport)
	discoveryFormat = strings.TrimSpace(discoveryFormat)
	if discoveryFormat == "" {
		discoveryFormat = "table"
	}

	envName := getEnvironmentName(cfg)
	if discoveryImport != "" {
		if len(args) > 0 {
			return fmt.Errorf("service argument cannot be combined with --import")
		}
		rows, warnings, err := gatherImportedDiscoveryRows(cfg, envName, discoveryImport, discoveryServer, discoveryRoundRobin)
		if err != nil {
			return err
		}
		for _, warning := range warnings {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warning)
		}
		if len(rows) == 0 {
			fmt.Println("\nNo healthy imported endpoints found")
			return nil
		}
		if err := displayDiscoveryRows(rows, discoveryFormat); err != nil {
			return err
		}
		return nil
	}

	services, err := cfg.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services: %w", err)
	}

	serviceNames := make([]string, 0, len(services))
	if len(args) > 0 {
		serviceName := args[0]
		if _, ok := services[serviceName]; !ok {
			return fmt.Errorf("service %s not found in environment %s", serviceName, envName)
		}
		serviceNames = append(serviceNames, serviceName)
	} else {
		for serviceName := range services {
			serviceNames = append(serviceNames, serviceName)
		}
		sort.Strings(serviceNames)
	}

	serverNames, err := statePullServerNames(cfg, envName, discoveryServer)
	if err != nil {
		return err
	}

	pool := ssh.NewPool()
	defer pool.CloseAll()

	rows, warnings, err := gatherDiscoveryRows(pool, cfg, envName, services, serviceNames, serverNames, discoveryRoundRobin)
	if err != nil {
		return err
	}
	for _, warning := range warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warning)
	}
	if len(rows) == 0 {
		fmt.Println("\nNo healthy endpoints found")
		return nil
	}

	if err := displayDiscoveryRows(rows, discoveryFormat); err != nil {
		return err
	}
	return nil
}

func gatherDiscoveryRows(
	pool *ssh.Pool,
	cfg *config.Config,
	envName string,
	services map[string]config.ServiceConfig,
	serviceNames []string,
	serverNames []string,
	roundRobin bool,
) ([]discoveryRow, []string, error) {
	if pool == nil {
		return nil, nil, fmt.Errorf("ssh pool not initialized")
	}
	rows, warnings, err := gatherDiscoveryRowsWith(cfg.Servers, serviceNames, serverNames, func(serverName string, server config.ServerConfig, serviceName string, port discoveryPortSpec) ([]discoveryRow, error) {
		client, err := pool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return nil, fmt.Errorf("connect failed: %w", err)
		}

		endpoint := takodclient.DiscoveryEndpoint(cfg.Project.Name, envName, serviceName, port.Target, roundRobin)
		output, err := takodclient.RequestJSON(client, takodSocketFromConfig(cfg), "GET", endpoint, nil)
		if err != nil {
			return nil, err
		}

		var response takod.DiscoveryResponse
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			return nil, fmt.Errorf("failed to parse discovery response: %w", err)
		}
		return discoveryRowsFromResponse(serverName, response, cfg.Project.Name, envName, serviceName, port)
	}, func(serviceName string) []discoveryPortSpec {
		return discoveryPortsForService(services[serviceName], discoveryPort)
	})
	if err != nil {
		return nil, warnings, err
	}
	return rows, warnings, nil
}

func gatherImportedDiscoveryRows(
	cfg *config.Config,
	envName string,
	alias string,
	requestedServer string,
	roundRobin bool,
) ([]discoveryRow, []string, error) {
	importConfig, ok := cfg.Imports[alias]
	if !ok {
		return nil, nil, fmt.Errorf("import %s not found in config", alias)
	}
	serverNames, err := importDiscoveryServerNames(cfg, envName, importConfig, requestedServer)
	if err != nil {
		return nil, nil, err
	}
	pool := ssh.NewPool()
	defer pool.CloseAll()

	return gatherImportedDiscoveryRowsWith(cfg.Servers, serverNames, func(serverName string, server config.ServerConfig) ([]discoveryRow, error) {
		client, err := pool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return nil, fmt.Errorf("connect failed: %w", err)
		}
		resolved, err := resolveImportExport(client, takodSocketFromConfig(cfg), alias, importConfig)
		if err != nil {
			return nil, err
		}
		target := resolved.Target
		if discoveryPort > 0 {
			target = discoveryPort
		}
		endpoint := takodclient.DiscoveryEndpoint(importConfig.Project, importConfig.Environment, importConfig.Service, target, roundRobin)
		output, err := takodclient.RequestJSON(client, takodSocketFromConfig(cfg), "GET", endpoint, nil)
		if err != nil {
			return nil, err
		}
		var response takod.DiscoveryResponse
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			return nil, fmt.Errorf("failed to parse discovery response: %w", err)
		}
		port := discoveryPortSpec{Name: importConfig.Port, Target: target, Protocol: resolved.Protocol}
		if discoveryPort > 0 {
			port = discoveryPortSpec{Name: "custom", Target: discoveryPort, Protocol: "tcp"}
		}
		return discoveryRowsFromResponse(serverName, response, importConfig.Project, importConfig.Environment, importConfig.Service, port)
	})
}

type importedDiscoveryReadFunc func(serverName string, server config.ServerConfig) ([]discoveryRow, error)

func gatherImportedDiscoveryRowsWith(
	servers map[string]config.ServerConfig,
	serverNames []string,
	read importedDiscoveryReadFunc,
) ([]discoveryRow, []string, error) {
	var rows []discoveryRow
	var warnings []string
	successes := 0
	var failures []string
	for _, serverName := range serverNames {
		server, ok := servers[serverName]
		if !ok {
			failures = append(failures, fmt.Sprintf("%s: server not found in configuration", serverName))
			continue
		}
		endpoints, err := read(serverName, server)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", serverName, err))
			continue
		}
		successes++
		rows = append(rows, endpoints...)
	}
	sort.Strings(failures)
	if successes == 0 && len(failures) > 0 {
		return nil, nil, fmt.Errorf("failed to discover imported endpoints: %s", strings.Join(failures, "; "))
	}
	warnings = append(warnings, failures...)
	return rows, warnings, nil
}

func importDiscoveryServerNames(cfg *config.Config, envName string, importConfig config.ImportConfig, requestedServer string) ([]string, error) {
	return serviceimport.ServerNames(cfg, envName, importConfig, requestedServer)
}

type resolvedImportExport = serviceimport.ResolvedExport

func resolveImportExport(client takodclient.RequestExecutor, socket string, alias string, importConfig config.ImportConfig) (resolvedImportExport, error) {
	return serviceimport.ResolveExport(client, socket, alias, importConfig)
}

type discoveryReadFunc func(serverName string, server config.ServerConfig, serviceName string, port discoveryPortSpec) ([]discoveryRow, error)
type discoveryPortsFunc func(serviceName string) []discoveryPortSpec

func gatherDiscoveryRowsWith(
	servers map[string]config.ServerConfig,
	serviceNames []string,
	serverNames []string,
	read discoveryReadFunc,
	portsForService discoveryPortsFunc,
) ([]discoveryRow, []string, error) {
	var rows []discoveryRow
	var warnings []string

	for _, serviceName := range serviceNames {
		ports := portsForService(serviceName)
		if len(ports) == 0 {
			ports = []discoveryPortSpec{{}}
		}
		for _, port := range ports {
			successes := 0
			var failures []string
			for _, serverName := range serverNames {
				server, ok := servers[serverName]
				if !ok {
					failures = append(failures, fmt.Sprintf("%s: server not found in configuration", serverName))
					continue
				}
				endpoints, err := read(serverName, server, serviceName, port)
				if err != nil {
					failures = append(failures, fmt.Sprintf("%s: %v", serverName, err))
					continue
				}
				successes++
				rows = append(rows, endpoints...)
			}
			sort.Strings(failures)
			if successes == 0 && len(failures) > 0 {
				return nil, nil, fmt.Errorf("failed to discover %s endpoints: %s", serviceName, strings.Join(failures, "; "))
			}
			warnings = append(warnings, failures...)
		}
	}

	return rows, warnings, nil
}

func discoveryRowsFromResponse(
	serverName string,
	response takod.DiscoveryResponse,
	project string,
	environment string,
	serviceName string,
	port discoveryPortSpec,
) ([]discoveryRow, error) {
	return serviceimport.RowsFromResponse(serverName, response, project, environment, serviceName, port)
}

func validateDiscoveryEndpoint(endpoint takod.DiscoveryEndpoint, serviceName string, port int) (takod.DiscoveryEndpoint, error) {
	return serviceimport.ValidateEndpoint(endpoint, serviceName, port)
}

func discoveryPortsForService(service config.ServiceConfig, override int) []discoveryPortSpec {
	if override > 0 {
		return []discoveryPortSpec{{Name: "custom", Target: override, Protocol: "tcp"}}
	}
	ports := service.EffectivePorts()
	if len(ports) == 0 {
		return []discoveryPortSpec{{}}
	}
	out := make([]discoveryPortSpec, 0, len(ports))
	for _, port := range ports {
		out = append(out, discoveryPortSpec{
			Name:     port.Name,
			Target:   port.Target,
			Protocol: port.Protocol,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].Target < out[j].Target
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func endpointAddress(endpoint takod.DiscoveryEndpoint) string {
	return serviceimport.EndpointAddress(endpoint)
}

func discoveryPortLabel(port discoveryPortSpec) string {
	if port.Target <= 0 {
		return "-"
	}
	name := port.Name
	if name == "" {
		name = "port"
	}
	label := fmt.Sprintf("%s:%d", name, port.Target)
	if port.Protocol != "" && port.Protocol != "http" && port.Protocol != "tcp" {
		label += "/" + port.Protocol
	}
	return label
}

func displayDiscoveryRows(rows []discoveryRow, format string) error {
	output, err := renderDiscoveryRows(rows, format)
	if err != nil {
		return err
	}
	fmt.Print(output)
	return nil
}

func renderDiscoveryRows(rows []discoveryRow, format string) (string, error) {
	switch strings.TrimSpace(format) {
	case "", "table":
		return renderDiscoveryRowsTable(rows), nil
	case "upstreams":
		return renderDiscoveryRowsUpstreams(rows), nil
	default:
		return "", fmt.Errorf("unsupported discovery format %q; use table or upstreams", format)
	}
}

func renderDiscoveryRowsTable(rows []discoveryRow) string {
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("%-14s %-15s %-16s %-34s %-22s\n", "NODE", "SERVICE", "PORT", "CONTAINER", "ADDRESS"))
	b.WriteString(strings.Repeat("-", 105))
	b.WriteString("\n")
	for _, row := range rows {
		b.WriteString(fmt.Sprintf("%-14s %-15s %-16s %-34s %-22s\n",
			row.Node,
			row.Service,
			discoveryPortLabel(row.Port),
			row.Container,
			row.Address,
		))
	}
	b.WriteString("\n")
	return b.String()
}

func renderDiscoveryRowsUpstreams(rows []discoveryRow) string {
	seen := make(map[string]bool, len(rows))
	upstreams := make([]string, 0, len(rows))
	for _, row := range rows {
		upstream := discoveryRowUpstream(row)
		if upstream == "" || seen[upstream] {
			continue
		}
		seen[upstream] = true
		upstreams = append(upstreams, upstream)
	}
	return strings.Join(upstreams, " ") + "\n"
}

func discoveryRowUpstream(row discoveryRow) string {
	return serviceimport.RowUpstream(row)
}
