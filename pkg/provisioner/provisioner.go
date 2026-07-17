package provisioner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/mesh"
	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/utils"
)

const takodAccessGroup = "tako"
const takodActualRefreshInterval = "30s"
const securityCommandTimeout = 10 * time.Minute

// provisionLog routes provisioning progress prose to an injectable writer so
// machine-output modes can keep stdout reserved for parseable output.
type provisionLog struct {
	verbose bool
	output  io.Writer
}

func (l provisionLog) logf(format string, args ...any) {
	if !l.verbose {
		return
	}
	writer := l.output
	if writer == nil {
		writer = os.Stdout
	}
	fmt.Fprintf(writer, format, args...)
}

// Provisioner handles server provisioning
type Provisioner struct {
	provisionLog
	client       *ssh.Client
	upgradeLocks map[string]string
}

const (
	takodUpgradeLockBase         = "/var/lib/tako/node-upgrade/locks"
	takodUpgradeNodeLockScope    = "node"
	takodUpgradeClusterLockScope = "cluster"
	takodUpgradeNodeLockTTL      = 3 * time.Minute
)

type upgradeCommandExecutor interface {
	Execute(string) (string, error)
}

// NewProvisioner creates a new provisioner
func NewProvisioner(client *ssh.Client, verbose bool) *Provisioner {
	return &Provisioner{
		provisionLog: provisionLog{verbose: verbose},
		client:       client,
		upgradeLocks: make(map[string]string),
	}
}

// SetOutput redirects provisioning progress output. Passing nil resets
// output to os.Stdout.
func (p *Provisioner) SetOutput(w io.Writer) {
	p.output = w
}

// CheckRequirements checks if the server meets basic requirements
func (p *Provisioner) CheckRequirements() error {
	osInfo, err := DetectOS(p.client)
	if err != nil {
		return fmt.Errorf("failed to check OS: %w", err)
	}
	if !osInfo.IsSupported() {
		return fmt.Errorf("unsupported OS: %s", osInfo.String())
	}

	p.logf("  OS: %s\n", osInfo.String())

	return nil
}

// UpdateSystem updates system packages
func (p *Provisioner) UpdateSystem() error {
	p.logf("  Updating system packages with detected package manager...\n")
	if _, err := p.client.Execute(runRootScript(basePackageInstallScript())); err != nil {
		return fmt.Errorf("failed to update system packages: %w", err)
	}
	return nil
}

// InstallDocker installs and enables the container runtime used by takod.
func (p *Provisioner) InstallDocker() error {
	// Check if Docker is already installed
	if output, err := p.client.Execute("which docker"); err == nil && output != "" {
		p.logf("  Docker already installed, ensuring it's enabled on boot...\n")
		// Make sure Docker is enabled to start on boot
		p.client.Execute("sudo systemctl enable docker")
		p.client.Execute("sudo systemctl enable containerd")
		p.client.Execute("sudo systemctl start docker")

		// Verify Docker is running
		if _, err := p.client.Execute("sudo systemctl is-active docker"); err != nil {
			p.logf("  Starting Docker service...\n")
			p.client.Execute("sudo systemctl start docker")
		}
		return p.VerifySupportedDockerRuntime()
	}

	p.logf("  Installing Docker from OS packages...\n")
	if _, err := p.client.Execute(runRootScript(dockerInstallScript())); err != nil {
		return fmt.Errorf("failed to install Docker packages: %w", err)
	}

	_, _ = p.client.Execute("sudo usermod -aG docker $(id -un) 2>/dev/null || true")

	// Enable Docker to start on boot
	p.logf("  Enabling Docker to start on boot...\n")
	enableCommands := []string{
		"sudo systemctl enable docker",
		"sudo systemctl enable containerd",
		"sudo systemctl start docker",
		"sudo systemctl start containerd",
	}

	for _, cmd := range enableCommands {
		p.logf("  Running: %s\n", cmd)
		// Don't fail if containerd doesn't exist (older Docker versions)
		p.client.Execute(cmd)
	}

	// Verify Docker installation and is running
	if _, err := p.client.Execute("docker --version"); err != nil {
		return fmt.Errorf("docker installation verification failed: %w", err)
	}

	if _, err := p.client.Execute("sudo systemctl is-active docker"); err != nil {
		return fmt.Errorf("docker daemon is not running: %w", err)
	}

	return p.VerifySupportedDockerRuntime()
}

func basePackageInstallScript() string {
	return `set -eu
if command -v apt-get >/dev/null 2>&1; then
  apt-get update
  DEBIAN_FRONTEND=noninteractive apt-get upgrade -y
  DEBIAN_FRONTEND=noninteractive apt-get install -y curl wget git build-essential ca-certificates
elif command -v dnf >/dev/null 2>&1; then
  dnf upgrade -y
  dnf install -y curl-minimal wget git gcc gcc-c++ make ca-certificates
elif command -v yum >/dev/null 2>&1; then
  yum update -y
  yum install -y curl wget git gcc gcc-c++ make ca-certificates
elif command -v zypper >/dev/null 2>&1; then
  zypper --non-interactive refresh
  zypper --non-interactive update -y
  zypper --non-interactive install -y curl wget git gcc gcc-c++ make ca-certificates
elif command -v apk >/dev/null 2>&1; then
  apk update
  apk upgrade
  apk add --no-cache curl wget git build-base ca-certificates
else
  echo "no supported package manager found" >&2
  exit 1
fi
`
}

func dockerInstallScript() string {
	return `set -eu
if command -v docker >/dev/null 2>&1; then
  exit 0
fi
if command -v apt-get >/dev/null 2>&1; then
  apt-get update
  DEBIAN_FRONTEND=noninteractive apt-get install -y docker.io containerd
elif command -v dnf >/dev/null 2>&1; then
  dnf install -y moby-engine containerd || dnf install -y docker containerd
elif command -v yum >/dev/null 2>&1; then
  yum install -y docker containerd || yum install -y moby-engine containerd
elif command -v zypper >/dev/null 2>&1; then
  zypper --non-interactive install -y docker containerd
elif command -v apk >/dev/null 2>&1; then
  apk add --no-cache docker docker-cli containerd
else
  echo "no supported package manager found for Docker installation" >&2
  exit 1
fi
command -v docker >/dev/null 2>&1
`
}

func (p *Provisioner) InstallWireGuard() error {
	return mesh.EnsureWireGuardToolsWithOutput(p.client, p.verbose, p.output)
}

func (p *Provisioner) InstallTakodBinary(version string) error {
	version, err := releaseVersionArg(version)
	if err != nil {
		existing, _ := p.client.Execute("command -v tako 2>/dev/null || true")
		if strings.TrimSpace(existing) != "" {
			p.logf("  Using existing server-side tako binary: %s\n", strings.TrimSpace(existing))
			return nil
		}
		return fmt.Errorf("cannot install takod binary from release: %w; pass --takod-binary with a Linux tako binary for development setup", err)
	}

	arch, err := p.detectLinuxArch()
	if err != nil {
		return err
	}

	binaryName := fmt.Sprintf("tako-linux-%s", arch)
	script := takodBinaryInstallScript(version, binaryName)
	if _, err := p.client.Execute(runRootScript(script)); err != nil {
		return fmt.Errorf("failed to install takod binary from release %s: %w", version, err)
	}
	return nil
}

func (p *Provisioner) InstallTakodBinaryFromFile(localPath string) error {
	info, err := os.Stat(localPath)
	if err != nil {
		return fmt.Errorf("failed to inspect local takod binary: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("local takod binary path is not a regular file: %s", localPath)
	}
	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("failed to open local takod binary: %w", err)
	}
	defer file.Close()

	remoteTemp := fmt.Sprintf("/tmp/tako-upload-%d", time.Now().UnixNano())
	p.logf("  Uploading local takod binary: %s\n", localPath)
	if err := p.client.UploadReader(file, remoteTemp, 0755); err != nil {
		return fmt.Errorf("failed to upload local takod binary: %w", err)
	}

	installCmd := fmt.Sprintf("sudo install -m 0755 %s /usr/local/bin/tako && rm -f -- %s && /usr/local/bin/tako --version >/dev/null", shellQuote(remoteTemp), shellQuote(remoteTemp))
	if _, err := p.client.Execute(installCmd); err != nil {
		_, _ = p.client.Execute("rm -f -- " + shellQuote(remoteTemp))
		return fmt.Errorf("failed to install uploaded takod binary: %w", err)
	}
	return nil
}

