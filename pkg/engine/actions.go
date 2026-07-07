package engine

// KindActionResult identifies the minimal acknowledgement document emitted
// by node-fanout maintenance operations (maintenance, live, cleanup).
const KindActionResult = "ActionResult"

// Action identifiers carried in ActionResult.Action.
const (
	ActionMaintenanceEnable  = "maintenance.enable"
	ActionMaintenanceDisable = "maintenance.disable"
	ActionCleanup            = "cleanup"
)

// Action outcomes.
const (
	ActionOutcomeOK      = "ok"
	ActionOutcomePartial = "partial"
	ActionOutcomeFailed  = "failed"
)

// ActionNodeOutcome reports one node's result for a fanout action.
type ActionNodeOutcome struct {
	Server   string   `json:"server"`
	Host     string   `json:"host,omitempty"`
	Done     bool     `json:"done"`
	Error    string   `json:"error,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// ActionResult is the serializable acknowledgement of maintenance, live,
// and cleanup: what action ran, on which service, and per-node outcomes.
type ActionResult struct {
	APIVersion  string              `json:"apiVersion"`
	Kind        string              `json:"kind"`
	Project     string              `json:"project"`
	Environment string              `json:"environment"`
	Action      string              `json:"action"`
	Service     string              `json:"service,omitempty"`
	Outcome     string              `json:"outcome"`
	Servers     []ActionNodeOutcome `json:"servers"`
	Error       string              `json:"error,omitempty"`
}
