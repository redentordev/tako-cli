package takod

import (
	"context"

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
	return readMeshStatus(ctx, interfaceName)
}
