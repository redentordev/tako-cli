package platform

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

func inspectionPaths(root, inventoryPath string) EnrollmentInspectionPaths {
	return EnrollmentInspectionPaths{
		IdentityPath: filepath.Join(root, "identity.json"), LocalBindingPath: filepath.Join(root, "local-node.json"), InventoryPath: inventoryPath,
		ConfigPath: filepath.Join(root, "platform.json"), MembershipPath: filepath.Join(root, "membership.json"), WorkerUnitPath: filepath.Join(root, "tako-platform-worker.service"),
		MeshKeyDir: filepath.Join(root, "wireguard"),
	}
}

func writeInspectionIdentityAndBinding(t *testing.T, paths EnrollmentInspectionPaths, identity *nodeidentity.Installation) {
	t.Helper()
	if err := nodeidentity.Create(paths.IdentityPath, *identity); err != nil {
		t.Fatal(err)
	}
	if err := nodeidentity.WriteLocalBinding(paths.LocalBindingPath, nodeidentity.LocalBinding{
		APIVersion: nodeidentity.InventoryAPIVersion, Kind: nodeidentity.LocalBindingKind,
		ClusterID: identity.ClusterID, NodeID: identity.NodeID, NodeName: identity.NodeName, WorkerUID: 1001,
	}); err != nil {
		t.Fatal(err)
	}
	seed := byte(1)
	if slicesContains(identity.EnrollmentRoles, nodeidentity.RoleWorker) && !slicesContains(identity.EnrollmentRoles, nodeidentity.RoleControlPlane) {
		seed = 2
	}
	private := make([]byte, 32)
	for index := range private {
		private[index] = seed
	}
	if err := os.MkdirAll(paths.MeshKeyDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.MeshKeyDir, "privatekey"), []byte(base64.StdEncoding.EncodeToString(private)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestInspectEnrollmentRequiresPublishedMeshIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(EnrollmentInspectionPaths) error
	}{
		{name: "missing-directory", mutate: func(paths EnrollmentInspectionPaths) error { return os.RemoveAll(paths.MeshKeyDir) }},
		{name: "missing-private-key", mutate: func(paths EnrollmentInspectionPaths) error {
			return os.Remove(filepath.Join(paths.MeshKeyDir, "privatekey"))
		}},
		{name: "mismatched-private-key", mutate: func(paths EnrollmentInspectionPaths) error {
			private := make([]byte, 32)
			for index := range private {
				private[index] = 9
			}
			return os.WriteFile(filepath.Join(paths.MeshKeyDir, "privatekey"), []byte(base64.StdEncoding.EncodeToString(private)+"\n"), 0600)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, controller, _ := newTestMembershipStore(t)
			root := t.TempDir()
			paths := inspectionPaths(root, store.inventoryPath)
			paths.MembershipPath = store.path
			writeInspectionIdentityAndBinding(t, paths, controller)
			for _, path := range []string{paths.ConfigPath, paths.WorkerUnitPath} {
				if err := os.WriteFile(path, []byte("present"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := test.mutate(paths); err != nil {
				t.Fatal(err)
			}
			result, err := InspectEnrollment(paths)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != EnrollmentIncomplete || result.NextAction != EnrollmentActionRecoverState || result.NextCommand != "" || !strings.Contains(result.Detail, "mesh") {
				t.Fatalf("mesh identity loss accepted: %#v", result)
			}
		})
	}
}

func TestInspectEnrollmentReportsUnenrolledHost(t *testing.T) {
	root := t.TempDir()
	result, err := InspectEnrollment(inspectionPaths(root, filepath.Join(root, "inventory.json")))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != EnrollmentNotEnrolled || result.NextAction != EnrollmentActionInitialize || result.NextCommand != "sudo tako platform init" || result.ClusterID != "" || result.LocalController {
		t.Fatalf("unexpected inspection: %#v", result)
	}
}

func TestInspectEnrollmentReportsWorkerAndLastPublishedController(t *testing.T) {
	store, controller, now := newTestMembershipStore(t)
	issued, err := store.CreateJoinToken(membershipWorkerID, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	request := testEnrollment(t, store, issued.Token, membershipWorkerID, now)
	if _, err := store.EnrollWorker(request); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkReady(membershipWorkerID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkSchedulable(membershipWorkerID); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	paths := inspectionPaths(root, store.inventoryPath)
	worker, err := nodeidentity.New(controller.ClusterID, membershipWorkerID, "worker-2", []string{nodeidentity.RoleWorker}, now)
	if err != nil {
		t.Fatal(err)
	}
	writeInspectionIdentityAndBinding(t, paths, worker)
	result, err := InspectEnrollment(paths)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != EnrollmentEnrolled || result.LocalController || result.PublishedControllerID != controller.NodeID || result.PublishedControllerName != controller.NodeName || result.PublishedLifecycle != nodeidentity.NodeLifecycleSchedulable || !result.PublishedSchedulable || strings.Join(result.PublishedRoles, ",") != nodeidentity.RoleWorker || result.IdentityVerification != IdentityVerificationVerified || result.InventoryUpdatedAt == nil || result.InventoryUpdatedAt.IsZero() || result.NextAction != EnrollmentActionUseExisting || len(result.Warnings) == 0 {
		t.Fatalf("unexpected worker inspection: %#v", result)
	}
}

func TestInspectEnrollmentIdentityOnlyControllerRequiresRecovery(t *testing.T) {
	root := t.TempDir()
	paths := inspectionPaths(root, filepath.Join(root, "missing-inventory.json"))
	identity, err := nodeidentity.New("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "node-1", firstNodeRoles, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := nodeidentity.Create(paths.IdentityPath, *identity); err != nil {
		t.Fatal(err)
	}
	result, err := InspectEnrollment(paths)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != EnrollmentIncomplete || result.NextAction != EnrollmentActionRecoverState || result.NextCommand != "" || !strings.Contains(result.Detail, "policy choices") {
		t.Fatalf("unexpected interrupted-init inspection: %#v", result)
	}
}

func TestInspectEnrollmentIdentityOnlyWorkerRequiresControllerRepair(t *testing.T) {
	root := t.TempDir()
	paths := inspectionPaths(root, filepath.Join(root, "missing-inventory.json"))
	identity, err := nodeidentity.New("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "worker-2", []string{nodeidentity.RoleWorker}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := nodeidentity.Create(paths.IdentityPath, *identity); err != nil {
		t.Fatal(err)
	}
	result, err := InspectEnrollment(paths)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != EnrollmentIncomplete || result.NextAction != EnrollmentActionRepairWorker || result.NextCommand != "" {
		t.Fatalf("worker repair inspection: %#v", result)
	}
}

func TestInspectEnrollmentRejectsProtectedIdentityBindingMismatch(t *testing.T) {
	root := t.TempDir()
	paths := inspectionPaths(root, filepath.Join(root, "inventory.json"))
	identity, err := nodeidentity.New("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "worker-2", []string{nodeidentity.RoleWorker}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := nodeidentity.Create(paths.IdentityPath, *identity); err != nil {
		t.Fatal(err)
	}
	if err := nodeidentity.WriteLocalBinding(paths.LocalBindingPath, nodeidentity.LocalBinding{
		APIVersion: nodeidentity.InventoryAPIVersion, Kind: nodeidentity.LocalBindingKind,
		ClusterID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", NodeID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", NodeName: "other",
	}); err != nil {
		t.Fatal(err)
	}
	_, err = InspectEnrollment(paths)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("identity/binding mismatch error = %v", err)
	}
}

func TestInspectEnrollmentRejectsUnsafeProtectedIdentityWithPublicState(t *testing.T) {
	root := t.TempDir()
	paths := inspectionPaths(root, filepath.Join(root, "inventory.json"))
	if err := os.Symlink(filepath.Join(root, "elsewhere"), paths.IdentityPath); err != nil {
		t.Fatal(err)
	}
	if err := nodeidentity.WriteLocalBinding(paths.LocalBindingPath, nodeidentity.LocalBinding{
		APIVersion: nodeidentity.InventoryAPIVersion, Kind: nodeidentity.LocalBindingKind,
		ClusterID: "11111111-1111-4111-8111-111111111111", NodeID: "22222222-2222-4222-8222-222222222222", NodeName: "worker-2",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := InspectEnrollment(paths)
	if err == nil || !strings.Contains(err.Error(), "protected installation identity") {
		t.Fatalf("unsafe identity error = %v", err)
	}
}

func TestInspectEnrollmentRejectsMalformedProtectedIdentityWithPublicState(t *testing.T) {
	root := t.TempDir()
	paths := inspectionPaths(root, filepath.Join(root, "inventory.json"))
	if err := os.WriteFile(paths.IdentityPath, []byte("{not-json\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := nodeidentity.WriteLocalBinding(paths.LocalBindingPath, nodeidentity.LocalBinding{
		APIVersion: nodeidentity.InventoryAPIVersion, Kind: nodeidentity.LocalBindingKind,
		ClusterID: "11111111-1111-4111-8111-111111111111", NodeID: "22222222-2222-4222-8222-222222222222", NodeName: "worker-2",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := InspectEnrollment(paths)
	if err == nil || !strings.Contains(err.Error(), "protected installation identity") || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("malformed identity error = %v", err)
	}
}

func TestInspectEnrollmentTreatsMissingProtectedIdentityAsIncomplete(t *testing.T) {
	root := t.TempDir()
	paths := inspectionPaths(root, filepath.Join(root, "missing-inventory.json"))
	if err := nodeidentity.WriteLocalBinding(paths.LocalBindingPath, nodeidentity.LocalBinding{
		APIVersion: nodeidentity.InventoryAPIVersion, Kind: nodeidentity.LocalBindingKind,
		ClusterID: "11111111-1111-4111-8111-111111111111", NodeID: "22222222-2222-4222-8222-222222222222", NodeName: "node-1",
	}); err != nil {
		t.Fatal(err)
	}
	result, err := InspectEnrollment(paths)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != EnrollmentIncomplete || result.NextAction != EnrollmentActionRecoverState || !strings.Contains(result.Detail, "identity is missing") {
		t.Fatalf("missing identity result = %#v", result)
	}
}

func TestInspectEnrollmentDetectsResidualStateWithoutIdentity(t *testing.T) {
	for _, artifact := range []string{"config", "membership", "worker-unit", "mesh-key"} {
		t.Run(artifact, func(t *testing.T) {
			root := t.TempDir()
			paths := inspectionPaths(root, filepath.Join(root, "missing-inventory.json"))
			path := map[string]string{"config": paths.ConfigPath, "membership": paths.MembershipPath, "worker-unit": paths.WorkerUnitPath, "mesh-key": paths.MeshKeyDir}[artifact]
			if artifact == "mesh-key" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte("residual"), 0600); err != nil {
				t.Fatal(err)
			}
			result, err := InspectEnrollment(paths)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != EnrollmentIncomplete || result.NextAction != EnrollmentActionRecoverState || !slicesContains(result.ResidualArtifacts, path) {
				t.Fatalf("residual inspection = %#v", result)
			}
		})
	}
}

func TestInspectEnrollmentComparesControllerMembershipAndSnapshot(t *testing.T) {
	store, controller, now := newTestMembershipStore(t)
	root := t.TempDir()
	paths := inspectionPaths(root, store.inventoryPath)
	paths.MembershipPath = store.path
	writeInspectionIdentityAndBinding(t, paths, controller)
	for _, path := range []string{paths.ConfigPath, paths.WorkerUnitPath} {
		if err := os.WriteFile(path, []byte("present"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	result, err := InspectEnrollment(paths)
	if err != nil {
		t.Fatal(err)
	}
	if !result.LocalController || result.MembershipComparison != MembershipComparisonMatches || result.MembershipGeneration != result.InventoryGeneration || result.InventoryUpdatedAt == nil || !result.InventoryUpdatedAt.Equal(now) {
		t.Fatalf("controller inspection = %#v", result)
	}

	state, err := readMembershipState(store.path)
	if err != nil {
		t.Fatal(err)
	}
	state.Generation++
	state.UpdatedAt = now.Add(time.Second)
	if err := writeMembershipState(store.path, state); err != nil {
		t.Fatal(err)
	}
	result, err = InspectEnrollment(paths)
	if err != nil {
		t.Fatal(err)
	}
	if result.MembershipComparison != MembershipComparisonStale || result.Status != EnrollmentIncomplete || result.NextAction != EnrollmentActionRecoverState || len(result.Warnings) < 2 {
		t.Fatalf("stale snapshot not surfaced: %#v", result)
	}
}

func TestInspectEnrollmentRejectsSameGenerationInventoryDivergence(t *testing.T) {
	store, controller, _ := newTestMembershipStore(t)
	root := t.TempDir()
	paths := inspectionPaths(root, store.inventoryPath)
	paths.MembershipPath = store.path
	writeInspectionIdentityAndBinding(t, paths, controller)
	for _, path := range []string{paths.ConfigPath, paths.WorkerUnitPath} {
		if err := os.WriteFile(path, []byte("present"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	state, err := readMembershipState(store.path)
	if err != nil {
		t.Fatal(err)
	}
	state.Nodes[0].MeshEndpoint = "divergent.example"
	if err := writeMembershipState(store.path, state); err != nil {
		t.Fatal(err)
	}
	result, err := InspectEnrollment(paths)
	if err != nil {
		t.Fatal(err)
	}
	if result.MembershipComparison != MembershipComparisonStale || result.Status != EnrollmentIncomplete || result.NextAction != EnrollmentActionRecoverState || result.NextCommand != "" {
		t.Fatalf("same-generation divergence was accepted: %#v", result)
	}
}

func TestInspectEnrollmentNeverResumesWhenAuthoritativeMembershipIsMissing(t *testing.T) {
	store, controller, _ := newTestMembershipStore(t)
	root := t.TempDir()
	paths := inspectionPaths(root, store.inventoryPath)
	paths.MembershipPath = filepath.Join(root, "missing-membership.json")
	writeInspectionIdentityAndBinding(t, paths, controller)
	for _, path := range []string{paths.ConfigPath, paths.WorkerUnitPath} {
		if err := os.WriteFile(path, []byte("present"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	result, err := InspectEnrollment(paths)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != EnrollmentIncomplete || result.NextAction != EnrollmentActionRecoverState || result.NextCommand != "" || !strings.Contains(result.Detail, "membership is missing") {
		t.Fatalf("missing mature membership was treated as resumable: %#v", result)
	}
}

func TestInspectEnrollmentFormerControllerWithoutBindingCannotResume(t *testing.T) {
	store, formerController, now := newTestMembershipStore(t)
	issued, err := store.CreateJoinToken(membershipWorkerID, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	request := testEnrollment(t, store, issued.Token, membershipWorkerID, now)
	if _, err := store.EnrollWorker(request); err != nil {
		t.Fatal(err)
	}
	inventory, err := nodeidentity.ReadInventory(store.inventoryPath)
	if err != nil {
		t.Fatal(err)
	}
	inventory.ControllerNodeID = membershipWorkerID
	for index := range inventory.Nodes {
		if inventory.Nodes[index].NodeID == formerController.NodeID {
			inventory.Nodes[index].Roles = []string{nodeidentity.RoleWorker}
		}
		if inventory.Nodes[index].NodeID == membershipWorkerID {
			inventory.Nodes[index].Roles = firstNodeRoles
		}
	}
	data, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.inventoryPath, data, 0644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	paths := inspectionPaths(root, store.inventoryPath)
	if err := nodeidentity.Create(paths.IdentityPath, *formerController); err != nil {
		t.Fatal(err)
	}
	result, err := InspectEnrollment(paths)
	if err != nil {
		t.Fatal(err)
	}
	if result.NextAction == EnrollmentActionResumeInit || result.NextCommand != "" || result.PublishedControllerID != membershipWorkerID {
		t.Fatalf("former controller was treated as resumable: %#v", result)
	}
}

func TestInspectEnrollmentResumesOnlyMissingRuntimeWithMatchingMembership(t *testing.T) {
	store, controller, _ := newTestMembershipStore(t)
	root := t.TempDir()
	paths := inspectionPaths(root, store.inventoryPath)
	paths.MembershipPath = store.path
	writeInspectionIdentityAndBinding(t, paths, controller)
	if err := os.WriteFile(paths.ConfigPath, []byte("present"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := InspectEnrollment(paths)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != EnrollmentIncomplete || result.NextAction != EnrollmentActionResumeInit || result.MembershipComparison != MembershipComparisonMatches || result.NextCommand == "" {
		t.Fatalf("matching membership runtime repair = %#v", result)
	}
}

func TestInspectEnrollmentDoesNotResumeWithoutPersistedPlatformConfig(t *testing.T) {
	store, controller, _ := newTestMembershipStore(t)
	root := t.TempDir()
	paths := inspectionPaths(root, store.inventoryPath)
	paths.MembershipPath = store.path
	writeInspectionIdentityAndBinding(t, paths, controller)
	result, err := InspectEnrollment(paths)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != EnrollmentIncomplete || result.NextAction != EnrollmentActionRecoverState || result.NextCommand != "" || !strings.Contains(result.Detail, "configuration is missing") {
		t.Fatalf("missing persisted config was treated as resumable: %#v", result)
	}
}

func TestInspectEnrollmentRejectsRelativePaths(t *testing.T) {
	_, err := InspectEnrollment(EnrollmentInspectionPaths{IdentityPath: "identity.json", LocalBindingPath: "/tmp/local-node.json", InventoryPath: "/tmp/inventory.json"})
	if err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("relative path error = %v", err)
	}
}

func TestExistingEnrollmentErrorIncludesTrustedControllerContext(t *testing.T) {
	store, controller, now := newTestMembershipStore(t)
	issued, err := store.CreateJoinToken(membershipWorkerID, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	request := testEnrollment(t, store, issued.Token, membershipWorkerID, now)
	if _, err := store.EnrollWorker(request); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkReady(membershipWorkerID); err != nil {
		t.Fatal(err)
	}
	worker, err := nodeidentity.New(controller.ClusterID, membershipWorkerID, "worker-2", []string{nodeidentity.RoleWorker}, now)
	if err != nil {
		t.Fatal(err)
	}
	message := existingEnrollmentErrorWithInventory(worker, store.inventoryPath, "worker enrollment cannot become a first controller").Error()
	for _, expected := range []string{"worker-2", worker.NodeID, worker.ClusterID, "published roles worker", "lifecycle ready", "last published controller node-1", controller.NodeID, "does not promote"} {
		if !strings.Contains(message, expected) {
			t.Fatalf("conflict lacks %q: %s", expected, message)
		}
	}
}

func slicesContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
