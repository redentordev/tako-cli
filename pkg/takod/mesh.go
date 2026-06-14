package takod

import (
	"context"
	"fmt"

	"github.com/redentordev/tako-cli/pkg/mesh"
)

type MeshKeyResponse struct {
	PublicKey string `json:"publicKey"`
}

type MeshApplyRequest struct {
	Config mesh.WireGuardConfig `json:"config"`
	Node   mesh.Node            `json:"node"`
	Peers  []mesh.Node          `json:"peers,omitempty"`
}

type MeshApplyResponse struct {
	Applied bool         `json:"applied"`
	Status  *mesh.Status `json:"status,omitempty"`
}

var (
	ensureMeshNodeKey = mesh.EnsureNodeKeysLocal
	applyMeshConfig   = mesh.ApplyLocal
	readMeshStatus    = mesh.ReadStatusLocal
)

func EnsureMeshKey(ctx context.Context) (*MeshKeyResponse, error) {
	publicKey, err := ensureMeshNodeKey(ctx, false)
	if err != nil {
		return nil, err
	}
	return &MeshKeyResponse{PublicKey: publicKey}, nil
}

func ReconcileMesh(ctx context.Context, request MeshApplyRequest) (*MeshApplyResponse, error) {
	if err := validateMeshApplyRequest(request); err != nil {
		return nil, err
	}
	status, err := applyMeshConfig(ctx, request.Node, request.Peers, request.Config, false)
	if err != nil {
		return nil, err
	}
	return &MeshApplyResponse{
		Applied: request.Config.Enabled,
		Status:  status,
	}, nil
}

func ReadMeshStatus(ctx context.Context, interfaceName string) (*mesh.Status, error) {
	if err := validateMeshStatusRequest(interfaceName); err != nil {
		return nil, err
	}
	return readMeshStatus(ctx, interfaceName)
}

func validateMeshApplyRequest(request MeshApplyRequest) error {
	if !request.Config.Enabled {
		return fmt.Errorf("mesh.enabled=false is not supported")
	}
	if _, err := mesh.ValidateInterfaceName(request.Config.Interface); err != nil {
		return err
	}
	if _, err := mesh.RenderConfigTemplate(request.Node, request.Peers, request.Config); err != nil {
		return err
	}
	return nil
}

func validateMeshStatusRequest(interfaceName string) error {
	if _, err := mesh.ValidateInterfaceName(interfaceName); err != nil {
		return err
	}
	return nil
}
