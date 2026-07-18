package platform

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

const (
	EnrollmentInspectionKind = "PlatformEnrollmentInspection"
	EnrollmentNotEnrolled    = "not-enrolled"
	EnrollmentIncomplete     = "incomplete"
	EnrollmentEnrolled       = "enrolled"

	EnrollmentActionInitialize        = "initialize"
	EnrollmentActionResumeInit        = "resume-platform-init"
	EnrollmentActionRepairWorker      = "repair-worker-enrollment"
	EnrollmentActionRecoverState      = "recover-platform-state"
	EnrollmentActionUseExisting       = "use-existing-cluster"
	IdentityVerificationVerified      = "verified"
	IdentityVerificationPermission    = "permission-limited"
	MembershipComparisonMatches       = "matches-published-inventory"
	MembershipComparisonStale         = "published-inventory-stale"
	MembershipComparisonPermission    = "permission-limited"
	MembershipComparisonNotApplicable = "not-applicable"

	DefaultPlatformConfigPath = "/etc/tako/platform.json"
	DefaultWorkerUnitPath     = "/etc/systemd/system/" + WorkerUnitName
)

// EnrollmentInspection is the credential-free description of this host's
// platform state. Published* fields are explicitly a local trust snapshot;
// they do not claim that the controller is reachable or currently live.
type EnrollmentInspection struct {
	APIVersion              string     `json:"apiVersion"`
	Kind                    string     `json:"kind"`
	Status                  string     `json:"status"`
	ClusterID               string     `json:"clusterId,omitempty"`
	NodeID                  string     `json:"nodeId,omitempty"`
	NodeName                string     `json:"nodeName,omitempty"`
	EnrollmentRoles         []string   `json:"enrollmentRoles,omitempty"`
	PublishedRoles          []string   `json:"publishedRoles,omitempty"`
	PublishedLifecycle      string     `json:"publishedLifecycle,omitempty"`
	PublishedSchedulable    bool       `json:"publishedSchedulable"`
	WorkerUID               int        `json:"workerUid,omitempty"`
	InventoryGeneration     uint64     `json:"inventoryGeneration,omitempty"`
	InventoryUpdatedAt      *time.Time `json:"inventoryUpdatedAt,omitempty"`
	PublishedControllerID   string     `json:"publishedControllerNodeId,omitempty"`
	PublishedControllerName string     `json:"publishedControllerNodeName,omitempty"`
	LocalController         bool       `json:"localController"`
	IdentityVerification    string     `json:"identityVerification,omitempty"`
	MembershipComparison    string     `json:"membershipComparison,omitempty"`
	MembershipGeneration    uint64     `json:"membershipGeneration,omitempty"`
	NextAction              string     `json:"nextAction"`
	NextCommand             string     `json:"nextCommand,omitempty"`
	Detail                  string     `json:"detail,omitempty"`
	Warnings                []string   `json:"warnings,omitempty"`
	ResidualArtifacts       []string   `json:"residualArtifacts,omitempty"`
}

type EnrollmentInspectionPaths struct {
	IdentityPath     string
	LocalBindingPath string
	InventoryPath    string
	ConfigPath       string
	MembershipPath   string
	WorkerUnitPath   string
	MeshKeyDir       string
}

func (p EnrollmentInspectionPaths) withDefaults() EnrollmentInspectionPaths {
	if strings.TrimSpace(p.IdentityPath) == "" {
		p.IdentityPath = nodeidentity.DefaultPath
	}
	if strings.TrimSpace(p.LocalBindingPath) == "" {
		p.LocalBindingPath = nodeidentity.DefaultLocalBindingPath
	}
	if strings.TrimSpace(p.InventoryPath) == "" {
		p.InventoryPath = nodeidentity.DefaultInventoryPath
	}
	if strings.TrimSpace(p.ConfigPath) == "" {
		p.ConfigPath = DefaultPlatformConfigPath
	}
	if strings.TrimSpace(p.MembershipPath) == "" {
		p.MembershipPath = DefaultMembershipPath(DefaultStateDir)
	}
	if strings.TrimSpace(p.WorkerUnitPath) == "" {
		p.WorkerUnitPath = DefaultWorkerUnitPath
	}
	if strings.TrimSpace(p.MeshKeyDir) == "" {
		p.MeshKeyDir = DefaultPlatformMeshKeyDir
	}
	return p
}

