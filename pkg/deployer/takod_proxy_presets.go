package deployer

import (
	"context"
	"fmt"
	"sort"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/proxypresets"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
)

// trustedProxyPresetResolver resolves one CDN preset to a validated snapshot.
// *proxypresets.Resolver satisfies it; tests inject fakes.
type trustedProxyPresetResolver interface {
	Resolve(ctx context.Context, name string) (proxypresets.Snapshot, error)
}

// SetTrustedProxyPresetResolver overrides how trusted-proxy presets are
// resolved (tests, alternative caches).
func (d *Deployer) SetTrustedProxyPresetResolver(resolver trustedProxyPresetResolver) {
	d.presetMu.Lock()
	d.presetResolver = resolver
	d.presetMu.Unlock()
}

func (d *Deployer) trustedProxyPresetResolver() trustedProxyPresetResolver {
	if d.presetResolver != nil {
		return d.presetResolver
	}
	version := d.cliVersion
	if version == "" {
		version = "unknown"
	}
	d.presetResolver = &proxypresets.Resolver{
		UserAgent: "tako-cli/" + version,
		Warn: func(message string) {
			d.emitEvent(events.Event{
				Type:    events.TypeWarning,
				Phase:   events.PhaseDeploy,
				Level:   events.LevelWarn,
				Message: fmt.Sprintf("  ⚠ %s\n", message),
				Data:    map[string]any{"reason": "trusted-proxy-preset"},
			})
		},
	}
	return d.presetResolver
}

// proxyServicesTrustedProxyPresets returns the sorted preset names used by
// proxied services.
func proxyServicesTrustedProxyPresets(services map[string]config.ServiceConfig) []string {
	seen := make(map[string]bool)
	for _, service := range services {
		if !service.IsProxied() || service.Proxy == nil {
			continue
		}
		for _, name := range service.Proxy.TrustedProxyPresets() {
			seen[name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// resolveTrustedProxyPresets resolves every preset the services declare and
// memoizes the snapshots for this operation. It runs inside proxy preflight,
// before any mutation, so a resolution failure stops the operation cleanly.
func (d *Deployer) resolveTrustedProxyPresets(services map[string]config.ServiceConfig) error {
	names := proxyServicesTrustedProxyPresets(services)
	if len(names) == 0 {
		return nil
	}
	d.presetMu.Lock()
	defer d.presetMu.Unlock()
	resolver := d.trustedProxyPresetResolver()
	for _, name := range names {
		if _, ok := d.presetSnapshots[name]; ok {
			continue
		}
		snapshot, err := resolver.Resolve(d.baseContext(), name)
		if err != nil {
			return fmt.Errorf("proxy preflight: %w", err)
		}
		if d.presetSnapshots == nil {
			d.presetSnapshots = make(map[string]proxypresets.Snapshot)
		}
		d.presetSnapshots[name] = snapshot
	}
	return nil
}

// trustedProxyRouteFields splits a service's trusted proxies into explicit
// CIDRs and memoized preset snapshots. Rendering a preset without a memoized
// snapshot is an internal error, never a manifest that silently drops it.
func (d *Deployer) trustedProxyRouteFields(serviceName string, proxy *config.ProxyConfig) ([]string, []takod.ProxyTrustedProxyPreset, error) {
	if proxy == nil {
		return nil, nil, nil
	}
	cidrs, names := config.SplitTrustedProxies(proxy.TrustedProxies)
	if len(names) == 0 {
		return cidrs, nil, nil
	}
	d.presetMu.Lock()
	defer d.presetMu.Unlock()
	presets := make([]takod.ProxyTrustedProxyPreset, 0, len(names))
	for _, name := range names {
		snapshot, ok := d.presetSnapshots[name]
		if !ok {
			return nil, nil, fmt.Errorf("internal error: trusted proxy preset %s for service %s was not resolved in proxy preflight", name, serviceName)
		}
		presets = append(presets, takod.ProxyTrustedProxyPreset{
			Name:              snapshot.Name,
			FetchedAt:         snapshot.FetchedAt.UTC(),
			SourceFingerprint: snapshot.SourceFingerprint,
			CIDRs:             append([]string(nil), snapshot.CIDRs...),
		})
	}
	return cidrs, presets, nil
}
