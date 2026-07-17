package engine

import (
	"fmt"
	"sort"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/upgradeprotocol"
)

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
	UpgradeOutcomeRolledBack        = "rolled-back"
	UpgradeOutcomeBlocked           = "blocked"
	UpgradeOutcomeDowngradeBlocked  = "downgrade-blocked"
)

const (
	UpgradeStageCanary     = "worker-canary"
	UpgradeStageWorkers    = "workers"
	UpgradeStageController = "controller-last"
	UpgradeStageLegacy     = "legacy"
)

// UpgradeProtocolCurrent is the stable node-lifecycle protocol emitted by this
// CLI. Compatible N/N-1 releases report a range containing this value.
const UpgradeProtocolCurrent = upgradeprotocol.Current

type UpgradeNode struct {
	Name  string
	Roles []string
}

type UpgradePlanNode struct {
	Name  string
	Stage string
}

// PlanNodeUpgrade returns a deterministic worker-first, controller-last plan.
// A full enrolled-cluster plan accepts at most one selected controller;
// targeted worker-only plans contain none. Legacy configs have no
// platform-owned roles and retain their configured ordering.
func PlanNodeUpgrade(nodes []UpgradeNode) ([]UpgradePlanNode, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("at least one upgrade node is required")
	}
	enrolled := false
	for _, node := range nodes {
		if len(node.Roles) > 0 {
			enrolled = true
			break
		}
	}
	if !enrolled {
		plan := make([]UpgradePlanNode, 0, len(nodes))
		for _, node := range nodes {
			plan = append(plan, UpgradePlanNode{Name: node.Name, Stage: UpgradeStageLegacy})
		}
		return plan, nil
	}

	workers := make([]string, 0, len(nodes))
	controllers := make([]string, 0, 1)
	for _, node := range nodes {
		if len(node.Roles) == 0 {
			return nil, fmt.Errorf("cannot mix enrolled and legacy nodes in one staged upgrade")
		}
		isController := hasUpgradeRole(node.Roles, nodeidentity.RoleControlPlane)
		if isController {
			controllers = append(controllers, node.Name)
			continue
		}
		if hasUpgradeRole(node.Roles, nodeidentity.RoleWorker) {
			workers = append(workers, node.Name)
			continue
		}
		return nil, fmt.Errorf("node %s is neither a worker nor the controller", node.Name)
	}
	if len(controllers) > 1 {
		return nil, fmt.Errorf("staged single-controller upgrade supports at most one selected controller, got %d", len(controllers))
	}
	sort.Strings(workers)
	plan := make([]UpgradePlanNode, 0, len(nodes))
	for index, name := range workers {
		stage := UpgradeStageWorkers
		if index == 0 {
			stage = UpgradeStageCanary
		}
		plan = append(plan, UpgradePlanNode{Name: name, Stage: stage})
	}
	if len(controllers) == 1 {
		plan = append(plan, UpgradePlanNode{Name: controllers[0], Stage: UpgradeStageController})
	}
	return plan, nil
}

func hasUpgradeRole(roles []string, want string) bool {
	for _, role := range roles {
		if role == want {
			return true
		}
	}
	return false
}

// ValidateUpgradeCompatibility enforces the N/N-1 rolling-upgrade window.
// Protocol zero is the pre-protocol status shape and is treated as N-1 only.
func ValidateUpgradeCompatibility(version string, protocol int, minimum int) error {
	return upgradeprotocol.ValidateStatus(version, protocol, minimum)
}

// UpgradeServersNodeOutcome reports one node's takod agent upgrade or
// dry-run assessment.
type UpgradeServersNodeOutcome struct {
	Server      string `json:"server"`
	Host        string `json:"host,omitempty"`
	FromVersion string `json:"fromVersion,omitempty"`
	ToVersion   string `json:"toVersion,omitempty"`
	Stage       string `json:"stage,omitempty"`
	Protocol    int    `json:"protocol,omitempty"`
	RolledBack  bool   `json:"rolledBack,omitempty"`
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
