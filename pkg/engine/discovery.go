package engine

import "github.com/redentordev/tako-cli/pkg/takod"

// KindDiscoveryExportsResult identifies a serialized discovery exports
// result document.
const KindDiscoveryExportsResult = "DiscoveryExportsResult"

// DiscoveryNodeExports is one node's exported service discovery records.
// Exports reuses the takod export-discovery record schema.
type DiscoveryNodeExports struct {
	Server  string                        `json:"server"`
	Host    string                        `json:"host,omitempty"`
	Exports []takod.ExportDiscoveryRecord `json:"exports,omitempty"`
	Error   string                        `json:"error,omitempty"`
}

// DiscoveryExportsResult is the serializable outcome of
// `tako discovery exports`. All nodes failing exits 1; a partial read
// exits 6; both still emit the document.
type DiscoveryExportsResult struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	// Environment is empty when --all-environments was requested.
	Environment     string                 `json:"environment,omitempty"`
	AllEnvironments bool                   `json:"allEnvironments,omitempty"`
	Nodes           []DiscoveryNodeExports `json:"nodes"`
	Error           string                 `json:"error,omitempty"`
}
