package deployer

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"gopkg.in/yaml.v3"
)

const (
	meshUpstreamPortBase      = 20000
	meshUpstreamPortMax       = 65000
	meshUpstreamPortSlotLimit = 64
)

type meshUpstreamPortKey struct {
	ServerName    string
	ServiceName   string
	Slot          int
	ContainerPort int
}

type traefikDynamicConfig struct {
	HTTP traefikHTTPConfig `yaml:"http,omitempty"`
}

type traefikHTTPConfig struct {
	Routers     map[string]traefikRouter     `yaml:"routers,omitempty"`
	Services    map[string]traefikService    `yaml:"services,omitempty"`
	Middlewares map[string]traefikMiddleware `yaml:"middlewares,omitempty"`
}

type traefikRouter struct {
	Rule        string      `yaml:"rule"`
	EntryPoints []string    `yaml:"entryPoints"`
	Service     string      `yaml:"service,omitempty"`
	Middlewares []string    `yaml:"middlewares,omitempty"`
	TLS         *traefikTLS `yaml:"tls,omitempty"`
}

type traefikTLS struct {
	CertResolver string `yaml:"certResolver"`
}

type traefikService struct {
	LoadBalancer traefikLoadBalancer `yaml:"loadBalancer"`
}

type traefikLoadBalancer struct {
	Servers        []traefikServer     `yaml:"servers"`
	HealthCheck    *traefikHealthCheck `yaml:"healthCheck,omitempty"`
	PassHostHeader bool                `yaml:"passHostHeader"`
}

type traefikServer struct {
	URL string `yaml:"url"`
}

type traefikHealthCheck struct {
	Path     string `yaml:"path,omitempty"`
	Interval string `yaml:"interval,omitempty"`
}

type traefikMiddleware struct {
	RedirectScheme *traefikRedirectScheme `yaml:"redirectScheme,omitempty"`
}

type traefikRedirectScheme struct {
	Scheme string `yaml:"scheme"`
}

func (d *Deployer) ReconcileTakodProxy(services map[string]config.ServiceConfig) error {
	targetServers, err := d.getTakodTargetServers()
	if err != nil {
		return fmt.Errorf("failed to get takod proxy targets: %w", err)
	}
	if len(targetServers) == 0 {
		return nil
	}

	return runTakodNodeActions(targetServers, func(serverName string) error {
		client, err := d.getEnvironmentClient(serverName)
		if err != nil {
			return err
		}

		dynamicConfig, hasPublicServices, err := d.renderTakodProxyDynamicConfigForNode(services, serverName)
		if err != nil {
			return err
		}
		if !hasPublicServices {
			if err := d.removeTakodProxyConfig(client); err != nil {
				return fmt.Errorf("failed to remove proxy config: %w", err)
			}
			return nil
		}

		if err := d.writeTakodProxyConfig(client, dynamicConfig); err != nil {
			return fmt.Errorf("failed to write proxy config: %w", err)
		}
		if err := d.ensureTakodProxy(client, takodNetworkName(d.config.Project.Name, d.environment), firstProxyEmail(services)); err != nil {
			return fmt.Errorf("failed to reconcile proxy: %w", err)
		}
		return nil
	})
}