func (p EnrollmentInspectionPaths) validate() error {
	for label, path := range map[string]string{
		"identity": p.IdentityPath, "local binding": p.LocalBindingPath, "inventory": p.InventoryPath,
		"platform config": p.ConfigPath, "membership": p.MembershipPath, "worker unit": p.WorkerUnitPath, "mesh key directory": p.MeshKeyDir,
	} {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("platform inspection %s path must be absolute", label)
		}
	}
	return nil
}

// InspectEnrollment reads create-once identity and root-published state. It
// never contacts a daemon, connects over SSH, or mutates the host. A protected
// identity that is unreadable to a non-root caller is clearly reported rather
// than silently treated as verified.
func InspectEnrollment(paths EnrollmentInspectionPaths) (*EnrollmentInspection, error) {
	paths = paths.withDefaults()
	if err := paths.validate(); err != nil {
		return nil, err
	}
	result := &EnrollmentInspection{
		APIVersion: APIVersion, Kind: EnrollmentInspectionKind, Status: EnrollmentNotEnrolled,
		NextAction: EnrollmentActionInitialize, NextCommand: "sudo tako platform init",
		MembershipComparison: MembershipComparisonNotApplicable,
	}

	binding, err := nodeidentity.ReadLocalBinding(paths.LocalBindingPath)
	if errors.Is(err, os.ErrNotExist) {
		binding = nil
	} else if err != nil {
		return nil, fmt.Errorf("read trusted local node binding: %w", err)
	}

	installation, identityErr := nodeidentity.ReadOptional(paths.IdentityPath)
	identityPermissionLimited := errors.Is(identityErr, os.ErrPermission)
	if identityErr != nil && !identityPermissionLimited {
		return nil, fmt.Errorf("read protected installation identity: %w", identityErr)
	}

	var inventory *nodeidentity.ClusterInventory
	inventoryExists, err := pathExists(paths.InventoryPath)
	if err != nil {
		return nil, fmt.Errorf("inspect trusted cluster inventory: %w", err)
	}
	if inventoryExists {
		inventory, err = nodeidentity.ReadInventory(paths.InventoryPath)
		if err != nil {
			return nil, fmt.Errorf("read trusted cluster inventory: %w", err)
		}
	}

	residual, err := existingArtifactPaths(map[string]string{
		"platform config": paths.ConfigPath, "controller membership": paths.MembershipPath, "platform worker unit": paths.WorkerUnitPath,
		"platform mesh key directory": paths.MeshKeyDir,
	})
	if err != nil {
		return nil, err
	}
	if binding == nil && installation == nil && !identityPermissionLimited {
		if inventory != nil || len(residual) > 0 {
			result.ResidualArtifacts = append([]string(nil), residual...)
			if inventory != nil {
				result.ResidualArtifacts = append(result.ResidualArtifacts, paths.InventoryPath)
			}
			result.Status = EnrollmentIncomplete
			result.NextAction = EnrollmentActionRecoverState
			result.NextCommand = ""
			result.Detail = "platform state exists without a usable installation identity; do not initialize over it"
			return result, nil
		}
		result.Detail = "no platform enrollment or residual controller artifacts were found"
		return result, nil
	}

	if binding != nil {
		result.ClusterID, result.NodeID, result.NodeName, result.WorkerUID = binding.ClusterID, binding.NodeID, binding.NodeName, binding.WorkerUID
	}
	if installation != nil {
		result.IdentityVerification = IdentityVerificationVerified
		result.EnrollmentRoles = append([]string(nil), installation.EnrollmentRoles...)
		if binding != nil && (!installation.Matches(binding.ClusterID, binding.NodeID) || installation.NodeName != binding.NodeName) {
			return nil, fmt.Errorf("protected installation identity does not match the public local node binding")
		}
		result.ClusterID, result.NodeID, result.NodeName = installation.ClusterID, installation.NodeID, installation.NodeName
	} else if identityPermissionLimited {
		if binding == nil {
			return nil, fmt.Errorf("installation identity exists but cannot be read and its public local binding is missing; rerun with sudo")
		}
		result.IdentityVerification = IdentityVerificationPermission
		result.Warnings = append(result.Warnings, "protected installation identity was not verified because this process lacks permission; rerun with sudo for a complete inspection")
	} else {
		result.Status = EnrollmentIncomplete
		result.NextAction = EnrollmentActionRecoverState
		result.NextCommand = ""
		result.Detail = "the public local binding exists, but the protected installation identity is missing"
		return result, nil
	}

	if binding == nil {
		// Identity creation currently follows account setup, so an identity-only
		// crash does not preserve custom account or policy choices. It cannot be
		// resumed safely without an explicit recovery workflow.
		if inventory == nil && len(residual) == 0 && slices.Equal(result.EnrollmentRoles, firstNodeRoles) {
			return identityOnlyInterruptedBootstrap(result), nil
		}
		if inventory != nil {
			if !strings.EqualFold(inventory.ClusterID, result.ClusterID) {
				return nil, fmt.Errorf("installation identity and cluster inventory identify different clusters")
			}
			localNode, found := inventory.Node(result.NodeID)
			if !found || localNode.NodeName != result.NodeName {
				return nil, fmt.Errorf("installation identity is absent from or disagrees with trusted cluster inventory")
			}
			populatePublishedInspection(result, inventory, localNode)
		}
		return incompleteWithoutBinding(result), nil
	}
	if inventory == nil {
		return incompleteWithoutInventory(result), nil
	}
	if !strings.EqualFold(inventory.ClusterID, result.ClusterID) {
		return nil, fmt.Errorf("local node identity and cluster inventory identify different clusters")
	}
	localNode, ok := inventory.Node(result.NodeID)
	if !ok {
		return nil, fmt.Errorf("local node %s is absent from the trusted cluster inventory", result.NodeID)
	}
	if result.NodeName != localNode.NodeName {
		return nil, fmt.Errorf("local node name %q does not match inventory name %q", result.NodeName, localNode.NodeName)
	}
	meshPrivate, meshErr := readMeshPrivateKey(filepath.Join(paths.MeshKeyDir, "privatekey"))
	if errors.Is(meshErr, os.ErrNotExist) {
		return incompleteWithoutMeshIdentity(result), nil
	}
	if errors.Is(meshErr, os.ErrPermission) {
		result.Warnings = append(result.Warnings, "protected platform mesh identity was not verified because this process lacks permission; rerun with sudo for a complete inspection")
	} else if meshErr != nil {
		return nil, fmt.Errorf("read protected platform mesh identity: %w", meshErr)
	} else {
		meshPublic, deriveErr := meshPublicKeyFromPrivate(meshPrivate)
		if deriveErr != nil {
			return nil, fmt.Errorf("derive protected platform mesh identity: %w", deriveErr)
		}
		if meshPublic != localNode.MeshPublicKey {
			return incompleteWithConflictingMeshIdentity(result), nil
		}
	}
	if inventory.ControllerNodeID == "" || inventory.Generation == 0 {
		result.Status = EnrollmentIncomplete
		result.NextAction = actionForIncompleteIdentity(result.EnrollmentRoles)
		result.Detail = "the cluster inventory has no authoritative controller generation"
		return result, nil
	}

	populatePublishedInspection(result, inventory, localNode)
	result.Status = EnrollmentEnrolled
	result.NextAction = EnrollmentActionUseExisting
	result.NextCommand = ""
	result.Detail = "local identity agrees with the last trusted inventory snapshot; controller reachability was not tested"
	result.Warnings = append(result.Warnings, "published roles, lifecycle, and controller come from a local inventory snapshot and do not prove current controller reachability")

	if result.LocalController {
		configExists := slices.Contains(residual, paths.ConfigPath)
		membershipExists := slices.Contains(residual, paths.MembershipPath)
		workerUnitExists := slices.Contains(residual, paths.WorkerUnitPath)
		if !membershipExists {
			result.Status = EnrollmentIncomplete
			result.NextAction = EnrollmentActionRecoverState
			result.NextCommand = ""
			result.Detail = "this node is published as controller, but protected membership is missing; recover authoritative state before running init"
			return result, nil
		}
		state, stateErr := readMembershipState(paths.MembershipPath)
		if errors.Is(stateErr, os.ErrPermission) {
			result.MembershipComparison = MembershipComparisonPermission
			result.Warnings = append(result.Warnings, "protected controller membership was not compared because this process lacks permission; rerun with sudo")
		} else if stateErr != nil {
			return nil, fmt.Errorf("read protected controller membership: %w", stateErr)
		} else {
			if err := state.Validate(time.Now().UTC()); err != nil {
				return nil, fmt.Errorf("validate protected controller membership: %w", err)
			}
			if state.ClusterID != result.ClusterID || state.ControllerNodeID != result.NodeID {
				return nil, fmt.Errorf("protected controller membership belongs to another cluster member")
			}
			result.MembershipGeneration = state.Generation
			if inventoriesExactlyMatch(state.Inventory(), *inventory) {
				result.MembershipComparison = MembershipComparisonMatches
			} else {
				result.MembershipComparison = MembershipComparisonStale
				result.Warnings = append(result.Warnings, fmt.Sprintf("published inventory generation %d does not exactly match protected membership generation %d", inventory.Generation, state.Generation))
			}
		}
		if result.MembershipComparison == MembershipComparisonStale {
			result.Status = EnrollmentIncomplete
			result.NextAction = EnrollmentActionRecoverState
			result.NextCommand = ""
			result.Detail = "protected controller membership and the published inventory differ; recover or republish the authoritative snapshot before running init"
			return result, nil
		}
		if !configExists || !workerUnitExists {
			if result.MembershipComparison != MembershipComparisonMatches {
				result.Status = EnrollmentIncomplete
				result.NextAction = EnrollmentActionRecoverState
				result.NextCommand = ""
				result.Detail = "controller runtime artifacts are missing and protected membership could not be proven equal to the published inventory"
				return result, nil
			}
			if !configExists {
				result.Status = EnrollmentIncomplete
				result.NextAction = EnrollmentActionRecoverState
				result.NextCommand = ""
				result.Detail = "protected platform configuration is missing; persisted account, service-path, and resource-policy settings must be recovered before running init"
				return result, nil
			}
			result.Status = EnrollmentIncomplete
			result.NextAction = EnrollmentActionResumeInit
			result.NextCommand = resumeInitCommand(result)
			result.Detail = "protected membership exactly matches the published inventory and platform configuration is present, but the platform worker service is missing"
			return result, nil
		}
	}
	return result, nil
}

