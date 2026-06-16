package deployer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/serviceimport"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

func (d *Deployer) PrepareGeneratedConfigArtifacts(services map[string]config.ServiceConfig) (map[string]config.ServiceConfig, error) {
	if len(services) == 0 {
		return services, nil
	}
	prepared := make(map[string]config.ServiceConfig, len(services))
	generated := make(map[string][]byte)

	for serviceName, service := range services {
		next := service
		if len(service.Configs) > 0 {
			next.Configs = append([]config.ServiceConfigFileMount(nil), service.Configs...)
		}
		for index := range next.Configs {
			mount := &next.Configs[index]
			configFile, ok := d.config.Configs[mount.Source]
			if !ok || configFile.Generate == nil {
				continue
			}
			data, err := d.renderGeneratedConfigArtifact(mount.Source, configFile.Generate)
			if err != nil {
				return nil, fmt.Errorf("service %s: config %s: %w", serviceName, mount.Source, err)
			}
			sum := sha256.Sum256(data)
			mount.ContentHash = hex.EncodeToString(sum[:])
			generated[mount.Source] = data
		}
		prepared[serviceName] = next
	}

	d.generatedConfigs = generated
	return prepared, nil
}

func (d *Deployer) renderGeneratedConfigArtifact(name string, generator *config.GeneratedConfigConfig) ([]byte, error) {
	if generator == nil {
		return nil, fmt.Errorf("generate is required")
	}
	if generator.Caddy != nil {
		data, err := d.renderGeneratedCaddyConfig(name, generator.Caddy)
		if err != nil {
			return nil, err
		}
		if len(data) > 1<<20 {
			return nil, fmt.Errorf("generated config exceeds 1 MiB")
		}
		return data, nil
	}
	return nil, fmt.Errorf("unsupported generator")
}

func (d *Deployer) renderGeneratedCaddyConfig(name string, caddy *config.GeneratedCaddyConfig) ([]byte, error) {
	if caddy == nil {
		return nil, fmt.Errorf("generate.caddy is required")
	}
	upstreamCache := make(map[string][]string)
	resolve := func(alias string) ([]string, error) {
		if upstreams, ok := upstreamCache[alias]; ok {
			return upstreams, nil
		}
		upstreams, err := d.resolveImportUpstreams(alias)
		if err != nil {
			return nil, err
		}
		if len(upstreams) == 0 {
			return nil, fmt.Errorf("import %s has no healthy upstreams", alias)
		}
		upstreamCache[alias] = upstreams
		return upstreams, nil
	}

	adminUpstreams, err := resolve(caddy.AdminImport)
	if err != nil {
		return nil, fmt.Errorf("adminImport %s: %w", caddy.AdminImport, err)
	}
	rendererUpstreams, err := resolve(caddy.RendererImport)
	if err != nil {
		return nil, fmt.Errorf("rendererImport %s: %w", caddy.RendererImport, err)
	}

	var askUpstream string
	if caddy.OnDemandTLS {
		askImport := caddy.AskImport
		if askImport == "" {
			askImport = caddy.AdminImport
		}
		askUpstreams, err := resolve(askImport)
		if err != nil {
			return nil, fmt.Errorf("askImport %s: %w", askImport, err)
		}
		askUpstream = strings.TrimRight(askUpstreams[0], "/") + caddy.AskPath
	}

	out, err := renderCaddyfileFromImports(caddy, adminUpstreams, rendererUpstreams, askUpstream)
	if err != nil {
		return nil, err
	}
	if d.verbose {
		fmt.Printf("  ✓ Rendered generated Caddy config %s\n", name)
	}
	return []byte(out), nil
}

func (d *Deployer) resolveImportUpstreams(alias string) ([]string, error) {
	if d.importResolver != nil {
		return d.importResolver(alias)
	}
	if d.sshPool == nil {
		return nil, fmt.Errorf("ssh pool not initialized")
	}
	importConfig, ok := d.config.Imports[alias]
	if !ok {
		return nil, fmt.Errorf("import not found in config")
	}
	return d.resolveImportConfigUpstreams(alias, importConfig)
}

