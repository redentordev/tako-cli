package takod

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

const proxyRouteManifestVersion = 2

const (
	proxyRouteVisibilityPublic   = "public"
	proxyRouteVisibilityInternal = "internal"
)

var (
	proxyCaddyfilePath  = "/etc/tako/proxy/caddy/Caddyfile"
	proxyCaddyDataDir   = "/etc/tako/proxy/caddy-data"
	proxyCaddyConfigDir = "/etc/tako/proxy/caddy-config"
	proxyLogDir         = "/var/log/tako/proxy"
	activeProxyPolicy   struct {
		sync.RWMutex
		clusterID     string
		inventoryPath string
	}
)

func activateEnrolledProxyPolicy(clusterID string, inventoryPath string) func() {
	activeProxyPolicy.Lock()
	activeProxyPolicy.clusterID = clusterID
	activeProxyPolicy.inventoryPath = inventoryPath
	activeProxyPolicy.Unlock()
	return func() {
		activeProxyPolicy.Lock()
		if activeProxyPolicy.clusterID == clusterID && activeProxyPolicy.inventoryPath == inventoryPath {
			activeProxyPolicy.clusterID = ""
			activeProxyPolicy.inventoryPath = ""
		}
		activeProxyPolicy.Unlock()
	}
}

func validateActiveProxyPolicy(content string) error {
	activeProxyPolicy.RLock()
	clusterID, inventoryPath := activeProxyPolicy.clusterID, activeProxyPolicy.inventoryPath
	activeProxyPolicy.RUnlock()
	if clusterID == "" {
		return nil
	}
	return validateEnrolledProxyRouteManifest(content, clusterID, inventoryPath)
}

type ProxyRouteManifest struct {
	Version     int          `json:"version"`
	Project     string       `json:"project"`
	Environment string       `json:"environment"`
	Network     string       `json:"network,omitempty"`
	ClusterID   string       `json:"clusterId,omitempty"`
	Routes      []ProxyRoute `json:"routes"`
}

type ProxyRoute struct {
	Service        string               `json:"service"`
	Revision       string               `json:"revision,omitempty"`
	Domains        []string             `json:"domains,omitempty"`
	RedirectFrom   []string             `json:"redirectFrom,omitempty"`
	Upstreams      []string             `json:"upstreams"`
	HealthCheck    *ProxyRouteHealth    `json:"healthCheck,omitempty"`
	Sticky         bool                 `json:"sticky,omitempty"`
	Priority       int                  `json:"priority,omitempty"`
	Visibility     string               `json:"visibility,omitempty"`
	DynamicDomain  *ProxyDynamicDomain  `json:"dynamicDomain,omitempty"`
	BasicAuth      *ProxyRouteBasicAuth `json:"basicAuth,omitempty"`
	AllowIPs       []string             `json:"allowIps,omitempty"`
	TrustedProxies []string             `json:"trustedProxies,omitempty"`
	Destinations   []ProxyDestination   `json:"destinations,omitempty"`
}

// ProxyRouteBasicAuth protects a route's serving domains with HTTP basic
// auth. PasswordBcrypt is a pre-computed hash, never plaintext.
type ProxyRouteBasicAuth struct {
	Username       string `json:"username"`
	PasswordBcrypt string `json:"passwordBcrypt"`
}

type ProxyRouteHealth struct {
	Path     string `json:"path,omitempty"`
	Interval string `json:"interval,omitempty"`
}

type ProxyDynamicDomain struct {
	AskURL      string            `json:"askUrl"`
	Destination *ProxyDestination `json:"destination,omitempty"`
}

const (
	ProxyDestinationRuntimeAlias = "runtime-alias"
	ProxyDestinationMesh         = "mesh-allocation"
)

