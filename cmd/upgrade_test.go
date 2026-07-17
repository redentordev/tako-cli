package cmd

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/takod"
)

func TestUpgradeTargetServersUsesAuthoritativeEnrolledInventory(t *testing.T) {
	cfg := &config.Config{Servers: map[string]config.ServerConfig{
		"controller": {Roles: []string{nodeidentity.RoleControlPlane, nodeidentity.RoleWorker}},
		"worker-b":   {Roles: []string{nodeidentity.RoleWorker}},
		"worker-a":   {Roles: []string{nodeidentity.RoleWorker}},
	}}
	names, _, err := upgradeTargetServers(cfg, "application-subset", "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"controller", "worker-a", "worker-b"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("targets = %v, want authoritative inventory %v", names, want)
	}
	if _, _, err := upgradeTargetServers(cfg, "application-subset", "missing"); err == nil || !strings.Contains(err.Error(), "authoritative") {
		t.Fatalf("missing enrolled target error = %v", err)
	}
}

func TestUpgradeTargetServersRejectsMixedLegacyInventory(t *testing.T) {
	cfg := &config.Config{Servers: map[string]config.ServerConfig{
		"controller": {Roles: []string{nodeidentity.RoleControlPlane}},
		"legacy":     {},
	}}
	if _, _, err := upgradeTargetServers(cfg, "production", ""); err == nil || !strings.Contains(err.Error(), "mix legacy") {
		t.Fatalf("mixed target error = %v", err)
	}
}

func TestUpgradeServersCommandRegistered(t *testing.T) {
	cmd, _, err := upgradeCmd.Find([]string{"servers"})
	if err != nil {
		t.Fatalf("Find(upgrade servers) returned error: %v", err)
	}
	if !upgradeCmd.SilenceUsage {
		t.Fatal("upgrade command should silence usage for execution errors")
	}
	if cmd != upgradeServersCmd {
		t.Fatalf("upgrade servers command was not registered")
	}
	if !upgradeServersCmd.SilenceUsage {
		t.Fatal("upgrade servers command should silence usage for execution errors")
	}
	for _, flagName := range []string{"server", "dry-run", "takod-binary"} {
		if flag := upgradeServersCmd.Flags().Lookup(flagName); flag == nil {
			t.Fatalf("upgrade servers missing --%s flag", flagName)
		}
	}
	if strings.Contains(upgradeServersCmd.Long, "selected environment") || !strings.Contains(upgradeServersCmd.Long, "authoritative enrolled cluster inventory") {
		t.Fatalf("upgrade help does not describe authoritative cluster targeting: %q", upgradeServersCmd.Long)
	}
}

func TestValidateUpgradeDoesNotDowngradeProtectedNodes(t *testing.T) {
	for _, test := range []struct {
		target, running string
		blocked         bool
	}{
		{target: "v0.9.0", running: "v0.9.3", blocked: true},
		{target: "0.9.3", running: "v0.9.3"},
		{target: "v0.9.4", running: "0.9.3"},
		{target: "v0.9.0-2-gabcdef0", running: "v0.9.3", blocked: true},
		{target: "v0.9.4-1-gabcdef0-dirty", running: "v0.9.3"},
		{target: "dev", running: "v0.9.3"},
	} {
		err := validateUpgradeDoesNotDowngrade(test.target, test.running)
		if (err != nil) != test.blocked {
			t.Fatalf("validateUpgradeDoesNotDowngrade(%q, %q) error=%v, blocked=%v", test.target, test.running, err, test.blocked)
		}
	}
}

