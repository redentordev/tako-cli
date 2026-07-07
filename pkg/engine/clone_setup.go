package engine

// KindCloneSetupResult identifies a serialized clone-setup result document.
const KindCloneSetupResult = "CloneSetupResult"

// CloneSetupResult is the serializable outcome of `tako clone-setup`: the
// guided post-clone walkthrough (config, .env, SSH, env bundle, state,
// secrets). Checks reuse the doctor check shape; Name carries the step the
// human output prints as a `=== Step N: Title ===` heading. Status is "ok"
// when no check failed and "attention" otherwise (exit code 6). Machine
// modes skip the interactive fix-up prompts and only report.
type CloneSetupResult struct {
	APIVersion  string        `json:"apiVersion"`
	Kind        string        `json:"kind"`
	Project     string        `json:"project,omitempty"`
	Environment string        `json:"environment,omitempty"`
	Status      string        `json:"status"`
	Checks      []DoctorCheck `json:"checks"`
	Passed      int           `json:"passed"`
	Warned      int           `json:"warned"`
	Failed      int           `json:"failed"`
}
