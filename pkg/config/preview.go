package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	// PreviewEnvironmentPrefix starts the environment identity of a preview.
	PreviewEnvironmentPrefix = "preview-"
	// PreviewTTLNone is the ttl value of a preview that never expires.
	PreviewTTLNone = "none"
	// DefaultPreviewTTL is the ttl of a preview whose base sets none.
	DefaultPreviewTTL = "168h"
	// DefaultPreviewMax is the number of previews a base may have.
	DefaultPreviewMax = 5
	// DefaultPreviewMaxReplicas caps the replicas of every preview service.
	DefaultPreviewMaxReplicas = 1
	// DefaultPreviewDomainTemplate renders the host of a proxied service.
	DefaultPreviewDomainTemplate = "{name}.{domain}"

	maxPreviewNameLength = 40
	previewNameCutLength = 33

	noPreviewBaseMessage = "no environment declares a preview: block; see docs/PREVIEW-ENVIRONMENTS.md"
)

// PreviewConfig is an environment's preview: block. It makes the
// environment a preview base; it is not inherited.
type PreviewConfig struct {
	// Domain is the host template of every proxied service. Placeholders:
	// {name}, {service}, {domain}. Default "{name}.{domain}".
	Domain string `yaml:"domain,omitempty" json:"domain,omitempty"`
	// TTL is a Go duration or "none"; default 168h.
	TTL string `yaml:"ttl,omitempty" json:"ttl,omitempty"`
	// Max is the number of previews this base may have; default 5.
	Max int `yaml:"max,omitempty" json:"max,omitempty"`
	// MaxReplicas caps the replicas of every non-global service; default 1.
	MaxReplicas int `yaml:"maxReplicas,omitempty" json:"maxReplicas,omitempty"`
	// KeepJobs keeps kind: job services, which are dropped by default.
	KeepJobs bool `yaml:"keepJobs,omitempty" json:"keepJobs,omitempty"`
	// Env is merged into every service. Placeholders: {name},
	// {host:<service>}, {url:<service>}.
	Env map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	// Services merges per-service env with the same placeholders.
	Services map[string]PreviewServiceConfig `yaml:"services,omitempty" json:"services,omitempty"`
}

// PreviewServiceConfig is preview.services.<name>.
type PreviewServiceConfig struct {
	Env map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
}

// EffectiveTTL returns the ttl string with the default applied.
func (p *PreviewConfig) EffectiveTTL() string {
	if p == nil || strings.TrimSpace(p.TTL) == "" {
		return DefaultPreviewTTL
	}
	return strings.TrimSpace(p.TTL)
}

// EffectiveMax returns max with the default applied.
func (p *PreviewConfig) EffectiveMax() int {
	if p == nil || p.Max <= 0 {
		return DefaultPreviewMax
	}
	return p.Max
}

// EffectiveMaxReplicas returns maxReplicas with the default applied.
func (p *PreviewConfig) EffectiveMaxReplicas() int {
	if p == nil || p.MaxReplicas <= 0 {
		return DefaultPreviewMaxReplicas
	}
	return p.MaxReplicas
}

func (p *PreviewConfig) domainTemplate() string {
	if p == nil || strings.TrimSpace(p.Domain) == "" {
		return DefaultPreviewDomainTemplate
	}
	return strings.TrimSpace(p.Domain)
}

// ParsePreviewTTL parses a ttl: a positive Go duration, or "none" for a
// preview that never expires (never is true and the duration zero).
func ParsePreviewTTL(value string) (duration time.Duration, never bool, err error) {
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, PreviewTTLNone) {
		return 0, true, nil
	}
	duration, err = time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, false, fmt.Errorf("ttl %q must be a positive duration such as 72h, or none", value)
	}
	return duration, false, nil
}

// PreviewInstance requests a preview from LoadConfigWithOptions. Name is
// the user-facing name (normalized on load); Base is the base environment,
// or empty to select the only environment that declares preview:.
type PreviewInstance struct {
	Base string
	Name string
}

