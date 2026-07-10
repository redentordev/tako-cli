//go:build windows

package takod

import "os"

// Windows does not expose Unix uid/gid ownership through os.FileInfo. Tako's
// node agent runs on Linux, but the CLI embeds this package in its Windows
// release binary, so content/type/mode verification remains available there
// without pretending Windows has portable uid/gid semantics.
func serviceFileOwnershipMatches(_ os.FileInfo, _, _ int) bool {
	return true
}