// BeginTakodUpgrade publishes a verified candidate with a durable rollback
// copy. When the protected platform-worker binary exists it is included in
// the same transaction, so the deployment worker never remains permanently
// pinned to an older executable. An interrupted prior transaction is rolled
// back before a new one begins.
func (p *Provisioner) BeginTakodUpgrade(version string, localPath string, revalidate func() (*nodeidentity.UpgradeContract, error)) error {
	if _, err := p.client.Execute(runRootScript(installTakodUpgradeRecoveryScript())); err != nil {
		return fmt.Errorf("install node upgrade boot recovery: %w", err)
	}
	randomSuffix := make([]byte, 16)
	if _, err := rand.Read(randomSuffix); err != nil {
		return fmt.Errorf("create node upgrade candidate name: %w", err)
	}
	candidate := "/tmp/tako-upgrade-candidate-" + hex.EncodeToString(randomSuffix)
	removeCandidate := true
	defer func() {
		if removeCandidate {
			_, _ = p.client.Execute(runRootScript("rm -f -- " + shellQuote(candidate)))
		}
	}()
	expectedDigest := ""
	if strings.TrimSpace(localPath) != "" {
		info, err := os.Stat(localPath)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("local takod binary path must be a non-empty regular file: %s", localPath)
		}
		file, err := os.Open(localPath)
		if err != nil {
			return err
		}
		defer file.Close()
		hash := sha256.New()
		if _, err := io.Copy(hash, file); err != nil {
			return fmt.Errorf("hash local node upgrade candidate: %w", err)
		}
		expectedDigest = hex.EncodeToString(hash.Sum(nil))
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := p.client.UploadReader(file, candidate, 0755); err != nil {
			return fmt.Errorf("upload node upgrade candidate: %w", err)
		}
	} else {
		release, err := releaseVersionArg(version)
		if err != nil {
			return err
		}
		arch, err := p.detectLinuxArch()
		if err != nil {
			return err
		}
		binaryName := fmt.Sprintf("tako-linux-%s", arch)
		if _, err := p.client.Execute(runRootScript(takodBinaryDownloadScript(release, binaryName, candidate))); err != nil {
			return fmt.Errorf("download verified node upgrade candidate: %w", err)
		}
	}

	// Candidate transfer and checksum verification happen outside the short
	// mutation lease. Once leased, recover an expired prior transaction and
	// re-attest protected membership immediately before publication.
	if err := p.acquireTakodUpgradeLock(takodUpgradeNodeLockScope, takodUpgradeNodeLockTTL); err != nil {
		return fmt.Errorf("acquire node upgrade transaction lock: %w", err)
	}
	completed := false
	defer func() {
		if !completed {
			_ = p.releaseTakodUpgradeLock(takodUpgradeNodeLockScope)
		}
	}()
	leaseToken := p.upgradeLocks[takodUpgradeNodeLockScope]
	if _, err := p.client.Execute(runRootScript(recoverPendingTakodUpgradeScript(leaseToken))); err != nil {
		return fmt.Errorf("recover expired pending node upgrade before staging: %w", err)
	}
	contractBase64 := ""
	if revalidate != nil {
		contract, err := revalidate()
		if err != nil {
			return fmt.Errorf("revalidate node upgrade publication contract: %w", err)
		}
		if contract == nil {
			return fmt.Errorf("revalidate node upgrade publication contract returned no contract")
		}
		if err := contract.Validate(); err != nil {
			return fmt.Errorf("validate node upgrade publication contract: %w", err)
		}
		encoded, err := json.Marshal(contract)
		if err != nil {
			return err
		}
		contractBase64 = base64.StdEncoding.EncodeToString(encoded)
	}
	if _, err := p.client.Execute(runRootScript(beginTakodUpgradeScriptWithContract(candidate, expectedDigest, contractBase64, leaseToken))); err != nil {
		_, _ = p.client.Execute(runRootScript("rm -f -- " + shellQuote(candidate)))
		if rollbackErr := p.RollbackTakodUpgrade(); rollbackErr != nil {
			return fmt.Errorf("begin atomic node upgrade: %v; rollback failed: %w", err, rollbackErr)
		}
		return fmt.Errorf("begin atomic node upgrade: %w", err)
	}
	completed = true
	removeCandidate = false
	return nil
}

// ActivateTakodUpgrade restarts both services after their binary handoff.
func (p *Provisioner) ActivateTakodUpgrade() error {
	if err := p.refreshTakodUpgradeLock(takodUpgradeNodeLockScope, takodUpgradeNodeLockTTL); err != nil {
		return fmt.Errorf("retain node upgrade transaction lock: %w", err)
	}
	if _, err := p.client.Execute(runRootScript(guardTakodUpgradeScript(p.upgradeLocks[takodUpgradeNodeLockScope], `set -eu
test -f /var/lib/tako/node-upgrade/pending
systemctl restart takod
if test -f /var/lib/tako/node-upgrade/had-platform-worker; then
  systemctl restart tako-platform-worker
fi
`))); err != nil {
		return fmt.Errorf("activate node upgrade: %w", err)
	}
	return nil
}

func (p *Provisioner) CommitTakodUpgrade() error {
	if err := p.refreshTakodUpgradeLock(takodUpgradeNodeLockScope, takodUpgradeNodeLockTTL); err != nil {
		return fmt.Errorf("retain node upgrade transaction lock before commit: %w", err)
	}
	err := commitTakodUpgrade(p.client, p.upgradeLocks[takodUpgradeNodeLockScope])
	if err == nil {
		if releaseErr := p.releaseTakodUpgradeLock(takodUpgradeNodeLockScope); releaseErr != nil {
			// The committed marker is authoritative and the lease expires. Never
			// turn a completed commit into a rollback because lock release was
			// transport-ambiguous.
			p.logf("  Warning: node upgrade lock release will rely on lease expiry: %v\n", releaseErr)
		}
	}
	return err
}

func commitTakodUpgrade(executor upgradeCommandExecutor, leaseToken string) error {
	if _, err := executor.Execute(runRootScript(guardTakodUpgradeScript(leaseToken, commitTakodUpgradeScript()))); err != nil {
		if _, probeErr := executor.Execute(runRootScript(`set -eu
dir=/var/lib/tako/node-upgrade
test -f "$dir/committed"
sync -f "$dir"
`)); probeErr == nil {
			return nil
		}
		return fmt.Errorf("commit node upgrade: %w", err)
	}
	return nil
}

func commitTakodUpgradeScript() string {
	return `set -eu
dir=/var/lib/tako/node-upgrade
test -f "$dir/pending"
mv "$dir/pending" "$dir/committed"
sync -f "$dir"
`
}

// VerifyTakodUpgradeServices checks the protected handoff before rollback
// evidence is discarded. systemd starts both units from the newly published
// inode; a worker that immediately crashes therefore fails the transaction.
func (p *Provisioner) VerifyTakodUpgradeServices(requirePlatformWorker bool) error {
	if err := p.refreshTakodUpgradeLock(takodUpgradeNodeLockScope, takodUpgradeNodeLockTTL); err != nil {
		return fmt.Errorf("retain node upgrade transaction lock before verification: %w", err)
	}
	if _, err := p.client.Execute(runRootScript(guardTakodUpgradeScript(p.upgradeLocks[takodUpgradeNodeLockScope], verifyTakodUpgradeServicesScript(requirePlatformWorker)))); err != nil {
		return fmt.Errorf("verify upgraded node services: %w", err)
	}
	return nil
}

func verifyTakodUpgradeServicesScript(requirePlatformWorker bool) string {
	requireWorker := "false"
	if requirePlatformWorker {
		requireWorker = "true"
	}
	return fmt.Sprintf(`set -eu
dir=/var/lib/tako/node-upgrade
test -f "$dir/pending"
if %s; then
  test -f "$dir/had-platform-worker"
fi
systemctl is-active --quiet takod
test "$(systemctl show -p MainPID --value takod)" != "0"
if test -f "$dir/had-platform-worker"; then
  systemctl is-active --quiet tako-platform-worker
  test "$(systemctl show -p MainPID --value tako-platform-worker)" != "0"
fi
`, requireWorker)
}

