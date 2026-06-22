package takod

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

const proxyRouteManifestVersion = 1

var (
	proxyCaddyfilePath  = "/etc/tako/proxy/caddy/Caddyfile"
	proxyCaddyDataDir   = "/etc/tako/proxy/caddy-data"
	proxyCaddyConfigDir = "/etc/tako/proxy/caddy-config"
	proxyLogDir         = "/var/log/tako/proxy"
)

type ProxyRouteManifest struct {
	Version     int          `json:"version"`
	Project     string       `json:"project"`
	Environment string       `json:"environment"`
	Network     string       `json:"network,omitempty"`
	Routes      []ProxyRoute `json:"routes"`
}

type ProxyRoute struct {
	Service       string              `json:"service"`
	Revision      string              `json:"revision,omitempty"`
	Domains       []string            `json:"domains,omitempty"`
	RedirectFrom  []string            `json:"redirectFrom,omitempty"`
	Upstreams     []string            `json:"upstreams"`
	HealthCheck   *ProxyRouteHealth   `json:"healthCheck,omitempty"`
	Sticky        bool                `json:"sticky,omitempty"`
	Priority      int                 `json:"priority,omitempty"`
	DynamicDomain *ProxyDynamicDomain `json:"dynamicDomain,omitempty"`
}

type ProxyRouteHealth struct {
	Path     string `json:"path,omitempty"`
	Interval string `json:"interval,omitempty"`
}

type ProxyDynamicDomain struct {
	AskURL string `json:"askUrl"`
}

