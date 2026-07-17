package deployer

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
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
	Revision      string
	Slot          int
	ContainerPort int
}

type takodProxyReconcileTarget struct {
	ServerName string
	Reconcile  bool
}

type takodProxyRenderOptions struct {
	ActiveRevisions map[string]string
}

func (d *Deployer) ReconcileTakodProxy(services map[string]config.ServiceConfig) error {
	return d.reconcileTakodProxyWithOptions(services, takodProxyRenderOptions{})
}

func (d *Deployer) ReconcileTakodProxyWithActiveRevisions(services map[string]config.ServiceConfig, activeRevisions map[string]string) error {
	normalizedRevisions, err := normalizeTakodProxyActiveRevisions(services, activeRevisions)
	if err != nil {
		return err
	}
	return d.reconcileTakodProxyWithOptions(services, takodProxyRenderOptions{ActiveRevisions: normalizedRevisions})
}

// PreflightTakodProxyCapabilities verifies every proxy target understands all
// route-manifest fields before an applying workflow mutates service state.
func (d *Deployer) PreflightTakodProxyCapabilities(services map[string]config.ServiceConfig) error {
	requirements := takodProxyCapabilityRequirements(services)
	hasRoutes := false
	for _, service := range services {
		hasRoutes = hasRoutes || service.IsProxied()
	}
	if !hasRoutes && len(requirements) == 0 {
		return nil
	}
	proxyServers, err := d.getTakodProxyTargetServers()
	if err != nil {
		return fmt.Errorf("failed to get takod proxy targets: %w", err)
	}
	requiresRemote, err := d.takodProxyRequiresRemoteMeshRoutes(services, proxyServers)
	if err != nil {
		return err
	}
	if requiresRemote {
		requirements = append(requirements, takodProxyCapabilityRequirement{
			Capability: takod.CapabilityProxyRemoteMeshRoutesV1,
			Feature:    "authoritative remote mesh proxy routes",
		})
	}
	if len(requirements) == 0 {
		return nil
	}
	return preflightTakodProxyRequirements(proxyServers, requirements, func(serverName string, requirement takodProxyCapabilityRequirement) error {
		client, err := d.getRuntimeClient(serverName)
		if err != nil {
			return err
		}
		return d.ensureTakodCapability(client, serverName, requirement.Capability, requirement.Feature)
	})
}