func (p *Provisioner) RollbackTakodUpgrade() error {
	var rollbackErr error
	if p.upgradeLocks[takodUpgradeNodeLockScope] == "" {
		if err := p.acquireTakodUpgradeLock(takodUpgradeNodeLockScope, takodUpgradeNodeLockTTL); err != nil {
			return fmt.Errorf("acquire node upgrade transaction lock before rollback: %w", err)
		}
	} else if err := p.refreshTakodUpgradeLock(takodUpgradeNodeLockScope, takodUpgradeNodeLockTTL); err != nil {
		return fmt.Errorf("retain node upgrade transaction lock before rollback: %w", err)
	}
	rollbackScript := guardTakodUpgradeScript(p.upgradeLocks[takodUpgradeNodeLockScope], rollbackTakodUpgradeScript())
	if _, err := p.client.Execute(runRootScript(rollbackScript)); err != nil {
		if _, retryErr := p.client.Execute(runRootScript(rollbackScript)); retryErr == nil {
			rollbackErr = nil
		} else {
			rollbackErr = fmt.Errorf("rollback node upgrade: %w", err)
		}
	}
	releaseErr := p.releaseTakodUpgradeLock(takodUpgradeNodeLockScope)
	if rollbackErr != nil {
		return rollbackErr
	}
	if releaseErr != nil {
		return fmt.Errorf("release node upgrade transaction lock: %w", releaseErr)
	}
	return nil
}

// AcquireTakodUpgradeCoordinatorLock serializes staged upgrade coordinators on
// the authoritative controller. The token-bound lease rejects overlap while
// still allowing recovery after a dead CLI.
func (p *Provisioner) AcquireTakodUpgradeCoordinatorLock(ttl time.Duration) error {
	return p.acquireTakodUpgradeLock(takodUpgradeClusterLockScope, ttl)
}

func (p *Provisioner) RefreshTakodUpgradeCoordinatorLock(ttl time.Duration) error {
	return p.refreshTakodUpgradeLock(takodUpgradeClusterLockScope, ttl)
}

func (p *Provisioner) ReleaseTakodUpgradeCoordinatorLock() error {
	return p.releaseTakodUpgradeLock(takodUpgradeClusterLockScope)
}