func ParseProxyRouteManifest(content string) (*ProxyRouteManifest, error) {
	var manifest ProxyRouteManifest
	if err := json.Unmarshal([]byte(content), &manifest); err != nil {
		return nil, fmt.Errorf("invalid route manifest JSON: %w", err)
	}
	if err := validateProxyRouteManifest(&manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func validateProxyRouteManifest(manifest *ProxyRouteManifest) error {
	if manifest.Version == 0 {
		manifest.Version = proxyRouteManifestVersion
	}
	if manifest.Version != proxyRouteManifestVersion {
		return fmt.Errorf("unsupported route manifest version %d", manifest.Version)
	}
	if !isSafeProjectName(manifest.Project) {
		return fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(manifest.Environment) {
		return fmt.Errorf("invalid environment name")
	}
	expectedNetwork := runtimeid.NetworkName(manifest.Project, manifest.Environment)
	if manifest.Network == "" {
		manifest.Network = expectedNetwork
	}
	if manifest.Network != expectedNetwork {
		return fmt.Errorf("route manifest network %q does not match project/environment network %q", manifest.Network, expectedNetwork)
	}
	if !isSafeRuntimeName(manifest.Network) {
		return fmt.Errorf("invalid network name")
	}
	for i := range manifest.Routes {
		route := &manifest.Routes[i]
		if !isSafeRuntimeName(route.Service) {
			return fmt.Errorf("route %d: invalid service name", i)
		}
		if route.Revision != "" && !isSafeRuntimeName(route.Revision) {
			return fmt.Errorf("route %s: invalid revision", route.Service)
		}
		if len(route.Domains) == 0 && route.DynamicDomain == nil {
			return fmt.Errorf("route %s: at least one explicit or dynamic domain is required", route.Service)
		}
		if len(route.Upstreams) == 0 {
			return fmt.Errorf("route %s: at least one upstream is required", route.Service)
		}
		if route.Priority < 0 {
			return fmt.Errorf("route %s: priority cannot be negative", route.Service)
		}
		for _, domain := range append(append([]string{}, route.Domains...), route.RedirectFrom...) {
			if !isSafeProxyHost(domain) {
				return fmt.Errorf("route %s: invalid proxy domain %q", route.Service, domain)
			}
		}
		for _, upstream := range route.Upstreams {
			if err := validateProxyUpstreamURL(upstream); err != nil {
				return fmt.Errorf("route %s: invalid upstream %q: %w", route.Service, upstream, err)
			}
		}
		if route.HealthCheck != nil && route.HealthCheck.Path != "" && !isSafeHTTPPath(route.HealthCheck.Path) {
			return fmt.Errorf("route %s: invalid health check path", route.Service)
		}
		if route.DynamicDomain != nil {
			if err := validateProxyUpstreamURL(route.DynamicDomain.AskURL); err != nil {
				return fmt.Errorf("route %s: invalid dynamic ask URL: %w", route.Service, err)
			}
		}
	}
	return nil
}

func renderCaddyfileFromRouteManifests(dir string) (string, error) {
	manifests, err := readProxyRouteManifests(dir)
	if err != nil {
		return "", err
	}
	return renderCaddyfile(manifests)
}

func readProxyRouteManifests(dir string) ([]ProxyRouteManifest, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read proxy routes: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	manifests := make([]ProxyRouteManifest, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("failed to read proxy route manifest %s: %w", entry.Name(), err)
		}
		manifest, err := ParseProxyRouteManifest(string(data))
		if err != nil {
			return nil, fmt.Errorf("invalid proxy route manifest %s: %w", entry.Name(), err)
		}
		manifests = append(manifests, *manifest)
	}
	return manifests, nil
}

func renderCaddyfile(manifests []ProxyRouteManifest) (string, error) {
	var routes []ProxyRoute
	for _, manifest := range manifests {
		routes = append(routes, manifest.Routes...)
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Service == routes[j].Service {
			return strings.Join(routes[i].Domains, ",") < strings.Join(routes[j].Domains, ",")
		}
		return routes[i].Service < routes[j].Service
	})

	effectiveRoutes, err := resolveProxyRouteClaims(routes)
	if err != nil {
		return "", err
	}
	sortProxyRoutesForCaddy(effectiveRoutes)
	if len(effectiveRoutes) == 0 {
		return emptyProxyCaddyfile(), nil
	}

	var dynamicRoute *ProxyRoute
	for i := range effectiveRoutes {
		if effectiveRoutes[i].DynamicDomain == nil {
			continue
		}
		if dynamicRoute != nil {
			return "", fmt.Errorf("multiple dynamic domain authorities are not supported on one proxy node")
		}
		dynamicRoute = &effectiveRoutes[i]
	}

	var b strings.Builder
	b.WriteString("{\n")
	b.WriteString("\temail {$TAKO_PROXY_EMAIL}\n")
	if dynamicRoute != nil {
		b.WriteString("\ton_demand_tls {\n")
		b.WriteString("\t\task " + dynamicRoute.DynamicDomain.AskURL + "\n")
		b.WriteString("\t}\n")
	}
	b.WriteString("}\n\n")
	b.WriteString(":80 {\n")
	writeCaddyAccessLog(&b, "tako_proxy")
	b.WriteString("\tredir https://{host}{uri} 308\n}\n")

	for _, route := range effectiveRoutes {
		for _, domain := range route.Domains {
			writeCaddyRoute(&b, domain, route)
		}
		primary := ""
		if len(route.Domains) > 0 {
			primary = route.Domains[0]
		}
		for _, redirect := range route.RedirectFrom {
			if primary == "" {
				continue
			}
			b.WriteString("\n" + redirect + " {\n")
			writeCaddyAccessLog(&b, caddyAccessLogName(route.Service))
			b.WriteString("\tredir https://" + primary + "{uri} 308\n")
			b.WriteString("}\n")
		}
	}

	if dynamicRoute != nil {
		writeCaddyRoute(&b, ":443", *dynamicRoute)
	}
	return b.String(), nil
}

func emptyProxyCaddyfile() string {
	var b strings.Builder
	b.WriteString("{\n\temail {$TAKO_PROXY_EMAIL}\n}\n\n:80 {\n")
	writeCaddyAccessLog(&b, "tako_proxy")
	b.WriteString("\trespond \"tako-proxy has no routes\" 404\n}\n")
	return b.String()
}

func writeCaddyAccessLog(b *strings.Builder, name string) {
	b.WriteString("\tlog " + name + " {\n")
	b.WriteString("\t\toutput file /var/log/caddy/access.log {\n")
	b.WriteString("\t\t\troll_size 100MiB\n")
	b.WriteString("\t\t\troll_keep 5\n")
	b.WriteString("\t\t\troll_keep_for 720h\n")
	b.WriteString("\t\t}\n")
	b.WriteString("\t\tformat json\n")
	b.WriteString("\t}\n")
}

type proxyRouteHostClaim struct {
	routeIndex int
	priority   int
	redirect   bool
	service    string
}

func resolveProxyRouteClaims(routes []ProxyRoute) ([]ProxyRoute, error) {
	effective := make([]ProxyRoute, 0, len(routes))
	claims := make(map[string]proxyRouteHostClaim)

	for _, route := range routes {
		routeIndex := len(effective)
		copyRoute := route
		copyRoute.Domains = nil
		copyRoute.RedirectFrom = nil
		effective = append(effective, copyRoute)

		for _, domain := range route.Domains {
			if err := claimProxyRouteHost(effective, claims, domain, routeIndex, false, route); err != nil {
				return nil, err
			}
		}
		for _, domain := range route.RedirectFrom {
			if err := claimProxyRouteHost(effective, claims, domain, routeIndex, true, route); err != nil {
				return nil, err
			}
		}
	}

	filtered := effective[:0]
	for _, route := range effective {
		if len(route.Domains) == 0 && len(route.RedirectFrom) == 0 && route.DynamicDomain == nil {
			continue
		}
		if len(route.Domains) == 0 {
			route.RedirectFrom = nil
		}
		if len(route.Domains) == 0 && len(route.RedirectFrom) == 0 && route.DynamicDomain == nil {
			continue
		}
		filtered = append(filtered, route)
	}
	return filtered, nil
}

func sortProxyRoutesForCaddy(routes []ProxyRoute) {
	sort.SliceStable(routes, func(i, j int) bool {
		if routes[i].Priority != routes[j].Priority {
			return routes[i].Priority > routes[j].Priority
		}
		if routes[i].Service != routes[j].Service {
			return routes[i].Service < routes[j].Service
		}
		return strings.Join(routes[i].Domains, ",") < strings.Join(routes[j].Domains, ",")
	})
}

func claimProxyRouteHost(effective []ProxyRoute, claims map[string]proxyRouteHostClaim, host string, routeIndex int, redirect bool, route ProxyRoute) error {
	if existing, exists := claims[host]; exists {
		if existing.routeIndex == routeIndex {
			return fmt.Errorf("proxy domain %q is duplicated in route %s", host, route.Service)
		}
		if existing.priority == route.Priority {
			return fmt.Errorf("proxy domain %q is configured by both %s and %s", host, existing.service, route.Service)
		}
		if existing.priority > route.Priority {
			return nil
		}
		removeClaimedHost(&effective[existing.routeIndex], host, existing.redirect)
	}

	claims[host] = proxyRouteHostClaim{
		routeIndex: routeIndex,
		priority:   route.Priority,
		redirect:   redirect,
		service:    route.Service,
	}
	if redirect {
		effective[routeIndex].RedirectFrom = append(effective[routeIndex].RedirectFrom, host)
		return nil
	}
	effective[routeIndex].Domains = append(effective[routeIndex].Domains, host)
	return nil
}

func removeClaimedHost(route *ProxyRoute, host string, redirect bool) {
	if redirect {
		route.RedirectFrom = removeString(route.RedirectFrom, host)
		return
	}
	route.Domains = removeString(route.Domains, host)
}

func removeString(values []string, value string) []string {
	for i := 0; i < len(values); i++ {
		if values[i] != value {
			continue
		}
		copy(values[i:], values[i+1:])
		values = values[:len(values)-1]
		i--
	}
	return values
}

func writeCaddyRoute(b *strings.Builder, address string, route ProxyRoute) {
	b.WriteString("\n" + address + " {\n")
	if address == ":443" {
		b.WriteString("\ttls {\n\t\ton_demand\n\t}\n")
	}
	writeCaddyAccessLog(b, caddyAccessLogName(route.Service))
	b.WriteString("\tencode zstd gzip\n")
	b.WriteString("\treverse_proxy")
	for _, upstream := range route.Upstreams {
		b.WriteString(" " + upstream)
	}
	if route.HealthCheck == nil && !route.Sticky {
		b.WriteString("\n")
	} else {
		b.WriteString(" {\n")
		if route.HealthCheck != nil && route.HealthCheck.Path != "" {
			b.WriteString("\t\thealth_uri " + route.HealthCheck.Path + "\n")
			if route.HealthCheck.Interval != "" {
				b.WriteString("\t\thealth_interval " + route.HealthCheck.Interval + "\n")
			}
			if host := caddyHealthHost(address, route); host != "" {
				b.WriteString("\t\thealth_headers {\n")
				b.WriteString("\t\t\tHost " + host + "\n")
				b.WriteString("\t\t}\n")
			}
		}
		if route.Sticky {
			b.WriteString("\t\tlb_policy cookie\n")
		}
		b.WriteString("\t}\n")
	}
	b.WriteString("}\n")
}

func caddyHealthHost(address string, route ProxyRoute) string {
	if address != ":443" {
		return address
	}
	if len(route.Domains) == 0 {
		return ""
	}
	return route.Domains[0]
}

func caddyAccessLogName(service string) string {
	if service == "" {
		return "tako_proxy"
	}
	return "tako_" + service
}

func isSafeProxyHost(value string) bool {
	if strings.TrimSpace(value) != value || value == "" || strings.Contains(value, "*") {
		return false
	}
	return isSafeRuntimeHost(value)
}

func isSafeRuntimeHost(value string) bool {
	if len(value) > 253 || strings.ContainsAny(value, " \t\r\n`{}") {
		return false
	}
	labels := strings.Split(value, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return false
		}
		for i, r := range label {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
				if (i == 0 || i == len(label)-1) && r == '-' {
					return false
				}
				continue
			}
			return false
		}
	}
	return true
}

func validateProxyUpstreamURL(value string) error {
	if strings.ContainsAny(value, " \t\r\n`{}") {
		return fmt.Errorf("must not contain unsafe characters")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https")
	}
	if parsed.Host == "" {
		return fmt.Errorf("host is required")
	}
	return nil
}

func isSafeHTTPPath(value string) bool {
	if value == "" || !strings.HasPrefix(value, "/") {
		return false
	}
	return !strings.ContainsAny(value, " \t\r\n`{}")
}
