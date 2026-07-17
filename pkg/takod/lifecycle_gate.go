package takod

import (
	"net/http"
	"os"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/recovery"
)

type takodRoute struct {
	path    string
	handler http.HandlerFunc
}

// registeredRoutes is the single route registry used by both the server and
// lifecycle policy tests, preventing a newly added mutation from silently
// bypassing the enrolled-node gate.
func (s *Server) registeredRoutes() []takodRoute {
	return []takodRoute{
		{"/healthz", s.handleHealthz}, {"/v1/status", s.handleStatus}, {"/v1/actual", s.handleActual},
		{"/v1/reconcile-service", s.handleReconcileService}, {"/v1/service-files", s.handleServiceFiles}, {"/v1/service-files/check", s.handleServiceFilesCheck},
		{"/v1/remove-service", s.handleRemoveService}, {"/v1/proxy-file", s.handleProxyFile}, {"/v1/proxy", s.handleProxy},
		{"/v1/certs", s.handleProxyCertificates}, {"/v1/acme-dns", s.handleProxyACMEDNS}, {"/v1/ports/allocate", s.handlePortAllocate},
		{"/v1/cleanup", s.handleCleanup}, {"/v1/state", s.handleState}, {"/v1/lease", s.handleLease}, {"/v1/fence", s.handleFence}, {"/v1/env-bundle", s.handleEnvBundle},
		{"/v1/backups", s.handleBackups}, {"/v1/backups/restore", s.handleBackupRestore}, {"/v1/backups/cleanup", s.handleBackupCleanup},
		{"/v1/backup-schedule", s.handleBackupSchedule}, {"/v1/metadata", s.handleMetadata}, {"/v1/mesh/key", s.handleMeshKey},
		{"/v1/mesh/apply", s.handleMeshApply}, {"/v1/mesh/status", s.handleMeshStatus}, {"/v1/images/exists", s.handleImageExists},
		{"/v1/images/inspect", s.handleImageInspect}, {"/v1/images/export", s.handleImageExport}, {"/v1/images/import", s.handleImageImport},
		{"/v1/images/build", s.handleImageBuild}, {"/v1/platform", s.handlePlatform}, {"/v1/platform/inventory", s.handleInventoryAuthority}, {"/v1/platform/allocations/authorize", s.handleAllocationAuthorization}, {"/v1/platform/membership/reconcile", s.handleMembershipReconcile},
		{"/v1/logs", s.handleLogs}, {"/v1/exec", s.handleExec}, {"/v1/jobs", s.handleJobs}, {"/v1/jobs/apply", s.handleJobsApply},
		{"/v1/jobs/runs", s.handleJobRuns}, {"/v1/jobs/trigger", s.handleJobsTrigger}, {"/v1/stats", s.handleStats},
		{"/v1/metrics", s.handleMetrics}, {"/v1/access-logs", s.handleAccessLogs}, {"/v1/discovery/exports", s.handleDiscoveryExports},
	}
}

var lifecycleSafeMutationPaths = map[string]struct{}{
	// This controller-only endpoint repairs the membership publication and
	// withdraws invalid proxy routes. It must remain available during cordon.
	"/v1/platform/membership/reconcile": {},
	// Nodes authenticate this publication against the already trusted
	// controller key and refuse generation rollback or same-generation drift.
	"/v1/platform/inventory": {},
	"/v1/fence":              {},
	"/v1/lease":              {},
	"/v1/platform":           {},
	"/v1/mesh/key":           {},
	"/v1/mesh/apply":         {},
}

// enrolledLifecycleHandler is the last node-local enforcement boundary. App
// config or a stale scheduler cannot mutate workloads on a joining, ready,
// cordoned, draining, removed, or revoked node merely because SSH still works.
func (s *Server) enrolledLifecycleHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s == nil || s.installation == nil {
			next.ServeHTTP(w, r)
			return
		}
		unlockSnapshot, lockErr := recovery.AcquireMutationLock(s.dataDir)
		if lockErr != nil {
			http.Error(w, "platform snapshot authority is unavailable: "+lockErr.Error(), http.StatusServiceUnavailable)
			return
		}
		defer unlockSnapshot()
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		if _, safe := lifecycleSafeMutationPaths[r.URL.Path]; safe {
			next.ServeHTTP(w, r)
			return
		}
		if _, err := os.Lstat(nodeidentity.DeploymentDenyPath(s.inventoryFile)); err == nil {
			http.Error(w, "node has a durable lifecycle deployment deny latch", http.StatusConflict)
			return
		} else if !os.IsNotExist(err) {
			http.Error(w, "cannot verify lifecycle deployment deny latch: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		// Keep activation/revocation and membership replacement serialized with
		// the complete admitted mutation, not just its header check. Revocation
		// therefore cannot race an already-read active record and clear it while
		// the handler continues mutating under stale authority.
		workerFenceAdmissionMu.RLock()
		defer workerFenceAdmissionMu.RUnlock()
		inventory, err := nodeidentity.ReadInventory(s.inventoryFile)
		if err != nil {
			http.Error(w, "trusted cluster lifecycle is unavailable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		// A generation-zero inventory is the Phase 1-3 compatibility form. It
		// has no lifecycle authority and therefore preserves legacy behavior.
		if inventory.ControllerNodeID == "" || inventory.Generation == 0 {
			next.ServeHTTP(w, r)
			return
		}
		node, ok := inventory.Node(s.installation.NodeID)
		if inventory.ClusterID != s.installation.ClusterID || !ok || node.MeshCredentialStatus != nodeidentity.MeshCredentialActive {
			http.Error(w, "node is absent or revoked in authoritative membership", http.StatusForbidden)
			return
		}
		if node.Lifecycle != nodeidentity.NodeLifecycleSchedulable || !node.Schedulable {
			if (node.Lifecycle == nodeidentity.NodeLifecycleCordoned || node.Lifecycle == nodeidentity.NodeLifecycleDraining) && r.URL.Path == "/v1/reconcile-service" {
				fencedContext, fenceErr := s.validateMutationFence(r)
				if fence, ok := operationFenceFromContext(fencedContext); fenceErr == nil && ok && fence.Operation == "placement-apply" {
					next.ServeHTTP(w, r.WithContext(withPlacementCleanupOnly(fencedContext)))
					return
				}
			}
			http.Error(w, "node lifecycle is "+node.Lifecycle+"; workload mutations require schedulable", http.StatusConflict)
			return
		}
		fencedContext, err := s.validateMutationFence(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		next.ServeHTTP(w, r.WithContext(fencedContext))
	})
}