func incompleteWithoutMeshIdentity(result *EnrollmentInspection) *EnrollmentInspection {
	result.Status = EnrollmentIncomplete
	result.NextAction = actionForIncompleteIdentity(result.EnrollmentRoles)
	result.NextCommand = ""
	result.Detail = "immutable node identity and published inventory exist, but the protected platform mesh private key is missing; recover the existing credential instead of generating a replacement"
	return result
}

func incompleteWithConflictingMeshIdentity(result *EnrollmentInspection) *EnrollmentInspection {
	result.Status = EnrollmentIncomplete
	result.NextAction = actionForIncompleteIdentity(result.EnrollmentRoles)
	result.NextCommand = ""
	result.Detail = "the protected platform mesh private key does not match this node's published mesh identity; recover the enrolled credential before mutation"
	return result
}

func inventoriesExactlyMatch(left, right nodeidentity.ClusterInventory) bool {
	canonicalize := func(inventory nodeidentity.ClusterInventory) nodeidentity.ClusterInventory {
		inventory.Nodes = append([]nodeidentity.InventoryNode(nil), inventory.Nodes...)
		for index := range inventory.Nodes {
			inventory.Nodes[index].Roles = append([]string(nil), inventory.Nodes[index].Roles...)
			slices.Sort(inventory.Nodes[index].Roles)
		}
		sort.Slice(inventory.Nodes, func(i, j int) bool { return inventory.Nodes[i].NodeID < inventory.Nodes[j].NodeID })
		inventory.Tombstones = append([]nodeidentity.NodeTombstone(nil), inventory.Tombstones...)
		sort.Slice(inventory.Tombstones, func(i, j int) bool { return inventory.Tombstones[i].NodeID < inventory.Tombstones[j].NodeID })
		inventory.ActiveAllocations = append([]nodeidentity.ActiveAllocation(nil), inventory.ActiveAllocations...)
		sort.Slice(inventory.ActiveAllocations, func(i, j int) bool {
			if inventory.ActiveAllocations[i].NodeID != inventory.ActiveAllocations[j].NodeID {
				return inventory.ActiveAllocations[i].NodeID < inventory.ActiveAllocations[j].NodeID
			}
			return inventory.ActiveAllocations[i].Key < inventory.ActiveAllocations[j].Key
		})
		return inventory
	}
	return reflect.DeepEqual(canonicalize(left), canonicalize(right))
}