func (d *Deployer) takodProxyRequiresRemoteMeshRoutes(services map[string]config.ServiceConfig, proxyServers []string) (bool, error) {
	for serviceName, service := range services {
		if !service.IsProxied() {
			continue
		}
		assignments, err := d.planTakodAssignments(&service)
		if err != nil {
			return false, fmt.Errorf("plan proxy preflight assignments for %s: %w", serviceName, err)
		}
		for _, proxyServer := range proxyServers {
			if assignmentsRequireRemote(proxyServer, assignments) {
				return true, nil
			}
		}
		if service.Proxy.DynamicDomains != nil && service.Proxy.DynamicDomains.IsEnabled() {
			askService, _, err := config.ParseDynamicDomainAsk(service.Proxy.DynamicDomains.Ask)
			if err != nil {
				return false, fmt.Errorf("parse dynamic-domain proxy preflight for %s: %w", serviceName, err)
			}
			askConfig, ok := services[askService]
			if !ok {
				return false, fmt.Errorf("dynamic-domain ask service %s is not configured", askService)
			}
			askAssignments, planErr := d.planTakodAssignments(&askConfig)
			if planErr != nil {
				return false, fmt.Errorf("plan dynamic-domain proxy preflight assignments for %s: %w", askService, planErr)
			}
			for _, proxyServer := range proxyServers {
				if dynamicAskAssignmentRequiresRemote(proxyServer, askAssignments) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

func proxyAssignmentsRequireRemote(proxyServers []string, assignmentsByService map[string][]takodAssignment) bool {
	for _, proxyServer := range proxyServers {
		for _, assignments := range assignmentsByService {
			if assignmentsRequireRemote(proxyServer, assignments) {
				return true
			}
		}
	}
	return false
}

func assignmentsRequireRemote(proxyServer string, assignments []takodAssignment) bool {
	for _, assignment := range assignments {
		if assignment.ServerName != proxyServer {
			return true
		}
	}
	return false
}

func dynamicAskAssignmentRequiresRemote(proxyServer string, assignments []takodAssignment) bool {
	if len(assignments) == 0 {
		return false
	}
	ordered := append([]takodAssignment(nil), assignments...)
	sortTakodAssignments(ordered)
	return ordered[0].ServerName != proxyServer
}

func (d *Deployer) reconcileTakodProxyWithOptions(services map[string]config.ServiceConfig, options takodProxyRenderOptions) error {
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
	return runTakodProxyReconcile(
		func() error { return d.PreflightTakodProxyCapabilities(services) },
		func() error {
			return runTakodNodeActions(targetNames, func(serverName string) error {
				target := targetByName[serverName]
				client, err := d.getRuntimeClient(serverName)
				if err != nil {
					return err
				}
				if !target.Reconcile {
					if err := d.removeTakodProxyConfig(client); err != nil {
						return fmt.Errorf("failed to remove proxy config: %w", err)
					}
					if err := d.removeTakodProxyACME(client); err != nil {
						return fmt.Errorf("failed to remove proxy ACME ownership: %w", err)
					}
					return nil
				}

				dynamicConfig, hasPublicServices, err := d.renderTakodProxyDynamicConfigForNodeWithOptions(services, serverName, options)
				if err != nil {
					return err
				}
				if !hasPublicServices {
					if err := d.removeTakodProxyConfig(client); err != nil {
						return fmt.Errorf("failed to remove proxy config: %w", err)
					}
					if err := d.removeTakodProxyACME(client); err != nil {
						return fmt.Errorf("failed to remove proxy ACME ownership: %w", err)
					}
					return nil
				}

				acmeRequest, err := d.syncTakodProxyACMEForServices(client, serverName, services)
				if err != nil {
					return fmt.Errorf("failed to issue proxy DNS certificate: %w", err)
				}
				if err := d.writeTakodProxyConfig(client, dynamicConfig); err != nil {
					return fmt.Errorf("failed to write proxy config: %w", err)
				}
				if acmeRequest == nil {
					if err := d.removeTakodProxyACME(client); err != nil {
						return fmt.Errorf("failed to remove proxy ACME ownership: %w", err)
					}
				} else if err := d.finalizeTakodProxyACME(client, *acmeRequest); err != nil {
					return fmt.Errorf("failed to finalize proxy ACME ownership: %w", err)
				}
				if err := d.ensureTakodProxy(client, takodNetworkName(d.config.Project.Name, d.environment), firstProxyEmail(services)); err != nil {
					return fmt.Errorf("failed to reconcile proxy: %w", err)
				}
				return nil
			})
		},
	)
}

func (d *Deployer) syncTakodProxyACMEForServices(client any, serverName string, services map[string]config.ServiceConfig) (*takod.ACMEDNSReconcileRequest, error) {
	request, err := d.takodProxyACMEDNSRequest(services)
	if err != nil {
		return nil, err
	}
	if request == nil {
		return nil, nil
	}
	if err := d.syncTakodProxyACME(client, serverName, *request); err != nil {
		return nil, err
	}
	return request, nil
}

func runTakodProxyReconcile(preflight func() error, mutate func() error) error {
	if err := preflight(); err != nil {
		return err
	}
	return mutate()
}

func normalizeTakodProxyActiveRevisions(services map[string]config.ServiceConfig, activeRevisions map[string]string) (map[string]string, error) {
	if len(activeRevisions) == 0 {
		return nil, nil
	}
	normalized := make(map[string]string, len(activeRevisions))
	for serviceName, revision := range activeRevisions {
		if _, ok := services[serviceName]; !ok {
			return nil, fmt.Errorf("active revision references unknown service %q", serviceName)
		}
		revision = strings.TrimSpace(revision)
		if revision == "" {
			return nil, fmt.Errorf("active revision for service %s is required", serviceName)
		}
		if !isSafeTakodProxyRevision(revision) {
			return nil, fmt.Errorf("active revision for service %s contains unsafe characters", serviceName)
		}
		normalized[serviceName] = revision
	}
	return normalized, nil
}

func isSafeTakodProxyRevision(value string) bool {
	if len(value) == 0 || len(value) > 63 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
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
	return d.renderTakodProxyDynamicConfigForNodeWithOptions(services, proxyServerName, takodProxyRenderOptions{})
}

func (d *Deployer) renderTakodProxyDynamicConfigForNodeWithOptions(services map[string]config.ServiceConfig, proxyServerName string, options takodProxyRenderOptions) ([]byte, bool, error) {
	if strings.TrimSpace(proxyServerName) == "" {
		return nil, false, fmt.Errorf("proxy server name is required")
	}
	proxyServer, ok := d.config.Servers[proxyServerName]
	if !ok {
		return nil, false, fmt.Errorf("proxy server %s is not configured", proxyServerName)
	}
	manifestVersion := 1
	if proxyServer.ClusterID != "" && proxyServer.NodeID != "" && proxyServer.WorkerUID > 0 {
		manifestVersion = 2
	}
	manifest := takod.ProxyRouteManifest{
		Version:     manifestVersion,
		Project:     d.config.Project.Name,
		Environment: d.environment,
		Network:     takodNetworkName(d.config.Project.Name, d.environment),
		ClusterID:   proxyServer.ClusterID,
	}

	serviceNames := make([]string, 0, len(services))
	for serviceName := range services {
		serviceNames = append(serviceNames, serviceName)
	}
	sort.Strings(serviceNames)

	for _, serviceName := range serviceNames {
		service := services[serviceName]
		if !service.IsProxied() {
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

		revision := proxyActiveRevisionForService(options.ActiveRevisions, serviceName)
		var upstreams []string
		var destinations []takod.ProxyDestination
		seenUpstreams := make(map[string]bool)
		for _, assignment := range assignments {
			url, err := d.takodProxyUpstreamURLForRevision(proxyServerName, assignment.ServerName, serviceName, revision, assignment.Slot, service.Port)
			if err != nil {
				return nil, false, err
			}
			if seenUpstreams[url] {
				continue
			}
			seenUpstreams[url] = true
			upstreams = append(upstreams, url)
			if manifestVersion >= 2 {
				proof, err := d.proxyDestinationProof(proxyServerName, assignment, serviceName, revision, service.Port, url)
				if err != nil {
					return nil, false, err
				}
				destinations = append(destinations, proof)
			}
		}

		route := takod.ProxyRoute{
			Service:        serviceName,
			Revision:       revision,
			Domains:        domains,
			RedirectFrom:   redirects,
			Upstreams:      upstreams,
			HealthCheck:    proxyRouteHealthCheckForService(service),
			Sticky:         service.LoadBalancer.Strategy == "sticky",
			Visibility:     service.Proxy.EffectiveVisibility(),
			AllowIPs:       append([]string(nil), service.Proxy.AllowIps...),
			TrustedProxies: append([]string(nil), service.Proxy.TrustedProxies...),
			Destinations:   destinations,
		}
		if auth := service.Proxy.BasicAuth; auth != nil {
			route.BasicAuth = &takod.ProxyRouteBasicAuth{
				Username:       auth.Username,
				PasswordBcrypt: auth.PasswordBcrypt,
			}
		}
		if dynamicDomainsEnabled {
			askURL, err := d.dynamicDomainAskURL(services, proxyServerName, service.Proxy.DynamicDomains.Ask, options.ActiveRevisions)
			if err != nil {
				return nil, false, fmt.Errorf("service %s dynamic domain ask: %w", serviceName, err)
			}
			route.DynamicDomain = &takod.ProxyDynamicDomain{AskURL: askURL}
			if manifestVersion >= 2 {
				askService, _, _ := config.ParseDynamicDomainAsk(service.Proxy.DynamicDomains.Ask)
				askConfig := services[askService]
				assignments, assignmentErr := d.planTakodAssignments(&askConfig)
				if assignmentErr != nil {
					return nil, false, fmt.Errorf("plan dynamic ask destination for %s: %w", askService, assignmentErr)
				}
				if len(assignments) == 0 {
					return nil, false, fmt.Errorf("dynamic ask service %s has no provable destination", askService)
				}
				sortTakodAssignments(assignments)
				proof, proofErr := d.proxyDestinationProof(proxyServerName, assignments[0], askService, proxyActiveRevisionForService(options.ActiveRevisions, askService), askConfig.Port, askURL)
				if proofErr != nil {
					return nil, false, proofErr
				}
				route.DynamicDomain.Destination = &proof
			}
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

func proxyServicesUseTrustedProxies(services map[string]config.ServiceConfig) bool {
	for _, service := range services {
		if service.IsProxied() && service.Proxy != nil && len(service.Proxy.TrustedProxies) > 0 {
			return true
		}
	}
	return false
}

func proxyServicesUseACMEDNS(services map[string]config.ServiceConfig) bool {
	for _, service := range services {
		if service.Proxy == nil || !service.IsPublic() {
			continue
		}
		if service.Proxy.TLS.Challenge == config.ProxyTLSChallengeDNS {
			return true
		}
		for _, domain := range append(service.Proxy.GetAllDomains(), service.Proxy.GetRedirectDomains()...) {
			if strings.HasPrefix(domain, "*.") {
				return true
			}
		}
	}
	return false
}

type takodProxyCapabilityRequirement struct {
	Capability string
	Feature    string
}

func takodProxyCapabilityRequirements(services map[string]config.ServiceConfig) []takodProxyCapabilityRequirement {
	var requirements []takodProxyCapabilityRequirement
	if proxyServicesUseTrustedProxies(services) {
		requirements = append(requirements, takodProxyCapabilityRequirement{Capability: takod.CapabilityProxyTrustedProxiesV1, Feature: "proxy trusted proxies"})
	}
	if proxyServicesUseACMEDNS(services) {
		requirements = append(requirements, takodProxyCapabilityRequirement{Capability: takod.CapabilityAcmeDNSV1, Feature: "embedded ACME DNS-01 issuance"})
	}
	return requirements
}

func preflightTakodProxyRequirements(proxyServers []string, requirements []takodProxyCapabilityRequirement, check func(string, takodProxyCapabilityRequirement) error) error {
	return runTakodNodeActions(proxyServers, func(serverName string) error {
		for _, requirement := range requirements {
			if err := check(serverName, requirement); err != nil {
				return err
			}
		}
		return nil
	})
}

func (d *Deployer) takodProxyACMEDNSRequest(services map[string]config.ServiceConfig) (*takod.ACMEDNSReconcileRequest, error) {
	if !proxyServicesUseACMEDNS(services) {
		return nil, nil
	}
	environment, err := d.config.GetEnvironment(d.environment)
	if err != nil {
		return nil, err
	}
	if environment.Proxy == nil || environment.Proxy.ACME == nil {
		return nil, fmt.Errorf("environment %s DNS-01 routes require proxy.acme configuration", d.environment)
	}
	request := &takod.ACMEDNSReconcileRequest{
		Project: d.config.Project.Name, Environment: d.environment,
		DNSProvider: environment.Proxy.ACME.DNSProvider,
		Credentials: cloneTakodProxyStringMap(environment.Proxy.ACME.Credentials),
	}
	seen := make(map[string]bool)
	serviceNames := make([]string, 0, len(services))
	for name := range services {
		serviceNames = append(serviceNames, name)
	}
	sort.Strings(serviceNames)
	for _, serviceName := range serviceNames {
		service := services[serviceName]
		if service.Proxy == nil || !service.IsPublic() {
			continue
		}
		domains := append(append([]string(nil), service.Proxy.GetAllDomains()...), service.Proxy.GetRedirectDomains()...)
		dnsRoute := service.Proxy.TLS.Challenge == config.ProxyTLSChallengeDNS
		for _, domain := range domains {
			if !dnsRoute && !strings.HasPrefix(domain, "*.") {
				continue
			}
			domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
			if seen[domain] {
				continue
			}
			seen[domain] = true
			request.Certificates = append(request.Certificates, takod.ACMEDNSCertificateRequest{
				Domain: domain, Email: service.Proxy.Email,
				CAProvider: service.Proxy.TLS.Provider, Staging: service.Proxy.TLS.Staging,
			})
		}
	}
	sort.Slice(request.Certificates, func(i, j int) bool { return request.Certificates[i].Domain < request.Certificates[j].Domain })
	return request, nil
}

func (d *Deployer) syncTakodProxyACME(client any, serverName string, request takod.ACMEDNSReconcileRequest) error {
	for _, certificate := range request.Certificates {
		d.emitEvent(events.Event{
			Type: events.TypeCertIssueStarted, Phase: events.PhaseDomains, Level: events.LevelInfo, Node: serverName,
			Message: fmt.Sprintf("  -> Issuing DNS certificate for %s on %s\n", certificate.Domain, serverName),
			Data:    map[string]any{"domain": certificate.Domain, "dnsProvider": request.DNSProvider},
		})
	}
	output, err := takodclient.RequestJSONWithTimeoutContext(d.baseContext(), client, d.takodSocket(), "PUT", takodclient.ACMEDNSEndpoint("", ""), request, takodclient.ACMEDNSRequestTimeout)
	if err != nil {
		typed := takodclient.ParseACMEDNSError(serverName, err)
		var operationErr *takodclient.ACMEOperationError
		if errors.As(typed, &operationErr) {
			for _, completed := range operationErr.Completed {
				d.emitACMEDNSCompletedEvent(serverName, request.DNSProvider, completed.Domain, completed.Action, completed.Certificate.NotAfter)
			}
		}
		for _, certificate := range request.Certificates {
			if operationErr != nil && operationErr.Domain != "" && certificate.Domain != operationErr.Domain {
				continue
			}
			data := map[string]any{"domain": certificate.Domain, "dnsProvider": request.DNSProvider, "error": typed.Error()}
			if operationErr != nil {
				data["errorClass"] = operationErr.Code
				if !operationErr.RetryAfter.IsZero() {
					data["retryAfter"] = operationErr.RetryAfter.UTC().Format(time.RFC3339)
				}
			}
			d.emitEvent(events.Event{
				Type: events.TypeCertIssueFailed, Phase: events.PhaseDomains, Level: events.LevelError, Node: serverName,
				Message: fmt.Sprintf("  ✗ DNS certificate issuance failed for %s on %s: %v\n", certificate.Domain, serverName, typed),
				Data:    data,
			})
		}
		return typed
	}
	var response takod.ACMEDNSReconcileResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return fmt.Errorf("invalid ACME DNS response: %w", err)
	}
	for _, certificate := range response.Certificates {
		d.emitACMEDNSCompletedEvent(serverName, request.DNSProvider, certificate.Domain, certificate.Action, certificate.Certificate.NotAfter)
	}
	return nil
}

func (d *Deployer) emitACMEDNSCompletedEvent(serverName, dnsProvider, domain, action string, notAfter time.Time) {
	eventType := events.TypeCertIssueCompleted
	message := fmt.Sprintf("  ✓ DNS certificate issued for %s on %s\n", domain, serverName)
	if action == "reused" {
		eventType = events.TypeCertIssueSkipped
		message = fmt.Sprintf("  ✓ DNS certificate for %s already valid on %s\n", domain, serverName)
	}
	data := map[string]any{"domain": domain, "dnsProvider": dnsProvider, "action": action}
	if !notAfter.IsZero() {
		data["notAfter"] = notAfter
	}
	d.emitEvent(events.Event{Type: eventType, Phase: events.PhaseDomains, Level: events.LevelInfo, Node: serverName, Message: message, Data: data})
}

func (d *Deployer) removeTakodProxyACME(client any) error {
	_, err := takodclient.RequestJSONWithContext(d.baseContext(), client, d.takodSocket(), "DELETE", takodclient.ACMEDNSEndpoint(d.config.Project.Name, d.environment), nil)
	if err != nil {
		var httpErr *takodclient.HTTPError
		if errors.As(err, &httpErr) && httpErr.Status == 404 {
			return nil
		}
	}
	return err
}

func (d *Deployer) finalizeTakodProxyACME(client any, request takod.ACMEDNSReconcileRequest) error {
	_, err := takodclient.RequestJSONWithContext(d.baseContext(), client, d.takodSocket(), "POST", takodclient.ACMEDNSEndpoint(request.Project, request.Environment), nil)
	return err
}

func cloneTakodProxyStringMap(values map[string]string) map[string]string {
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func preflightTakodProxyCapabilitiesWithCheck(services map[string]config.ServiceConfig, proxyServers []string, check func(string) error) error {
	if !proxyServicesUseTrustedProxies(services) {
		return nil
	}
	return runTakodNodeActions(proxyServers, check)
}

func proxyActiveRevisionForService(activeRevisions map[string]string, serviceName string) string {
	if len(activeRevisions) == 0 {
		return ""
	}
	return strings.TrimSpace(activeRevisions[serviceName])
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
	return d.takodProxyUpstreamURLForRevision(proxyServerName, upstreamServerName, serviceName, "", slot, servicePort)
}

func (d *Deployer) takodProxyUpstreamURLForRevision(proxyServerName string, upstreamServerName string, serviceName string, revision string, slot int, servicePort int) (string, error) {
	if upstreamServerName == proxyServerName {
		if servicePort <= 0 {
			return "", fmt.Errorf("service %s has invalid local proxy port %d", serviceName, servicePort)
		}
		alias := d.takodContainerAlias(serviceName, slot)
		if revision != "" {
			alias = runtimeid.RevisionContainerAlias(d.config.Project.Name, d.environment, serviceName, revision, slot)
		}
		return "http://" + net.JoinHostPort(alias, strconv.Itoa(servicePort)), nil
	}
	return d.meshUpstreamURLForRevision(upstreamServerName, serviceName, revision, slot, servicePort)
}

func explicitProxyDomains(proxy *config.ProxyConfig) ([]string, error) {
	if proxy == nil {
		return nil, nil
	}
	domains := proxy.GetAllHosts()
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
		if strings.HasPrefix(value, "*.") {
			return nil, fmt.Errorf("wildcard proxy domain %q is not supported in redirectFrom", value)
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

func (d *Deployer) dynamicDomainAskURL(services map[string]config.ServiceConfig, proxyServerName string, ask string, activeRevisions map[string]string) (string, error) {
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
	baseURL, err := d.takodDynamicAskBaseURLForRevision(proxyServerName, assignments[0], askService, proxyActiveRevisionForService(activeRevisions, askService), service.Port)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(baseURL, "/") + askPath, nil
}

func (d *Deployer) takodDynamicAskBaseURL(proxyServerName string, assignment takodAssignment, serviceName string, servicePort int) (string, error) {
	return d.takodDynamicAskBaseURLForRevision(proxyServerName, assignment, serviceName, "", servicePort)
}

func (d *Deployer) takodDynamicAskBaseURLForRevision(proxyServerName string, assignment takodAssignment, serviceName string, revision string, servicePort int) (string, error) {
	if assignment.ServerName == proxyServerName {
		if servicePort <= 0 {
			return "", fmt.Errorf("service %s has invalid local proxy port %d", serviceName, servicePort)
		}
		host := d.takodContainerAlias(serviceName, assignment.Slot)
		if revision != "" {
			host = runtimeid.RevisionContainerAlias(d.config.Project.Name, d.environment, serviceName, revision, assignment.Slot)
		}
		return "http://" + net.JoinHostPort(host, strconv.Itoa(servicePort)), nil
	}
	return d.meshUpstreamURLForRevision(assignment.ServerName, serviceName, revision, assignment.Slot, servicePort)
}

func (d *Deployer) proxyDestinationProof(proxyServerName string, assignment takodAssignment, serviceName string, revision string, containerPort int, destinationURL string) (takod.ProxyDestination, error) {
	proof := takod.ProxyDestination{
		URL: destinationURL, Project: d.config.Project.Name, Environment: d.environment,
		Service: serviceName, Revision: revision, Slot: assignment.Slot,
		ContainerPort: containerPort, HostPort: containerPort,
	}
	if assignment.ServerName == proxyServerName {
		proof.Kind = takod.ProxyDestinationRuntimeAlias
		return proof, nil
	}
	server, ok := d.config.Servers[assignment.ServerName]
	if !ok {
		return takod.ProxyDestination{}, fmt.Errorf("destination node %s is not configured", assignment.ServerName)
	}
	if server.ClusterID == "" || server.NodeID == "" || server.ClusterID != d.config.Servers[proxyServerName].ClusterID {
		return takod.ProxyDestination{}, fmt.Errorf("remote proxy destination %s must be an authenticated member of cluster %s", assignment.ServerName, d.config.Servers[proxyServerName].ClusterID)
	}
	key := meshUpstreamPortKey{ServerName: assignment.ServerName, ServiceName: serviceName, Revision: revision, Slot: assignment.Slot, ContainerPort: containerPort}
	d.meshPortCacheMu.Lock()
	evidence, found := d.meshPortEvidence[key]
	d.meshPortCacheMu.Unlock()
	if !found {
		return takod.ProxyDestination{}, fmt.Errorf("remote proxy destination %s has no authenticated mesh allocation evidence", assignment.ServerName)
	}
	if evidence.ClusterID != server.ClusterID || evidence.NodeID != server.NodeID || evidence.Project != proof.Project || evidence.Environment != proof.Environment || evidence.Service != proof.Service || evidence.Revision != proof.Revision || evidence.Slot != proof.Slot || evidence.ContainerPort != containerPort || evidence.Generation == 0 || evidence.IssuedAt.IsZero() || strings.TrimSpace(evidence.Signature) == "" {
		return takod.ProxyDestination{}, fmt.Errorf("remote proxy destination %s returned mismatched mesh allocation identity", assignment.ServerName)
	}
	proof.Kind = takod.ProxyDestinationMesh
	proof.ClusterID = evidence.ClusterID
	proof.NodeID = evidence.NodeID
	proof.AllocationKey = evidence.Key
	proof.Generation = evidence.Generation
	proof.IssuedAt = evidence.IssuedAt
	proof.Signature = evidence.Signature
	proof.HostPort = evidence.HostPort
	proof.HostIP = evidence.HostIP
	return proof, nil
}

func (d *Deployer) writeTakodProxyConfig(client any, data []byte) error {
	_, err := takodclient.RequestJSON(client, d.takodSocket(), "PUT", "/v1/proxy-file", takod.ProxyFileRequest{
		Name:    d.takodProxyConfigFileName(),
		Content: string(data),
	})
	return err
}

func (d *Deployer) removeTakodProxyConfig(client any) error {
	if _, err := takodclient.RequestJSON(client, d.takodSocket(), "DELETE", takodclient.ProxyFileEndpoint(d.takodProxyConfigFileName()), nil); err != nil {
		return err
	}
	return nil
}

func (d *Deployer) takodProxyConfigFileName() string {
	return runtimeid.ProxyConfigFileName(d.config.Project.Name, d.environment)
}

func (d *Deployer) meshUpstreamURL(serverName string, serviceName string, slot int, servicePort int) (string, error) {
	return d.meshUpstreamURLForRevision(serverName, serviceName, "", slot, servicePort)
}

func (d *Deployer) meshUpstreamURLForRevision(serverName string, serviceName string, revision string, slot int, servicePort int) (string, error) {
	hostIP, err := d.meshHostIPForServer(serverName)
	if err != nil {
		return "", err
	}
	port, err := d.meshUpstreamPortForServerRevision(serverName, serviceName, revision, slot, servicePort)
	if err != nil {
		return "", err
	}
	return "http://" + net.JoinHostPort(hostIP, strconv.Itoa(port)), nil
}

func (d *Deployer) meshUpstreamPortForServer(serverName string, serviceName string, slot int, containerPort int) (int, error) {
	return d.meshUpstreamPortForServerRevision(serverName, serviceName, "", slot, containerPort)
}

func (d *Deployer) meshUpstreamPortForServerRevision(serverName string, serviceName string, revision string, slot int, containerPort int) (int, error) {
	if d.meshPortAllocator != nil {
		return d.meshPortAllocator(serverName, serviceName, revision, slot, containerPort)
	}
	client, err := d.getRuntimeClient(serverName)
	if err != nil {
		return 0, err
	}
	return d.allocateMeshUpstreamPort(client, serverName, serviceName, revision, slot, containerPort)
}

func (d *Deployer) allocateMeshUpstreamPort(client any, serverName string, serviceName string, revision string, slot int, containerPort int) (int, error) {
	key := meshUpstreamPortKey{ServerName: serverName, ServiceName: serviceName, Revision: revision, Slot: slot, ContainerPort: containerPort}
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
		Revision:      revision,
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
	d.meshPortCacheMu.Lock()
	if d.meshPortEvidence == nil {
		d.meshPortEvidence = make(map[meshUpstreamPortKey]takod.PortAllocationResponse)
	}
	d.meshPortEvidence[key] = response
	d.meshPortCacheMu.Unlock()
	return response.HostPort, nil
}

func meshUpstreamRevisionForStrategy(revision string, strategy string) string {
	if deployStrategyUsesRevisionScopedContainers(strategy) {
		return strings.TrimSpace(revision)
	}
	return ""
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
	if server, ok := d.config.Servers[serverName]; ok && strings.TrimSpace(server.MeshIP) != "" {
		if net.ParseIP(strings.TrimSpace(server.MeshIP)) == nil {
			return "", fmt.Errorf("invalid platform mesh IP for %s", serverName)
		}
		return strings.TrimSpace(server.MeshIP), nil
	}
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