func (p *Provisioner) acquireTakodUpgradeLock(scope string, ttl time.Duration) error {
	if p == nil || p.client == nil {
		return fmt.Errorf("upgrade lock requires an SSH client")
	}
	if ttl <= 0 {
		return fmt.Errorf("upgrade lock TTL must be positive")
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return fmt.Errorf("create upgrade lock token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	if err := acquireTakodUpgradeLock(p.client, scope, token, ttl); err != nil {
		return err
	}
	p.upgradeLocks[scope] = token
	return nil
}

func acquireTakodUpgradeLock(executor upgradeCommandExecutor, scope string, token string, ttl time.Duration) error {
	if _, err := executor.Execute(runRootScript(acquireTakodUpgradeLockScript(scope, token, ttl))); err != nil {
		if _, probeErr := executor.Execute(runRootScript(probeTakodUpgradeLockScript(scope, token))); probeErr == nil {
			return nil
		}
		return fmt.Errorf("acquire upgrade %s lock: %w", scope, err)
	}
	return nil
}

func probeTakodUpgradeLockScript(scope string, token string) string {
	return fmt.Sprintf(`set -eu
lock=%s/%s
token=%s
test -d "$lock"
test ! -L "$lock"
test "$(cat "$lock/owner")" = "$token"
expiry="$(cat "$lock/expiry")"
case "$expiry" in ''|*[!0-9]*) exit 1;; esac
test "$expiry" -gt "$(date +%%s)"
`, shellQuote(takodUpgradeLockBase), scope, shellQuote(token))
}

func guardTakodUpgradeScript(token string, body string) string {
	return fmt.Sprintf(`set -eu
umask 077
base=/var/lib/tako/node-upgrade/locks
token=%s
exec 9>"$base/.guard"
flock -x 9
chmod 0600 "$base/.guard"
lock="$base/node"
test -d "$lock"
test ! -L "$lock"
test "$(cat "$lock/owner")" = "$token"
expiry="$(cat "$lock/expiry")"
case "$expiry" in ''|*[!0-9]*) exit 1;; esac
test "$expiry" -gt "$(date +%%s)"
%s
`, shellQuote(token), body)
}

func (p *Provisioner) refreshTakodUpgradeLock(scope string, ttl time.Duration) error {
	token := p.upgradeLocks[scope]
	if token == "" {
		return fmt.Errorf("upgrade %s lock is not owned by this coordinator", scope)
	}
	if _, err := p.client.Execute(runRootScript(refreshTakodUpgradeLockScript(scope, token, ttl))); err != nil {
		return fmt.Errorf("refresh upgrade %s lock: %w", scope, err)
	}
	return nil
}

func (p *Provisioner) releaseTakodUpgradeLock(scope string) error {
	token := p.upgradeLocks[scope]
	if token == "" {
		return nil
	}
	if _, err := p.client.Execute(runRootScript(releaseTakodUpgradeLockScript(scope, token))); err != nil {
		return err
	}
	delete(p.upgradeLocks, scope)
	return nil
}

func acquireTakodUpgradeLockScript(scope string, token string, ttl time.Duration) string {
	return fmt.Sprintf(`set -eu
umask 077
parent=/var/lib/tako/node-upgrade
base=%s
scope=%s
token=%s
ttl=%d
if test -e "$parent" || test -L "$parent"; then
  test -d "$parent"
  test ! -L "$parent"
else
  install -d -m 0700 -o root -g root "$parent"
fi
test "$(stat -c '%%u:%%g:%%a' "$parent")" = "0:0:700"
if test -e "$base" || test -L "$base"; then
  test -d "$base"
  test ! -L "$base"
else
  install -d -m 0700 -o root -g root "$base"
fi
test ! -L "$base"
test "$(stat -c '%%u:%%g:%%a' "$base")" = "0:0:700"
exec 9>"$base/.guard"
flock -x 9
chmod 0600 "$base/.guard"
lock="$base/$scope"
now="$(date +%%s)"
if test -d "$lock" && test ! -L "$lock"; then
  owner="$(cat "$lock/owner" 2>/dev/null || true)"
  expiry="$(cat "$lock/expiry" 2>/dev/null || true)"
  case "$expiry" in ''|*[!0-9]*) expiry=0;; esac
  if test "$owner" != "$token" && test "$expiry" -gt "$now"; then
    echo "upgrade $scope lock held until $expiry" >&2
    exit 73
  fi
  rm -rf -- "$lock"
elif test -e "$lock" || test -L "$lock"; then
  echo "unsafe upgrade lock path" >&2
  exit 74
fi
mkdir -m 0700 "$lock"
printf '%%s\n' "$token" > "$lock/owner.tmp"
printf '%%s\n' "$((now + ttl))" > "$lock/expiry.tmp"
chmod 0600 "$lock/owner.tmp" "$lock/expiry.tmp"
mv "$lock/owner.tmp" "$lock/owner"
mv "$lock/expiry.tmp" "$lock/expiry"
sync -f "$lock"
sync -f "$base"
`, shellQuote(takodUpgradeLockBase), shellQuote(scope), shellQuote(token), int64(ttl/time.Second))
}

func refreshTakodUpgradeLockScript(scope string, token string, ttl time.Duration) string {
	return fmt.Sprintf(`set -eu
umask 077
base=%s
lock="$base/%s"
token=%s
ttl=%d
exec 9>"$base/.guard"
flock -x 9
chmod 0600 "$base/.guard"
test -d "$lock"
test ! -L "$lock"
test "$(cat "$lock/owner")" = "$token"
now="$(date +%%s)"
expiry="$(cat "$lock/expiry")"
case "$expiry" in ''|*[!0-9]*) exit 1;; esac
test "$expiry" -gt "$now"
printf '%%s\n' "$((now + ttl))" > "$lock/expiry.tmp"
chmod 0600 "$lock/expiry.tmp"
mv "$lock/expiry.tmp" "$lock/expiry"
sync -f "$lock"
`, shellQuote(takodUpgradeLockBase), scope, shellQuote(token), int64(ttl/time.Second))
}

func releaseTakodUpgradeLockScript(scope string, token string) string {
	return fmt.Sprintf(`set -eu
umask 077
base=%s
lock="$base/%s"
token=%s
exec 9>"$base/.guard"
flock -x 9
chmod 0600 "$base/.guard"
if ! test -e "$lock" && ! test -L "$lock"; then
  exit 0
fi
test -d "$lock"
test ! -L "$lock"
test "$(cat "$lock/owner")" = "$token"
rm -rf -- "$lock"
sync -f "$base"
`, shellQuote(takodUpgradeLockBase), scope, shellQuote(token))
}

func beginTakodUpgradeScript(candidate string) string {
	return beginTakodUpgradeScriptWithDigest(candidate, "")
}

func beginTakodUpgradeScriptWithDigest(candidate string, expectedDigest string) string {
	return beginTakodUpgradeScriptWithContract(candidate, expectedDigest, "", "test-node-lease")
}

func beginTakodUpgradeScriptWithContract(candidate string, expectedDigest string, contractBase64 string, leaseToken string) string {
	contractArg := ""
	if contractBase64 != "" {
		contractArg = " --contract-base64 " + shellQuote(contractBase64)
	}
	return fmt.Sprintf(`set -eu
umask 077
dir=/var/lib/tako/node-upgrade
base=/var/lib/tako/node-upgrade/locks
token=%s
renew_ttl=%d
if test -e "$dir" || test -L "$dir"; then
  test ! -L "$dir"
else
  install -d -m 0700 -o root -g root "$dir"
fi
test "$(stat -c '%%u:%%g:%%a' "$dir")" = "0:0:700"
test -d "$base"
test ! -L "$base"
exec 9>"$base/.guard"
flock -x 9
chmod 0600 "$base/.guard"
lock="$base/node"
test -d "$lock"
test ! -L "$lock"
test "$(cat "$lock/owner")" = "$token"
expiry="$(cat "$lock/expiry")"
case "$expiry" in ''|*[!0-9]*) exit 1;; esac
test "$expiry" -gt "$(date +%%s)"
renew_node_lease() {
  now="$(date +%%s)"
  printf '%%s\n' "$((now + renew_ttl))" > "$lock/expiry.tmp"
  chmod 0600 "$lock/expiry.tmp"
  mv "$lock/expiry.tmp" "$lock/expiry"
  sync -f "$lock"
}
trap renew_node_lease EXIT
if test -f "$dir/committed" || test -f "$dir/rolled-back"; then
  sync -f "$dir"
  rm -f -- "$dir/candidate" "$dir/previous-takod" "$dir/previous-platform-worker" "$dir/previous-version-manifest" "$dir/had-platform-worker" "$dir/had-version-manifest" "$dir/committed" "$dir/rolled-back"
  sync -f "$dir"
fi
if test -f "$dir/pending"; then
%s
fi
test -f %s
test -x %s
test ! -L %s
install -m 0755 %s "$dir/candidate"
rm -f -- %s
expected=%s
if test -n "$expected"; then
  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$dir/candidate" | awk '{print $1}')"
  else
    actual="$(shasum -a 256 "$dir/candidate" | awk '{print $1}')"
  fi
	  test "$actual" = "$expected"
fi
"$dir/candidate" platform node upgrade-publication-guard --lease-token "$token"%s
`, shellQuote(leaseToken), int64(takodUpgradeNodeLockTTL/time.Second), indentRootScript(rollbackTakodUpgradeScript(), "  "), shellQuote(candidate), shellQuote(candidate), shellQuote(candidate), shellQuote(candidate), shellQuote(candidate), shellQuote(expectedDigest), contractArg)
}

func recoverPendingTakodUpgradeScript(leaseToken string) string {
	return fmt.Sprintf(`set -eu
umask 077
dir=/var/lib/tako/node-upgrade
base=/var/lib/tako/node-upgrade/locks
token=%s
exec 9>"$base/.guard"
flock -x 9
chmod 0600 "$base/.guard"
lock="$base/node"
test -d "$lock"
test ! -L "$lock"
test "$(cat "$lock/owner")" = "$token"
expiry="$(cat "$lock/expiry")"
case "$expiry" in ''|*[!0-9]*) exit 1;; esac
test "$expiry" -gt "$(date +%%s)"
if test -f "$dir/pending"; then
%s
fi
`, shellQuote(leaseToken), indentRootScript(rollbackTakodUpgradeScript(), "  "))
}

func rollbackTakodUpgradeScript() string {
	return `set -eu
dir=/var/lib/tako/node-upgrade
if ! test -f "$dir/pending"; then
	if test -f "$dir/committed"; then
	  echo "node upgrade is already durably committed" >&2
	  exit 75
	fi
	if test -f "$dir/rolled-back"; then
	  sync -f "$dir"
	  rm -f -- "$dir/candidate" "$dir/previous-takod" "$dir/previous-platform-worker" "$dir/previous-version-manifest" "$dir/had-platform-worker" "$dir/had-version-manifest" "$dir/rolled-back"
    sync -f "$dir"
  else
    rm -f -- "$dir/candidate"
  fi
  exit 0
fi
test -f "$dir/previous-takod"
install -m 0755 "$dir/previous-takod" /usr/local/bin/.tako-rollback-next
mv -f /usr/local/bin/.tako-rollback-next /usr/local/bin/tako
sync -f /usr/local/bin
if test -f "$dir/had-platform-worker"; then
  test -f "$dir/previous-platform-worker"
  install -m 0755 "$dir/previous-platform-worker" /usr/local/lib/tako/.tako-rollback-next
  mv -f /usr/local/lib/tako/.tako-rollback-next /usr/local/lib/tako/tako
  sync -f /usr/local/lib/tako
fi
if test -f "$dir/had-version-manifest"; then
  test -f "$dir/previous-version-manifest"
  install -m 0644 "$dir/previous-version-manifest" /etc/tako/version.json
fi
systemctl restart takod
if test -f "$dir/had-platform-worker"; then
  systemctl restart tako-platform-worker
fi
mv "$dir/pending" "$dir/rolled-back"
sync -f "$dir"
set +e
rm -f -- "$dir/candidate" "$dir/previous-takod" "$dir/previous-platform-worker" "$dir/previous-version-manifest" "$dir/had-platform-worker" "$dir/had-version-manifest" "$dir/rolled-back"
sync -f "$dir" >/dev/null 2>&1
exit 0`
}

func bootRecoverTakodUpgradeScript() string {
	return `#!/bin/sh
set -eu
dir=/var/lib/tako/node-upgrade
clear_locks() {
  if test -d "$dir/locks" && test ! -L "$dir/locks"; then
	 rm -rf -- "$dir/locks/node"
    sync -f "$dir/locks" >/dev/null 2>&1 || true
  fi
}
trap clear_locks EXIT
if ! test -f "$dir/pending"; then
	 exit 0
fi
test -f "$dir/previous-takod"
install -m 0755 "$dir/previous-takod" /usr/local/bin/.tako-rollback-next
mv -f /usr/local/bin/.tako-rollback-next /usr/local/bin/tako
sync -f /usr/local/bin
if test -f "$dir/had-platform-worker"; then
  test -f "$dir/previous-platform-worker"
  install -m 0755 "$dir/previous-platform-worker" /usr/local/lib/tako/.tako-rollback-next
  mv -f /usr/local/lib/tako/.tako-rollback-next /usr/local/lib/tako/tako
  sync -f /usr/local/lib/tako
fi
if test -f "$dir/had-version-manifest"; then
  test -f "$dir/previous-version-manifest"
  install -m 0644 "$dir/previous-version-manifest" /etc/tako/version.json
fi
mv "$dir/pending" "$dir/rolled-back"
sync -f "$dir"
set +e
rm -f -- "$dir/candidate" "$dir/previous-takod" "$dir/previous-platform-worker" "$dir/previous-version-manifest" "$dir/had-platform-worker" "$dir/had-version-manifest" "$dir/rolled-back"
sync -f "$dir" >/dev/null 2>&1
exit 0
`
}

func takodUpgradeRecoveryUnit() string {
	return `[Unit]
Description=Recover an interrupted Tako node upgrade
After=local-fs.target
Before=takod.service tako-platform-worker.service

[Service]
Type=oneshot
ExecStart=/var/lib/tako/node-upgrade/recover-on-boot

[Install]
WantedBy=multi-user.target
`
}

func installTakodUpgradeRecoveryScript() string {
	recovery := base64.StdEncoding.EncodeToString([]byte(bootRecoverTakodUpgradeScript()))
	unit := base64.StdEncoding.EncodeToString([]byte(takodUpgradeRecoveryUnit()))
	return fmt.Sprintf(`set -eu
dir=/var/lib/tako/node-upgrade
if test -e "$dir" || test -L "$dir"; then
  test ! -L "$dir"
else
  install -d -m 0700 -o root -g root "$dir"
fi
test "$(stat -c '%%u:%%g:%%a' "$dir")" = "0:0:700"
printf %%s %s | base64 -d > "$dir/recover-on-boot.tmp"
chmod 0700 "$dir/recover-on-boot.tmp"
chown root:root "$dir/recover-on-boot.tmp"
mv -f "$dir/recover-on-boot.tmp" "$dir/recover-on-boot"
printf %%s %s | base64 -d > /etc/systemd/system/.tako-node-upgrade-recovery.service.tmp
chmod 0644 /etc/systemd/system/.tako-node-upgrade-recovery.service.tmp
chown root:root /etc/systemd/system/.tako-node-upgrade-recovery.service.tmp
mv -f /etc/systemd/system/.tako-node-upgrade-recovery.service.tmp /etc/systemd/system/tako-node-upgrade-recovery.service
sync -f "$dir"
sync -f /etc/systemd/system
systemctl daemon-reload
systemctl enable tako-node-upgrade-recovery.service
`, shellQuote(recovery), shellQuote(unit))
}

func indentRootScript(script string, prefix string) string {
	return prefix + strings.ReplaceAll(script, "\n", "\n"+prefix)
}

func takodBinaryDownloadScript(version string, binaryName string, destination string) string {
	downloadURL := fmt.Sprintf("https://github.com/redentordev/tako-cli/releases/download/%s/%s", version, binaryName)
	checksumsURL := fmt.Sprintf("https://github.com/redentordev/tako-cli/releases/download/%s/checksums.txt", version)
	return fmt.Sprintf(`set -eu
tmp="$(mktemp)"
checksums="$(mktemp)"
cleanup() { rm -f "$tmp" "$checksums"; }
trap cleanup EXIT
if command -v curl >/dev/null 2>&1; then
  curl -fL --retry 3 --connect-timeout 15 -o "$tmp" %s
  curl -fL --retry 3 --connect-timeout 15 -o "$checksums" %s
elif command -v wget >/dev/null 2>&1; then
  wget --tries=3 --timeout=15 -O "$tmp" %s
  wget --tries=3 --timeout=15 -O "$checksums" %s
else
  echo "curl or wget is required" >&2
  exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$tmp" | awk '{print $1}')"
else
  actual="$(shasum -a 256 "$tmp" | awk '{print $1}')"
fi
expected="$(awk -v name=%s '$2 == name { print $1; found = 1; exit } END { if (!found) exit 1 }' "$checksums")"
test "$actual" = "$expected"
install -m 0755 "$tmp" %s
`, shellQuote(downloadURL), shellQuote(checksumsURL), shellQuote(downloadURL), shellQuote(checksumsURL), shellQuote(binaryName), shellQuote(destination))
}

func takodBinaryInstallScript(version string, binaryName string) string {
	downloadURL := fmt.Sprintf("https://github.com/redentordev/tako-cli/releases/download/%s/%s", version, binaryName)
	checksumsURL := fmt.Sprintf("https://github.com/redentordev/tako-cli/releases/download/%s/checksums.txt", version)
	return fmt.Sprintf(`set -eu
tmp="$(mktemp)"
checksums="$(mktemp)"
cleanup() { rm -f "$tmp" "$checksums"; }
trap cleanup EXIT

fetch() {
  url="$1"
  dest="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fL --retry 3 --connect-timeout 15 -o "$dest" "$url"
  elif command -v wget >/dev/null 2>&1; then
    wget --tries=3 --timeout=15 -O "$dest" "$url"
  else
    echo "curl or wget is required to install takod binary" >&2
    exit 1
  fi
}

calc_sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
    return 0
  fi
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
    return 0
  fi
  echo "sha256sum or shasum is required to verify takod binary" >&2
  return 1
}

fetch %s "$tmp"
fetch %s "$checksums"

expected="$(awk -v name=%s '$2 == name { print $1; found = 1; exit } END { if (!found) exit 1 }' "$checksums")" || {
  echo "checksum for %s not found in checksums.txt" >&2
  exit 1
}
calculated="$(calc_sha256 "$tmp")"
if [ "$calculated" != "$expected" ]; then
  echo "checksum mismatch for %s" >&2
  echo "expected: $expected" >&2
  echo "got:      $calculated" >&2
  exit 1
fi

install -m 0755 "$tmp" /usr/local/bin/tako
/usr/local/bin/tako --version >/dev/null
`,
		shellQuote(downloadURL),
		shellQuote(checksumsURL),
		shellQuote(binaryName),
		binaryName,
		binaryName,
	)
}

func (p *Provisioner) InstallTakodService(socket string, dataDir string, nodeName string) error {
	binaryPath, _ := p.client.Execute(takodBinaryPathCommand())
	binaryPath = strings.TrimSpace(binaryPath)
	if binaryPath == "" {
		return fmt.Errorf("no server-side tako binary found; run setup with a release build or pass --takod-binary")
	}
	var err error
	if binaryPath, err = systemdPathArg(binaryPath, ""); err != nil {
		return fmt.Errorf("invalid server-side tako binary path: %w", err)
	}
	if socket, err = systemdPathArg(socket, "/run/tako/takod.sock"); err != nil {
		return fmt.Errorf("invalid takod socket path: %w", err)
	}
	if dataDir, err = systemdPathArg(dataDir, "/var/lib/tako"); err != nil {
		return fmt.Errorf("invalid takod data directory: %w", err)
	}
	if nodeName, err = systemdIdentifierArg(nodeName); err != nil {
		return fmt.Errorf("invalid takod node name: %w", err)
	}
	if err := p.ensureTakodAccessGroup(); err != nil {
		return err
	}

	unit := buildTakodSystemdUnit(binaryPath, socket, dataDir, nodeName, takodActualRefreshInterval)

	uploadServiceCmd := fmt.Sprintf("sudo tee /etc/systemd/system/takod.service > /dev/null << 'EOFSERVICE'\n%s\nEOFSERVICE", unit)
	if _, err := p.client.Execute(uploadServiceCmd); err != nil {
		return fmt.Errorf("failed to write takod service: %w", err)
	}

	commands := []string{
		"sudo systemctl daemon-reload",
		"sudo systemctl enable takod",
		"sudo systemctl restart takod",
	}
	for _, cmd := range commands {
		if _, err := p.client.Execute(cmd); err != nil {
			return fmt.Errorf("failed to run %s: %w", cmd, err)
		}
	}
	return nil
}

func takodBinaryPathCommand() string {
	return "command -v tako 2>/dev/null || { test -x /usr/local/bin/tako && echo /usr/local/bin/tako; } || true"
}

func buildTakodSystemdUnit(binaryPath string, socket string, dataDir string, nodeName string, actualRefreshInterval string) string {
	return fmt.Sprintf(`[Unit]
Description=Tako node agent
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
Type=simple
User=root
Group=%s
RuntimeDirectory=tako
RuntimeDirectoryMode=0750
UMask=0007
ExecStart=%s takod run --socket %s --data-dir %s --node %s --identity-file %s --actual-refresh-interval %s --build-cache-prune-interval %s --build-cache-keep-storage %s
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`, takodAccessGroup, binaryPath, socket, dataDir, nodeName, nodeidentity.DefaultPath, actualRefreshInterval, takod.DefaultBuildCachePruneInterval, takod.DefaultBuildCacheKeepStorage)
}

func (p *Provisioner) ensureTakodAccessGroup() error {
	script := fmt.Sprintf(`set -eu
if ! getent group %[1]s >/dev/null 2>&1; then
  if command -v groupadd >/dev/null 2>&1; then
    groupadd --system %[1]s 2>/dev/null || groupadd -r %[1]s 2>/dev/null || groupadd %[1]s
  elif command -v addgroup >/dev/null 2>&1; then
    addgroup -S %[1]s 2>/dev/null || addgroup --system %[1]s 2>/dev/null || addgroup %[1]s
  else
    echo "groupadd or addgroup is required to create the takod access group" >&2
    exit 1
  fi
fi
`, takodAccessGroup)
	if _, err := p.client.Execute(runRootScript(script)); err != nil {
		return fmt.Errorf("failed to ensure takod access group: %w", err)
	}
	return nil
}

func (p *Provisioner) detectLinuxArch() (string, error) {
	output, err := p.client.Execute("uname -m")
	if err != nil {
		return "", fmt.Errorf("failed to detect server architecture: %w", err)
	}
	return normalizeLinuxArch(strings.TrimSpace(output))
}

func normalizeLinuxArch(machine string) (string, error) {
	switch strings.ToLower(machine) {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported Linux architecture %q", machine)
	}
}

func releaseVersionArg(version string) (string, error) {
	trimmed := strings.TrimSpace(version)
	if trimmed != version {
		return "", fmt.Errorf("release version must not contain leading or trailing whitespace")
	}
	if version == "" || version == "dev" || version == "unknown" {
		return "", fmt.Errorf("release version is not available for this build")
	}
	if isGitDescribeSnapshot(version) {
		return "", fmt.Errorf("release version %q is a non-release build; pass --takod-binary with a Linux tako binary", version)
	}
	for _, r := range version {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return "", fmt.Errorf("release version contains unsupported characters")
	}
	return version, nil
}

func isGitDescribeSnapshot(version string) bool {
	if strings.Contains(version, "-dirty") {
		return true
	}
	parts := strings.Split(version, "-")
	if len(parts) < 3 {
		return false
	}
	for i := 1; i < len(parts)-1; i++ {
		if !allASCIIBytes(parts[i], isASCIIDigit) {
			continue
		}
		next := parts[i+1]
		if len(next) > 1 && next[0] == 'g' && allASCIIBytes(next[1:], isASCIIHex) {
			return true
		}
	}
	return false
}

func allASCIIBytes(value string, valid func(byte) bool) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if !valid(value[i]) {
			return false
		}
	}
	return true
}

func isASCIIDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

func isASCIIHex(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

func systemdPathArg(value string, fallback string) (string, error) {
	if value == "" {
		value = fallback
	}
	if value == "" {
		return "", fmt.Errorf("path is required")
	}
	if !strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("path must be absolute")
	}
	if strings.ContainsAny(value, " \t\r\n") {
		return "", fmt.Errorf("path must not contain whitespace")
	}
	return value, nil
}

func systemdIdentifierArg(value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("identifier is required")
	}
	if len(value) > 63 {
		return "", fmt.Errorf("identifier is too long")
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return "", fmt.Errorf("identifier contains unsupported characters")
	}
	return value, nil
}

func runRootScript(script string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(script))
	return fmt.Sprintf(`sudo sh -c 'set -eu
tmp=$(mktemp /tmp/tako-root-script.XXXXXX)
trap '\''rm -f "$tmp"'\'' EXIT
base64 -d > "$tmp"
chmod 700 "$tmp"
sh "$tmp"' <<'TAKO_ROOT_SCRIPT'
%s
TAKO_ROOT_SCRIPT`, encoded)
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// Note: tako-proxy is handled per-deployment by the deployer.
// No system-wide reverse proxy installation is needed during server setup

// ConfigureFirewall configures UFW when available. Non-Debian cloud images
// commonly rely on provider firewalls instead of UFW; setup should not fail on
// those hosts merely because the ufw package is unavailable.
func (p *Provisioner) ConfigureFirewall(meshListenPort int) error {
	if meshListenPort < 1 || meshListenPort > 65535 {
		return fmt.Errorf("mesh listen port must be between 1 and 65535")
	}

	hasUFW := true
	if _, err := p.client.Execute("command -v ufw"); err != nil {
		hasUFW = false
	}
	if !hasUFW {
		osInfo, err := DetectOS(p.client)
		if err == nil && shouldSkipUFWFirewall(osInfo) {
			p.logf("  UFW not available on %s; relying on provider/native firewall\n", osInfo.String())
			return nil
		}
		p.logf("  Running: sudo apt-get install -y ufw\n")
		if _, err := p.client.Execute("sudo apt-get install -y ufw"); err != nil {
			return fmt.Errorf("failed to install ufw: %w", err)
		}
	}

	// Check if UFW is already active
	output, _ := p.client.Execute("sudo ufw status | grep -i 'Status: active'")
	isActive := strings.TrimSpace(output) != ""

	if isActive {
		p.logf("  UFW already active, updating rules...\n")
	}

	// Disable UFW temporarily to safely update rules
	if isActive {
		p.logf("  Temporarily disabling UFW to update rules\n")
		p.client.Execute("sudo ufw --force disable")
	}

	// Reset UFW to clean state (only if not active before)
	if !isActive {
		p.logf("  Running: sudo ufw --force reset\n")
		p.client.Execute("sudo ufw --force reset")
	}

	// Set default policies
	commands := []string{
		"sudo ufw --force default deny incoming",
		"sudo ufw --force default allow outgoing",
	}

	for _, cmd := range commands {
		p.logf("  Running: %s\n", cmd)
		if _, err := p.client.Execute(cmd); err != nil {
			return fmt.Errorf("command failed '%s': %w", cmd, err)
		}
	}

	// Allow essential services with rate limiting for SSH (use || true to ignore "Skipping adding existing rule" errors)
	for _, cmd := range firewallAllowCommands(meshListenPort) {
		p.logf("  Running: %s\n", cmd)
		// Execute but don't fail on "rule already exists" errors
		p.client.Execute(cmd)
	}

	// Enable UFW
	p.logf("  Running: sudo ufw --force enable\n")
	if _, err := p.client.Execute("sudo ufw --force enable"); err != nil {
		return fmt.Errorf("failed to enable ufw: %w", err)
	}

	// Show status
	if p.verbose {
		output, _ := p.client.Execute("sudo ufw status verbose")
		p.logf("\n  UFW Status:\n%s\n", output)
	}

	return nil
}