func (d *Deployer) renderTakodProxyDynamicConfigForNode(services map[string]config.ServiceConfig, proxyServerName string) ([]byte, bool, error) {
	if strings.TrimSpace(proxyServerName) == "" {
		return nil, false, fmt.Errorf("proxy server name is required")
	}

	httpConfig := traefikHTTPConfig{
		Routers:     make(map[string]traefikRouter),
		Services:    make(map[string]traefikService),
		Middlewares: make(map[string]traefikMiddleware),
	}

	serviceNames := make([]string, 0, len(services))
	for serviceName := range services {
		serviceNames = append(serviceNames, serviceName)
	}
	sort.Strings(serviceNames)

	for _, serviceName := range serviceNames {
		service := services[serviceName]
		proxyPorts := proxyPortsForService(service)
		if len(proxyPorts) == 0 {
			continue
		}

		assignments, err := d.planTakodAssignments(serviceName, &service)
		if err != nil {
			return nil, false, fmt.Errorf("failed to plan proxy upstreams for %s: %w", serviceName, err)
		}
		if len(assignments) == 0 {
			continue
		}
		sort.Slice(assignments, func(i, j int) bool {
			if assignments[i].ServerName == assignments[j].ServerName {
				return assignments[i].Slot < assignments[j].Slot
			}
			return assignments[i].ServerName < assignments[j].ServerName
		})

		for _, port := range proxyPorts {
			routerName := proxyRouterName(d.config.Project.Name, d.environment, serviceName, port)
			rule, err := proxyHostRule(port.Proxy)
			if err != nil {
				return nil, false, fmt.Errorf("service %s port %s has invalid proxy domains: %w", serviceName, port.Name, err)
			}
			if rule == "" {
				return nil, false, fmt.Errorf("service %s port %s has proxy config but no domains", serviceName, port.Name)
			}

			var upstreams []traefikServer
			for _, assignment := range assignments {
				url, err := d.takodProxyUpstreamURL(proxyServerName, assignment.ServerName, serviceName, assignment.Slot, port)
				if err != nil {
					return nil, false, err
				}
				upstreams = append(upstreams, traefikServer{URL: url})
			}

			redirectName := routerName + "-redirect"
			httpRouterName := routerName + "-http"
			httpConfig.Routers[routerName] = traefikRouter{
				Rule:        rule,
				EntryPoints: []string{"websecure"},
				Service:     routerName,
				TLS:         &traefikTLS{CertResolver: "letsencrypt"},
			}
			httpConfig.Routers[httpRouterName] = traefikRouter{
				Rule:        rule,
				EntryPoints: []string{"web"},
				Service:     routerName,
				Middlewares: []string{redirectName},
			}
			httpConfig.Middlewares[redirectName] = traefikMiddleware{
				RedirectScheme: &traefikRedirectScheme{Scheme: "https"},
			}

			lb := traefikLoadBalancer{
				Servers:        upstreams,
				PassHostHeader: true,
			}
			if healthCheck := proxyHealthCheckForPort(service, port); healthCheck != nil {
				lb.HealthCheck = healthCheck
			}
			httpConfig.Services[routerName] = traefikService{LoadBalancer: lb}
		}
	}

	if len(httpConfig.Routers) == 0 {
		return nil, false, nil
	}

	data, err := yaml.Marshal(traefikDynamicConfig{HTTP: httpConfig})
	if err != nil {
		return nil, false, fmt.Errorf("failed to render proxy dynamic config: %w", err)
	}
	return data, true, nil
}

func (d *Deployer) takodProxyUpstreamURL(proxyServerName string, upstreamServerName string, serviceName string, slot int, port config.PortConfig) (string, error) {
	if upstreamServerName == proxyServerName {
		if port.Target <= 0 {
			return "", fmt.Errorf("service %s has invalid local proxy port %d", serviceName, port.Target)
		}
		alias := runtimeid.ContainerNetworkAlias(d.config.Project.Name, d.environment, serviceName, slot)
		return port.ProxyScheme() + "://" + net.JoinHostPort(alias, strconv.Itoa(port.Target)), nil
	}
	return d.meshUpstreamURL(upstreamServerName, serviceName, slot, port.Target, port.ProxyScheme())
}

func proxyHealthCheckForPort(service config.ServiceConfig, port config.PortConfig) *traefikHealthCheck {
	if service.LoadBalancer.HealthCheck.Enabled {
		return &traefikHealthCheck{
			Path:     service.LoadBalancer.HealthCheck.Path,
			Interval: service.LoadBalancer.HealthCheck.Interval,
		}
	}
	if service.HealthCheck.Path == "" {
		return nil
	}
	interval := service.HealthCheck.Interval
	if interval == "" {
		interval = "10s"
	}
	return &traefikHealthCheck{
		Path:     service.HealthCheck.Path,
		Interval: interval,
	}
}

