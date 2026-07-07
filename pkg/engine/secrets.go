package engine

// Kinds of serialized secrets result documents.
const (
	KindSecretsListResult     = "SecretsListResult"
	KindSecretsValidateResult = "SecretsValidateResult"
)

// SecretsListResult is the serializable outcome of `tako secrets list`.
// It carries secret KEYS only — values never appear in any machine output
// (test-enforced alongside the event redaction tests).
type SecretsListResult struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	// Environment is the --env filter; empty means all environments.
	Environment string   `json:"environment,omitempty"`
	Keys        []string `json:"keys"`
	Count       int      `json:"count"`
}

// SecretsValidateResult is the serializable outcome of
// `tako secrets validate`: whether every secret referenced by the
// environment's services is configured. Names only — never values.
type SecretsValidateResult struct {
	APIVersion  string   `json:"apiVersion"`
	Kind        string   `json:"kind"`
	Project     string   `json:"project,omitempty"`
	Environment string   `json:"environment,omitempty"`
	Valid       bool     `json:"valid"`
	Required    []string `json:"required"`
	Missing     []string `json:"missing,omitempty"`
}
