package deployer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/proxypresets"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

type fakePresetResolver struct {
	calls     []string
	snapshots map[string]proxypresets.Snapshot
	err       error
}

func (f *fakePresetResolver) Resolve(_ context.Context, name string) (proxypresets.Snapshot, error) {
	f.calls = append(f.calls, name)
	if f.err != nil {
		return proxypresets.Snapshot{}, f.err
	}
	return f.snapshots[name], nil
}

var testPresetFetchedAt = time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)

func testCloudFrontSnapshot() proxypresets.Snapshot {
	return proxypresets.Snapshot{
		Name:              proxypresets.CloudFront,
		FetchedAt:         testPresetFetchedAt,
		SourceFingerprint: strings.Repeat("ab", 32),
		CIDRs:             []string{"13.124.199.0/24", "15.158.0.0/16"},
	}
}

func presetProxyServices(deploy *Deployer, trusted ...string) map[string]config.ServiceConfig {
	services := deploy.config.Environments["production"].Services
	web := services["web"]
	web.Proxy = &config.ProxyConfig{Domain: "example.com", TrustedProxies: trusted}
	services["web"] = web
	return services
}

func TestRenderTakodProxyCarriesTrustedProxyPresetProvenance(t *testing.T) {
	deploy := testProxyDeployer()
	resolver := &fakePresetResolver{snapshots: map[string]proxypresets.Snapshot{proxypresets.CloudFront: testCloudFrontSnapshot()}}
	deploy.SetTrustedProxyPresetResolver(resolver)
	services := presetProxyServices(deploy, "10.0.0.0/8", "cloudfront")

	if err := deploy.resolveTrustedProxyPresets(services); err != nil {
		t.Fatal(err)
	}
	// Memoized: a second preflight in the same operation does not refetch.
	if err := deploy.resolveTrustedProxyPresets(services); err != nil {
		t.Fatal(err)
	}
	if strings.Join(resolver.calls, ",") != "cloudfront" {
		t.Fatalf("resolver calls = %v", resolver.calls)
	}
	data, _, err := deploy.renderTakodProxyDynamicConfigForNode(services, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	manifest := parseProxyManifest(t, data)
	if manifest.Version != 1 && manifest.Version != 2 {
		t.Fatalf("presets must not bump the manifest version, got %d", manifest.Version)
	}
	route := onlyProxyRoute(t, manifest)
	assertStringsEqual(t, route.TrustedProxies, []string{"10.0.0.0/8"})
	if len(route.TrustedProxyPresets) != 1 {
		t.Fatalf("presets = %#v", route.TrustedProxyPresets)
	}
	preset := route.TrustedProxyPresets[0]
	if preset.Name != "cloudfront" || !preset.FetchedAt.Equal(testPresetFetchedAt) || preset.SourceFingerprint != strings.Repeat("ab", 32) || strings.Join(preset.CIDRs, ",") != "13.124.199.0/24,15.158.0.0/16" {
		t.Fatalf("preset provenance = %#v", preset)
	}
}

func TestRenderTakodProxyWithoutPresetPreflightErrors(t *testing.T) {
	deploy := testProxyDeployer()
	services := presetProxyServices(deploy, "cloudfront")
	_, _, err := deploy.renderTakodProxyDynamicConfigForNode(services, "node-a")
	if err == nil || !strings.Contains(err.Error(), "not resolved in proxy preflight") {
		t.Fatalf("render without preflight error = %v", err)
	}
}

func TestPresetResolutionFailureStopsPreflightBeforeMutation(t *testing.T) {
	resolveErr := &proxypresets.ResolveError{Preset: "cloudfront", URL: "https://ip-ranges.amazonaws.com/ip-ranges.json", Cause: "dial tcp: no route to host"}

	deploy := testProxyDeployer()
	resolver := &fakePresetResolver{err: resolveErr}
	deploy.SetTrustedProxyPresetResolver(resolver)
	services := presetProxyServices(deploy, "cloudfront")
	err := deploy.PreflightTakodProxyCapabilities(services)
	var got *proxypresets.ResolveError
	if !errors.As(err, &got) || !strings.Contains(err.Error(), "ip-ranges.amazonaws.com") {
		t.Fatalf("preflight error = %v", err)
	}

	// The reconcile path (used by deploy, run, rollback, scale, and placement
	// apply) preflights before rendering or writing any proxy file.
	reconcileDeploy := testProxyDeployer()
	reconcileDeploy.SetTrustedProxyPresetResolver(&fakePresetResolver{err: resolveErr})
	err = reconcileDeploy.ReconcileTakodProxy(presetProxyServices(reconcileDeploy, "cloudfront"))
	if !errors.As(err, &got) {
		t.Fatalf("reconcile error = %v, want preset resolution failure before any write", err)
	}

	mutations := 0
	err = runTakodProxyReconcile(func() error {
		return reconcileDeploy.resolveTrustedProxyPresets(presetProxyServices(reconcileDeploy, "cloudfront"))
	}, func() error {
		mutations++
		return nil
	})
	if err == nil || mutations != 0 {
		t.Fatalf("error = %v, mutations = %d", err, mutations)
	}
}

func TestPresetsSkipResolutionWhenUnused(t *testing.T) {
	deploy := testProxyDeployer()
	resolver := &fakePresetResolver{err: errors.New("must not be called")}
	deploy.SetTrustedProxyPresetResolver(resolver)
	if err := deploy.resolveTrustedProxyPresets(presetProxyServices(deploy, "203.0.113.0/24")); err != nil {
		t.Fatal(err)
	}
	if len(resolver.calls) != 0 {
		t.Fatalf("resolver calls = %v", resolver.calls)
	}
}

func TestTrustedProxyPresetsRequireCapabilityOnlyWhenUsed(t *testing.T) {
	services := map[string]config.ServiceConfig{
		"web": {Port: 3000, Proxy: &config.ProxyConfig{Domain: "example.com", TrustedProxies: []string{"cloudflare"}}},
	}
	requirements := takodProxyCapabilityRequirements(services)
	var capabilities []string
	for _, requirement := range requirements {
		capabilities = append(capabilities, requirement.Capability)
	}
	if strings.Join(capabilities, ",") != takod.CapabilityProxyTrustedProxiesV1+","+takod.CapabilityProxyTrustedProxyPresetsV1 {
		t.Fatalf("requirements = %v", capabilities)
	}
	cidrOnly := takodProxyCapabilityRequirements(map[string]config.ServiceConfig{
		"web": {Port: 3000, Proxy: &config.ProxyConfig{Domain: "example.com", TrustedProxies: []string{"203.0.113.0/24"}}},
	})
	for _, requirement := range cidrOnly {
		if requirement.Capability == takod.CapabilityProxyTrustedProxyPresetsV1 {
			t.Fatalf("CIDR-only config required the presets capability: %+v", cidrOnly)
		}
	}
	mutations := 0
	err := runTakodProxyReconcile(func() error {
		return preflightTakodProxyRequirements([]string{"node-a"}, requirements, func(server string, requirement takodProxyCapabilityRequirement) error {
			if requirement.Capability == takod.CapabilityProxyTrustedProxyPresetsV1 {
				return &takodclient.CapabilityRequiredError{Server: server, Capability: requirement.Capability, Feature: requirement.Feature}
			}
			return nil
		})
	}, func() error {
		mutations++
		return nil
	})
	var capabilityErr *takodclient.CapabilityRequiredError
	if !errors.As(err, &capabilityErr) || !strings.Contains(err.Error(), "tako upgrade servers") || mutations != 0 {
		t.Fatalf("old takod error = %v, mutations = %d", err, mutations)
	}
}
