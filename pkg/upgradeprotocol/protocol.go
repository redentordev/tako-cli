// Package upgradeprotocol defines the node-lifecycle compatibility contract.
// It is deliberately independent from application API and release versions.
package upgradeprotocol

import (
	"fmt"
	"strings"
)

// Current remains stable across wire-compatible releases. A future breaking
// lifecycle protocol must increment it; an old controller whose maximum is
// lower will then reject worker-first canarying before mutation.
const Current = 1

var legacyBridgeVersions = map[string]struct{}{
	"v0.8.0": {}, "v0.8.1": {}, "v0.8.2": {}, "v0.8.3": {},
	"v0.9.0": {}, "v0.9.1": {}, "v0.9.2": {}, "v0.9.3": {},
}

// ValidateStatus validates the complete supported protocol range. The only
// zero-valued bridge is an explicit finite list of releases that predate
// protocol reporting but are known to interoperate with lifecycle protocol 1.
func ValidateStatus(version string, maximum int, minimum int) error {
	if maximum == 0 && minimum == 0 {
		normalized := strings.TrimSpace(version)
		if !strings.HasPrefix(normalized, "v") {
			normalized = "v" + normalized
		}
		if _, ok := legacyBridgeVersions[normalized]; ok {
			return nil
		}
		return fmt.Errorf("node release %q does not report an upgrade protocol and is not a known N-1 bridge release", version)
	}
	if minimum < 1 || maximum < minimum {
		return fmt.Errorf("node upgrade protocol range [%d,%d] is invalid", minimum, maximum)
	}
	if Current < minimum || Current > maximum {
		return fmt.Errorf("node upgrade protocol range [%d,%d] does not include required protocol %d", minimum, maximum, Current)
	}
	return nil
}
