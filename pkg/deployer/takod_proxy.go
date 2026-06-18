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

type takodProxyReconcileTarget struct {
	ServerName string
	Reconcile  bool
}

func (d *Deployer) ReconcileTakodProxy(services map[string]config.ServiceConfig) error {
	allServers, err := d.getTakodTargetServers()
	if err != nil {
		return fmt.Errorf("failed to get takod proxy cleanup targets: %w", err)
	}
	proxyServers, err := d.getTakodProxyTargetServers()
	if err != nil {
		return fmt.Errorf("failed to get takod proxy targets: %w", err)
	}
	targets := takodProxyReconcileTargets(allServers, proxyServers)
	if len(targets) == 0 {
		return nil
	}

	targetNames := make([]string, 0, len(targets))
	targetByName := make(map[string]takodProxyReconcileTarget, len(targets))
	for _, target := range targets {
		targetNames = append(targetNames, target.ServerName)
		targetByName[target.ServerName] = target
	}
	return runTakodNodeActions(targetNames, func(serverName string) error {
		target := targetByName[serverName]
		client, err := d.getEnvironmentClient(serverName)
		if err != nil {
			return err
		}
		if !target.Reconcile {
			if err := d.removeTakodProxyConfig(client); err != nil {
				return fmt.Errorf("failed to remove proxy config: %w", err)
			}
			return nil
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

func takodProxyReconcileTargets(allServers []string, proxyServers []string) []takodProxyReconcileTarget {
	proxySet := make(map[string]bool, len(proxyServers))
	for _, serverName := range proxyServers {
		proxySet[serverName] = true
	}
	targets := make([]takodProxyReconcileTarget, 0, len(allServers))
	seen := make(map[string]bool, len(allServers))
	for _, serverName := range allServers {
		if seen[serverName] {
			continue
		}
		targets = append(targets, takodProxyReconcileTarget{
			ServerName: serverName,
			Reconcile:  proxySet[serverName],
		})
		seen[serverName] = true
	}
	return targets
}

func (d *Deployer) getTakodProxyTargetServers() ([]string, error) {
	if d.config == nil {
		return nil, fmt.Errorf("config is required")
	}
	if len(d.targetServers) > 0 {
		environmentServers, err := d.config.GetEnvironmentServers(d.environment)
		if err != nil {
			return nil, err
		}
		env, err := d.config.GetEnvironment(d.environment)
		if err != nil {
			return nil, err
		}
		return config.ResolveEnvironmentProxyTargets(env.Proxy, d.config.Servers, environmentServers, d.environment)
	}
	return d.config.GetEnvironmentProxyServers(d.environment)
}

func (d *Deployer) renderTakodProxyDynamicConfigForNode(services map[string]config.ServiceConfig, proxyServerName string) ([]byte, bool, error) {
	if strings.TrimSpace(proxyServerName) == "" {
		return nil, false, fmt.Errorf("proxy server name is required")
	}
	manifest := takod.ProxyRouteManifest{
		Version:     1,
		Project:     d.config.Project.Name,
		Environment: d.environment,
		Network:     takodNetworkName(d.config.Project.Name, d.environment),
	}

	serviceNames := make([]string, 0, len(services))
	for serviceName := range services {
		serviceNames = append(serviceNames, serviceName)
	}
	sort.Strings(serviceNames)

	for _, serviceName := range serviceNames {
		service := services[serviceName]
		if !service.IsPublic() {
			continue
		}
		if service.Port <= 0 {
			return nil, false, fmt.Errorf("service %s has proxy config but no port", serviceName)
		}

		assignments, err := d.planTakodAssignments(&service)
		if err != nil {
			return nil, false, fmt.Errorf("failed to plan proxy upstreams for %s: %w", serviceName, err)
		}
		if len(assignments) == 0 {
			continue
		}
		sortTakodAssignments(assignments)

		dynamicDomainsEnabled := service.Proxy.DynamicDomains != nil && service.Proxy.DynamicDomains.IsEnabled()
		domains, err := explicitProxyDomains(service.Proxy)
		if err != nil {
			return nil, false, fmt.Errorf("service %s has invalid proxy domains: %w", serviceName, err)
		}
		if len(domains) == 0 && !dynamicDomainsEnabled {
			return nil, false, fmt.Errorf("service %s has proxy config but no domains", serviceName)
		}
		redirects, err := redirectProxyDomains(service.Proxy)
		if err != nil {
			return nil, false, fmt.Errorf("service %s has invalid redirect domains: %w", serviceName, err)
		}

		var upstreams []string
		seenUpstreams := make(map[string]bool)
		for _, assignment := range assignments {
			url, err := d.takodProxyUpstreamURL(proxyServerName, assignment.ServerName, serviceName, assignment.Slot, service.Port)
			if err != nil {
				return nil, false, err
			}
			if seenUpstreams[url] {
				continue
			}
			seenUpstreams[url] = true
			upstreams = append(upstreams, url)
		}

		route := takod.ProxyRoute{
			Service:      serviceName,
			Domains:      domains,
			RedirectFrom: redirects,
			Upstreams:    upstreams,
			HealthCheck:  proxyRouteHealthCheckForService(service),
			Sticky:       service.LoadBalancer.Strategy == "sticky",
		}
		if dynamicDomainsEnabled {
			askURL, err := d.dynamicDomainAskURL(services, proxyServerName, service.Proxy.DynamicDomains.Ask)
			if err != nil {
				return nil, false, fmt.Errorf("service %s dynamic domain ask: %w", serviceName, err)
			}
			route.DynamicDomain = &takod.ProxyDynamicDomain{AskURL: askURL}
		}
		manifest.Routes = append(manifest.Routes, route)
	}

	if len(manifest.Routes) == 0 {
		return nil, false, nil
	}

	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, false, fmt.Errorf("failed to render proxy route manifest: %w", err)
	}
	return data, true, nil
}

func sortTakodAssignments(assignments []takodAssignment) {
	sort.Slice(assignments, func(i, j int) bool {
		if assignments[i].ServerName == assignments[j].ServerName {
			return assignments[i].Slot < assignments[j].Slot
		}
		return assignments[i].ServerName < assignments[j].ServerName
	})
}

func (d *Deployer) takodProxyUpstreamURL(proxyServerName string, upstreamServerName string, serviceName string, slot int, servicePort int) (string, error) {
	if upstreamServerName == proxyServerName {
		if servicePort <= 0 {
			return "", fmt.Errorf("service %s has invalid local proxy port %d", serviceName, servicePort)
		}
		return "http://" + net.JoinHostPort(d.takodContainerAlias(serviceName, slot), strconv.Itoa(servicePort)), nil
	}
	return d.meshUpstreamURL(upstreamServerName, serviceName, slot, servicePort)
}

func explicitProxyDomains(proxy *config.ProxyConfig) ([]string, error) {
	if proxy == nil {
		return nil, nil
	}
	domains := proxy.GetAllDomains()
	if len(domains) == 0 {
		primary := proxy.GetPrimaryDomain()
		if primary != "" {
			domains = []string{primary}
		}
	}
	normalized := make([]string, 0, len(domains))
	for _, domain := range domains {
		value, err := normalizeExplicitProxyDomain(domain)
		if err != nil {
			return nil, err
		}
		normalized = append(normalized, value)
	}
	return normalized, nil
}

func redirectProxyDomains(proxy *config.ProxyConfig) ([]string, error) {
	if proxy == nil || len(proxy.GetRedirectDomains()) == 0 {
		return nil, nil
	}
	normalized := make([]string, 0, len(proxy.GetRedirectDomains()))
	for _, domain := range proxy.GetRedirectDomains() {
		value, err := normalizeExplicitProxyDomain(domain)
		if err != nil {
			return nil, err
		}
		normalized = append(normalized, value)
	}
	return normalized, nil
}

func proxyRouteHealthCheckForService(service config.ServiceConfig) *takod.ProxyRouteHealth {
	if service.LoadBalancer.HealthCheck.Enabled {
		return &takod.ProxyRouteHealth{
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
	return &takod.ProxyRouteHealth{
		Path:     service.HealthCheck.Path,
		Interval: interval,
	}
}

func (d *Deployer) dynamicDomainAskURL(services map[string]config.ServiceConfig, proxyServerName string, ask string) (string, error) {
	askService, askPath, err := config.ParseDynamicDomainAsk(ask)
	if err != nil {
		return "", err
	}
	service, ok := services[askService]
	if !ok {
		return "", fmt.Errorf("unknown service %q", askService)
	}
	if service.Port <= 0 {
		return "", fmt.Errorf("service %q must expose a port", askService)
	}
	assignments, err := d.planTakodAssignments(&service)
	if err != nil {
		return "", err
	}
	if len(assignments) == 0 {
		return "", fmt.Errorf("service %q has no active replicas", askService)
	}
	sortTakodAssignments(assignments)
	baseURL, err := d.takodProxyUpstreamURL(proxyServerName, assignments[0].ServerName, askService, assignments[0].Slot, service.Port)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(baseURL, "/") + askPath, nil
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

func (d *Deployer) meshUpstreamURL(serverName string, serviceName string, slot int, servicePort int) (string, error) {
	hostIP, err := d.meshHostIPForServer(serverName)
	if err != nil {
		return "", err
	}
	port, err := d.meshUpstreamPortForServer(serverName, serviceName, slot, servicePort)
	if err != nil {
		return "", err
	}
	return "http://" + net.JoinHostPort(hostIP, strconv.Itoa(port)), nil
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
	preferredPort, err := d.meshUpstreamPort(serviceName, slot)
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

func (d *Deployer) meshUpstreamPort(serviceName string, slot int) (int, error) {
	if slot <= 0 {
		return 0, fmt.Errorf("invalid takod slot %d for %s", slot, serviceName)
	}
	if slot > meshUpstreamPortSlotLimit {
		return 0, fmt.Errorf("service %s slot %d exceeds per-service mesh upstream limit %d", serviceName, slot, meshUpstreamPortSlotLimit)
	}
	if d.config == nil || d.config.Project.Name == "" || d.environment == "" {
		return 0, fmt.Errorf("project and environment are required for mesh upstream port allocation")
	}
	blockCount := (meshUpstreamPortMax - meshUpstreamPortBase + 1) / meshUpstreamPortSlotLimit
	if blockCount <= 0 {
		return 0, fmt.Errorf("invalid mesh upstream port range")
	}
	block, err := d.meshUpstreamPortBlock(serviceName, blockCount)
	if err != nil {
		return 0, err
	}

	port := meshUpstreamPortBase + block*meshUpstreamPortSlotLimit + (slot - 1)
	if port > meshUpstreamPortMax {
		return 0, fmt.Errorf("mesh upstream port %d for %s slot %d exceeds maximum %d", port, serviceName, slot, meshUpstreamPortMax)
	}
	return port, nil
}

func (d *Deployer) meshUpstreamPortBlock(serviceName string, blockCount int) (int, error) {
	services, err := d.config.GetServices(d.environment)
	if err != nil {
		return 0, err
	}
	serviceNames := make([]string, 0, len(services))
	for name := range services {
		serviceNames = append(serviceNames, name)
	}
	sort.Strings(serviceNames)

	allocated := make(map[int]string, len(serviceNames))
	for _, name := range serviceNames {
		block := preferredMeshUpstreamPortBlock(d.config.Project.Name, d.environment, name, blockCount)
		for attempts := 0; attempts < blockCount; attempts++ {
			candidate := (block + attempts) % blockCount
			if _, exists := allocated[candidate]; exists {
				continue
			}
			allocated[candidate] = name
			if name == serviceName {
				return candidate, nil
			}
			break
		}
	}
	return 0, fmt.Errorf("service %s not found or no mesh upstream port blocks are available", serviceName)
}

func preferredMeshUpstreamPortBlock(project string, environment string, serviceName string, blockCount int) int {
	hash := fnv.New32a()
	for index, part := range []string{project, environment, serviceName} {
		if index > 0 {
			_, _ = hash.Write([]byte{0})
		}
		_, _ = hash.Write([]byte(part))
	}
	return int(hash.Sum32() % uint32(blockCount))
}

func normalizeExplicitProxyDomain(domain string) (string, error) {
	normalized, err := config.NormalizeProxyDomain(domain)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(normalized, "*.") {
		return "", fmt.Errorf("wildcard proxy domain %q is not supported by the built-in tako-proxy yet", normalized)
	}
	return normalized, nil
}

func firstProxyEmail(services map[string]config.ServiceConfig) string {
	for _, service := range services {
		if service.Proxy != nil && service.Proxy.Email != "" {
			return service.Proxy.Email
		}
	}
	return ""
}