// PreviewInstanceInfo describes the preview a config was loaded for.
type PreviewInstanceInfo struct {
	// Name is the normalized preview name, Identity the environment name.
	Name     string
	Identity string
	Base     string
	Policy   PreviewConfig
	// Hosts maps every proxied service to its preview host.
	Hosts map[string]string
	// URLs maps every proxied service to its preview URL.
	URLs map[string]string
}

// PreviewInstance returns the preview this config was loaded for, or nil.
func (c *Config) PreviewInstance() *PreviewInstanceInfo {
	if c == nil {
		return nil
	}
	return c.preview
}

// SecretsEnvironment returns the environment name used to find local
// secret files for env: the base for the loaded preview, env otherwise.
func (c *Config) SecretsEnvironment(env string) string {
	if c != nil && c.preview != nil && env == c.preview.Identity {
		return c.preview.Base
	}
	return env
}

// PreviewBases returns, sorted, the environments that declare preview:.
func (c *Config) PreviewBases() []string {
	if c == nil || c.resolution == nil {
		return nil
	}
	var out []string
	for name := range c.resolution.previews {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// PreviewPolicy returns the preview: block declared by env, or nil.
func (c *Config) PreviewPolicy(env string) *PreviewConfig {
	if c == nil || c.resolution == nil {
		return nil
	}
	return c.resolution.previews[env]
}

// DeclaredEnvironmentNames returns the deployable environments declared in
// the config, excluding a synthesized preview instance.
func (c *Config) DeclaredEnvironmentNames() []string {
	var out []string
	for name := range c.Environments {
		if c.preview != nil && name == c.preview.Identity {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// IsPreviewIdentity reports whether env has the preview identity form.
func IsPreviewIdentity(env string) bool {
	return strings.HasPrefix(env, PreviewEnvironmentPrefix) && len(env) > len(PreviewEnvironmentPrefix)
}

var previewNameSeparatorPattern = regexp.MustCompile(`[^a-z0-9]+`)

// NormalizePreviewName lowercases name, turns every run of characters
// outside [a-z0-9] into "-", trims "-", and shortens results longer than
// 40 characters to 33 characters plus "-" and 6 hex characters of the
// SHA-256 of the original input.
func NormalizePreviewName(name string) (string, error) {
	normalized := strings.Trim(previewNameSeparatorPattern.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if normalized == "" {
		return "", fmt.Errorf("preview name %q is empty after normalization; use letters and digits, for example pr-42", name)
	}
	if len(normalized) > maxPreviewNameLength {
		digest := sha256.Sum256([]byte(name))
		normalized = strings.TrimRight(normalized[:previewNameCutLength], "-") + "-" + hex.EncodeToString(digest[:])[:6]
	}
	return normalized, nil
}

// PreviewIdentity returns the environment identity of a normalized name.
func PreviewIdentity(name string) string {
	return PreviewEnvironmentPrefix + name
}

// ResolvePreviewBase selects the base of a preview (D3): explicit when
// given, which must be declared and declare preview:, otherwise the only
// preview base. bases lists the environments that declare preview: and
// declared every declared environment.
func ResolvePreviewBase(bases []string, declared []string, explicit string) (string, error) {
	explicit = strings.TrimSpace(explicit)
	if explicit != "" {
		for _, base := range bases {
			if base == explicit {
				return explicit, nil
			}
		}
		for _, name := range declared {
			if name == explicit {
				return "", fmt.Errorf("environment %s declares no preview: block; it cannot be the base of a preview", explicit)
			}
		}
		return "", fmt.Errorf("environment '%s' not found", explicit)
	}
	switch len(bases) {
	case 0:
		return "", fmt.Errorf("%s", noPreviewBaseMessage)
	case 1:
		return bases[0], nil
	}
	sorted := append([]string{}, bases...)
	sort.Strings(sorted)
	return "", fmt.Errorf("several environments declare a preview: block (%s); choose the base with -e", strings.Join(sorted, ", "))
}

// applyPreviewPolicy turns the synthesized preview environment into a safe
// preview (D4): drop jobs, reject shared host resources and storage, strip
// backup and monitoring, force automatic promotion, cap replicas, rewrite
// proxy hosts and inject preview env. It changes only the preview
// environment.
func applyPreviewPolicy(cfg *Config) error {
	if cfg.resolution == nil || cfg.resolution.instance == nil {
		return nil
	}
	instance := cfg.resolution.instance
	policy := cfg.resolution.previews[instance.base]
	if policy == nil {
		return fmt.Errorf("environment %s declares no preview: block; it cannot be the base of a preview", instance.base)
	}
	env, ok := cfg.Environments[instance.identity]
	if !ok {
		return fmt.Errorf("preview %s could not be resolved from %s", instance.name, instance.base)
	}
	services := make(map[string]ServiceConfig, len(env.Services))
	for name, service := range env.Services {
		services[name] = service
	}

	// 1. Services kept.
	if !policy.KeepJobs {
		for name, service := range services {
			if service.IsJob() {
				delete(services, name)
			}
		}
	}
	names := sortedServiceNames(services)

	// 2. Isolation checks.
	for _, name := range names {
		if err := checkPreviewServiceIsolation(cfg, name, services[name]); err != nil {
			return err
		}
	}

	maxReplicas := policy.EffectiveMaxReplicas()
	for _, name := range names {
		service := services[name]
		// 3. Strip schedules and alerts; previews promote automatically.
		service.Backup = nil
		service.Monitoring = nil
		if service.IsRun() || service.IsJob() {
			// Runs and jobs have no rollout, promotion or replicas.
			services[name] = service
			continue
		}
		if service.Deploy.Promotion != "" {
			service.Deploy.Promotion = DeployPromotionAutomatic
		}
		// 4. Replica cap; global placement runs one per node and is kept.
		if service.Placement == nil || service.Placement.Strategy != "global" {
			replicas := service.Replicas
			if replicas <= 0 {
				replicas = 1
			}
			if replicas > maxReplicas {
				replicas = maxReplicas
			}
			service.Replicas = replicas
		}
		services[name] = service
	}

	// 5. Proxy hosts.
	hosts, urls, err := renderPreviewHosts(cfg, instance, policy, services, names)
	if err != nil {
		return err
	}
	for _, name := range names {
		service := services[name]
		if service.Proxy == nil {
			continue
		}
		proxy := *service.Proxy
		if proxy.IsInternal() {
			proxy.Host = hosts[name]
		} else {
			proxy.Domain = hosts[name]
		}
		proxy.Domains = nil
		proxy.RedirectFrom = nil
		proxy.DynamicDomains = nil
		proxy.CDN = ""
		service.Proxy = &proxy
		services[name] = service
	}

	// 6. Env.
	expand := func(key string, value string) (string, error) {
		return expandPreviewEnvValue(value, instance.name, hosts, urls, key)
	}
	shared := map[string]string{}
	for key, value := range policy.Env {
		expanded, err := expand("preview.env."+key, value)
		if err != nil {
			return fmt.Errorf("environment %s: %w", instance.base, err)
		}
		shared[key] = expanded
	}
	for serviceName := range policy.Services {
		if _, ok := services[serviceName]; !ok {
			return fmt.Errorf("environment %s: preview.services.%s: service %s is not part of the preview", instance.base, serviceName, serviceName)
		}
	}
	for _, name := range names {
		service := services[name]
		merged := make(map[string]string, len(service.Env)+len(shared)+3)
		for key, value := range service.Env {
			merged[key] = value
		}
		merged["TAKO_PREVIEW"] = "true"
		merged["TAKO_PREVIEW_NAME"] = instance.name
		if url, ok := urls[name]; ok {
			merged["TAKO_PREVIEW_URL"] = url
		}
		for key, value := range shared {
			merged[key] = value
		}
		for key, value := range policy.Services[name].Env {
			expanded, err := expand("preview.services."+name+".env."+key, value)
			if err != nil {
				return fmt.Errorf("environment %s: %w", instance.base, err)
			}
			merged[key] = expanded
		}
		service.Env = merged
		services[name] = service
	}

	env.Services = services
	cfg.Environments[instance.identity] = env
	cfg.preview = &PreviewInstanceInfo{
		Name:     instance.name,
		Identity: instance.identity,
		Base:     instance.base,
		Policy:   *policy,
		Hosts:    hosts,
		URLs:     urls,
	}
	return nil
}

func sortedServiceNames(services map[string]ServiceConfig) []string {
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// previewIsolationFindings lists every way service can reach resources
// outside the preview's own namespace (D4 step 2).
func previewIsolationFindings(cfg *Config, name string, service ServiceConfig) []string {
	var findings []string
	if len(service.Ports) > 0 {
		findings = append(findings, fmt.Sprintf("service %s publishes host ports; remove it or set ports: null in the preview template", name))
	}
	if len(service.Imports) > 0 {
		findings = append(findings, fmt.Sprintf("service %s imports services of other projects; remove it or set imports: null in the preview template", name))
	}
	for _, mount := range service.Volumes {
		source := strings.SplitN(strings.TrimSpace(mount), ":", 2)[0]
		if source == "" {
			continue
		}
		if strings.HasPrefix(source, "/") || strings.HasPrefix(source, ".") || strings.HasPrefix(source, "~") {
			findings = append(findings, fmt.Sprintf("service %s bind-mounts host path %s; previews only get fresh volumes", name, source))
			continue
		}
		volume, ok := cfg.GetVolume(source)
		if !ok {
			continue
		}
		driver := strings.TrimSpace(volume.Driver)
		if volume.Name != "" || volume.External || (driver != "" && driver != "local") || len(volume.DriverOpts) > 0 {
			findings = append(findings, fmt.Sprintf("service %s mounts volume %s, which is shared across environments (name/external/driver); previews only get fresh volumes", name, source))
		}
	}
	return findings
}

func checkPreviewServiceIsolation(cfg *Config, name string, service ServiceConfig) error {
	if findings := previewIsolationFindings(cfg, name, service); len(findings) > 0 {
		return fmt.Errorf("%s", findings[0])
	}
	return nil
}

var previewPlaceholderPattern = regexp.MustCompile(`\{([^{}]*)\}`)

// renderPreviewHost expands a domain template for one service.
func renderPreviewHost(template string, name string, service string, domain string) (string, error) {
	var unknown string
	rendered := previewPlaceholderPattern.ReplaceAllStringFunc(template, func(match string) string {
		switch match {
		case "{name}":
			return name
		case "{service}":
			return service
		case "{domain}":
			return domain
		}
		if unknown == "" {
			unknown = match
		}
		return match
	})
	if unknown != "" {
		return "", fmt.Errorf("preview.domain: unknown placeholder %s (use {name}, {service} or {domain})", unknown)
	}
	if strings.Contains(template, "{domain}") && domain == "" {
		return "", fmt.Errorf("preview.domain: service %s has no domain or host to render {domain}", service)
	}
	rendered = strings.ToLower(strings.TrimSpace(rendered))
	if err := validatePreviewHostname(rendered); err != nil {
		return "", fmt.Errorf("preview.domain: service %s: %w", service, err)
	}
	return rendered, nil
}

func validatePreviewHostname(host string) error {
	if len(host) > 253 {
		return fmt.Errorf("host %s is longer than 253 characters", host)
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) > 63 {
			return fmt.Errorf("host %s has a label longer than 63 characters", host)
		}
	}
	if strings.Contains(host, "*") || !isValidDomain(host) {
		return fmt.Errorf("host %s is not a valid hostname", host)
	}
	return nil
}

// previewURL is the URL tako-proxy serves host at for proxy.
func previewURL(proxy *ProxyConfig, host string) string {
	if proxy.IsInternal() || strings.EqualFold(proxy.TLS.Mode, ProxyTLSModeOff) {
		return "http://" + host
	}
	return "https://" + host
}

// renderPreviewHosts renders one host per proxied service (D5) and checks
// uniqueness within the preview and against every declared environment.
func renderPreviewHosts(cfg *Config, instance *previewRequest, policy *PreviewConfig, services map[string]ServiceConfig, names []string) (map[string]string, map[string]string, error) {
	hosts := map[string]string{}
	urls := map[string]string{}
	usedBy := map[string]string{}
	declared := declaredHostOwners(cfg, instance.identity)
	for _, name := range names {
		service := services[name]
		if service.Proxy == nil {
			continue
		}
		host, err := renderPreviewHost(policy.domainTemplate(), instance.name, name, strings.ToLower(service.Proxy.GetPrimaryHost()))
		if err != nil {
			return nil, nil, fmt.Errorf("environment %s: %w", instance.base, err)
		}
		if other, ok := usedBy[host]; ok {
			return nil, nil, fmt.Errorf("preview %s: services %s and %s both render host %s; include {service} in preview.domain", instance.name, other, name, host)
		}
		if owner, ok := declared[host]; ok {
			return nil, nil, fmt.Errorf("preview %s: host %s of service %s is already served by environment %s; change preview.domain", instance.name, host, name, owner)
		}
		usedBy[host] = name
		hosts[name] = host
		urls[name] = previewURL(service.Proxy, host)
	}
	return hosts, urls, nil
}

// declaredHostOwners maps every host served by a declared environment to
// that environment.
func declaredHostOwners(cfg *Config, skip string) map[string]string {
	owners := map[string]string{}
	envNames := make([]string, 0, len(cfg.Environments))
	for name := range cfg.Environments {
		if name != skip {
			envNames = append(envNames, name)
		}
	}
	sort.Strings(envNames)
	for _, envName := range envNames {
		for _, service := range cfg.Environments[envName].Services {
			if service.Proxy == nil {
				continue
			}
			hosts := append([]string{}, service.Proxy.GetAllHosts()...)
			hosts = append(hosts, service.Proxy.RedirectFrom...)
			for _, host := range hosts {
				host = strings.ToLower(strings.TrimSpace(host))
				if _, ok := owners[host]; host != "" && !ok {
					owners[host] = envName
				}
			}
		}
	}
	return owners
}

// expandPreviewEnvValue expands {name}, {host:<service>} and
// {url:<service>} in one preview env value.
func expandPreviewEnvValue(value string, name string, hosts map[string]string, urls map[string]string, key string) (string, error) {
	var failure error
	expanded := previewPlaceholderPattern.ReplaceAllStringFunc(value, func(match string) string {
		inner := match[1 : len(match)-1]
		switch {
		case inner == "name":
			return name
		case strings.HasPrefix(inner, "host:"):
			if host, ok := hosts[strings.TrimPrefix(inner, "host:")]; ok {
				return host
			}
		case strings.HasPrefix(inner, "url:"):
			if url, ok := urls[strings.TrimPrefix(inner, "url:")]; ok {
				return url
			}
		default:
			if failure == nil {
				failure = fmt.Errorf("%s: unknown placeholder %s (use {name}, {host:<service>} or {url:<service>})", key, match)
			}
			return match
		}
		if failure == nil {
			target := strings.SplitN(inner, ":", 2)[1]
			failure = fmt.Errorf("%s: placeholder %s names %s, which is not a proxied service of the preview", key, match, target)
		}
		return match
	})
	return expanded, failure
}

// previewBaseServices returns the resolved services of a preview base.
func previewBaseServices(cfg *Config, base string) (map[string]ServiceConfig, bool) {
	if env, ok := cfg.Environments[base]; ok {
		return env.Services, true
	}
	if cfg.resolution != nil {
		if env := cfg.resolution.previewBases[base]; env != nil {
			return env.Services, true
		}
	}
	return nil, false
}

// validatePreviewBlocks statically validates every declared preview: block
// (D4): ttl, max and maxReplicas, placeholders and their service targets,
// and a sample host render for the name pr-1.
func validatePreviewBlocks(cfg *Config) error {
	if cfg.resolution == nil {
		return nil
	}
	bases := cfg.PreviewBases()
	for _, base := range bases {
		policy := cfg.resolution.previews[base]
		prefix := fmt.Sprintf("environment %s: preview", base)
		if strings.TrimSpace(policy.TTL) != "" {
			if _, _, err := ParsePreviewTTL(policy.TTL); err != nil {
				return fmt.Errorf("%s.ttl: %w", prefix, err)
			}
		}
		if policy.Max < 0 {
			return fmt.Errorf("%s.max must be at least 1", prefix)
		}
		if policy.MaxReplicas < 0 {
			return fmt.Errorf("%s.maxReplicas must be at least 1", prefix)
		}
		services, known := previewBaseServices(cfg, base)
		hosts := map[string]string{}
		urls := map[string]string{}
		for _, name := range sortedServiceNames(services) {
			service := services[name]
			if service.Proxy == nil {
				continue
			}
			if service.IsJob() && !policy.KeepJobs {
				continue
			}
			host, err := renderPreviewHost(policy.domainTemplate(), "pr-1", name, strings.ToLower(service.Proxy.GetPrimaryHost()))
			if err != nil {
				return fmt.Errorf("environment %s: %w", base, err)
			}
			hosts[name] = host
			urls[name] = previewURL(service.Proxy, host)
		}
		if !known {
			// Without resolved services only the placeholder syntax is checked.
			if _, err := renderPreviewHost(policy.domainTemplate(), "pr-1", "web", "example.com"); err != nil {
				return fmt.Errorf("environment %s: %w", base, err)
			}
		}
		check := func(key string, value string) error {
			if !known {
				for _, match := range previewPlaceholderPattern.FindAllString(value, -1) {
					inner := match[1 : len(match)-1]
					if inner != "name" && !strings.HasPrefix(inner, "host:") && !strings.HasPrefix(inner, "url:") {
						return fmt.Errorf("environment %s: %s: unknown placeholder %s (use {name}, {host:<service>} or {url:<service>})", base, key, match)
					}
				}
				return nil
			}
			if _, err := expandPreviewEnvValue(value, "pr-1", hosts, urls, key); err != nil {
				return fmt.Errorf("environment %s: %w", base, err)
			}
			return nil
		}
		for _, key := range sortedStringKeys(policy.Env) {
			if err := check("preview.env."+key, policy.Env[key]); err != nil {
				return err
			}
		}
		serviceNames := make([]string, 0, len(policy.Services))
		for name := range policy.Services {
			serviceNames = append(serviceNames, name)
		}
		sort.Strings(serviceNames)
		for _, serviceName := range serviceNames {
			if known {
				if _, ok := services[serviceName]; !ok {
					return fmt.Errorf("environment %s: preview.services.%s: service %s is not defined in the base", base, serviceName, serviceName)
				}
			}
			env := policy.Services[serviceName].Env
			for _, key := range sortedStringKeys(env) {
				if err := check("preview.services."+serviceName+".env."+key, env[key]); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func sortedStringKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// previewIsolationWarnings reports, per preview base, the isolation checks
// a preview instance of it would fail, so CI catches them before the first
// preview deploy.
func previewIsolationWarnings(cfg *Config) []ValidationWarning {
	var warnings []ValidationWarning
	for _, base := range cfg.PreviewBases() {
		policy := cfg.resolution.previews[base]
		services, ok := previewBaseServices(cfg, base)
		if !ok {
			continue
		}
		for _, name := range sortedServiceNames(services) {
			service := services[name]
			if service.IsJob() && !policy.KeepJobs {
				continue
			}
			for _, finding := range previewIsolationFindings(cfg, name, service) {
				warnings = append(warnings, ValidationWarning{
					Environment: base,
					Service:     name,
					Field:       "preview",
					Message:     fmt.Sprintf("environment %s: a preview would fail: %s", base, finding),
				})
			}
		}
	}
	return warnings
}
