package deployer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/registryauth"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
)

func registryDeployerFixture(sink events.Sink) *Deployer {
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "demo"},
		Registries: map[string]config.RegistryConfig{
			"ghcr.io":   {Username: "octocat", Password: "gh-token"},
			"docker.io": {Username: "acme", Password: "hub-token"},
		},
	}
	d := NewDeployer(nil, cfg, "production", false)
	if sink != nil {
		d.SetEventSink(sink)
	}
	return d
}

func TestRegistryAuthsSortedByHost(t *testing.T) {
	d := registryDeployerFixture(nil)
	auths, err := d.registryAuths(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(auths) != 2 {
		t.Fatalf("auths = %+v, want 2", auths)
	}
	if auths[0].Registry != "docker.io" || auths[1].Registry != "ghcr.io" {
		t.Fatalf("auths not sorted: %+v", auths)
	}
	if auths[1].Username != "octocat" || auths[1].Password != "gh-token" {
		t.Fatalf("ghcr auth = %+v", auths[1])
	}
}

func TestRegistryAuthsEmptyWithoutConfig(t *testing.T) {
	d := NewDeployer(nil, &config.Config{Project: config.ProjectConfig{Name: "demo"}}, "production", false)
	if auths, err := d.registryAuths(context.Background(), "nginx:1.27"); err != nil || auths != nil {
		t.Fatalf("auths = %+v, want nil", auths)
	}
}

func TestWrapRegistryAuthErrorTypedAndEmitsEvent(t *testing.T) {
	sink := &capturingSink{}
	d := registryDeployerFixture(sink)

	plain := fmt.Errorf("failed to pull image: exit status 1: manifest unknown")
	if err := d.wrapRegistryAuthError("node-a", plain); err != plain {
		t.Fatalf("non-auth error was wrapped: %v", err)
	}
	if len(sink.events) != 0 {
		t.Fatalf("event emitted for non-auth error: %+v", sink.events)
	}

	authErr := fmt.Errorf("failed to pull image: %s: unauthorized", takod.RegistryAuthFailedMarker)
	wrapped := d.wrapRegistryAuthError("node-a", authErr)
	var typed *RegistryAuthError
	if !errors.As(wrapped, &typed) || typed.Node != "node-a" {
		t.Fatalf("wrapped = %v, want RegistryAuthError for node-a", wrapped)
	}
	if len(sink.events) != 1 || sink.events[0].Type != events.TypeImagePullAuthFailed {
		t.Fatalf("events = %+v, want one image.pull.auth_failed", sink.events)
	}
}

const (
	testECRHost  = "123456789012.dkr.ecr.us-west-1.amazonaws.com"
	fakeECRToken = "fake-token-0123456789abcdef"
	fakeGHToken  = "fake-token-fedcba9876543210"
)

type fakeRegistryMinter struct {
	mu     sync.Mutex
	calls  []string
	err    error
	fromGH bool
}

func (m *fakeRegistryMinter) Auth(_ context.Context, host string, entry config.RegistryConfig) (registryauth.Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, host)
	if m.err != nil {
		return registryauth.Credential{}, m.err
	}
	switch config.NormalizeRegistryProvider(entry.Provider) {
	case config.RegistryProviderECR:
		return registryauth.Credential{Username: "AWS", Password: fakeECRToken}, nil
	case config.RegistryProviderGHCR:
		return registryauth.Credential{Username: "octocat", Password: fakeGHToken, FromGHCLI: m.fromGH}, nil
	default:
		return registryauth.Credential{Username: "oauth2accesstoken", Password: fakeECRToken}, nil
	}
}

func providerDeployerFixture(minter RegistryMinter) *Deployer {
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "demo"},
		Registries: map[string]config.RegistryConfig{
			testECRHost:            {Provider: "ecr"},
			"ghcr.io":              {Provider: "ghcr"},
			"registry.example.com": {Username: "acme", Password: "static-token-value"},
		},
	}
	d := NewDeployer(nil, cfg, "production", false)
	if minter != nil {
		d.SetRegistryMinter(minter)
	}
	return d
}

func TestRegistryAuthsPullMintsOnlyImageHost(t *testing.T) {
	minter := &fakeRegistryMinter{}
	d := providerDeployerFixture(minter)
	auths, err := d.registryAuths(context.Background(), testECRHost+"/web:v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(auths) != 2 || auths[0].Registry != testECRHost || auths[0].Username != "AWS" || auths[0].Password != fakeECRToken {
		t.Fatalf("auths = %+v, want ECR provider credential first", auths)
	}
	if auths[1].Registry != "registry.example.com" {
		t.Fatalf("static entry missing: %+v", auths)
	}
	if strings.Join(minter.calls, ",") != testECRHost {
		t.Fatalf("minted %v, want only the ECR host", minter.calls)
	}
}

func TestRegistryAuthsDockerHubPullMintsNothing(t *testing.T) {
	minter := &fakeRegistryMinter{}
	d := providerDeployerFixture(minter)
	auths, err := d.registryAuths(context.Background(), "nginx:1.27")
	if err != nil {
		t.Fatal(err)
	}
	if len(minter.calls) != 0 {
		t.Fatalf("minted %v for a docker.io pull", minter.calls)
	}
	if len(auths) != 1 || auths[0].Registry != "registry.example.com" {
		t.Fatalf("auths = %+v, want static entry only", auths)
	}
}

