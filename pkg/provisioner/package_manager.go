package provisioner

import (
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/ssh"
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
	switch osInfo.Family {
	case OSFamilyDebian:
		return &AptManager{client: client, verbose: verbose}, nil
	case OSFamilyRHEL:
		if osInfo.PackageManager == "dnf" {
			return &DnfManager{client: client, verbose: verbose}, nil
		}
		return &YumManager{client: client, verbose: verbose}, nil
	case OSFamilySUSE:
		return &ZypperManager{client: client, verbose: verbose}, nil
	case OSFamilyAlpine:
		return &ApkManager{client: client, verbose: verbose}, nil
	default:
		return nil, fmt.Errorf("unsupported OS family: %s", osInfo.Family)
	}
}

// AptManager manages packages using apt/apt-get
type AptManager struct {
	client  *ssh.Client
	verbose bool
}

func (a *AptManager) Update() error {
	if a.verbose {
		fmt.Println("  Updating package lists (apt)...")
	}
	_, err := a.client.Execute("sudo DEBIAN_FRONTEND=noninteractive apt-get update -y")
	return err
}

func (a *AptManager) Install(packages ...string) error {
	if len(packages) == 0 {
		return nil
	}
	if a.verbose {
		fmt.Printf("  Installing packages: %s\n", strings.Join(packages, ", "))
	}
	cmd := fmt.Sprintf("sudo DEBIAN_FRONTEND=noninteractive apt-get install -y %s", strings.Join(packages, " "))
	_, err := a.client.Execute(cmd)
	return err
}

func (a *AptManager) Remove(packages ...string) error {
	if len(packages) == 0 {
		return nil
	}
	cmd := fmt.Sprintf("sudo DEBIAN_FRONTEND=noninteractive apt-get remove -y %s", strings.Join(packages, " "))
	_, err := a.client.Execute(cmd)
	return err
}

func (a *AptManager) Search(packageName string) (bool, error) {
	output, err := a.client.Execute(fmt.Sprintf("apt-cache search ^%s$", packageName))
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(output) != "", nil
}

// DnfManager manages packages using dnf
type DnfManager struct {
	client  *ssh.Client
	verbose bool
}

func (d *DnfManager) Update() error {
	if d.verbose {
		fmt.Println("  Updating package lists (dnf)...")
	}
	_, err := d.client.Execute("sudo dnf check-update -y || true")
	return err
}

func (d *DnfManager) Install(packages ...string) error {
	if len(packages) == 0 {
		return nil
	}
	if d.verbose {
		fmt.Printf("  Installing packages: %s\n", strings.Join(packages, ", "))
	}
	cmd := fmt.Sprintf("sudo dnf install -y %s", strings.Join(packages, " "))
	_, err := d.client.Execute(cmd)
	return err
}

func (d *DnfManager) Remove(packages ...string) error {
	if len(packages) == 0 {
		return nil
	}
	cmd := fmt.Sprintf("sudo dnf remove -y %s", strings.Join(packages, " "))
	_, err := d.client.Execute(cmd)
	return err
}

func (d *DnfManager) Search(packageName string) (bool, error) {
	output, err := d.client.Execute(fmt.Sprintf("dnf list %s", packageName))
	if err != nil {
		return false, nil // Package not found
	}
	return strings.Contains(output, packageName), nil
}

// YumManager manages packages using yum
type YumManager struct {
	client  *ssh.Client
	verbose bool
}

func (y *YumManager) Update() error {
	if y.verbose {
		fmt.Println("  Updating package lists (yum)...")
	}
	_, err := y.client.Execute("sudo yum check-update -y || true")
	return err
}

func (y *YumManager) Install(packages ...string) error {
	if len(packages) == 0 {
		return nil
	}
	if y.verbose {
		fmt.Printf("  Installing packages: %s\n", strings.Join(packages, ", "))
	}
	cmd := fmt.Sprintf("sudo yum install -y %s", strings.Join(packages, " "))
	_, err := y.client.Execute(cmd)
	return err
}

func (y *YumManager) Remove(packages ...string) error {
	if len(packages) == 0 {
		return nil
	}
	cmd := fmt.Sprintf("sudo yum remove -y %s", strings.Join(packages, " "))
	_, err := y.client.Execute(cmd)
	return err
}

func (y *YumManager) Search(packageName string) (bool, error) {
	output, err := y.client.Execute(fmt.Sprintf("yum list %s", packageName))
	if err != nil {
		return false, nil
	}
	return strings.Contains(output, packageName), nil
}

// ZypperManager manages packages using zypper
type ZypperManager struct {
	client  *ssh.Client
	verbose bool
}

func (z *ZypperManager) Update() error {
	if z.verbose {
		fmt.Println("  Updating package lists (zypper)...")
	}
	_, err := z.client.Execute("sudo zypper refresh")
	return err
}

func (z *ZypperManager) Install(packages ...string) error {
	if len(packages) == 0 {
		return nil
	}
	if z.verbose {
		fmt.Printf("  Installing packages: %s\n", strings.Join(packages, " "))
	}
	cmd := fmt.Sprintf("sudo zypper install -y --no-confirm %s", strings.Join(packages, " "))
	_, err := z.client.Execute(cmd)
	return err
}

func (z *ZypperManager) Remove(packages ...string) error {
	if len(packages) == 0 {
		return nil
	}
	cmd := fmt.Sprintf("sudo zypper remove -y --no-confirm %s", strings.Join(packages, " "))
	_, err := z.client.Execute(cmd)
	return err
}

func (z *ZypperManager) Search(packageName string) (bool, error) {
	output, err := z.client.Execute(fmt.Sprintf("zypper search %s", packageName))
	if err != nil {
		return false, err
	}
	return strings.Contains(output, packageName), nil
}

// ApkManager manages packages using apk
type ApkManager struct {
	client  *ssh.Client
	verbose bool
}

func (ap *ApkManager) Update() error {
	if ap.verbose {
		fmt.Println("  Updating package lists (apk)...")
	}
	_, err := ap.client.Execute("sudo apk update")
	return err
}

func (ap *ApkManager) Install(packages ...string) error {
	if len(packages) == 0 {
		return nil
	}
	if ap.verbose {
		fmt.Printf("  Installing packages: %s\n", strings.Join(packages, ", "))
	}
	cmd := fmt.Sprintf("sudo apk add %s", strings.Join(packages, " "))
	_, err := ap.client.Execute(cmd)
	return err
}

func (ap *ApkManager) Remove(packages ...string) error {
	if len(packages) == 0 {
		return nil
	}
	cmd := fmt.Sprintf("sudo apk del %s", strings.Join(packages, " "))
	_, err := ap.client.Execute(cmd)
	return err
}

func (ap *ApkManager) Search(packageName string) (bool, error) {
	output, err := ap.client.Execute(fmt.Sprintf("apk search %s", packageName))
	if err != nil {
		return false, err
	}
	return strings.Contains(output, packageName), nil
}
