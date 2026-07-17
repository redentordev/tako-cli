package provisioner

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/recovery"
)

type takodUpgradePublicationPaths struct {
	dir           string
	locks         string
	candidate     string
	takod         string
	worker        string
	manifest      string
	identity      string
	inventory     string
	platformState string
}

func defaultTakodUpgradePublicationPaths() takodUpgradePublicationPaths {
	return takodUpgradePublicationPaths{
		dir: "/var/lib/tako/node-upgrade", locks: takodUpgradeLockBase,
		candidate: "/var/lib/tako/node-upgrade/candidate",
		takod:     "/usr/local/bin/tako", worker: "/usr/local/lib/tako/tako",
		manifest: "/etc/tako/version.json", identity: nodeidentity.DefaultPath,
		inventory: nodeidentity.DefaultInventoryPath, platformState: "/var/lib/tako",
	}
}

// PublishTakodUpgrade is executed by the checksum-verified candidate while
// its parent root shell holds the upgrade guard flock. It additionally holds
// the controller lifecycle barrier and the exact inventory publication lock
// across contract verification, rollback publication, and both binary renames.
func PublishTakodUpgrade(contract *nodeidentity.UpgradeContract, leaseToken string) error {
	return publishTakodUpgrade(defaultTakodUpgradePublicationPaths(), contract, leaseToken, nil)
}

func publishTakodUpgrade(paths takodUpgradePublicationPaths, contract *nodeidentity.UpgradeContract, leaseToken string, afterValidate func()) (returnErr error) {
	if err := validateTakodUpgradePublicationDir(paths.dir); err != nil {
		return err
	}
	if err := validateTakodUpgradeLease(paths, leaseToken); err != nil {
		return err
	}

	var releaseSnapshot func()
	var releaseInventory func()
	if contract != nil {
		var err error
		releaseSnapshot, err = recovery.AcquireSnapshotLock(paths.platformState)
		if err != nil {
			return fmt.Errorf("acquire platform lifecycle publication barrier: %w", err)
		}
		defer releaseSnapshot()
		releaseInventory, err = nodeidentity.AcquireInventoryMutationLock(paths.inventory)
		if err != nil {
			return fmt.Errorf("acquire protected inventory publication barrier: %w", err)
		}
		defer releaseInventory()
		if err := nodeidentity.VerifyUpgradeContract(paths.identity, paths.inventory, *contract); err != nil {
			return err
		}
	}
	if afterValidate != nil {
		afterValidate()
	}
	// The parent still holds the kernel guard; rechecking its token after the
	// lifecycle barriers closes the gap before the first durable mutation.
	if err := validateTakodUpgradeLeaseOwner(paths, leaseToken); err != nil {
		return err
	}

	pendingPublished := false
	defer func() {
		if returnErr == nil || pendingPublished {
			return
		}
		_ = removeTakodUpgradeEvidence(paths.dir)
	}()
	previousTakod := filepath.Join(paths.dir, "previous-takod")
	previousWorker := filepath.Join(paths.dir, "previous-platform-worker")
	previousManifest := filepath.Join(paths.dir, "previous-version-manifest")
	hadWorker := filepath.Join(paths.dir, "had-platform-worker")
	hadManifest := filepath.Join(paths.dir, "had-version-manifest")
	if err := removeTakodUpgradeEvidence(paths.dir); err != nil {
		return err
	}
	if err := installUpgradeCopy(paths.takod, previousTakod, 0755); err != nil {
		return fmt.Errorf("preserve previous takod: %w", err)
	}
	if info, err := os.Lstat(paths.worker); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("protected platform worker is not a regular file")
		}
		if err := installUpgradeCopy(paths.worker, previousWorker, 0755); err != nil {
			return fmt.Errorf("preserve previous platform worker: %w", err)
		}
		if err := writeUpgradeMarker(hadWorker); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if info, err := os.Lstat(paths.manifest); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("setup version manifest is not a regular file")
		}
		if err := installUpgradeCopy(paths.manifest, previousManifest, 0644); err != nil {
			return fmt.Errorf("preserve previous setup manifest: %w", err)
		}
		if err := writeUpgradeMarker(hadManifest); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := writeUpgradeMarker(filepath.Join(paths.dir, "pending")); err != nil {
		return err
	}
	pendingPublished = true
	if err := syncDirectory(paths.dir); err != nil {
		return err
	}
	if err := installUpgradeCopy(paths.candidate, paths.takod, 0755); err != nil {
		return fmt.Errorf("publish takod candidate: %w", err)
	}
	if _, err := os.Stat(hadWorker); err == nil {
		if err := installUpgradeCopy(paths.candidate, paths.worker, 0755); err != nil {
			return fmt.Errorf("publish platform worker candidate: %w", err)
		}
	}
	if err := os.Remove(paths.candidate); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncDirectory(paths.dir)
}

func validateTakodUpgradePublicationDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 {
		return fmt.Errorf("node upgrade directory is not a protected real directory")
	}
	return nil
}

func validateTakodUpgradeLease(paths takodUpgradePublicationPaths, token string) error {
	if err := validateTakodUpgradeLeaseOwner(paths, token); err != nil {
		return err
	}
	expiryData, err := os.ReadFile(filepath.Join(paths.locks, takodUpgradeNodeLockScope, "expiry"))
	if err != nil {
		return err
	}
	expiry, err := strconv.ParseInt(strings.TrimSpace(string(expiryData)), 10, 64)
	if err != nil || expiry <= time.Now().Unix() {
		return fmt.Errorf("node upgrade lease expired before protected publication")
	}
	return nil
}

func validateTakodUpgradeLeaseOwner(paths takodUpgradePublicationPaths, token string) error {
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("node upgrade lease token is required")
	}
	owner, err := os.ReadFile(filepath.Join(paths.locks, takodUpgradeNodeLockScope, "owner"))
	if err != nil || strings.TrimSpace(string(owner)) != token {
		return fmt.Errorf("node upgrade lease ownership changed before protected publication")
	}
	return nil
}

func removeTakodUpgradeEvidence(dir string) error {
	for _, name := range []string{"previous-takod", "previous-platform-worker", "previous-version-manifest", "had-platform-worker", "had-version-manifest"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return syncDirectory(dir)
}

func installUpgradeCopy(source string, destination string, mode os.FileMode) error {
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() == 0 {
		return fmt.Errorf("source is not a non-empty regular file")
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	temporary := destination + ".upgrade-next"
	_ = os.Remove(temporary)
	output, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		_ = output.Close()
		if cleanup {
			_ = os.Remove(temporary)
		}
	}()
	if _, err := io.Copy(output, input); err != nil {
		return err
	}
	if err := output.Chmod(mode); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, destination); err != nil {
		return err
	}
	cleanup = false
	return syncDirectory(filepath.Dir(destination))
}

func writeUpgradeMarker(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