func (d *Deployer) resolveImportConfigUpstreams(alias string, importConfig config.ImportConfig) ([]string, error) {
	serverNames, err := serviceimport.ServerNames(d.config, d.environment, importConfig, "")
	if err != nil {
		return nil, err
	}

	var rows []serviceimport.Row
	var failures []string
	successes := 0
	for _, serverName := range serverNames {
		server, ok := d.config.Servers[serverName]
		if !ok {
			failures = append(failures, fmt.Sprintf("%s: server not found in configuration", serverName))
			continue
		}
		client, err := d.sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: connect failed: %v", serverName, err))
			continue
		}
		resolved, err := serviceimport.ResolveExport(client, d.takodSocket(), alias, importConfig)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", serverName, err))
			continue
		}
		endpoint := takodclient.DiscoveryEndpoint(importConfig.Project, importConfig.Environment, importConfig.Service, resolved.Target, false)
		output, err := takodclient.RequestJSON(client, d.takodSocket(), "GET", endpoint, nil)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", serverName, err))
			continue
		}
		var response takod.DiscoveryResponse
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			failures = append(failures, fmt.Sprintf("%s: failed to parse discovery response: %v", serverName, err))
			continue
		}
		port := serviceimport.PortSpec{Name: importConfig.Port, Target: resolved.Target, Protocol: resolved.Protocol}
		endpoints, err := serviceimport.RowsFromResponse(serverName, response, importConfig.Project, importConfig.Environment, importConfig.Service, port)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", serverName, err))
			continue
		}
		successes++
		rows = append(rows, endpoints...)
	}

	sort.Strings(failures)
	if successes == 0 && len(failures) > 0 {
		return nil, fmt.Errorf("failed to discover imported endpoints: %s", strings.Join(failures, "; "))
	}
	if d.verbose {
		for _, warning := range failures {
			fmt.Printf("  Warning: generated config import %s: %s\n", alias, warning)
		}
	}
	upstreams := serviceimport.RowsUpstreams(rows)
	if len(upstreams) == 0 {
		return nil, fmt.Errorf("no healthy endpoints found")
	}
	return upstreams, nil
}

func renderCaddyfileFromImports(caddy *config.GeneratedCaddyConfig, adminUpstreams []string, rendererUpstreams []string, askURL string) (string, error) {
	for label, value := range map[string]string{
		"email":     caddy.Email,
		"adminHost": caddy.AdminHost,
		"siteHost":  caddy.SiteHost,
	} {
		if !isSafeCaddyToken(value) {
			return "", fmt.Errorf("unsafe Caddy %s value", label)
		}
	}
	for label, upstreams := range map[string][]string{
		"admin":    adminUpstreams,
		"renderer": rendererUpstreams,
	} {
		if len(upstreams) == 0 {
			return "", fmt.Errorf("%s upstreams are required", label)
		}
		for _, upstream := range upstreams {
			if !isSafeCaddyToken(upstream) {
				return "", fmt.Errorf("unsafe %s upstream %q", label, upstream)
			}
		}
	}
	if caddy.OnDemandTLS && !isSafeCaddyToken(askURL) {
		return "", fmt.Errorf("unsafe ask URL")
	}

	var b strings.Builder
	b.WriteString("{\n")
	b.WriteString("\temail ")
	b.WriteString(caddy.Email)
	b.WriteString("\n")
	if caddy.OnDemandTLS {
		b.WriteString("\ton_demand_tls {\n")
		b.WriteString("\t\task ")
		b.WriteString(askURL)
		b.WriteString("\n")
		b.WriteString("\t}\n")
	}
	b.WriteString("}\n\n")
	writeCaddyReverseProxyBlock(&b, caddy.AdminHost, adminUpstreams)
	b.WriteString("\n")
	writeCaddyReverseProxyBlock(&b, caddy.SiteHost, rendererUpstreams)
	if caddy.OnDemandTLS {
		b.WriteString("\n")
		writeCaddyOnDemandReverseProxyBlock(&b, rendererUpstreams)
	}
	return b.String(), nil
}

func writeCaddyReverseProxyBlock(b *strings.Builder, matcher string, upstreams []string) {
	b.WriteString(matcher)
	b.WriteString(" {\n")
	b.WriteString("\treverse_proxy ")
	b.WriteString(strings.Join(upstreams, " "))
	b.WriteString(" {\n")
	b.WriteString("\t\theader_up Host {host}\n")
	b.WriteString("\t\theader_up X-Forwarded-Host {host}\n")
	b.WriteString("\t}\n")
	b.WriteString("}\n")
}

func writeCaddyOnDemandReverseProxyBlock(b *strings.Builder, upstreams []string) {
	b.WriteString("https:// {\n")
	b.WriteString("\ttls {\n")
	b.WriteString("\t\ton_demand\n")
	b.WriteString("\t}\n")
	b.WriteString("\treverse_proxy ")
	b.WriteString(strings.Join(upstreams, " "))
	b.WriteString(" {\n")
	b.WriteString("\t\theader_up Host {host}\n")
	b.WriteString("\t\theader_up X-Forwarded-Host {host}\n")
	b.WriteString("\t}\n")
	b.WriteString("}\n")
}

func isSafeCaddyToken(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, " \t\r\n{}\"'\\#") {
		return false
	}
	return true
}
