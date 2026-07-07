package engine

// KindValidateResult identifies a serialized validate result document.
const KindValidateResult = "ValidateResult"

// Validate finding severities.
const (
	ValidateSeverityError = "error"
)

// ValidateFinding is one problem found while validating a configuration.
// Field is the dotted config field when the validator can attribute the
// problem to one; the schema carries it so attribution can be added without
// a breaking change.
type ValidateFinding struct {
	Severity string `json:"severity"`
	Path     string `json:"path,omitempty"`
	Field    string `json:"field,omitempty"`
	Message  string `json:"message"`
}

// ValidateResult is the serializable outcome of `tako validate`: the strict
// preflight verdict for a config plus the resolved-environment summary the
// human output prints. Validation is a pure-local read, so the cmd layer
// populates this document directly; it lives here because pkg/engine is the
// source of truth for machine-facing documents.
type ValidateResult struct {
	APIVersion  string            `json:"apiVersion"`
	Kind        string            `json:"kind"`
	ConfigPath  string            `json:"configPath"`
	Project     string            `json:"project,omitempty"`
	Environment string            `json:"environment,omitempty"`
	Valid       bool              `json:"valid"`
	Findings    []ValidateFinding `json:"findings,omitempty"`

	// Summary of the validated environment; set only when Valid.
	Runtime         string `json:"runtime,omitempty"`
	StateBackend    string `json:"stateBackend,omitempty"`
	Consistency     string `json:"consistency,omitempty"`
	MeshEnabled     bool   `json:"meshEnabled,omitempty"`
	MeshNetworkCIDR string `json:"meshNetworkCIDR,omitempty"`
	MeshInterface   string `json:"meshInterface,omitempty"`
	Servers         int    `json:"servers,omitempty"`
	Services        int    `json:"services,omitempty"`
}
