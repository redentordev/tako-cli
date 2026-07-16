// Package nodeidentity owns the installation-level identity of a Tako node.
//
// Installation identity is deliberately separate from takod's mutable
// project/environment metadata. It is created only by an explicit platform
// bootstrap or enrollment operation and is never writable through the takod
// runtime API.
package nodeidentity

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	APIVersion  = "tako.io/v1"
	Kind        = "InstallationIdentity"
	Capability  = "node.identity-v1"
	DefaultPath = "/etc/tako/identity.json"
	maxFileSize = 64 << 10

	RoleControlPlane = "control-plane"
	RoleWorker       = "worker"
	RoleEdge         = "edge"
	RoleBuilder      = "builder"
)

var (
	nodeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	allowedRoles    = map[string]struct{}{
		RoleControlPlane: {},
		RoleWorker:       {},
		RoleEdge:         {},
		RoleBuilder:      {},
	}
)

// Identity is the immutable, installation-level identity of one Tako node.
// ClusterID and NodeID are UUIDs created during explicit enrollment. Runtime
// role assignments deliberately live outside this value so roles can move
// without changing node identity.
type Identity struct {
	APIVersion string    `json:"apiVersion"`
	Kind       string    `json:"kind"`
	ClusterID  string    `json:"clusterId"`
	NodeID     string    `json:"nodeId"`
	NodeName   string    `json:"nodeName"`
	CreatedAt  time.Time `json:"createdAt"`
}

// Installation is the create-once identity document. EnrollmentRoles record
// the roles requested at bootstrap/enrollment time; they are an audit fact,
// not the mutable current-role assignment introduced by the control plane.
type Installation struct {
	Identity
	EnrollmentRoles []string `json:"roles"`
}

// Reference identifies an enrolled cluster member without carrying mutable
// names, addresses, or roles.
type Reference struct {
	ClusterID string `json:"clusterId" yaml:"clusterId"`
	NodeID    string `json:"nodeId" yaml:"nodeId"`
}

// Validate checks that both immutable identity components are UUIDs.
func (r Reference) Validate() error {
	if err := ValidateClusterID(r.ClusterID); err != nil {
		return err
	}
	if err := ValidateNodeID(r.NodeID); err != nil {
		return err
	}
	return nil
}

// ValidateClusterID checks the serialized cluster identity format.
func ValidateClusterID(value string) error {
	if !isUUID(value) {
		return fmt.Errorf("clusterId must be a UUID")
	}
	return nil
}

// ValidateNodeID checks the serialized node identity format.
func ValidateNodeID(value string) error {
	if !isUUID(value) {
		return fmt.Errorf("nodeId must be a UUID")
	}
	return nil
}

// New creates a validated in-memory identity. An empty clusterID or nodeID is
// generated as a UUIDv4. Persist it with Create, which refuses replacement.
func New(clusterID string, nodeID string, nodeName string, roles []string, now time.Time) (*Installation, error) {
	var err error
	if strings.TrimSpace(clusterID) == "" {
		clusterID, err = newUUID()
		if err != nil {
			return nil, fmt.Errorf("generate cluster ID: %w", err)
		}
	}
	if strings.TrimSpace(nodeID) == "" {
		nodeID, err = newUUID()
		if err != nil {
			return nil, fmt.Errorf("generate node ID: %w", err)
		}
	}
	if now.IsZero() {
		now = time.Now()
	}
	installation := &Installation{
		Identity: Identity{
			APIVersion: APIVersion,
			Kind:       Kind,
			ClusterID:  strings.ToLower(strings.TrimSpace(clusterID)),
			NodeID:     strings.ToLower(strings.TrimSpace(nodeID)),
			NodeName:   strings.TrimSpace(nodeName),
			CreatedAt:  now.UTC(),
		},
		EnrollmentRoles: canonicalRoles(roles),
	}
	if err := installation.Validate(); err != nil {
		return nil, err
	}
	return installation, nil
}

// Validate checks the immutable identity fields.
func (i Identity) Validate() error {
	if i.APIVersion != APIVersion {
		return fmt.Errorf("identity apiVersion must be %q", APIVersion)
	}
	if i.Kind != Kind {
		return fmt.Errorf("identity kind must be %q", Kind)
	}
	if !isUUID(i.ClusterID) {
		return fmt.Errorf("identity clusterId must be a UUID")
	}
	if !isUUID(i.NodeID) {
		return fmt.Errorf("identity nodeId must be a UUID")
	}
	if !nodeNamePattern.MatchString(i.NodeName) {
		return fmt.Errorf("identity nodeName %q must start with a letter or digit and contain at most 63 letters, digits, dots, underscores, or hyphens", i.NodeName)
	}
	if i.CreatedAt.IsZero() {
		return fmt.Errorf("identity createdAt is required")
	}
	return nil
}

