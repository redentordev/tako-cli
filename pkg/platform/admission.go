package platform

import (
	"context"
	"fmt"
	"sync"
)

type DiskProbe interface {
	AvailableBytes(path string) (int64, error)
}

type AdmissionController struct {
	policy ResourcePolicy
	disk   DiskProbe
	path   string
	ops    chan struct{}
	builds chan struct{}
}

func NewAdmissionController(policy ResourcePolicy, disk DiskProbe, path string) (*AdmissionController, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if disk == nil || path == "" {
		return nil, fmt.Errorf("disk probe and admission path are required")
	}
	return &AdmissionController{
		policy: policy,
		disk:   disk,
		path:   path,
		ops:    make(chan struct{}, policy.MaximumConcurrentOps),
		builds: make(chan struct{}, policy.MaximumConcurrentBuilds),
	}, nil
}

func (a *AdmissionController) Admit(ctx context.Context, effect OperationEffect) (func(), error) {
	if effect.DiskGrowth {
		available, err := a.disk.AvailableBytes(a.path)
		if err != nil {
			return nil, fmt.Errorf("probe free disk: %w", err)
		}
		if available < a.policy.MinimumFreeDiskBytes {
			return nil, fmt.Errorf("operation denied: %d free bytes is below %d-byte platform minimum", available, a.policy.MinimumFreeDiskBytes)
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case a.ops <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	releaseOps := sync.OnceFunc(func() { <-a.ops })
	if !effect.Build {
		return releaseOps, nil
	}
	select {
	case a.builds <- struct{}{}:
		return sync.OnceFunc(func() { <-a.builds; releaseOps() }), nil
	case <-ctx.Done():
		releaseOps()
		return nil, ctx.Err()
	}
}