func identityOnlyInterruptedBootstrap(result *EnrollmentInspection) *EnrollmentInspection {
	result.Status = EnrollmentIncomplete
	result.NextAction = EnrollmentActionRecoverState
	result.NextCommand = ""
	result.Detail = "only the protected first-controller identity exists; prior account or policy choices may not have been persisted, so recover state instead of resuming init"
	return result
}

func incompleteWithoutBinding(result *EnrollmentInspection) *EnrollmentInspection {
	result.Status = EnrollmentIncomplete
	result.NextAction = EnrollmentActionRecoverState
	result.NextCommand = ""
	if slices.Equal(result.EnrollmentRoles, []string{nodeidentity.RoleWorker}) {
		result.NextAction = EnrollmentActionRepairWorker
	}
	result.Detail = "the protected installation identity exists, but the public local node binding is missing; existing published state must be repaired, not reinitialized"
	return result
}

func incompleteWithoutInventory(result *EnrollmentInspection) *EnrollmentInspection {
	result.Status = EnrollmentIncomplete
	result.NextAction = EnrollmentActionRecoverState
	result.NextCommand = ""
	if slices.Equal(result.EnrollmentRoles, []string{nodeidentity.RoleWorker}) {
		result.NextAction = EnrollmentActionRepairWorker
	}
	result.Detail = "immutable node identity and binding exist, but the trusted cluster inventory is missing; recover or republish it instead of reinitializing"
	return result
}

