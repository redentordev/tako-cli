// Package nodeclient selects and records how the engine reaches one enrolled
// Tako node. It does not decide workload placement or control authority.
package nodeclient

import (
	"context"
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

// Policy is the configured transport-selection policy for one node.
type Policy string

const (
	// PolicySSH preserves the historical Tako behavior and never probes local
	// runtime state. An omitted policy has the same meaning.
	PolicySSH Policy = "ssh"
	// PolicyAuto selects local only after immutable identity attestation and
	// otherwise decides SSH before any SSH connection attempt.
	PolicyAuto Policy = "auto"
	// PolicyLocal requires immutable local identity attestation.
	PolicyLocal Policy = "local"
)

// Transport is the resolved runtime path. It is deliberately independent of
// scheduling and node roles.
type Transport string

const (
	TransportSSH   Transport = "ssh"
	TransportLocal Transport = "local"
)

const (
	EvidenceLegacySSHDefault    = "legacy_ssh_default"
	EvidenceExplicitSSH         = "explicit_ssh"
	EvidenceInstallationMatch   = "installation_identity_match"
	EvidenceLocalProbeFailed    = "local_identity_probe_failed"
	EvidenceLocalIdentityAbsent = "local_installation_identity_absent"
	EvidenceLocalIdentityWrong  = "local_installation_identity_mismatch"
)

// Decision is safe to include in plans, results, and audit records. It
// explains the preflight decision without exposing credentials.
type Decision struct {
	Transport Transport `json:"transport"`
	Evidence  string    `json:"evidence"`
	NodeID    string    `json:"nodeId,omitempty"`
}

// IdentityProbe is the minimal local agent surface needed for transport
// resolution. AgentClient satisfies it.
type IdentityProbe interface {
	Status(context.Context) (*takodclient.AgentStatus, error)
}

// Resolve selects one transport exactly once. It never attempts SSH and can
// therefore never turn an SSH failure into a local host mutation.
func Resolve(ctx context.Context, policy Policy, expected nodeidentity.Reference, local IdentityProbe) (Decision, error) {
	policy = Policy(strings.ToLower(strings.TrimSpace(string(policy))))
	switch policy {
	case "", PolicySSH:
		evidence := EvidenceExplicitSSH
		if policy == "" {
			evidence = EvidenceLegacySSHDefault
		}
		return Decision{Transport: TransportSSH, Evidence: evidence, NodeID: strings.ToLower(strings.TrimSpace(expected.NodeID))}, nil
	case PolicyAuto, PolicyLocal:
	default:
		return Decision{}, fmt.Errorf("unsupported node transport policy %q", policy)
	}
	if err := expected.Validate(); err != nil {
		return Decision{}, fmt.Errorf("identity-verified %s transport requires a valid node reference: %w", policy, err)
	}
	if local == nil {
		return resolveLocalFailure(policy, expected, EvidenceLocalProbeFailed, fmt.Errorf("local takod identity probe is unavailable"))
	}
	status, err := local.Status(ctx)
	if err != nil {
		return resolveLocalFailure(policy, expected, EvidenceLocalProbeFailed, err)
	}
	if status == nil {
		return resolveLocalFailure(policy, expected, EvidenceLocalProbeFailed, fmt.Errorf("local takod returned an empty status"))
	}
	if err := status.Validate(); err != nil {
		return resolveLocalFailure(policy, expected, EvidenceLocalProbeFailed, fmt.Errorf("local takod returned invalid status: %w", err))
	}
	if status.Identity == nil {
		return resolveLocalFailure(policy, expected, EvidenceLocalIdentityAbsent, fmt.Errorf("local takod is not enrolled with an installation identity"))
	}
	if !status.Identity.MatchesReference(expected) {
		return resolveLocalFailure(policy, expected, EvidenceLocalIdentityWrong, fmt.Errorf("local takod identity does not match configured cluster member"))
	}
	return Decision{Transport: TransportLocal, Evidence: EvidenceInstallationMatch, NodeID: status.Identity.NodeID}, nil
}

func resolveLocalFailure(policy Policy, expected nodeidentity.Reference, evidence string, cause error) (Decision, error) {
	decision := Decision{Transport: TransportSSH, Evidence: evidence, NodeID: strings.ToLower(strings.TrimSpace(expected.NodeID))}
	if policy == PolicyAuto {
		return decision, nil
	}
	return Decision{}, fmt.Errorf("local node transport identity verification failed: %w", cause)
}
