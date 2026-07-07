package engine

// KindDoctorResult identifies a serialized doctor result document.
const KindDoctorResult = "DoctorResult"

// Doctor check statuses.
const (
	DoctorStatusPass = "pass"
	DoctorStatusWarn = "warn"
	DoctorStatusFail = "fail"
	DoctorStatusSkip = "skip"
)

// DoctorCheck is one health-check outcome. Name is the check section the
// human output prints as a `=== Section ===` heading; a section may emit
// several checks.
type DoctorCheck struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Detail      string `json:"detail"`
	Remediation string `json:"remediation,omitempty"`
}

// DoctorResult is the serializable outcome of `tako doctor`. Status is
// "ok" when no check failed and "attention" otherwise (exit code 6).
// Doctor is populated cmd-local: its checks mix pure-local reads with
// direct SSH probes that predate the engine.
type DoctorResult struct {
	APIVersion  string        `json:"apiVersion"`
	Kind        string        `json:"kind"`
	Project     string        `json:"project,omitempty"`
	Environment string        `json:"environment,omitempty"`
	SkipRemote  bool          `json:"skipRemote,omitempty"`
	Status      string        `json:"status"`
	Checks      []DoctorCheck `json:"checks"`
	Passed      int           `json:"passed"`
	Warned      int           `json:"warned"`
	Failed      int           `json:"failed"`
}
