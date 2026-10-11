package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/takod"
)

func stubNodeMaintenanceLease(t *testing.T, fn func(context.Context, engine.NodeMaintenanceLeaseRequest) (*engine.NodeMaintenanceLease, error)) *[]engine.NodeMaintenanceLeaseRequest {
	t.Helper()
	var calls []engine.NodeMaintenanceLeaseRequest
	previous := acquireNodeMaintenanceLeaseWith
	acquireNodeMaintenanceLeaseWith = func(ctx context.Context, req engine.NodeMaintenanceLeaseRequest) (*engine.NodeMaintenanceLease, error) {
		calls = append(calls, req)
		return fn(ctx, req)
	}
	t.Cleanup(func() { acquireNodeMaintenanceLeaseWith = previous })
	return &calls
}

func nodeLeaseTestConfig() *config.Config {
	return &config.Config{Project: config.ProjectConfig{Name: "app"}, Servers: map[string]config.ServerConfig{"s1": {Host: "203.0.113.5"}}}
}

// An environment lease held on the node makes the upgrade exit 3 and name
// the holder.
func TestUpgradeLeaseRefusalExitsLocked(t *testing.T) {
	holder := &engine.LockedError{Operation: "upgrade-servers", Holder: "app/preview (deploy by ci)", Err: errors.New("cannot start upgrade-servers on s1: environment operations hold leases on the node: app/preview (deploy by ci)")}
	calls := stubNodeMaintenanceLease(t, func(context.Context, engine.NodeMaintenanceLeaseRequest) (*engine.NodeMaintenanceLease, error) {
		return nil, holder
	})
	cfg := nodeLeaseTestConfig()
	var out bytes.Buffer
	_, err := acquireUpgradeNodeLease(struct{}{}, cfg, "production", "s1", cfg.Servers["s1"], &takod.Status{Runtime: "takod"}, nil, &out)
	if !isLockedError(err) {
		t.Fatalf("err = %v, want locked", err)
	}
	if len(*calls) != 1 || (*calls)[0].Operation != "upgrade-servers" || (*calls)[0].Environment != "production" || (*calls)[0].Project != "app" {
		t.Fatalf("lease request = %+v", *calls)
	}
	commandErr := lockedUpgradeError([]error{err})
	if engine.Classify(commandErr) != engine.ClassLocked {
		t.Fatalf("command error class = %v, want locked (exit 3)", engine.Classify(commandErr))
	}
	if !strings.Contains(commandErr.Error(), "app/preview") {
		t.Fatalf("command error does not name the holder: %v", commandErr)
	}
	if lockedUpgradeError(nil) != nil {
		t.Fatal("no lease errors produced a command error")
	}
}

// A node whose takod does not answer cannot grant environment leases, so the
// upgrade (which then reinstalls takod) continues without one.
func TestUpgradeLeaseSkippedWhenTakodIsDown(t *testing.T) {
	calls := stubNodeMaintenanceLease(t, func(context.Context, engine.NodeMaintenanceLeaseRequest) (*engine.NodeMaintenanceLease, error) {
		t.Fatal("lease requested from an unavailable agent")
		return nil, nil
	})
	cfg := nodeLeaseTestConfig()
	var out bytes.Buffer
	lease, err := acquireUpgradeNodeLease(struct{}{}, cfg, "production", "s1", cfg.Servers["s1"], nil, errors.New("connection refused"), &out)
	if err != nil || lease != nil || len(*calls) != 0 {
		t.Fatalf("lease = %v, err = %v, calls = %v", lease, err, *calls)
	}
	if !strings.Contains(out.String(), "continuing without a node maintenance lease") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestUpgradeLeaseFallbackWarningIsPrinted(t *testing.T) {
	stubNodeMaintenanceLease(t, func(context.Context, engine.NodeMaintenanceLeaseRequest) (*engine.NodeMaintenanceLease, error) {
		return &engine.NodeMaintenanceLease{Server: "s1", Fallback: true, Warning: "takod on s1 does not support node maintenance leases"}, nil
	})
	cfg := nodeLeaseTestConfig()
	var out bytes.Buffer
	lease, err := acquireUpgradeNodeLease(struct{}{}, cfg, "production", "s1", cfg.Servers["s1"], &takod.Status{Runtime: "takod"}, nil, &out)
	if err != nil || lease == nil {
		t.Fatalf("lease = %v, err = %v", lease, err)
	}
	if !strings.Contains(out.String(), "does not support node maintenance leases") {
		t.Fatalf("output = %q", out.String())
	}
}

// renewRefusingAgent grants the maintenance lease once and then reports it
// held by another operation, as a node whose lease lapsed during a long
// setup and was taken over.
type renewRefusingAgent struct{ posts int }

func (a *renewRefusingAgent) RequestJSON(ctx context.Context, method, endpoint string, value any) (string, error) {
	return a.RequestJSONWithTimeout(ctx, method, endpoint, value, time.Second)
}

func (a *renewRefusingAgent) RequestJSONWithTimeout(_ context.Context, method, endpoint string, value any, _ time.Duration) (string, error) {
	if endpoint != "/v1/node-lease/maintenance" || method != "POST" {
		return "{}", nil
	}
	a.posts++
	request := value.(takod.NodeMaintenanceLeaseRequest)
	response := takod.NodeMaintenanceLeaseResponse{Acquired: true, Found: true, Lease: &takod.NodeMaintenanceLease{Version: 1, ID: request.ID, Operation: request.Operation, ExpiresAt: time.Now().Add(time.Minute)}}
	if a.posts > 1 {
		response = takod.NodeMaintenanceLeaseResponse{Found: true, Lease: &takod.NodeMaintenanceLease{Version: 1, ID: "maint-other", Operation: "upgrade-servers", Who: "ops", ExpiresAt: time.Now().Add(time.Hour)}, Message: "node maintenance lease is held"}
	}
	data, err := json.Marshal(response)
	return string(data), err
}

// Review finding 3: setup renews the lease before every step and stops when
// it was lost, instead of reinstalling takod under someone else's lease.
func TestCheckNodeMaintenanceLeaseRenewsAndStopsOnLostLease(t *testing.T) {
	if err := checkNodeMaintenanceLease(nil); err != nil {
		t.Fatalf("no lease (takod down) err = %v", err)
	}
	agent := &renewRefusingAgent{}
	cfg := nodeLeaseTestConfig()
	var out bytes.Buffer
	lease, err := acquireNodeMaintenanceLeaseForCommand(agent, cfg, "production", "s1", cfg.Servers["s1"], &takod.Status{Runtime: "takod", Capabilities: []string{takod.CapabilityNodeMaintenanceLeaseV1}}, nil, "setup", &out)
	if err != nil {
		t.Fatal(err)
	}
	err = checkNodeMaintenanceLease(lease)
	if agent.posts != 2 {
		t.Fatalf("lease POSTs = %d, want acquire + renewal", agent.posts)
	}
	if !isLockedError(err) || !strings.Contains(err.Error(), "lost the node maintenance lease on s1") {
		t.Fatalf("check err = %v, want lost lease", err)
	}
}
