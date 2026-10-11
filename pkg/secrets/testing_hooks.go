package secrets

import (
	"context"
	"os/exec"

	"github.com/redentordev/tako-cli/internal/providercli"
)

// SetProviderCommandForTesting replaces the provider CLI runner and lookup
// used by secretSources adapters and by registry credential minting
// (pkg/registryauth, through internal/providercli), and clears the per-process source cache. It
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
	restoreShared := providercli.SetForTesting(command, lookPath)
	resetSecretSourceCache()
	return func() {
		restoreShared()
		providerCommandContext = oldCommand
		providerLookPath = oldLookPath
		resetSecretSourceCache()
	}
}