func shouldSkipUFWFirewall(osInfo *OSInfo) bool {
	if osInfo == nil {
		return false
	}
	return osInfo.Family != OSFamilyDebian
}

// FirewallAllowedPorts lists the port/protocol pairs setup allows through
// UFW, in the order the rules are applied.
func FirewallAllowedPorts(meshListenPort int) []string {
	return []string{
		"22/tcp",
		"80/tcp",
		"443/tcp",
		"443/udp",
		fmt.Sprintf("%d/udp", meshListenPort),
	}
}

func firewallAllowCommands(meshListenPort int) []string {
	return []string{
		// SSH with rate limiting (max 10 connections per 30 seconds per IP).
		"sudo ufw limit 22/tcp comment 'SSH with rate limiting' || true",

		// HTTP/HTTPS.
		"sudo ufw allow 80/tcp comment 'HTTP' || true",
		"sudo ufw allow 443/tcp comment 'HTTPS' || true",
		"sudo ufw allow 443/udp comment 'HTTPS HTTP/3' || true",

		fmt.Sprintf("sudo ufw allow %d/udp comment 'Tako mesh' || true", meshListenPort),
	}
}

// HardenSecurity applies comprehensive security hardening
func (p *Provisioner) HardenSecurity() error {
	p.logf("  Installing and configuring security tools...\n")

	p.logf("  Installing security packages...\n")
	if _, err := p.executeSecurityCommand(runRootScript(securityPackagesInstallScript())); err != nil {
		return fmt.Errorf("failed to install security packages: %w", err)
	}

	if p.fail2banAvailable() {
		// Configure fail2ban with custom jail for SSH
		fail2banConfig := fail2banSSHDJailConfig(p.detectSSHClientIP())

		p.logf("  Configuring fail2ban jail for SSH...\n")

		// Write fail2ban jail config
		fail2banCmd := fmt.Sprintf("sudo mkdir -p /etc/fail2ban/jail.d && sudo tee /etc/fail2ban/jail.d/sshd.local > /dev/null << 'EOF'\n%s\nEOF", fail2banConfig)
		if _, err := p.executeSecurityCommand(fail2banCmd); err != nil {
			return fmt.Errorf("failed to configure fail2ban SSH jail: %w", err)
		}

		// Enable and start fail2ban
		if _, err := p.executeSecurityCommand("sudo systemctl enable fail2ban"); err != nil {
			return fmt.Errorf("failed to enable fail2ban: %w", err)
		}
		if _, err := p.executeSecurityCommand("sudo systemctl restart fail2ban"); err != nil {
			return fmt.Errorf("failed to restart fail2ban: %w", err)
		}
	} else {
		p.logf("  fail2ban unavailable on this host; skipping fail2ban jail\n")
	}

	// Configure SSH hardening
	p.logf("  Hardening SSH configuration...\n")

	sshHardeningCommands := []string{
		// Increase connection limits to prevent lockouts
		"sudo sed -i 's/^#MaxStartups.*/MaxStartups 100:30:100/' /etc/ssh/sshd_config",
		"sudo grep -q '^MaxStartups' /etc/ssh/sshd_config || echo 'MaxStartups 100:30:100' | sudo tee -a /etc/ssh/sshd_config",

		"sudo sed -i 's/^#MaxSessions.*/MaxSessions 100/' /etc/ssh/sshd_config",
		"sudo grep -q '^MaxSessions' /etc/ssh/sshd_config || echo 'MaxSessions 100' | sudo tee -a /etc/ssh/sshd_config",

		// Keep connections alive
		"sudo sed -i 's/^#ClientAliveInterval.*/ClientAliveInterval 60/' /etc/ssh/sshd_config",
		"sudo grep -q '^ClientAliveInterval' /etc/ssh/sshd_config || echo 'ClientAliveInterval 60' | sudo tee -a /etc/ssh/sshd_config",

		"sudo sed -i 's/^#ClientAliveCountMax.*/ClientAliveCountMax 10/' /etc/ssh/sshd_config",
		"sudo grep -q '^ClientAliveCountMax' /etc/ssh/sshd_config || echo 'ClientAliveCountMax 10' | sudo tee -a /etc/ssh/sshd_config",

		// Increase login grace time
		"sudo sed -i 's/^#LoginGraceTime.*/LoginGraceTime 120/' /etc/ssh/sshd_config",
		"sudo grep -q '^LoginGraceTime' /etc/ssh/sshd_config || echo 'LoginGraceTime 120' | sudo tee -a /etc/ssh/sshd_config",

		// Disable password authentication (key-based only)
		"sudo sed -i 's/^PasswordAuthentication yes/PasswordAuthentication no/' /etc/ssh/sshd_config",
		"sudo sed -i 's/^#PasswordAuthentication yes/PasswordAuthentication no/' /etc/ssh/sshd_config",

		// Keep PermitRootLogin yes for Tako deployments (we use keys, not passwords)
		// This is needed for Tako to deploy applications
		"sudo sed -i 's/^PermitRootLogin no/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config",
		"sudo sed -i 's/^#PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config",
		"sudo grep -q '^PermitRootLogin' /etc/ssh/sshd_config || echo 'PermitRootLogin prohibit-password' | sudo tee -a /etc/ssh/sshd_config",
	}

	for _, cmd := range sshHardeningCommands {
		p.client.Execute(cmd)
	}

	// Configure automatic security updates
	p.logf("  Enabling automatic security updates...\n")
	if _, err := p.executeSecurityCommand(runRootScript(unattendedUpgradesConfigScript())); err != nil {
		return fmt.Errorf("failed to configure unattended upgrades: %w", err)
	}

	// Enable and restart SSH service
	p.logf("  Enabling and restarting SSH service...\n")

	// CRITICAL: Enable SSH to start on boot
	if _, err := p.client.Execute("sudo systemctl enable ssh"); err != nil {
		p.logf("  Warning: Failed to enable SSH service: %v\n", err)
	}

	// Restart SSH service to apply changes (try both ssh and sshd)
	p.client.Execute("sudo systemctl restart ssh")
	p.client.Execute("sudo systemctl restart sshd")

	// Verify SSH is running
	output, err := p.client.Execute("sudo systemctl is-active ssh")
	if err != nil || strings.TrimSpace(output) != "active" {
		p.logf("  Warning: SSH service may not be running properly\n")
	}

	p.logf("  ✓ Security hardening completed\n")
	p.logf("  - fail2ban: enabled (5 retries in 10min = 1hr ban)\n")
	p.logf("  - SSH: hardened (key-based auth only)\n")
	p.logf("  - Auto-updates: enabled\n")
	p.logf("  - SSH service: enabled on boot\n")

	return nil
}