// ProxyDestination binds one rendered URL to the workload identity that was
// authorized by the controller. Raw URLs without this evidence are rejected
// by enrolled nodes.
type ProxyDestination struct {
	Kind          string    `json:"kind"`
	URL           string    `json:"url"`
	Project       string    `json:"project"`
	Environment   string    `json:"environment"`
	Service       string    `json:"service"`
	Revision      string    `json:"revision,omitempty"`
	Slot          int       `json:"slot"`
	ClusterID     string    `json:"clusterId,omitempty"`
	NodeID        string    `json:"nodeId,omitempty"`
	AllocationKey string    `json:"allocationKey,omitempty"`
	Generation    uint64    `json:"generation,omitempty"`
	IssuedAt      time.Time `json:"issuedAt,omitempty"`
	Signature     string    `json:"signature,omitempty"`
	ContainerPort int       `json:"containerPort"`
	HostPort      int       `json:"hostPort"`
	HostIP        string    `json:"hostIp,omitempty"`
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

func validateEnrolledProxyRouteManifest(content string, clusterID string, inventoryPath string) error {
	manifest, err := ParseProxyRouteManifest(content)
	if err != nil {
		return err
	}
	if manifest.Version != proxyRouteManifestVersion {
		return fmt.Errorf("enrolled nodes require proxy route manifest version %d destination proofs", proxyRouteManifestVersion)
	}
	if manifest.ClusterID != clusterID {
		return fmt.Errorf("proxy route manifest cluster identity does not match this enrolled node")
	}
	inventory, err := nodeidentity.ReadInventory(inventoryPath)
	if err != nil {
		return fmt.Errorf("authoritative allocation inventory is unavailable: %w", err)
	}
	if inventory.ClusterID != clusterID {
		return fmt.Errorf("authoritative allocation inventory belongs to another cluster")
	}
	for _, route := range manifest.Routes {
		destinations := append([]ProxyDestination(nil), route.Destinations...)
		if route.DynamicDomain != nil && route.DynamicDomain.Destination != nil {
			destinations = append(destinations, *route.DynamicDomain.Destination)
		}
		for _, proof := range destinations {
			if proof.Kind != ProxyDestinationMesh {
				continue
			}
			if err := validateAuthorizedRemoteAllocation(inventory, proof); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateProxyRouteManifest(manifest *ProxyRouteManifest) error {
	if manifest.Version == 0 {
		manifest.Version = proxyRouteManifestVersion
	}
	if manifest.Version != 1 && manifest.Version != proxyRouteManifestVersion {
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
		if route.Visibility == "" {
			route.Visibility = proxyRouteVisibilityPublic
		}
		switch route.Visibility {
		case proxyRouteVisibilityPublic, proxyRouteVisibilityInternal:
		default:
			return fmt.Errorf("route %s: invalid visibility %q", route.Service, route.Visibility)
		}
		if route.Visibility == proxyRouteVisibilityInternal {
			if route.DynamicDomain != nil {
				return fmt.Errorf("route %s: internal routes do not support dynamic domains", route.Service)
			}
			if len(route.RedirectFrom) > 0 {
				return fmt.Errorf("route %s: internal routes do not support redirects", route.Service)
			}
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
		if manifest.Version >= 2 {
			if len(route.Destinations) != len(route.Upstreams) {
				return fmt.Errorf("route %s: every upstream requires destination identity proof", route.Service)
			}
			for index := range route.Destinations {
				if route.Destinations[index].URL != route.Upstreams[index] {
					return fmt.Errorf("route %s: destination proof order does not match upstreams", route.Service)
				}
				if err := validateProxyDestination(*manifest, *route, route.Destinations[index], false); err != nil {
					return fmt.Errorf("route %s: invalid destination proof: %w", route.Service, err)
				}
			}
		}
		if route.HealthCheck != nil && route.HealthCheck.Path != "" && !isSafeHTTPPath(route.HealthCheck.Path) {
			return fmt.Errorf("route %s: invalid health check path", route.Service)
		}
		if route.DynamicDomain != nil {
			if err := validateProxyUpstreamURL(route.DynamicDomain.AskURL); err != nil {
				return fmt.Errorf("route %s: invalid dynamic ask URL: %w", route.Service, err)
			}
			if manifest.Version >= 2 {
				if route.DynamicDomain.Destination == nil || route.DynamicDomain.Destination.URL != route.DynamicDomain.AskURL {
					return fmt.Errorf("route %s: dynamic ask URL requires destination identity proof", route.Service)
				}
				if err := validateProxyDestination(*manifest, *route, *route.DynamicDomain.Destination, true); err != nil {
					return fmt.Errorf("route %s: invalid dynamic ask destination: %w", route.Service, err)
				}
			}
		}
		if route.BasicAuth != nil {
			if !isSafeProxyBasicAuthUser(route.BasicAuth.Username) {
				return fmt.Errorf("route %s: invalid basic auth username", route.Service)
			}
			if !isSafeProxyBcryptHash(route.BasicAuth.PasswordBcrypt) {
				return fmt.Errorf("route %s: basic auth password must be a bcrypt hash", route.Service)
			}
		}
		for _, entry := range route.AllowIPs {
			if !isSafeProxyAllowIP(entry) {
				return fmt.Errorf("route %s: invalid allow IP entry %q", route.Service, entry)
			}
		}
		for _, entry := range route.TrustedProxies {
			if !isSafeTrustedProxyPrefix(entry) {
				return fmt.Errorf("route %s: invalid trusted proxy CIDR %q", route.Service, entry)
			}
		}
	}
	return nil
}

func validateProxyDestination(manifest ProxyRouteManifest, route ProxyRoute, proof ProxyDestination, allowPath bool) error {
	parsed, err := url.Parse(proof.URL)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Hostname() == "" || parsed.Fragment != "" || parsed.RawQuery != "" {
		return fmt.Errorf("destination URL must be plain HTTP without userinfo, query, or fragment")
	}
	if (!allowPath && parsed.Path != "") || (allowPath && !isSafeHTTPPath(parsed.Path)) {
		return fmt.Errorf("destination URL path is invalid")
	}
	if proof.Project != manifest.Project || proof.Environment != manifest.Environment || !isSafeServiceName(proof.Service) || proof.Slot < 1 || proof.Slot > 10000 || proof.ContainerPort < 1 || proof.ContainerPort > 65535 {
		return fmt.Errorf("destination workload identity does not match the manifest")
	}
	if proof.Revision != "" && !isSafeRuntimeName(proof.Revision) {
		return fmt.Errorf("destination revision is invalid")
	}
	if !allowPath && (proof.Service != route.Service || proof.Revision != route.Revision) {
		return fmt.Errorf("destination service/revision does not match its route")
	}
	port := parsed.Port()
	if proof.HostPort < 1 || proof.HostPort > 65535 || port == "" || port != fmt.Sprint(proof.HostPort) {
		return fmt.Errorf("destination port does not match its workload proof")
	}
	host := parsed.Hostname()
	switch proof.Kind {
	case ProxyDestinationRuntimeAlias:
		if proof.HostPort != proof.ContainerPort {
			return fmt.Errorf("runtime alias proof must use the container port directly")
		}
		if net.ParseIP(host) != nil || proof.Service != route.Service && !allowPath {
			return fmt.Errorf("runtime alias proof cannot authorize this host")
		}
		expected := runtimeid.ContainerAlias(manifest.Project, manifest.Environment, proof.Service, proof.Slot)
		if proof.Revision != "" {
			expected = runtimeid.RevisionContainerAlias(manifest.Project, manifest.Environment, proof.Service, proof.Revision, proof.Slot)
		}
		if host != expected {
			return fmt.Errorf("runtime alias %q does not match workload identity", host)
		}
		if proof.AllocationKey != "" || proof.ClusterID != "" || proof.NodeID != "" || proof.HostIP != "" || proof.Generation != 0 || !proof.IssuedAt.IsZero() || proof.Signature != "" {
			return fmt.Errorf("runtime alias proof carries unexpected mesh evidence")
		}
	case ProxyDestinationMesh:
		ip, err := netip.ParseAddr(host)
		if err != nil || !safeProxyMeshAddress(ip) {
			return fmt.Errorf("mesh destination must use a non-special IP address")
		}
		if proof.HostIP != host {
			return fmt.Errorf("mesh destination IP does not match its allocation proof")
		}
		if err := nodeidentity.ValidateClusterID(proof.ClusterID); err != nil || proof.ClusterID != manifest.ClusterID {
			return fmt.Errorf("mesh destination cluster identity is invalid")
		}
		if err := nodeidentity.ValidateNodeID(proof.NodeID); err != nil {
			return fmt.Errorf("mesh destination node identity is invalid")
		}
		key := portAllocationKey(PortAllocationKindMeshUpstream, proof.Project, proof.Environment, proof.Service, proof.Revision, proof.Slot)
		if proof.AllocationKey != key {
			return fmt.Errorf("mesh destination allocation identity is invalid")
		}
		if strings.TrimSpace(proof.Signature) == "" {
			return fmt.Errorf("mesh destination allocation signature is required")
		}
		if proof.Generation == 0 || proof.IssuedAt.IsZero() {
			return fmt.Errorf("mesh destination allocation generation is required")
		}
	default:
		return fmt.Errorf("destination proof kind %q is unsupported", proof.Kind)
	}
	return nil
}

func safeProxyMeshAddress(address netip.Addr) bool {
	if !address.IsValid() || address.IsUnspecified() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast() {
		return false
	}
	for _, blocked := range []string{"169.254.169.254", "100.100.100.200", "fd00:ec2::254"} {
		if address == netip.MustParseAddr(blocked) {
			return false
		}
	}
	return true
}

func renderCaddyfileFromRouteManifests(dir string) (string, error) {
	return renderCaddyfileFromRouteManifestsExcluding(dir, "")
}

func renderCaddyfileFromRouteManifestsExcluding(dir string, excludedCertificateDomain string) (string, error) {
	manifests, err := readProxyRouteManifests(dir)
	if err != nil {
		return "", err
	}
	certificates, err := loadProxyCertificateEntriesExcluding(true, excludedCertificateDomain)
	if err != nil {
		return "", err
	}
	owners, err := loadACMEDNSOwnerClaims()
	if err != nil {
		return "", err
	}
	return renderCaddyfileWithCertificatesAndOwners(manifests, certificates, owners)
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
		if err := validateActiveProxyPolicy(string(data)); err != nil {
			return nil, fmt.Errorf("proxy route manifest %s violates enrolled-node policy: %w", entry.Name(), err)
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
	return renderCaddyfileWithCertificates(manifests, nil)
}

func renderCaddyfileWithCertificates(manifests []ProxyRouteManifest, certificates []proxyCertificateEntry) (string, error) {
	return renderCaddyfileWithCertificatesAndOwners(manifests, certificates, nil)
}

func renderCaddyfileWithCertificatesAndOwners(manifests []ProxyRouteManifest, certificates []proxyCertificateEntry, owners []acmeDNSOwnerClaim) (string, error) {
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
	trustedProxies, err := proxyTrustedProxySet(effectiveRoutes)
	if err != nil {
		return "", err
	}
	if len(trustedProxies) > 0 {
		b.WriteString("\tservers {\n")
		b.WriteString("\t\ttrusted_proxies static " + strings.Join(trustedProxies, " ") + "\n")
		b.WriteString("\t\ttrusted_proxies_strict\n")
		b.WriteString("\t}\n")
	}
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
			var certificate *proxyCertificateEntry
			if route.Visibility != proxyRouteVisibilityInternal {
				certificate = selectProxyCertificate(certificates, domain)
			}
			if certificate == nil {
				if owner := selectACMEDNSOwner(owners, domain); owner != nil {
					return "", fmt.Errorf("domain %s is covered by ACME DNS certificate %s owned by %s/%s, but no valid certificate is stored; deploy the owning configuration first", domain, owner.Domain, owner.Project, owner.Environment)
				}
			}
			writeCaddyRoute(&b, caddyRouteAddress(domain, route), route, certificate)
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
			if certificate := selectProxyCertificate(certificates, redirect); certificate != nil {
				writeCaddyCertificate(&b, "\t", certificate)
			} else if owner := selectACMEDNSOwner(owners, redirect); owner != nil {
				return "", fmt.Errorf("redirect domain %s is covered by ACME DNS certificate %s owned by %s/%s, but no valid certificate is stored; deploy the owning configuration first", redirect, owner.Domain, owner.Project, owner.Environment)
			}
			writeCaddyAccessLog(&b, caddyAccessLogName(route.Service))
			b.WriteString("\tredir https://" + primary + "{uri} 308\n")
			b.WriteString("}\n")
		}
	}

	if dynamicRoute != nil {
		writeCaddyRoute(&b, ":443", *dynamicRoute, nil)
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

func writeCaddyRoute(b *strings.Builder, address string, route ProxyRoute, certificate *proxyCertificateEntry) {
	b.WriteString("\n" + address + " {\n")
	if address == ":443" {
		b.WriteString("\ttls {\n\t\ton_demand\n\t}\n")
	} else if certificate != nil {
		writeCaddyCertificate(b, "\t", certificate)
	}
	writeCaddyAccessLog(b, caddyAccessLogName(route.Service))
	b.WriteString("\tencode zstd gzip\n")
	if len(route.AllowIPs) > 0 {
		// handle blocks force the allowlist to win before basic_auth:
		// Caddy's default directive order would otherwise run basic_auth
		// ahead of a bare respond matcher and 401 denied addresses.
		matcher := "remote_ip"
		if len(route.TrustedProxies) > 0 {
			matcher = "client_ip"
		}
		b.WriteString("\t@tako_allowed " + matcher + " " + strings.Join(route.AllowIPs, " ") + "\n")
		b.WriteString("\thandle @tako_allowed {\n")
		writeCaddyBasicAuth(b, "\t\t", route)
		writeCaddyReverseProxy(b, "\t\t", address, route)
		b.WriteString("\t}\n")
		b.WriteString("\thandle {\n\t\trespond 403\n\t}\n")
	} else {
		writeCaddyBasicAuth(b, "\t", route)
		writeCaddyReverseProxy(b, "\t", address, route)
	}
	b.WriteString("}\n")
}

func writeCaddyCertificate(b *strings.Builder, indent string, certificate *proxyCertificateEntry) {
	b.WriteString(indent + "tls " + certificate.CertPath + " " + certificate.KeyPath + "\n")
}

func writeCaddyBasicAuth(b *strings.Builder, indent string, route ProxyRoute) {
	if route.BasicAuth == nil {
		return
	}
	b.WriteString(indent + "basic_auth {\n")
	b.WriteString(indent + "\t" + route.BasicAuth.Username + " " + route.BasicAuth.PasswordBcrypt + "\n")
	b.WriteString(indent + "}\n")
}

func writeCaddyReverseProxy(b *strings.Builder, indent string, address string, route ProxyRoute) {
	b.WriteString(indent + "reverse_proxy")
	for _, upstream := range route.Upstreams {
		b.WriteString(" " + upstream)
	}
	if route.HealthCheck == nil && !route.Sticky {
		b.WriteString("\n")
		return
	}
	b.WriteString(" {\n")
	if route.HealthCheck != nil && route.HealthCheck.Path != "" {
		b.WriteString(indent + "\thealth_uri " + route.HealthCheck.Path + "\n")
		if route.HealthCheck.Interval != "" {
			b.WriteString(indent + "\thealth_interval " + route.HealthCheck.Interval + "\n")
		}
		if host := caddyHealthHost(address, route); host != "" {
			b.WriteString(indent + "\thealth_headers {\n")
			b.WriteString(indent + "\t\tHost " + host + "\n")
			b.WriteString(indent + "\t}\n")
		}
	}
	if route.Sticky {
		b.WriteString(indent + "\tlb_policy cookie\n")
	}
	b.WriteString(indent + "}\n")
}

func caddyHealthHost(address string, route ProxyRoute) string {
	if address != ":443" {
		return strings.TrimPrefix(strings.TrimPrefix(address, "http://"), "https://")
	}
	if len(route.Domains) == 0 {
		return ""
	}
	return route.Domains[0]
}

func caddyRouteAddress(host string, route ProxyRoute) string {
	if route.Visibility == proxyRouteVisibilityInternal {
		return "http://" + host
	}
	return host
}

func caddyAccessLogName(service string) string {
	if service == "" {
		return "tako_proxy"
	}
	return "tako_" + service
}

var proxyBcryptHashPattern = regexp.MustCompile(`^\$2[aby]\$[0-9]{2}\$[./A-Za-z0-9]{53}$`)

func isSafeProxyBasicAuthUser(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '@' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func isSafeProxyBcryptHash(value string) bool {
	return proxyBcryptHashPattern.MatchString(value)
}

func isSafeProxyAllowIP(value string) bool {
	if strings.TrimSpace(value) != value || value == "" {
		return false
	}
	if strings.Contains(value, "/") {
		_, _, err := net.ParseCIDR(value)
		return err == nil
	}
	return net.ParseIP(value) != nil
}

func isSafeTrustedProxyPrefix(value string) bool {
	if strings.TrimSpace(value) != value || value == "" {
		return false
	}
	prefix, err := netip.ParsePrefix(value)
	if err != nil || prefix != prefix.Masked() {
		return false
	}
	if prefix.Addr().Is4() {
		return prefix.Bits() >= 8
	}
	return prefix.Bits() >= 24
}

func proxyTrustedProxySet(routes []ProxyRoute) ([]string, error) {
	var selected []string
	selectedService := ""
	for _, route := range routes {
		if len(route.TrustedProxies) == 0 {
			continue
		}
		seen := make(map[string]struct{}, len(route.TrustedProxies))
		for _, prefix := range route.TrustedProxies {
			seen[prefix] = struct{}{}
		}
		current := make([]string, 0, len(seen))
		for prefix := range seen {
			current = append(current, prefix)
		}
		sort.Strings(current)
		if selected == nil {
			selected = current
			selectedService = route.Service
			continue
		}
		if strings.Join(current, "\x00") != strings.Join(selected, "\x00") {
			return nil, fmt.Errorf("proxy routes %s and %s declare conflicting trusted proxy CIDR sets; all routes sharing a node must use the same nonempty proxy.trustedProxies set", selectedService, route.Service)
		}
	}
	return selected, nil
}

func isSafeProxyHost(value string) bool {
	if strings.TrimSpace(value) != value || value == "" {
		return false
	}
	if strings.HasPrefix(value, "*.") {
		base := strings.TrimPrefix(value, "*.")
		return !strings.Contains(base, "*") && isSafeRuntimeHost(base)
	}
	if strings.Contains(value, "*") {
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
