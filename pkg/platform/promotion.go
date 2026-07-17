package platform

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

const passivePromotionFileLimit = 16 << 20

// PassivePromotionProof proves that a cold recovery staging tree contains one
// internally consistent single-writer controller bound to an externally held
// controller-key fingerprint. It never activates services.
type PassivePromotionProof struct {
	ClusterID                string `json:"clusterId"`
	ControllerNodeID         string `json:"controllerNodeId"`
	ControllerKeyFingerprint string `json:"controllerKeyFingerprint"`
	MembershipGeneration     uint64 `json:"membershipGeneration"`
	ControllerMode           string `json:"controllerMode"`
	ActiveActive             bool   `json:"activeActive"`
}

func ControllerRecoveryKeyFingerprint(publicKey string) (string, error) {
	decoded, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(publicKey))
	if err != nil || len(decoded) == 0 {
		return "", fmt.Errorf("controller recovery public key is invalid")
	}
	digest := sha256.Sum256(decoded)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(digest[:]), nil
}

func VerifyPassivePromotion(stagedRoot string, expectedClusterID string, expectedKeyFingerprint string) (*PassivePromotionProof, error) {
	root, err := filepath.Abs(strings.TrimSpace(stagedRoot))
	if err != nil || root == string(filepath.Separator) {
		return nil, fmt.Errorf("passive promotion requires a non-root absolute staging directory")
	}
	required := map[string]string{
		"identity":   filepath.Join("etc", "tako", filepath.Base(nodeidentity.DefaultPath)),
		"inventory":  filepath.Join("etc", "tako", filepath.Base(nodeidentity.DefaultInventoryPath)),
		"config":     filepath.Join("etc", "tako", "platform.json"),
		"membership": filepath.Join("var", "lib", "tako", DefaultMembershipDirName, DefaultMembershipName),
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("passive promotion staging root must be a real directory")
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve passive promotion staging root: %w", err)
	}
	if err := validatePassivePromotionAncestors(root); err != nil {
		return nil, err
	}
	info, err = os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if err := validatePassivePromotionOwner(info, true); err != nil {
		return nil, err
	}
	for _, relative := range required {
		if err := validatePassivePromotionPath(root, filepath.Join(root, relative)); err != nil {
			return nil, err
		}
	}

	// OpenRoot confines every subsequent open to the already validated staging
	// directory. Each file is read once from its verified descriptor and copied
	// to a private snapshot before parsers reopen it by name.
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer rootHandle.Close()
	snapshot, err := os.MkdirTemp("", "tako-passive-promotion-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(snapshot)
	if err := os.Chmod(snapshot, 0700); err != nil {
		return nil, err
	}
	for label, relative := range required {
		data, err := readPassivePromotionFile(rootHandle, relative)
		if err != nil {
			return nil, fmt.Errorf("read staged %s: %w", label, err)
		}
		destination := filepath.Join(snapshot, relative)
		if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(destination, data, 0600); err != nil {
			return nil, err
		}
	}

	identityPath := filepath.Join(snapshot, required["identity"])
	inventoryPath := filepath.Join(snapshot, required["inventory"])
	configPath := filepath.Join(snapshot, required["config"])
	membershipPath := filepath.Join(snapshot, required["membership"])
	installation, err := nodeidentity.Read(identityPath)
	if err != nil {
		return nil, fmt.Errorf("read staged controller identity: %w", err)
	}
	if expected := strings.TrimSpace(expectedClusterID); expected == "" || !strings.EqualFold(expected, installation.ClusterID) {
		return nil, fmt.Errorf("staged controller cluster does not match the required cluster ID")
	}
	fingerprint, err := ControllerRecoveryKeyFingerprint(installation.AllocationPublicKey)
	if err != nil {
		return nil, err
	}
	if expected := strings.TrimSpace(expectedKeyFingerprint); expected == "" || expected != fingerprint {
		return nil, fmt.Errorf("staged controller authority does not match the externally trusted key fingerprint")
	}
	document, err := readConfigDocument(configPath)
	if err != nil {
		return nil, fmt.Errorf("read staged platform config: %w", err)
	}
	// Keep promotion's trust boundary explicit even though the shared strict
	// decoder currently performs both validations. A future decoder refactor
	// must not weaken cold-controller promotion.
	if err := document.State.Validate(); err != nil {
		return nil, fmt.Errorf("validate staged platform state: %w", err)
	}
	if err := document.Policy.Validate(); err != nil {
		return nil, fmt.Errorf("validate staged platform policy: %w", err)
	}
	if document.State.ClusterID != installation.ClusterID || document.State.NodeID != installation.NodeID || document.State.ControllerMode != "single-writer" {
		return nil, fmt.Errorf("staged platform config is not bound to the recovered single controller")
	}
	if err := ValidateControllerRecoverySnapshot(membershipPath, inventoryPath, installation); err != nil {
		return nil, fmt.Errorf("prove recovered controller authority: %w", err)
	}
	state, err := readMembershipState(membershipPath)
	if err != nil {
		return nil, err
	}
	return &PassivePromotionProof{
		ClusterID: installation.ClusterID, ControllerNodeID: installation.NodeID,
		ControllerKeyFingerprint: fingerprint, MembershipGeneration: state.Generation,
		ControllerMode: "passive-recovery", ActiveActive: false,
	}, nil
}

func validatePassivePromotionAncestors(root string) error {
	for current := filepath.Clean(root); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("passive promotion ancestor %s is not a real directory", current)
		}
		if err := validatePassivePromotionAncestorOwner(info); err != nil {
			return fmt.Errorf("passive promotion ancestor %s is untrusted: %w", current, err)
		}
		if info.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("passive promotion ancestor %s is writable by group or others", current)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return nil
}

func readPassivePromotionFile(root *os.Root, relative string) ([]byte, error) {
	file, err := root.Open(relative)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if err := validatePassivePromotionOwner(info, false); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, passivePromotionFileLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > passivePromotionFileLimit {
		return nil, fmt.Errorf("staged recovery file exceeds %d bytes", passivePromotionFileLimit)
	}
	return data, nil
}

func validatePassivePromotionOwner(info os.FileInfo, directory bool) error {
	if directory {
		if !info.IsDir() {
			return fmt.Errorf("staged recovery ancestor is not a directory")
		}
		if err := validateMembershipDirectoryOwner(info); err != nil {
			return fmt.Errorf("staged recovery directory ownership is not trusted: %w", err)
		}
	} else {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("staged recovery file is not regular")
		}
		if err := validateMembershipFileOwner(info); err != nil {
			return fmt.Errorf("staged recovery file ownership is not trusted: %w", err)
		}
	}
	if info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("staged recovery path must not be writable by group or others")
	}
	return nil
}

func validatePassivePromotionPath(root string, path string) error {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("passive promotion path escapes the staging root")
	}
	current := root
	parts := strings.Split(relative, string(filepath.Separator))
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect staged recovery path %s: %w", relative, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("staged recovery path %s contains a symbolic link", relative)
		}
		if err := validatePassivePromotionOwner(info, index < len(parts)-1); err != nil {
			return fmt.Errorf("staged recovery path %s is untrusted: %w", relative, err)
		}
	}
	return nil
}