func (p *Provisioner) executeSecurityCommand(command string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), securityCommandTimeout)
	defer cancel()
	return p.client.ExecuteWithContext(ctx, command)
}

func (p *Provisioner) fail2banAvailable() bool {
	if _, err := p.executeSecurityCommand("command -v fail2ban-client"); err == nil {
		return true
	}
	if output, err := p.executeSecurityCommand("systemctl list-unit-files fail2ban.service --no-legend 2>/dev/null | awk '{print $1}'"); err == nil {
		return strings.TrimSpace(output) == "fail2ban.service"
	}
	return false
}

func securityPackagesInstallScript() string {
	return `set -eu
if command -v apt-get >/dev/null 2>&1; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get update
  apt-get install -y fail2ban unattended-upgrades ufw
elif command -v dnf >/dev/null 2>&1; then
  echo "security package bootstrap skipped for dnf hosts; using SSH hardening and provider/native firewall"
elif command -v yum >/dev/null 2>&1; then
  echo "security package bootstrap skipped for yum hosts; using SSH hardening and provider/native firewall"
elif command -v apk >/dev/null 2>&1; then
  apk add --no-cache fail2ban
else
  echo "security package bootstrap skipped for this host; using SSH hardening only"
fi
`
}

func unattendedUpgradesConfigScript() string {
	return `set -eu
if command -v apt-get >/dev/null 2>&1; then
  export DEBIAN_FRONTEND=noninteractive
  cat > /etc/apt/apt.conf.d/20auto-upgrades <<'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
EOF
fi
`
}

func fail2banSSHDJailConfig(clientIP string) string {
	var b strings.Builder
	b.WriteString(`[sshd]
enabled = true
port = ssh
filter = sshd
logpath = /var/log/auth.log
maxretry = 5
findtime = 600
bantime = 3600
`)
	if parsed := net.ParseIP(strings.TrimSpace(clientIP)); parsed != nil {
		b.WriteString("ignoreip = 127.0.0.1/8 ::1 ")
		b.WriteString(parsed.String())
		b.WriteString("\n")
	}
	return b.String()
}

func (p *Provisioner) detectSSHClientIP() string {
	output, err := p.client.Execute("printf '%s' \"$SSH_CONNECTION\" | awk '{print $1}'")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(output)
}

// SetupDeployUser ensures deploy user exists and has proper permissions
func (p *Provisioner) SetupDeployUser(username string) error {
	// Defense-in-depth: validate username before using in shell commands
	if !utils.IsValidUnixUsername(username) {
		return fmt.Errorf("invalid username %q: must be a valid POSIX username", username)
	}
	if err := p.ensureTakodAccessGroup(); err != nil {
		return err
	}

	// Check if user exists
	output, err := p.client.Execute(buildUserIDCommand(username))
	if err != nil || output == "" {
		// User doesn't exist, create it
		commands := []string{
			buildUserCreateCommand(username),
		}

		for _, cmd := range commands {
			p.logf("  Running: %s\n", cmd)
			if _, err := p.client.Execute(cmd); err != nil {
				// May fail if user already exists, that's okay
				p.logf("  Warning: %v\n", err)
			}
		}
	} else {
		p.logf("  User %s already exists\n", username)
	}

	// Runtime access is mediated by takod's Unix socket, not broad sudo or Docker group membership.
	if username != "root" {
		if _, err := p.client.Execute(buildTakodAccessCommand(username)); err != nil {
			return fmt.Errorf("failed to grant takod socket access to %s: %w", username, err)
		}
	}

	return nil
}

func buildUserIDCommand(username string) string {
	return fmt.Sprintf("id -u %s", shellQuote(username))
}

func buildUserCreateCommand(username string) string {
	return fmt.Sprintf("sudo useradd -m -s /bin/bash %s", shellQuote(username))
}

func buildTakodAccessCommand(username string) string {
	return fmt.Sprintf("sudo usermod -aG %s %s", shellQuote(takodAccessGroup), shellQuote(username))
}

// VerifyAutoRecovery verifies that critical services are enabled for auto-recovery
func (p *Provisioner) VerifyAutoRecovery() error {
	p.logf("→ Verifying auto-recovery configuration...\n")

	// Check if critical services are enabled
	services := []string{"docker", "containerd", "ssh"}

	for _, service := range services {
		output, err := p.client.Execute(fmt.Sprintf("sudo systemctl is-enabled %s 2>/dev/null || echo 'not-found'", service))
		status := strings.TrimSpace(output)

		if err != nil || (status != "enabled" && status != "static") {
			p.logf("  ⚠ %s is not enabled, enabling now...\n", service)
			p.client.Execute(fmt.Sprintf("sudo systemctl enable %s", service))
		} else {
			p.logf("  ✓ %s is enabled on boot\n", service)
		}
	}

	// Verify services are running
	for _, service := range services {
		output, _ := p.client.Execute(fmt.Sprintf("sudo systemctl is-active %s 2>/dev/null", service))
		status := strings.TrimSpace(output)

		if status != "active" {
			p.logf("  ⚠ %s is not running, starting...\n", service)
			p.client.Execute(fmt.Sprintf("sudo systemctl start %s", service))
		} else {
			p.logf("  ✓ %s is running\n", service)
		}
	}

	p.logf("  ✓ Auto-recovery verification complete\n")

	return nil
}

