package platform

import (
	"context"
	"strings"
	"testing"
	"time"
)

type fixedDiskProbe struct {
	available int64
	err       error
}

func (p fixedDiskProbe) AvailableBytes(string) (int64, error) { return p.available, p.err }

func TestAdmissionRejectsDiskPressureAndBoundsBuilds(t *testing.T) {
	policy := DefaultResourcePolicy()
	if _, err := NewAdmissionController(policy, fixedDiskProbe{available: policy.MinimumFreeDiskBytes - 1}, "/data"); err != nil {
		t.Fatal(err)
	}
	low, _ := NewAdmissionController(policy, fixedDiskProbe{available: policy.MinimumFreeDiskBytes - 1}, "/data")
	if _, err := low.Admit(context.Background(), OperationEffect{DiskGrowth: true}); err == nil || !strings.Contains(err.Error(), "below") {
		t.Fatalf("low-disk admission error = %v", err)
	}
	if release, err := low.Admit(context.Background(), OperationEffect{DiskGrowth: false}); err != nil {
		t.Fatalf("disk-freeing operation was denied: %v", err)
	} else {
		release()
	}

	controller, err := NewAdmissionController(policy, fixedDiskProbe{available: policy.MinimumFreeDiskBytes + 1}, "/data")
	if err != nil {
		t.Fatal(err)
	}
	release, err := controller.Admit(context.Background(), OperationEffect{DiskGrowth: true, Build: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := controller.Admit(ctx, OperationEffect{DiskGrowth: true, Build: true}); err == nil {
		t.Fatal("second concurrent build unexpectedly admitted")
	}
	release()
	if releaseAgain, err := controller.Admit(context.Background(), OperationEffect{DiskGrowth: true, Build: true}); err != nil {
		t.Fatalf("build was not admitted after release: %v", err)
	} else {
		releaseAgain()
	}
}