// Validate checks the complete persisted installation contract.
func (i Installation) Validate() error {
	if err := i.Identity.Validate(); err != nil {
		return err
	}
	if len(i.EnrollmentRoles) == 0 {
		return fmt.Errorf("identity must declare at least one node role")
	}
	seen := make(map[string]struct{}, len(i.EnrollmentRoles))
	for index, role := range i.EnrollmentRoles {
		if role != strings.TrimSpace(role) {
			return fmt.Errorf("identity role %q must not contain surrounding whitespace", role)
		}
		if _, ok := allowedRoles[role]; !ok {
			return fmt.Errorf("identity role %q is unsupported", role)
		}
		if _, ok := seen[role]; ok {
			return fmt.Errorf("identity role %q is duplicated", role)
		}
		seen[role] = struct{}{}
		if index > 0 && i.EnrollmentRoles[index-1] > role {
			return fmt.Errorf("identity roles must be sorted")
		}
	}
	return nil
}

// HasEnrollmentRole reports whether bootstrap/enrollment declared role.
func (i Installation) HasEnrollmentRole(role string) bool {
	role = strings.TrimSpace(role)
	for _, candidate := range i.EnrollmentRoles {
		if candidate == role {
			return true
		}
	}
	return false
}

// Matches reports whether the immutable IDs identify the expected cluster
// member. Node names and IP addresses are intentionally not authority.
func (i Identity) Matches(clusterID string, nodeID string) bool {
	return strings.EqualFold(i.ClusterID, strings.TrimSpace(clusterID)) &&
		strings.EqualFold(i.NodeID, strings.TrimSpace(nodeID))
}

// Matches reports whether the installation contains the expected identity.
func (i Installation) Matches(clusterID string, nodeID string) bool {
	return i.Identity.Matches(clusterID, nodeID)
}

// MatchesReference reports whether identity is the enrolled cluster member.
func (i Identity) MatchesReference(reference Reference) bool {
	return i.Matches(reference.ClusterID, reference.NodeID)
}

// Create atomically publishes identity at path with mode 0600. Its protected
// parent directory must already exist. Create refuses to overwrite any
// existing path, including a symlink, so enrollment cannot silently replace
// a node's authority.
func Create(path string, installation Installation) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("identity path is required")
	}
	installation.APIVersion = strings.TrimSpace(installation.APIVersion)
	installation.Kind = strings.TrimSpace(installation.Kind)
	installation.ClusterID = strings.ToLower(strings.TrimSpace(installation.ClusterID))
	installation.NodeID = strings.ToLower(strings.TrimSpace(installation.NodeID))
	installation.NodeName = strings.TrimSpace(installation.NodeName)
	installation.EnrollmentRoles = canonicalRoles(installation.EnrollmentRoles)
	installation.CreatedAt = installation.CreatedAt.UTC()
	if err := installation.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(installation, "", "  ")
	if err != nil {
		return fmt.Errorf("encode installation identity: %w", err)
	}
	data = append(data, '\n')

	return createSecureFile(path, data)
}

// Read validates and returns the installation identity at path. Symlinks,
// non-regular files, and group/world-accessible files are rejected.
func Read(path string) (*Installation, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("identity path is required")
	}
	data, err := readSecureFile(path, maxFileSize)
	if err != nil {
		return nil, err
	}
	var installation Installation
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&installation); err != nil {
		return nil, fmt.Errorf("decode installation identity: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode installation identity: multiple JSON values are not allowed")
		}
		return nil, fmt.Errorf("decode installation identity: %w", err)
	}
	if err := installation.Validate(); err != nil {
		return nil, err
	}
	return &installation, nil
}

// ReadOptional returns nil when no identity has been enrolled.
func ReadOptional(path string) (*Installation, error) {
	identity, err := Read(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return identity, err
}

func canonicalRoles(roles []string) []string {
	out := make([]string, 0, len(roles))
	seen := make(map[string]struct{}, len(roles))
	for _, role := range roles {
		role = strings.TrimSpace(role)
		if role == "" {
			continue
		}
		if _, ok := seen[role]; ok {
			continue
		}
		seen[role] = struct{}{}
		out = append(out, role)
	}
	sort.Strings(out)
	return out
}

func newUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}

func isUUID(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for index, ch := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			return false
		}
	}
	return true
}