func TestValidateUpgradeStatusIdentityRequiresCurrentMembershipAndRoles(t *testing.T) {
	identity, err := nodeidentity.New(
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
		"node-a",
		[]string{nodeidentity.RoleWorker, nodeidentity.RoleEdge},
		time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	server := config.ServerConfig{
		ClusterID: identity.ClusterID,
		NodeID:    identity.NodeID,
		Lifecycle: nodeidentity.NodeLifecycleSchedulable,
		Roles:     []string{nodeidentity.RoleWorker, nodeidentity.RoleEdge},
	}
	status := &takod.Status{
		Capabilities: []string{nodeidentity.Capability},
		Identity:     &identity.Identity,
		Membership: &nodeidentity.InventoryNode{
			NodeID: server.NodeID, Lifecycle: server.Lifecycle,
			Roles:               []string{nodeidentity.RoleEdge, nodeidentity.RoleWorker},
			AllocationPublicKey: identity.AllocationPublicKey,
		},
		MembershipGeneration: 4,
	}
	if err := validateUpgradeStatusIdentity("node-a", server, status); err != nil {
		t.Fatalf("valid mutation-time attestation rejected: %v", err)
	}
	status.Membership.Roles = []string{nodeidentity.RoleWorker}
	if err := validateUpgradeStatusIdentity("node-a", server, status); err == nil || !strings.Contains(err.Error(), "roles") {
		t.Fatalf("stale membership roles accepted: %v", err)
	}
	status.Membership = nil
	if err := validateUpgradeStatusIdentity("node-a", server, status); err == nil || !strings.Contains(err.Error(), "membership") {
		t.Fatalf("missing membership accepted: %v", err)
	}
}

func TestValidateUpgradeServersOptionsRequiresBinaryForDevApply(t *testing.T) {
	err := validateUpgradeServersOptions("dev", "", false)
	if err == nil {
		t.Fatal("validateUpgradeServersOptions should reject dev apply without --takod-binary")
	}
	if !strings.Contains(err.Error(), "--takod-binary") {
		t.Fatalf("error = %q, want --takod-binary guidance", err)
	}
}

func TestValidateUpgradeServersOptionsRequiresBinaryForNonReleaseApply(t *testing.T) {
	for _, version := range []string{
		"v0.4.13-1-gabcdef0",
		"v0.4.13-1-gabcdef0-dirty",
		"v0.4.13-dirty",
	} {
		if err := validateUpgradeServersOptions(version, "", false); err == nil {
			t.Fatalf("validateUpgradeServersOptions(%q) should reject non-release apply without --takod-binary", version)
		}
		if err := validateUpgradeServersOptions(version, "/tmp/tako-linux-amd64", false); err != nil {
			t.Fatalf("validateUpgradeServersOptions(%q) should allow explicit --takod-binary: %v", version, err)
		}
	}
}

func TestValidateUpgradeServersOptionsAllowsDevDryRun(t *testing.T) {
	if err := validateUpgradeServersOptions("dev", "", true); err != nil {
		t.Fatalf("dev dry-run should be allowed: %v", err)
	}
}

func TestValidateUpgradeServersOptionsAllowsReleaseApply(t *testing.T) {
	for _, version := range []string{"v0.4.5", "v0.5.0-rc.1"} {
		if err := validateUpgradeServersOptions(version, "", false); err != nil {
			t.Fatalf("release apply should be allowed for %q: %v", version, err)
		}
	}
}

func TestIsGitDescribeSnapshot(t *testing.T) {
	tests := map[string]bool{
		"v0.4.13":                  false,
		"v0.5.0-rc.1":              false,
		"v0.4.13-1-gabcdef0":       true,
		"v0.4.13-12-gABCDEF0":      true,
		"v0.5.0-rc.1-2-gabcdef0":   true,
		"v0.4.13-dirty":            true,
		"v0.4.13-1-gabcdef0-dirty": true,
	}
	for version, want := range tests {
		if got := isGitDescribeSnapshot(version); got != want {
			t.Fatalf("isGitDescribeSnapshot(%q) = %v, want %v", version, got, want)
		}
	}
}

func TestWaitForTakodAgentVersionRetriesUntilTargetVersion(t *testing.T) {
	client := &upgradeStatusExecutor{
		outputs: []string{
			`{"runtime":"takod","version":"v0.4.40"}` + "\n__TAKO_HTTP_STATUS__:200",
			`{"runtime":"takod","version":"v0.4.41"}` + "\n__TAKO_HTTP_STATUS__:200",
		},
	}

	status, err := waitForTakodAgentVersion(client, &config.Config{}, "v0.4.41", 100*time.Millisecond, time.Millisecond, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("waitForTakodAgentVersion returned error: %v", err)
	}
	if status.Version != "v0.4.41" {
		t.Fatalf("verified version = %q, want target version", status.Version)
	}
	if client.contextCalls != 2 {
		t.Fatalf("status probes = %d, want stale then target", client.contextCalls)
	}
	if !strings.Contains(client.commands[0], "/v1/status") {
		t.Fatalf("status probe command = %q, want /v1/status request", client.commands[0])
	}
}

func TestWaitForTakodAgentVersionReportsLastVersion(t *testing.T) {
	client := &upgradeStatusExecutor{
		outputs: []string{
			`{"runtime":"takod","version":"v0.4.40"}` + "\n__TAKO_HTTP_STATUS__:200",
		},
	}

	_, err := waitForTakodAgentVersion(client, &config.Config{}, "v0.4.41", time.Millisecond, time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("waitForTakodAgentVersion should fail when status never reports the target version")
	}
	for _, want := range []string{"expected version v0.4.41", "last v0.4.40"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err.Error(), want)
		}
	}
}

func TestWaitForTakodAgentVersionReportsUnavailableStatus(t *testing.T) {
	client := &upgradeStatusExecutor{err: errors.New("socket unavailable")}

	_, err := waitForTakodAgentVersion(client, &config.Config{}, "v0.4.41", time.Millisecond, time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("waitForTakodAgentVersion should fail when status is unavailable")
	}
	for _, want := range []string{"expected version v0.4.41", "status unavailable", "socket unavailable"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err.Error(), want)
		}
	}
}

type upgradeStatusExecutor struct {
	outputs      []string
	err          error
	contextCalls int
	inputCalls   int
	commands     []string
}

func (u *upgradeStatusExecutor) ExecuteWithContext(ctx context.Context, cmd string) (string, error) {
	u.contextCalls++
	u.commands = append(u.commands, cmd)
	return u.next()
}

func (u *upgradeStatusExecutor) ExecuteWithInput(ctx context.Context, cmd string, input io.Reader) (string, error) {
	u.inputCalls++
	u.commands = append(u.commands, cmd)
	return u.next()
}

func (u *upgradeStatusExecutor) next() (string, error) {
	if u.err != nil {
		return "", u.err
	}
	if len(u.outputs) == 0 {
		return `{"runtime":"takod","version":"v0.4.40"}` + "\n__TAKO_HTTP_STATUS__:200", nil
	}
	output := u.outputs[0]
	if len(u.outputs) > 1 {
		u.outputs = u.outputs[1:]
	}
	return output, nil
}