func TestRegistryAuthsBuildAttachesEveryProviderEntry(t *testing.T) {
	minter := &fakeRegistryMinter{}
	d := providerDeployerFixture(minter)
	auths, err := d.registryAuths(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	hosts := make([]string, 0, len(auths))
	for _, auth := range auths {
		hosts = append(hosts, auth.Registry)
	}
	if strings.Join(hosts, ",") != testECRHost+",ghcr.io,registry.example.com" {
		t.Fatalf("hosts = %v", hosts)
	}
}

func TestRegistryAuthsWithoutMinterFailsClosed(t *testing.T) {
	d := providerDeployerFixture(nil)
	_, err := d.registryAuths(context.Background(), testECRHost+"/web:v1")
	var mintErr *registryauth.MintError
	if !errors.As(err, &mintErr) {
		t.Fatalf("err = %v, want MintError", err)
	}
}

func TestStaticOnlyRegistryAuthsUnchanged(t *testing.T) {
	d := registryDeployerFixture(nil)
	pull, err := d.registryAuths(context.Background(), "ghcr.io/acme/web:v1")
	if err != nil {
		t.Fatal(err)
	}
	build, err := d.registryAuths(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	static := d.staticRegistryAuths()
	want := `[{"registry":"docker.io","username":"acme","password":"hub-token"},{"registry":"ghcr.io","username":"octocat","password":"gh-token"}]`
	for name, auths := range map[string]any{"pull": pull, "build": build, "static": static} {
		encoded, err := json.Marshal(auths)
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != want {
			t.Fatalf("%s auths = %s, want %s", name, encoded, want)
		}
	}
}

func TestDeployRegistryAuthNeeds(t *testing.T) {
	d := providerDeployerFixture(&fakeRegistryMinter{})
	d.config.Deployment = &config.DeploymentConfig{}
	services := map[string]config.ServiceConfig{
		"web":   {Image: testECRHost + "/web:v1"},
		"cache": {Image: "redis:7"},
	}
	needs := d.DeployRegistryAuthNeeds(services, services)
	if needs.Build || len(needs.PullImages) != 2 {
		t.Fatalf("needs = %+v", needs)
	}

	buildOnly := map[string]config.ServiceConfig{"api": {Build: "."}}
	d.SetSkipBuild(true)
	if needs := d.DeployRegistryAuthNeeds(buildOnly, buildOnly); needs.Build || len(needs.PullImages) != 0 {
		t.Fatalf("skip-build needs = %+v, want none (no pull, no build)", needs)
	}
	d.SetSkipBuild(false)
	if needs := d.DeployRegistryAuthNeeds(buildOnly, buildOnly); !needs.Build {
		t.Fatalf("remote build needs = %+v, want build", needs)
	}
}

func TestRollbackRegistryAuthNeeds(t *testing.T) {
	d := providerDeployerFixture(&fakeRegistryMinter{})
	image := config.ServiceConfig{Image: testECRHost + "/web:v1"}
	needs := d.RollbackRegistryAuthNeeds(image, &state.ServiceState{Image: testECRHost + "/web:v0"}, RollbackOptions{})
	if needs.Build || len(needs.PullImages) != 1 || needs.PullImages[0] != testECRHost+"/web:v0" {
		t.Fatalf("pull rollback needs = %+v", needs)
	}
	retained := d.RollbackRegistryAuthNeeds(config.ServiceConfig{Build: "."}, &state.ServiceState{Image: "demo/api:v0"}, RollbackOptions{ImageSource: RollbackImageSourceRetained})
	if retained.Build || len(retained.PullImages) != 0 {
		t.Fatalf("retained rollback needs = %+v, want none", retained)
	}
}

func TestPreflightRegistryAuthMintsNeededHostsOnly(t *testing.T) {
	minter := &fakeRegistryMinter{}
	d := providerDeployerFixture(minter)
	if err := d.PreflightRegistryAuth(context.Background(), RegistryAuthNeeds{PullImages: []string{"nginx:1.27", "registry.example.com/app:v1"}}); err != nil {
		t.Fatal(err)
	}
	if len(minter.calls) != 0 {
		t.Fatalf("minted %v with no provider pull", minter.calls)
	}
	if err := d.PreflightRegistryAuth(context.Background(), RegistryAuthNeeds{PullImages: []string{testECRHost + "/a:1", testECRHost + "/b:1"}}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(minter.calls, ",") != testECRHost {
		t.Fatalf("minted %v", minter.calls)
	}
	failing := &fakeRegistryMinter{err: &registryauth.MintError{Host: "ghcr.io", Provider: "ghcr", Err: errors.New("gh missing")}}
	d = providerDeployerFixture(failing)
	if err := d.PreflightRegistryAuth(context.Background(), RegistryAuthNeeds{Build: true}); err == nil {
		t.Fatal("expected preflight failure")
	}
}

func TestWrapRegistryAuthErrorAddsGHCRScopeHint(t *testing.T) {
	minter := &fakeRegistryMinter{fromGH: true}
	d := providerDeployerFixture(minter)
	if _, err := d.registryAuths(context.Background(), "ghcr.io/acme/web:v1"); err != nil {
		t.Fatal(err)
	}
	wrapped := d.wrapRegistryAuthError("node-a", fmt.Errorf("pull: %s: denied", takod.RegistryAuthFailedMarker))
	if !strings.Contains(wrapped.Error(), "gh auth refresh -s read:packages") {
		t.Fatalf("wrapped = %v, want scope hint", wrapped)
	}
	plain := registryDeployerFixture(nil).wrapRegistryAuthError("node-a", fmt.Errorf("pull: %s: denied", takod.RegistryAuthFailedMarker))
	if strings.Contains(plain.Error(), "hint") {
		t.Fatalf("static entry got a ghcr hint: %v", plain)
	}
}