// InstallMonitoringAgent installs the lightweight monitoring agent
func (p *Provisioner) InstallMonitoringAgent() error {
	p.logf("→ Installing monitoring agent...\n")

	// Check if already installed and running (but allow updates)
	output, err := p.client.Execute("systemctl is-active tako-monitor 2>/dev/null")
	alreadyRunning := err == nil && strings.TrimSpace(output) == "active"

	if alreadyRunning {
		p.logf("  Updating monitoring agent...\n")
	}

	// Read the agent script from embedded file
	agentScript := `#!/bin/bash
# Tako CLI Monitoring Agent
set -euo pipefail

INTERVAL=${MONITOR_INTERVAL:-60}
STATE_DIR="/var/lib/tako/metrics"
METRICS_FILE="$STATE_DIR/current.json"

mkdir -p "$STATE_DIR"

get_cpu_usage() {
    local cpu_line=$(grep '^cpu ' /proc/stat)
    local cpu_times=($cpu_line)
    local user=${cpu_times[1]}
    local nice=${cpu_times[2]}
    local system=${cpu_times[3]}
    local idle=${cpu_times[4]}
    local iowait=${cpu_times[5]}
    local irq=${cpu_times[6]}
    local softirq=${cpu_times[7]}
    local steal=${cpu_times[8]:-0}

    local total=$((user + nice + system + idle + iowait + irq + softirq + steal))
    local busy=$((total - idle - iowait))

    local prev_file="$STATE_DIR/cpu_prev"
    if [ -f "$prev_file" ]; then
        local prev_values=$(cat "$prev_file")
        local prev_total=$(echo "$prev_values" | cut -d' ' -f1)
        local prev_busy=$(echo "$prev_values" | cut -d' ' -f2)
        local total_delta=$((total - prev_total))
        local busy_delta=$((busy - prev_busy))
        if [ $total_delta -gt 0 ]; then
            local cpu_pct=$((busy_delta * 10000 / total_delta))
            echo "scale=2; $cpu_pct / 100" | bc
        else
            echo "0.00"
        fi
    else
        echo "0.00"
    fi
    echo "$total $busy" > "$prev_file"
}

get_memory_usage() {
    local mem_total=$(grep '^MemTotal:' /proc/meminfo | awk '{print $2}')
    local mem_available=$(grep '^MemAvailable:' /proc/meminfo | awk '{print $2}')
    local swap_total=$(grep '^SwapTotal:' /proc/meminfo | awk '{print $2}')
    local swap_free=$(grep '^SwapFree:' /proc/meminfo | awk '{print $2}')
    local mem_used=$((mem_total - mem_available))
    local mem_total_mb=$((mem_total / 1024))
    local mem_used_mb=$((mem_used / 1024))
    local mem_available_mb=$((mem_available / 1024))
    local swap_total_mb=$((swap_total / 1024))
    local swap_used_mb=$(((swap_total - swap_free) / 1024))
    local mem_pct=$(echo "scale=2; $mem_used * 100 / $mem_total" | bc)
    echo "{\"total_mb\":$mem_total_mb,\"used_mb\":$mem_used_mb,\"available_mb\":$mem_available_mb,\"percent\":\"$mem_pct\",\"swap_total_mb\":$swap_total_mb,\"swap_used_mb\":$swap_used_mb}"
}

get_disk_usage() {
    local disk_info=$(df -BM / | tail -1)
    local total=$(echo "$disk_info" | awk '{print $2}' | sed 's/M//')
    local used=$(echo "$disk_info" | awk '{print $3}' | sed 's/M//')
    local available=$(echo "$disk_info" | awk '{print $4}' | sed 's/M//')
    local percent=$(echo "$disk_info" | awk '{print $5}' | sed 's/%//')
    echo "{\"total_mb\":$total,\"used_mb\":$used,\"available_mb\":$available,\"percent\":\"$percent\"}"
}

get_uptime() {
    cat /proc/uptime | awk '{print int($1)}'
}

get_load_average() {
    local loadavg=$(cat /proc/loadavg)
    local load1=$(echo "$loadavg" | awk '{print $1}')
    local load5=$(echo "$loadavg" | awk '{print $2}')
    local load15=$(echo "$loadavg" | awk '{print $3}')
    echo "{\"1min\":\"$load1\",\"5min\":\"$load5\",\"15min\":\"$load15\"}"
}

collect_metrics() {
    local timestamp=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
    local cpu=$(get_cpu_usage)
    local memory=$(get_memory_usage)
    local disk=$(get_disk_usage)
    local uptime=$(get_uptime)
    local load=$(get_load_average)

    cat > "$METRICS_FILE" <<EOF
{
  "timestamp": "$timestamp",
  "cpu_percent": "$cpu",
  "memory": $memory,
  "disk": $disk,
  "uptime_seconds": $uptime,
  "load_average": $load
}
EOF

    if [ "${OUTPUT_STDOUT:-0}" = "1" ]; then
        cat "$METRICS_FILE"
    fi
}

main() {
    while true; do
        collect_metrics
        sleep "$INTERVAL"
    done
}

trap 'exit 0' SIGTERM SIGINT

if [ "${1:-}" = "once" ]; then
    collect_metrics
    exit 0
fi

main
`

	systemdService := `[Unit]
Description=Tako CLI Monitoring Agent
After=network.target

[Service]
Type=simple
User=root
ExecStart=/usr/local/bin/tako-monitor.sh
Restart=always
RestartSec=10
Environment="MONITOR_INTERVAL=60"
StandardOutput=journal
StandardError=journal
SyslogIdentifier=tako-monitor

[Install]
WantedBy=multi-user.target
`

	// Install bc (required for floating point calculations)
	p.logf("  Installing bc (calculator)...\n")
	if err := p.installPackages("bc"); err != nil {
		return fmt.Errorf("failed to install bc: %w", err)
	}

	// Upload agent script
	p.logf("  Uploading monitoring agent script...\n")
	scriptPath := "/usr/local/bin/tako-monitor.sh"
	uploadCmd := fmt.Sprintf("sudo tee %s > /dev/null << 'EOFSCRIPT'\n%s\nEOFSCRIPT", scriptPath, agentScript)
	_, err = p.client.Execute(uploadCmd)
	if err != nil {
		return fmt.Errorf("failed to upload agent script: %w", err)
	}

	// Make script executable
	_, err = p.client.Execute(fmt.Sprintf("sudo chmod +x %s", scriptPath))
	if err != nil {
		return fmt.Errorf("failed to make script executable: %w", err)
	}

	// Upload systemd service
	p.logf("  Creating systemd service...\n")
	servicePath := "/etc/systemd/system/tako-monitor.service"
	uploadServiceCmd := fmt.Sprintf("sudo tee %s > /dev/null << 'EOFSERVICE'\n%s\nEOFSERVICE", servicePath, systemdService)
	_, err = p.client.Execute(uploadServiceCmd)
	if err != nil {
		return fmt.Errorf("failed to create systemd service: %w", err)
	}

	// Reload systemd, enable and start service
	p.logf("  Starting monitoring service...\n")
	commands := []string{
		"sudo systemctl daemon-reload",
		"sudo systemctl enable tako-monitor",
		"sudo systemctl restart tako-monitor",
	}

	for _, cmd := range commands {
		if _, err := p.client.Execute(cmd); err != nil {
			return fmt.Errorf("failed to setup systemd service: %w", err)
		}
	}

	// Verify service is running
	output, err = p.client.Execute("systemctl is-active tako-monitor")
	if err != nil || strings.TrimSpace(output) != "active" {
		return fmt.Errorf("monitoring service failed to start")
	}

	p.logf("  ✓ Monitoring agent installed and running\n")

	return nil
}

func (p *Provisioner) installPackages(packages ...string) error {
	osInfo, err := DetectOS(p.client)
	if err != nil {
		return fmt.Errorf("failed to detect OS: %w", err)
	}
	manager, err := newPackageManagerWithLog(p.client, osInfo, p.provisionLog)
	if err != nil {
		return err
	}
	return manager.Install(packages...)
}
