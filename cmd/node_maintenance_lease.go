package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/takod"
)

// acquireNodeMaintenanceLeaseWith is the engine seam (tests replace it).
var acquireNodeMaintenanceLeaseWith = engine.AcquireNodeMaintenanceLease

// lastUpgradeLeaseError carries a node's lease refusal from upgradeServerNode
// to runUpgradeServers, which upgrades nodes one at a time.
var lastUpgradeLeaseError error

// acquireUpgradeNodeLease takes the node maintenance lease for a
// non-enrolled node upgrade. A node whose takod does not answer has no agent
// that could grant environment leases either, so the upgrade proceeds
// without one.
func acquireUpgradeNodeLease(client any, cfg *config.Config, envName, name string, server config.ServerConfig, status *takod.Status, statusErr error, out io.Writer) (*engine.NodeMaintenanceLease, error) {
	return acquireNodeMaintenanceLeaseForCommand(client, cfg, envName, name, server, status, statusErr, "upgrade-servers", out)
}

func acquireNodeMaintenanceLeaseForCommand(client any, cfg *config.Config, envName, name string, server config.ServerConfig, status *takod.Status, statusErr error, operation string, out io.Writer) (*engine.NodeMaintenanceLease, error) {
	if statusErr != nil || status == nil {
		fmt.Fprintf(out, "  ⚠ takod is not answering on %s; continuing without a node maintenance lease\n", name)
		return nil, nil
	}
	lease, err := acquireNodeMaintenanceLeaseWith(context.Background(), engine.NodeMaintenanceLeaseRequest{
		Client:      client,
		Socket:      takodSocketFromConfig(cfg),
		Server:      name,
		Host:        server.Host,
		Project:     cfg.Project.Name,
		Environment: envName,
		Operation:   operation,
		Status:      status,
	})
	if err != nil {
		return nil, err
	}
	if lease != nil && lease.Warning != "" {
		fmt.Fprintf(out, "  ⚠ %s\n", lease.Warning)
	}
	return lease, nil
}

// setupLineWriter routes lease messages through setup's per-node output.
type setupLineWriter struct{ node string }

func (w setupLineWriter) Write(p []byte) (int, error) {
	emitSetupLine(w.node, string(p))
	return len(p), nil
}

func isLockedError(err error) bool {
	var locked *engine.LockedError
	return errors.As(err, &locked)
}

func releaseUpgradeNodeLease(lease *engine.NodeMaintenanceLease, out io.Writer) {
	if lease == nil {
		return
	}
	if err := lease.Release(context.Background()); err != nil {
		fmt.Fprintf(out, "  ⚠ %v\n", err)
	}
}

// lockedUpgradeError makes a lease refusal the command's error so it exits 3
// even when other nodes upgraded.
func lockedUpgradeError(leaseErrors []error) error {
	if len(leaseErrors) == 0 {
		return nil
	}
	messages := make([]string, 0, len(leaseErrors))
	holders := make([]string, 0, len(leaseErrors))
	for _, err := range leaseErrors {
		messages = append(messages, err.Error())
		var locked *engine.LockedError
		if errors.As(err, &locked) && locked.Holder != "" {
			holders = append(holders, locked.Holder)
		}
	}
	return &engine.LockedError{
		Operation: "upgrade-servers",
		Holder:    strings.Join(holders, "; "),
		Err:       fmt.Errorf("server agent upgrade blocked on %d node(s): %s", len(leaseErrors), strings.Join(messages, "; ")),
	}
}
