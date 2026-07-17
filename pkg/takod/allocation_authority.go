package takod

import (
	"fmt"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

// Phase 4 deliberately keeps remote mesh proxy routes fail-closed. A node
// signature proves origin but not that an allocation is still current. Phase 6
// will add a controller challenge/fencing protocol before this capability is
// advertised; accepting self-presented proofs here would permit replay of an
// unseen historical generation.
func (s *Server) supportsAuthoritativeRemoteMeshRoutes() bool { return false }

func (s *Server) supportsMembershipController() bool {
	if s == nil || s.installation == nil {
		return false
	}
	inventory, err := nodeidentity.ReadInventory(s.inventoryFile)
	if err != nil || inventory.ClusterID != s.installation.ClusterID || inventory.ControllerNodeID != s.installation.NodeID || inventory.Generation == 0 {
		return false
	}
	node, ok := inventory.Node(s.installation.NodeID)
	return ok && hasInventoryRole(node.Roles, nodeidentity.RoleControlPlane) && node.MeshCredentialStatus == nodeidentity.MeshCredentialActive
}

func (s *Server) reconcileAllStoredProxyAuthority() error { return nil }

func (s *Server) authorizeProxyFileCandidate(name, content string) error {
	if _, err := validateProxyFileName(name); err != nil {
		return err
	}
	return s.authorizeProxyManifestAllocations(content)
}

func (s *Server) reconcileStoredProxyScope(_, _, _ string) error { return nil }

func (s *Server) authorizeProxyManifestAllocations(content string) error {
	manifest, err := ParseProxyRouteManifest(content)
	if err != nil {
		return err
	}
	for _, route := range manifest.Routes {
		destinations := append([]ProxyDestination(nil), route.Destinations...)
		if route.DynamicDomain != nil && route.DynamicDomain.Destination != nil {
			destinations = append(destinations, *route.DynamicDomain.Destination)
		}
		for _, destination := range destinations {
			if destination.Kind == ProxyDestinationMesh {
				return fmt.Errorf("remote mesh proxy routes are disabled until controller-observed allocation fencing is available")
			}
		}
	}
	return nil
}

func validateAuthorizedRemoteAllocation(_ *nodeidentity.ClusterInventory, _ ProxyDestination) error {
	return fmt.Errorf("remote mesh proxy routes are disabled until controller-observed allocation fencing is available")
}

func hasInventoryRole(roles []string, wanted string) bool {
	for _, role := range roles {
		if role == wanted {
			return true
		}
	}
	return false
}
