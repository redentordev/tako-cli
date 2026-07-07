package provisioner

import (
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/utils"
)

// PackageManager defines interface for OS package management
type PackageManager interface {
	Update() error
	Install(packages ...string) error
	Remove(packages ...string) error
	Search(packageName string) (bool, error)
}

// NewPackageManager creates a package manager for the detected OS
func NewPackageManager(client *ssh.Client, osInfo *OSInfo, verbose bool) (PackageManager, error) {
	return newPackageManagerWithLog(client, osInfo, provisionLog{verbose: verbose})
}

func newPackageManagerWithLog(client *ssh.Client, osInfo *OSInfo, log provisionLog) (PackageManager, error) {
	switch osInfo.Family {
	case OSFamilyDebian:
		return &AptManager{provisionLog: log, client: client}, nil
	case OSFamilyRHEL:
		if osInfo.PackageManager == "dnf" {
			return &DnfManager{provisionLog: log, client: client}, nil
		}
		return &YumManager{provisionLog: log, client: client}, nil
	case OSFamilySUSE:
		return &ZypperManager{provisionLog: log, client: client}, nil
	case OSFamilyAlpine:
		return &ApkManager{provisionLog: log, client: client}, nil
	default:
		return nil, fmt.Errorf("unsupported OS family: %s", osInfo.Family)
	}
}

// AptManager manages packages using apt/apt-get
type AptManager struct {
	provisionLog
	client *ssh.Client
}

func (a *AptManager) Update() error {
	a.logf("  Updating package lists (apt)...\n")
	_, err := a.client.Execute("sudo DEBIAN_FRONTEND=noninteractive apt-get update -y")
	return err
}

func (a *AptManager) Install(packages ...string) error {
	if len(packages) == 0 {
		return nil
	}
	args, err := quotePackageArgs(packages...)
	if err != nil {
		return err
	}
	a.logf("  Installing packages: %s\n", strings.Join(packages, ", "))
	cmd := fmt.Sprintf("sudo DEBIAN_FRONTEND=noninteractive apt-get install -y %s", args)
	_, err = a.client.Execute(cmd)
	return err
}

func (a *AptManager) Remove(packages ...string) error {
	if len(packages) == 0 {
		return nil
	}
	args, err := quotePackageArgs(packages...)
	if err != nil {
		return err
	}
	cmd := fmt.Sprintf("sudo DEBIAN_FRONTEND=noninteractive apt-get remove -y %s", args)
	_, err = a.client.Execute(cmd)
	return err
}

func (a *AptManager) Search(packageName string) (bool, error) {
	arg, err := quotePackageArg("^" + packageName + "$")
	if err != nil {
		return false, err
	}
	output, err := a.client.Execute(fmt.Sprintf("apt-cache search %s", arg))
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(output) != "", nil
}

// DnfManager manages packages using dnf
type DnfManager struct {
	provisionLog
	client *ssh.Client
}

func (d *DnfManager) Update() error {
	d.logf("  Updating package lists (dnf)...\n")
	_, err := d.client.Execute("sudo dnf check-update -y || true")
	return err
}

func (d *DnfManager) Install(packages ...string) error {
	if len(packages) == 0 {
		return nil
	}
	args, err := quotePackageArgs(packages...)
	if err != nil {
		return err
	}
	d.logf("  Installing packages: %s\n", strings.Join(packages, ", "))
	cmd := fmt.Sprintf("sudo dnf install -y %s", args)
	_, err = d.client.Execute(cmd)
	return err
}

func (d *DnfManager) Remove(packages ...string) error {
	if len(packages) == 0 {
		return nil
	}
	args, err := quotePackageArgs(packages...)
	if err != nil {
		return err
	}
	cmd := fmt.Sprintf("sudo dnf remove -y %s", args)
	_, err = d.client.Execute(cmd)
	return err
}

func (d *DnfManager) Search(packageName string) (bool, error) {
	arg, err := quotePackageArg(packageName)
	if err != nil {
		return false, err
	}
	output, err := d.client.Execute(fmt.Sprintf("dnf list %s", arg))
	if err != nil {
		return false, nil // Package not found
	}
	return strings.Contains(output, packageName), nil
}

// YumManager manages packages using yum
type YumManager struct {
	provisionLog
	client *ssh.Client
}

func (y *YumManager) Update() error {
	y.logf("  Updating package lists (yum)...\n")
	_, err := y.client.Execute("sudo yum check-update -y || true")
	return err
}

