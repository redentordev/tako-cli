package takod

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/redentordev/tako-cli/pkg/platform"
)

type MembershipReconcileResponse struct {
	Generation    uint64 `json:"generation"`
	ProxyStopped  bool   `json:"proxyStopped"`
	InvalidRoutes int    `json:"invalidRoutes"`
}

func (s *Server) handleMembershipReconcile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.installation == nil {
		http.Error(w, "platform membership is unavailable on an unenrolled node", http.StatusConflict)
		return
	}
	workerFenceAdmissionMu.Lock()
	defer workerFenceAdmissionMu.Unlock()
	s.proxyAuthorityMu.Lock()
	defer s.proxyAuthorityMu.Unlock()
	invalidBefore, err := s.invalidStoredProxyRoutes()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	newStore := platform.NewMembershipStore
	if lifecycleMutationBarrierHeld(r.Context()) {
		newStore = platform.NewMembershipStoreWithinMutationBarrier
	}
	store, err := newStore(s.membershipFile, s.inventoryFile)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	state, err := store.Read()
	if err != nil {
		http.Error(w, "reconcile controller membership snapshot: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if state.ClusterID != s.installation.ClusterID || state.ControllerNodeID != s.installation.NodeID {
		http.Error(w, "this node is not the authoritative membership controller", http.StatusForbidden)
		return
	}
	if err := s.reconcileAllStoredProxyAuthority(); err != nil {
		if stopErr := stopProxyAndVerifyAbsent(r.Context()); stopErr != nil {
			http.Error(w, "reconcile stored allocation authority: "+err.Error()+"; fail-closed proxy stop also failed: "+stopErr.Error(), http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "reconcile stored allocation authority; proxy stopped fail-closed: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	state, err = store.Read()
	if err != nil {
		http.Error(w, "read reconciled membership: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	invalid, err := s.invalidStoredProxyRoutes()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	stopped := invalidBefore > 0
	if invalid > 0 {
		if err := stopProxyAndVerifyAbsent(r.Context()); err != nil {
			http.Error(w, "stop proxy after membership revocation: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(MembershipReconcileResponse{Generation: state.Generation, ProxyStopped: stopped, InvalidRoutes: invalidBefore})
}

func (s *Server) invalidStoredProxyRoutes() (int, error) {
	entries, err := os.ReadDir(proxyRoutesDir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("inspect stored proxy routes: %w", err)
	}
	invalid := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(proxyRoutesDir, entry.Name()))
		if err != nil {
			return 0, err
		}
		if err := validateEnrolledProxyRouteManifest(string(data), s.installation.ClusterID, s.inventoryFile); err != nil {
			invalid++
		}
	}
	return invalid, nil
}
