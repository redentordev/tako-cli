package deployer

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/registryauth"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
)

// ghcrScopeHint is appended to registry auth failures when a ghcr token came
// from `gh auth token`, whose default scopes may not include read:packages.
const ghcrScopeHint = "token from gh auth token may need `gh auth refresh -s read:packages`"

// RegistryAuthError marks an image pull/build failure caused by registry
// credentials (missing, wrong, or expired) rather than a missing image, so
// callers can prompt for credential rotation instead of retrying.
type RegistryAuthError struct {
	Node string
	Err  error
	// Hint is an optional remediation appended to the message.
	Hint string
}

func (e *RegistryAuthError) Error() string {
	message := fmt.Sprintf("registry authentication failed: %v", e.Err)
	if e.Node != "" {
		message = fmt.Sprintf("registry authentication failed on %s: %v", e.Node, e.Err)
	}
	if e.Hint != "" {
		message += " (hint: " + e.Hint + ")"
	}
	return message
}

func (e *RegistryAuthError) Unwrap() error { return e.Err }

// RegistryMinter mints provider credentials for registries.<host> entries
// that declare a provider. *registryauth.Minter implements it.
type RegistryMinter interface {
	Auth(ctx context.Context, host string, entry config.RegistryConfig) (registryauth.Credential, error)
}

// SetRegistryMinter installs the per-operation provider credential minter.
// Without one, any request that needs a provider credential fails closed.
func (d *Deployer) SetRegistryMinter(minter RegistryMinter) {
	d.registryMinter = minter
}

// RegistryAuthNeeds lists the provider credentials an operation will attach:
// pulls of specific images, and whether a takod remote build will run.
type RegistryAuthNeeds struct {
	PullImages []string
	Build      bool
}

// Add merges other into n.
func (n *RegistryAuthNeeds) Add(other RegistryAuthNeeds) {
	n.PullImages = append(n.PullImages, other.PullImages...)
	n.Build = n.Build || other.Build
}

// remoteBuildMintsUpfront reports whether builds always run on takod. Under
// the auto strategy a build tries the local docker first and only falls back
// to takod, so its credentials are minted lazily at fallback time.
func (d *Deployer) remoteBuildMintsUpfront() bool {
	if d.config == nil {
		return true
	}
	return d.config.GetBuildStrategy() == config.BuildStrategyRemote
}

// DeployRegistryAuthNeeds computes the provider credentials that deploying
// (or scaling) services will attach, using the same inputs that decide
// PullImage and builds in the rollout: takodDeployOptionsForService for
// long-running services, the run image resolution for run steps, and shared
// builds unless builds are skipped.
func (d *Deployer) DeployRegistryAuthNeeds(services map[string]config.ServiceConfig, allServices map[string]config.ServiceConfig) RegistryAuthNeeds {
	var needs RegistryAuthNeeds
	for _, service := range services {
		service := service
		if service.SharedBuildHash != "" && !d.skipBuild && d.remoteBuildMintsUpfront() {
			needs.Build = true
		}
		if service.IsRun() {
			if service.Image != "" {
				needs.PullImages = append(needs.PullImages, service.Image)
			} else if service.SharedBuildHash == "" && service.ImageFrom != "" {
				if source, ok := allServices[service.ImageFrom]; ok && source.Image != "" {
					needs.PullImages = append(needs.PullImages, source.Image)
				}
			}
			continue
		}
		options := takodDeployOptionsForService(&service, d.skipBuild)
		if options.BuildImage && d.remoteBuildMintsUpfront() {
			needs.Build = true
		}
		if options.PullImage && !service.IsJob() {
			needs.PullImages = append(needs.PullImages, service.Image)
		}
	}
	return needs
}

// RollbackRegistryAuthNeeds computes the provider credentials a rollback of
// one service to state will attach, using rollbackDeployOptions.
func (d *Deployer) RollbackRegistryAuthNeeds(service config.ServiceConfig, serviceState *state.ServiceState, opts RollbackOptions) RegistryAuthNeeds {
	var needs RegistryAuthNeeds
	if serviceState == nil {
		return needs
	}
	image := serviceState.Image
	options, _ := rollbackDeployOptions(&service, serviceState, opts)
	if options.BuildImage && d.remoteBuildMintsUpfront() {
		needs.Build = true
	}
	if options.PullImage && strings.TrimSpace(image) != "" {
		needs.PullImages = append(needs.PullImages, image)
	}
	return needs
}

