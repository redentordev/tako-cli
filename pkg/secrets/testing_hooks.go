package secrets

import (
	"context"
	"os/exec"
)

// SetProviderCommandForTesting replaces the provider CLI runner and lookup
// used by secretSources adapters and clears the per-process source cache. It
// returns a function restoring the previous hooks. It exists so tests in
// other packages can stub provider CLIs the same way this package's tests do;
// production code must never call it.
func SetProviderCommandForTesting(command func(ctx context.Context, name string, args ...string) *exec.Cmd, lookPath func(name string) (string, error)) func() {
	oldCommand := providerCommandContext
	oldLookPath := providerLookPath
	if command != nil {
		providerCommandContext = command
	}
	if lookPath != nil {
		providerLookPath = lookPath
	}
	resetSecretSourceCache()
	return func() {
		providerCommandContext = oldCommand
		providerLookPath = oldLookPath
		resetSecretSourceCache()
	}
}