func (y *YumManager) Install(packages ...string) error {
	if len(packages) == 0 {
		return nil
	}
	args, err := quotePackageArgs(packages...)
	if err != nil {
		return err
	}
	y.logf("  Installing packages: %s\n", strings.Join(packages, ", "))
	cmd := fmt.Sprintf("sudo yum install -y %s", args)
	_, err = y.client.Execute(cmd)
	return err
}

func (y *YumManager) Remove(packages ...string) error {
	if len(packages) == 0 {
		return nil
	}
	args, err := quotePackageArgs(packages...)
	if err != nil {
		return err
	}
	cmd := fmt.Sprintf("sudo yum remove -y %s", args)
	_, err = y.client.Execute(cmd)
	return err
}

func (y *YumManager) Search(packageName string) (bool, error) {
	arg, err := quotePackageArg(packageName)
	if err != nil {
		return false, err
	}
	output, err := y.client.Execute(fmt.Sprintf("yum list %s", arg))
	if err != nil {
		return false, nil
	}
	return strings.Contains(output, packageName), nil
}

// ZypperManager manages packages using zypper
type ZypperManager struct {
	provisionLog
	client *ssh.Client
}

func (z *ZypperManager) Update() error {
	z.logf("  Updating package lists (zypper)...\n")
	_, err := z.client.Execute("sudo zypper refresh")
	return err
}

func (z *ZypperManager) Install(packages ...string) error {
	if len(packages) == 0 {
		return nil
	}
	args, err := quotePackageArgs(packages...)
	if err != nil {
		return err
	}
	z.logf("  Installing packages: %s\n", strings.Join(packages, " "))
	cmd := fmt.Sprintf("sudo zypper install -y --no-confirm %s", args)
	_, err = z.client.Execute(cmd)
	return err
}

func (z *ZypperManager) Remove(packages ...string) error {
	if len(packages) == 0 {
		return nil
	}
	args, err := quotePackageArgs(packages...)
	if err != nil {
		return err
	}
	cmd := fmt.Sprintf("sudo zypper remove -y --no-confirm %s", args)
	_, err = z.client.Execute(cmd)
	return err
}

func (z *ZypperManager) Search(packageName string) (bool, error) {
	arg, err := quotePackageArg(packageName)
	if err != nil {
		return false, err
	}
	output, err := z.client.Execute(fmt.Sprintf("zypper search %s", arg))
	if err != nil {
		return false, err
	}
	return strings.Contains(output, packageName), nil
}

// ApkManager manages packages using apk
type ApkManager struct {
	provisionLog
	client *ssh.Client
}

func (ap *ApkManager) Update() error {
	ap.logf("  Updating package lists (apk)...\n")
	_, err := ap.client.Execute("sudo apk update")
	return err
}

func (ap *ApkManager) Install(packages ...string) error {
	if len(packages) == 0 {
		return nil
	}
	args, err := quotePackageArgs(packages...)
	if err != nil {
		return err
	}
	ap.logf("  Installing packages: %s\n", strings.Join(packages, ", "))
	cmd := fmt.Sprintf("sudo apk add %s", args)
	_, err = ap.client.Execute(cmd)
	return err
}

func (ap *ApkManager) Remove(packages ...string) error {
	if len(packages) == 0 {
		return nil
	}
	args, err := quotePackageArgs(packages...)
	if err != nil {
		return err
	}
	cmd := fmt.Sprintf("sudo apk del %s", args)
	_, err = ap.client.Execute(cmd)
	return err
}

func (ap *ApkManager) Search(packageName string) (bool, error) {
	arg, err := quotePackageArg(packageName)
	if err != nil {
		return false, err
	}
	output, err := ap.client.Execute(fmt.Sprintf("apk search %s", arg))
	if err != nil {
		return false, err
	}
	return strings.Contains(output, packageName), nil
}

func quotePackageArgs(packages ...string) (string, error) {
	quoted := make([]string, 0, len(packages))
	for _, packageName := range packages {
		arg, err := quotePackageArg(packageName)
		if err != nil {
			return "", err
		}
		quoted = append(quoted, arg)
	}
	return strings.Join(quoted, " "), nil
}

func quotePackageArg(packageName string) (string, error) {
	if strings.TrimSpace(packageName) == "" {
		return "", fmt.Errorf("package name is required")
	}
	return utils.ShellQuote(packageName), nil
}
