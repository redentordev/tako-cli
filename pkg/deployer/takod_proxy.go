package deployer

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"gopkg.in/yaml.v3"
)

const (
	meshUpstreamPortBase = 20000
	meshUpstreamPortStep = 1000
	meshUpstreamPortMax  = 65000
)

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

	dynamicConfig, hasPublicServices, err := d.renderTakodProxyDynamicConfig(services)
	if err != nil {
		return err
	}

	for _, serverName := range targetServers {
		client, err := d.getEnvironmentClient(serverName)
		if err != nil {
			return err
		}

		if !hasPublicServices {
			if err := d.removeTakodProxyConfig(client); err != nil {
				return fmt.Errorf("%s: failed to remove proxy config: %w", serverName, err)
			}
			continue
		}

		if err := d.writeTakodProxyConfig(client, dynamicConfig); err != nil {
			return fmt.Errorf("%s: failed to write proxy config: %w", serverName, err)
		}
		if err := d.ensureTakodProxy(client, takodNetworkName(d.config.Project.Name, d.environment), firstProxyEmail(services)); err != nil {
			return fmt.Errorf("%s: failed to reconcile proxy: %w", serverName, err)
		}
	}

	return nil
}

func (d *Deployer) renderTakodProxyDynamicConfig(services map[string]config.ServiceConfig) ([]byte, bool, error) {
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
		sort.Slice(assignments, func(i, j int) bool {
			if assignments[i].ServerName == assignments[j].ServerName {
				return assignments[i].Slot < assignments[j].Slot
			}
			return assignments[i].ServerName < assignments[j].ServerName
		})

		routerName := sanitizeRouterName(d.config.Project.Name + "-" + d.environment + "-" + serviceName)
		rule := proxyHostRule(service.Proxy)
		if rule == "" {
			return nil, false, fmt.Errorf("service %s has proxy config but no domains", serviceName)
		}

		var upstreams []traefikServer
		for _, assignment := range assignments {
			url, err := d.meshUpstreamURL(assignment.ServerName, serviceName, assignment.Slot)
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
		if service.LoadBalancer.HealthCheck.Enabled {
			lb.HealthCheck = &traefikHealthCheck{
				Path:     service.LoadBalancer.HealthCheck.Path,
				Interval: service.LoadBalancer.HealthCheck.Interval,
			}
		}
		httpConfig.Services[routerName] = traefikService{LoadBalancer: lb}
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

func (d *Deployer) writeTakodProxyConfig(client *ssh.Client, data []byte) error {
	_, err := takodclient.RequestJSON(client, d.takodSocket(), "PUT", "/v1/proxy-file", takod.ProxyFileRequest{
		Name:    d.takodProxyConfigFileName(),
		Content: string(data),
	})
	return err
}

func (d *Deployer) removeTakodProxyConfig(client *ssh.Client) error {
	_, err := takodclient.RequestJSON(client, d.takodSocket(), "DELETE", takodclient.ProxyFileEndpoint(d.takodProxyConfigFileName()), nil)
	return err
}

func (d *Deployer) takodProxyConfigFileName() string {
	return sanitizeRouterName(d.config.Project.Name+"-"+d.environment) + ".yml"
}

func (d *Deployer) meshUpstreamURL(serverName string, serviceName string, slot int) (string, error) {
	hostIP, err := d.meshHostIPForServer(serverName)
	if err != nil {
		return "", err
	}
	port, err := d.meshUpstreamPort(serviceName, slot)
	if err != nil {
		return "", err
	}
	return "http://" + net.JoinHostPort(hostIP, strconv.Itoa(port)), nil
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
	if slot >= meshUpstreamPortStep {
		return 0, fmt.Errorf("service %s slot %d exceeds per-service mesh upstream limit %d", serviceName, slot, meshUpstreamPortStep-1)
	}
	services, err := d.config.GetServices(d.environment)
	if err != nil {
		return 0, err
	}
	serviceNames := make([]string, 0, len(services))
	for name := range services {
		serviceNames = append(serviceNames, name)
	}
	sort.Strings(serviceNames)

	serviceIndex := -1
	for i, name := range serviceNames {
		if name == serviceName {
			serviceIndex = i
			break
		}
	}
	if serviceIndex < 0 {
		return 0, fmt.Errorf("service %s not found", serviceName)
	}

	port := meshUpstreamPortBase + serviceIndex*meshUpstreamPortStep + slot
	if port > meshUpstreamPortMax {
		return 0, fmt.Errorf("mesh upstream port %d for %s slot %d exceeds maximum %d", port, serviceName, slot, meshUpstreamPortMax)
	}
	return port, nil
}

func proxyHostRule(proxy *config.ProxyConfig) string {
	if proxy == nil {
		return ""
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
		hostRules = append(hostRules, "Host(`"+domain+"`)")
	}
	return strings.Join(hostRules, " || ")
}

func firstProxyEmail(services map[string]config.ServiceConfig) string {
	for _, service := range services {
		if service.Proxy != nil && service.Proxy.Email != "" {
			return service.Proxy.Email
		}
	}
	return ""
}
