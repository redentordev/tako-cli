package deployer

import (
	"fmt"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
)

// RegistryAuthError marks an image pull/build failure caused by registry
// credentials (missing, wrong, or expired) rather than a missing image, so
// callers can prompt for credential rotation instead of retrying.
type RegistryAuthError struct {
	Node string
	Err  error
}

func (e *RegistryAuthError) Error() string {
	if e.Node != "" {
		return fmt.Sprintf("registry authentication failed on %s: %v", e.Node, e.Err)
	}
	return fmt.Sprintf("registry authentication failed: %v", e.Err)
}

func (e *RegistryAuthError) Unwrap() error { return e.Err }

// registryAuths renders the config's registries block as request-scoped
// credentials, sorted by host for deterministic payloads.
func (d *Deployer) registryAuths() []takod.RegistryAuth {
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
		if registry.Username == "" || registry.Password == "" {
			continue
		}
		auths = append(auths, takod.RegistryAuth{
			Registry: host,
			Username: registry.Username,
			Password: registry.Password,
		})
	}
	return auths
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