func proxyPortsForService(service config.ServiceConfig) []config.PortConfig {
	var ports []config.PortConfig
	for _, port := range service.EffectivePorts() {
		if port.Mode == "proxy" && port.Proxy != nil {
			ports = append(ports, port)
		}
	}
	return ports
}

func proxyRouterName(project string, environment string, serviceName string, port config.PortConfig) string {
	if port.Name == "http" {
		return runtimeid.RouterName(project, environment, serviceName)
	}
	return runtimeid.RouterName(project, environment, serviceName+"_"+port.Name)
}

func (d *Deployer) writeTakodProxyConfig(client *ssh.Client, data []byte) error {
	_, err := takodclient.RequestJSON(client, d.takodSocket(), "PUT", "/v1/proxy-file", takod.ProxyFileRequest{
		Name:    d.takodProxyConfigFileName(),
		Content: string(data),
	})
	return err
}

func (d *Deployer) removeTakodProxyConfig(client *ssh.Client) error {
	if _, err := takodclient.RequestJSON(client, d.takodSocket(), "DELETE", takodclient.ProxyFileEndpoint(d.takodProxyConfigFileName()), nil); err != nil {
		return err
	}
	return nil
}

func (d *Deployer) takodProxyConfigFileName() string {
	return runtimeid.ProxyConfigFileName(d.config.Project.Name, d.environment)
}

func (d *Deployer) meshUpstreamURL(serverName string, serviceName string, slot int, servicePort int, scheme string) (string, error) {
	hostIP, err := d.meshHostIPForServer(serverName)
	if err != nil {
		return "", err
	}
	port, err := d.meshUpstreamPortForServer(serverName, serviceName, slot, servicePort)
	if err != nil {
		return "", err
	}
	if scheme == "" {
		scheme = "http"
	}
	return scheme + "://" + net.JoinHostPort(hostIP, strconv.Itoa(port)), nil
}

func (d *Deployer) meshUpstreamPortForServer(serverName string, serviceName string, slot int, containerPort int) (int, error) {
	if d.meshPortAllocator != nil {
		return d.meshPortAllocator(serverName, serviceName, slot, containerPort)
	}
	client, err := d.getEnvironmentClient(serverName)
	if err != nil {
		return 0, err
	}
	return d.allocateMeshUpstreamPort(client, serverName, serviceName, slot, containerPort)
}

func (d *Deployer) allocateMeshUpstreamPort(client takodclient.RequestExecutor, serverName string, serviceName string, slot int, containerPort int) (int, error) {
	key := meshUpstreamPortKey{ServerName: serverName, ServiceName: serviceName, Slot: slot, ContainerPort: containerPort}
	if port, ok := d.cachedMeshUpstreamPort(key); ok {
		return port, nil
	}

	hostIP, err := d.meshHostIPForServer(serverName)
	if err != nil {
		return 0, err
	}
	preferredPort, err := d.meshUpstreamPort(serviceName, slot, containerPort)
	if err != nil {
		return 0, err
	}
	output, err := takodclient.RequestJSON(client, d.takodSocket(), "POST", "/v1/ports/allocate", takod.PortAllocationRequest{
		Kind:          takod.PortAllocationKindMeshUpstream,
		Project:       d.config.Project.Name,
		Environment:   d.environment,
		Service:       serviceName,
		Slot:          slot,
		HostIP:        hostIP,
		ContainerPort: containerPort,
		PreferredPort: preferredPort,
		MinPort:       meshUpstreamPortBase,
		MaxPort:       meshUpstreamPortMax,
	})
	if err != nil {
		return 0, fmt.Errorf("failed to allocate mesh upstream port for %s slot %d on %s: %w", serviceName, slot, serverName, err)
	}
	var response takod.PortAllocationResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return 0, fmt.Errorf("failed to parse mesh upstream port allocation: %w", err)
	}
	if response.HostPort < meshUpstreamPortBase || response.HostPort > meshUpstreamPortMax {
		return 0, fmt.Errorf("takod allocated invalid mesh upstream port %d for %s slot %d", response.HostPort, serviceName, slot)
	}
	d.storeMeshUpstreamPort(key, response.HostPort)
	return response.HostPort, nil
}