// PreflightRegistryAuth mints, before any node mutation, every provider
// credential described by needs. Static entries and images without a
// provider entry run no CLI.
func (d *Deployer) PreflightRegistryAuth(ctx context.Context, needs RegistryAuthNeeds) error {
	if d.config == nil || !d.hasProviderRegistries() {
		return nil
	}
	if needs.Build {
		_, err := d.registryAuths(ctx, "")
		return err
	}
	seen := map[string]bool{}
	for _, image := range needs.PullImages {
		host := config.ImageRegistryHost(image)
		if seen[host] {
			continue
		}
		seen[host] = true
		if _, err := d.registryAuths(ctx, image); err != nil {
			return err
		}
	}
	return nil
}

func (d *Deployer) hasProviderRegistries() bool {
	for _, registry := range d.config.Registries {
		if strings.TrimSpace(registry.Provider) != "" {
			return true
		}
	}
	return false
}

// registryAuths renders the registries block as request-scoped credentials,
// sorted by host for deterministic payloads. Every complete static entry is
// attached as before. Provider entries are minted: only the entry whose host
// matches imageRef's registry for a pull, and every provider entry for a
// build (imageRef == "").
func (d *Deployer) registryAuths(ctx context.Context, imageRef string) ([]takod.RegistryAuth, error) {
	if d.config == nil || len(d.config.Registries) == 0 {
		return nil, nil
	}
	pullHost := ""
	if imageRef != "" {
		pullHost = config.ImageRegistryHost(imageRef)
	}
	hosts := make([]string, 0, len(d.config.Registries))
	for host := range d.config.Registries {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	auths := make([]takod.RegistryAuth, 0, len(hosts))
	for _, host := range hosts {
		registry := d.config.Registries[host]
		if strings.TrimSpace(registry.Provider) != "" {
			if imageRef != "" && !strings.EqualFold(host, pullHost) {
				continue
			}
			credential, err := d.mintRegistryCredential(ctx, host, registry)
			if err != nil {
				return nil, err
			}
			auths = append(auths, takod.RegistryAuth{Registry: host, Username: credential.Username, Password: credential.Password})
			continue
		}
		if registry.Username == "" || registry.Password == "" {
			continue
		}
		auths = append(auths, takod.RegistryAuth{
			Registry: host,
			Username: registry.Username,
			Password: registry.Password,
		})
	}
	return auths, nil
}

// staticRegistryAuths returns only the static entries, for requests that
// carry credentials without pulling (provider credentials would be unused).
func (d *Deployer) staticRegistryAuths() []takod.RegistryAuth {
	if d.config == nil || len(d.config.Registries) == 0 {
		return nil
	}
	hosts := make([]string, 0, len(d.config.Registries))
	for host := range d.config.Registries {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	auths := make([]takod.RegistryAuth, 0, len(hosts))
	for _, host := range hosts {
		registry := d.config.Registries[host]
		if strings.TrimSpace(registry.Provider) != "" || registry.Username == "" || registry.Password == "" {
			continue
		}
		auths = append(auths, takod.RegistryAuth{Registry: host, Username: registry.Username, Password: registry.Password})
	}
	return auths
}

func (d *Deployer) mintRegistryCredential(ctx context.Context, host string, registry config.RegistryConfig) (registryauth.Credential, error) {
	if d.registryMinter == nil {
		return registryauth.Credential{}, &registryauth.MintError{Host: host, Provider: registry.Provider, Err: fmt.Errorf("no registry credential minter is configured for this operation")}
	}
	if ctx == nil {
		ctx = d.baseContext()
	}
	credential, err := d.registryMinter.Auth(ctx, host, registry)
	if err != nil {
		return registryauth.Credential{}, err
	}
	if credential.FromGHCLI {
		d.ghCLITokenUsed.Store(true)
	}
	return credential, nil
}

// wrapRegistryAuthError converts an error whose takod-side output was
// classified as an authentication failure into the typed error and emits
// the image.pull.auth_failed event.
func (d *Deployer) wrapRegistryAuthError(node string, err error) error {
	if err == nil {
		return nil
	}
	if !strings.Contains(err.Error(), takod.RegistryAuthFailedMarker) {
		return err
	}
	wrapped := &RegistryAuthError{Node: node, Err: err}
	if d.ghCLITokenUsed.Load() {
		wrapped.Hint = ghcrScopeHint
	}
	d.emitEvent(events.Event{
		Type:    events.TypeImagePullAuthFailed,
		Phase:   events.PhaseDeploy,
		Level:   events.LevelError,
		Node:    node,
		Message: fmt.Sprintf("  ✗ %s\n", wrapped.Error()),
		Data:    map[string]any{"node": node},
	})
	return wrapped
}
