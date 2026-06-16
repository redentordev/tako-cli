package deployer

import (
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
)

func (d *Deployer) resolveServiceEnv(serviceName string, service *config.ServiceConfig) (map[string]config.EnvValue, error) {
	if len(service.Env) == 0 {
		return nil, nil
	}
	resolved := make(map[string]config.EnvValue, len(service.Env))
	for key, value := range service.Env {
		switch {
		case value.IsPlain():
			resolved[key] = value
		case value.URL != "":
			resolved[key] = config.PlainEnvValue(value.URL)
		case value.Link != nil:
			url, err := d.resolveServiceLinkURL(serviceName, key, *value.Link)
			if err != nil {
				return nil, err
			}
			resolved[key] = config.PlainEnvValue(url)
		default:
			return nil, fmt.Errorf("service %s env %s has invalid value", serviceName, key)
		}
	}
	return resolved, nil
}

func (d *Deployer) resolveServiceLinkURL(serviceName string, envKey string, link config.ServiceLinkRef) (string, error) {
	if strings.TrimSpace(link.App) == "" && strings.TrimSpace(link.Stage) == "" {
		services, err := d.config.GetServices(d.environment)
		if err != nil {
			return "", err
		}
		target, ok := services[link.Service]
		if !ok {
			return "", fmt.Errorf("service %s env %s links unknown service %q", serviceName, envKey, link.Service)
		}
		port, err := config.ResolveServicePort(link.Service, target, link.Port)
		if err != nil {
			return "", fmt.Errorf("service %s env %s: %w", serviceName, envKey, err)
		}
		return serviceURL(port.ProxyScheme(), link.Service, port.Target), nil
	}

	importConfig := config.ImportConfig{
		Project:     strings.TrimSpace(link.App),
		Environment: strings.TrimSpace(link.Stage),
		Service:     strings.TrimSpace(link.Service),
		Port:        strings.TrimSpace(link.Port),
		Servers:     append([]string(nil), link.Servers...),
	}
	if importConfig.Port == "" {
		importConfig.Port = config.DefaultSharedPortName
	}
	upstreams, err := d.resolveImportConfigUpstreams(envKey, importConfig)
	if err != nil {
		return "", fmt.Errorf("service %s env %s import %s/%s/%s: %w", serviceName, envKey, importConfig.Project, importConfig.Environment, importConfig.Service, err)
	}
	if len(upstreams) == 0 {
		return "", fmt.Errorf("service %s env %s import %s/%s/%s has no healthy upstreams", serviceName, envKey, importConfig.Project, importConfig.Environment, importConfig.Service)
	}
	sort.Strings(upstreams)
	return upstreams[0], nil
}

func serviceURL(scheme string, host string, port int) string {
	if scheme == "" {
		scheme = "http"
	}
	return scheme + "://" + net.JoinHostPort(host, fmt.Sprintf("%d", port))
}
