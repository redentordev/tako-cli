package deployer

import (
	"fmt"
	"path/filepath"
	"strings"
)

// VolumeTransformer handles volume path transformations
type VolumeTransformer struct {
	projectName string
	environment string
}

// NewVolumeTransformer creates a new volume transformer
func NewVolumeTransformer(projectName, environment string) *VolumeTransformer {
	return &VolumeTransformer{
		projectName: projectName,
		environment: environment,
	}
}

// Transform prefixes named volumes with project name and environment for isolation
// Leaves bind mounts (absolute paths) unchanged
func (vt *VolumeTransformer) Transform(volumes []string) []string {
	var transformed []string

	for _, vol := range volumes {
		// Split volume spec: source:destination[:options]
		parts := strings.Split(vol, ":")
		if len(parts) < 2 {
			// Invalid volume spec, keep as-is
			transformed = append(transformed, vol)
			continue
		}

		source := parts[0]

		// Only prefix named volumes (not absolute paths or Windows paths)
		// Check if it's NOT an absolute path (Unix: starts with /, Windows: starts with C:\ or similar)
		isAbsolutePath := filepath.IsAbs(source) || (len(source) >= 2 && source[1] == ':')

		if !isAbsolutePath {
			// It's a named volume - prefix with project name and environment
			source = fmt.Sprintf("%s_%s_%s", vt.projectName, vt.environment, source)
			parts[0] = source
		}

		transformed = append(transformed, strings.Join(parts, ":"))
	}

	return transformed
}