func actionForIncompleteIdentity(roles []string) string {
	if slices.Equal(roles, []string{nodeidentity.RoleWorker}) {
		return EnrollmentActionRepairWorker
	}
	return EnrollmentActionRecoverState

}

func populatePublishedInspection(result *EnrollmentInspection, inventory *nodeidentity.ClusterInventory, localNode nodeidentity.InventoryNode) {
	result.PublishedRoles = append([]string(nil), localNode.Roles...)
	result.PublishedLifecycle = localNode.Lifecycle
	result.PublishedSchedulable = localNode.Schedulable
	result.InventoryGeneration = inventory.Generation
	updatedAt := inventory.UpdatedAt
	result.InventoryUpdatedAt = &updatedAt
	result.PublishedControllerID = inventory.ControllerNodeID
	if controller, found := inventory.Node(inventory.ControllerNodeID); found {
		result.PublishedControllerName = controller.NodeName
	}
	result.LocalController = strings.EqualFold(inventory.ControllerNodeID, result.NodeID) && slices.Contains(localNode.Roles, nodeidentity.RoleControlPlane)
}

func resumeInitCommand(result *EnrollmentInspection) string {
	return fmt.Sprintf("sudo tako platform init --node %s --cluster-id %s --node-id %s", result.NodeName, result.ClusterID, result.NodeID)
}