func (d *Deployer) cachedMeshUpstreamPort(key meshUpstreamPortKey) (int, bool) {
	d.meshPortCacheMu.Lock()
	defer d.meshPortCacheMu.Unlock()
	if d.meshPortCache == nil {
		return 0, false
	}
	port, ok := d.meshPortCache[key]
	return port, ok
}

func (d *Deployer) storeMeshUpstreamPort(key meshUpstreamPortKey, port int) {
	d.meshPortCacheMu.Lock()
	defer d.meshPortCacheMu.Unlock()
	if d.meshPortCache == nil {
		d.meshPortCache = make(map[meshUpstreamPortKey]int)
	}
	d.meshPortCache[key] = port
}

func (d *Deployer) meshHostIPForServer(serverName string) (string, error) {
	servers, err := d.getTakodTargetServers()
	if err != nil {
		return "", err
	}
	for i, name := range servers {
		if name != serverName {
			continue
		}
		address, err := d.meshAddress(i)
		if err != nil {
			return "", err
		}
		ip, _, err := net.ParseCIDR(address)
		if err != nil {
			return "", fmt.Errorf("invalid mesh address for %s: %w", serverName, err)
		}
		return ip.String(), nil
	}
	return "", fmt.Errorf("server %s is not a takod target", serverName)
}

func (d *Deployer) meshUpstreamPort(serviceName string, slot int, containerPort int) (int, error) {
	if slot <= 0 {
		return 0, fmt.Errorf("invalid takod slot %d for %s", slot, serviceName)
	}
	if containerPort <= 0 || containerPort > 65535 {
		return 0, fmt.Errorf("invalid container port %d for %s", containerPort, serviceName)
	}
	if slot > meshUpstreamPortSlotLimit {
		return 0, fmt.Errorf("service %s slot %d exceeds per-service mesh upstream limit %d", serviceName, slot, meshUpstreamPortSlotLimit)
	}
	if d.config == nil || d.config.Project.Name == "" || d.environment == "" {
		return 0, fmt.Errorf("project and environment are required for mesh upstream port allocation")
	}
	services, err := d.config.GetServices(d.environment)
	if err != nil {
		return 0, err
	}
	if _, ok := services[serviceName]; !ok {
		return 0, fmt.Errorf("service %s not found", serviceName)
	}
	portRange := meshUpstreamPortMax - meshUpstreamPortBase + 1
	offset := preferredMeshUpstreamPortOffset(d.config.Project.Name, d.environment, serviceName, containerPort, portRange)
	return meshUpstreamPortBase + ((offset + slot - 1) % portRange), nil
}

func preferredMeshUpstreamPortOffset(project string, environment string, serviceName string, containerPort int, portRange int) int {
	hash := fnv.New32a()
	for index, part := range []string{project, environment, serviceName, strconv.Itoa(containerPort)} {
		if index > 0 {
			_, _ = hash.Write([]byte{0})
		}
		_, _ = hash.Write([]byte(part))
	}
	return int(hash.Sum32() % uint32(portRange))
}

func proxyHostRule(proxy *config.ProxyConfig) (string, error) {
	if proxy == nil {
		return "", nil
	}
	domains := proxy.GetAllDomains()
	if len(domains) == 0 {
		primary := proxy.GetPrimaryDomain()
		if primary != "" {
			domains = []string{primary}
		}
	}

	var hostRules []string
	for _, domain := range domains {
		normalized, err := config.NormalizeProxyDomain(domain)
		if err != nil {
			return "", err
		}
		hostRules = append(hostRules, "Host(`"+normalized+"`)")
	}
	return strings.Join(hostRules, " || "), nil
}

func firstProxyEmail(services map[string]config.ServiceConfig) string {
	for _, service := range services {
		for _, port := range proxyPortsForService(service) {
			if port.Proxy != nil && port.Proxy.Email != "" {
				return port.Proxy.Email
			}
		}
	}
	return ""
}
