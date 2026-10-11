package engine

// KindAliasList identifies a serialized `tako aliases` result document.
const KindAliasList = "AliasList"

// AliasEntry is one project command alias as declared in the config.
type AliasEntry struct {
	// Name is the alias name invoked as `tako <name>`.
	Name string `json:"name"`
	// Args are the Tako arguments the alias expands to, already split for
	// string-form aliases.
	Args []string `json:"args"`
	// Form is "list" or "string": how the value was written in the config.
	Form string `json:"form"`
	// Target is the top-level command the alias runs (Args[0]).
	Target string `json:"target"`
	// Shadowed reports that a built-in command with the same name always
	// wins, so the alias can never run.
	Shadowed bool `json:"shadowed"`
	// ShadowedBy names the built-in command that shadows the alias.
	ShadowedBy string `json:"shadowedBy,omitempty"`
}

// AliasListResult is the serializable outcome of `tako aliases`: every
// project alias sorted by name plus target and shadowing findings. Listing is
// a pure-local read of the config's aliases section, so the cmd layer
// populates this document directly.
type AliasListResult struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	ConfigPath string            `json:"configPath"`
	Aliases    []AliasEntry      `json:"aliases"`
	Findings   []ValidateFinding `json:"findings,omitempty"`
}