func existingArtifactPaths(paths map[string]string) ([]string, error) {
	var found []string
	for label, path := range paths {
		exists, err := pathExists(path)
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", label, err)
		}
		if exists {
			found = append(found, path)
		}
	}
	slices.Sort(found)
	return found, nil
}

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// ExistingEnrollmentError explains why platform init refused to reinterpret a
// create-once node identity as a new first controller.
type ExistingEnrollmentError struct {
	ClusterID          string
	NodeID             string
	NodeName           string
	EnrollmentRoles    []string
	PublishedRoles     []string
	PublishedLifecycle string
	ControllerNodeID   string
	ControllerNodeName string
	Reason             string
}

func (e *ExistingEnrollmentError) Error() string {
	if e == nil {
		return "platform init refused an existing enrollment"
	}
	state := ""
	if len(e.PublishedRoles) > 0 || e.PublishedLifecycle != "" {
		state = fmt.Sprintf("; published roles %s, lifecycle %s", strings.Join(e.PublishedRoles, ","), e.PublishedLifecycle)
	}
	controller := ""
	if e.ControllerNodeID != "" {
		name := e.ControllerNodeName
		if name == "" {
			name = "published controller"
		}
		controller = fmt.Sprintf("; last published controller %s (%s)", name, e.ControllerNodeID)
	}
	return fmt.Sprintf("platform init refused: this server is already enrolled as %s (%s) in cluster %s with enrollment roles %s%s%s; %s. Run 'tako platform inspect' for the complete read-only diagnosis. Running Tako here does not promote this node. Use another server or VM for an independent cluster; Tako will not replace this identity automatically",
		e.NodeName, e.NodeID, e.ClusterID, strings.Join(e.EnrollmentRoles, ","), state, controller, strings.TrimSuffix(strings.TrimSpace(e.Reason), "."))
}

func existingEnrollmentError(installation *nodeidentity.Installation, reason string) error {
	if installation == nil {
		return fmt.Errorf("platform init refused: %s", reason)
	}
	return &ExistingEnrollmentError{
		ClusterID: installation.ClusterID, NodeID: installation.NodeID, NodeName: installation.NodeName,
		EnrollmentRoles: append([]string(nil), installation.EnrollmentRoles...), Reason: reason,
	}
}

func existingEnrollmentErrorWithInventory(installation *nodeidentity.Installation, inventoryPath, reason string) error {
	err := existingEnrollmentError(installation, reason)
	conflict, ok := err.(*ExistingEnrollmentError)
	if !ok || installation == nil {
		return err
	}
	inventory, readErr := nodeidentity.ReadInventory(inventoryPath)
	if readErr != nil || !strings.EqualFold(inventory.ClusterID, installation.ClusterID) {
		return conflict
	}
	localNode, found := inventory.Node(installation.NodeID)
	if !found {
		return conflict
	}
	conflict.PublishedRoles = append([]string(nil), localNode.Roles...)
	conflict.PublishedLifecycle = localNode.Lifecycle
	conflict.ControllerNodeID = inventory.ControllerNodeID
	if controller, found := inventory.Node(inventory.ControllerNodeID); found {
		conflict.ControllerNodeName = controller.NodeName
	}
	return conflict
}
