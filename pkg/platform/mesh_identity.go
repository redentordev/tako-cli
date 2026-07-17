package platform

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/redentordev/tako-cli/pkg/fileutil"
	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"golang.org/x/crypto/curve25519"
)

const DefaultPlatformMeshKeyDir = "/etc/tako/wireguard"

// EnsureMeshPublicKey creates one durable WireGuard private key, or derives
// the public identity from the existing protected key. The key is compatible
// with wg(8)'s base64 file format and is never returned to the caller.
func EnsureMeshPublicKey(keyDir string) (string, error) {
	if !filepath.IsAbs(keyDir) {
		return "", fmt.Errorf("mesh key directory must be absolute")
	}
	if err := os.MkdirAll(keyDir, 0700); err != nil {
		return "", fmt.Errorf("create mesh key directory: %w", err)
	}
	if err := os.Chmod(keyDir, 0700); err != nil {
		return "", fmt.Errorf("protect mesh key directory: %w", err)
	}
	privatePath := filepath.Join(keyDir, "privatekey")
	private, err := readMeshPrivateKey(privatePath)
	if os.IsNotExist(err) {
		private = make([]byte, curve25519.ScalarSize)
		if _, err := rand.Read(private); err != nil {
			return "", fmt.Errorf("generate mesh private key: %w", err)
		}
		private[0] &= 248
		private[31] &= 127
		private[31] |= 64
		encoded := []byte(base64.StdEncoding.EncodeToString(private) + "\n")
		if err := createPrivateMeshKey(privatePath, encoded); err != nil {
			if !os.IsExist(err) {
				return "", err
			}
			private, err = readMeshPrivateKey(privatePath)
			if err != nil {
				return "", err
			}
		}
	} else if err != nil {
		return "", err
	}
	public, err := curve25519.X25519(private, curve25519.Basepoint)
	if err != nil {
		return "", fmt.Errorf("derive mesh public key: %w", err)
	}
	encodedPublic := base64.StdEncoding.EncodeToString(public)
	if err := nodeidentity.ValidateMeshPublicKey(encodedPublic); err != nil {
		return "", err
	}
	if err := fileutil.WriteFileAtomic(filepath.Join(keyDir, "publickey"), []byte(encodedPublic+"\n"), 0644); err != nil {
		return "", fmt.Errorf("write mesh public key: %w", err)
	}
	return encodedPublic, nil
}

func createPrivateMeshKey(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("write mesh private key: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("sync mesh private key: %w", err)
	}
	if err := file.Close(); err != nil {
		return err
	}
	return fileutil.SyncDirectory(filepath.Dir(path))
}

func readMeshPrivateKey(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 || info.Size() > 256 {
		return nil, fmt.Errorf("mesh private key must be a protected regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("mesh private key changed while opening")
	}
	if err := validateMembershipFileOwner(opened); err != nil {
		return nil, fmt.Errorf("mesh private key ownership is invalid: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(file, 257))
	if err != nil || len(data) > 256 {
		return nil, fmt.Errorf("read mesh private key: invalid size")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(decoded) != curve25519.ScalarSize {
		return nil, fmt.Errorf("mesh private key is invalid")
	}
	return decoded, nil
}
