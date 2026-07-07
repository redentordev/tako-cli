package engine

// KindUpgradeServersResult identifies a serialized server-agent upgrade
// result document.
const KindUpgradeServersResult = "UpgradeServersResult"

// Upgrade outcomes carried in UpgradeServersNodeOutcome.Outcome. Apply runs
// report upgraded/failed; dry runs report the remaining assessments.
const (
	UpgradeOutcomeUpgraded          = "upgraded"
	UpgradeOutcomeFailed            = "failed"
	UpgradeOutcomeCurrent           = "current"
	UpgradeOutcomeUpgradeNeeded     = "upgrade-needed"
	UpgradeOutcomeSetupRequired     = "setup-required"
	UpgradeOutcomeStatusUnavailable = "status-unavailable"
)

// UpgradeServersNodeOutcome reports one node's takod agent upgrade or
// dry-run assessment.
type UpgradeServersNodeOutcome struct {
	Server      string `json:"server"`
	Host        string `json:"host,omitempty"`
	FromVersion string `json:"fromVersion,omitempty"`
	ToVersion   string `json:"toVersion,omitempty"`
	Outcome     string `json:"outcome"`
	Error       string `json:"error,omitempty"`
}

// UpgradeServersResult is the serializable outcome of `tako upgrade servers`.
type UpgradeServersResult struct {
	APIVersion    string                      `json:"apiVersion"`
	Kind          string                      `json:"kind"`
	Project       string                      `json:"project"`
	Environment   string                      `json:"environment"`
	TargetVersion string                      `json:"targetVersion"`
	DryRun        bool                        `json:"dryRun,omitempty"`
	Nodes         []UpgradeServersNodeOutcome `json:"nodes"`
	Error         string                      `json:"error,omitempty"`
}
