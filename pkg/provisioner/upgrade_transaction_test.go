package provisioner

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBeginTakodUpgradeScriptIsRollbackCapableAndHandsOffWorker(t *testing.T) {
	script := beginTakodUpgradeScript("/tmp/candidate")
	for _, required := range []string{
		"if test -f \"$dir/pending\"", "flock -x 9", "test \"$(cat \"$lock/owner\")\" = \"$token\"",
		"platform node upgrade-publication-guard", "sync -f \"$dir\"",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("upgrade transaction is missing %q:\n%s", required, script)
		}
	}
}

func TestBeginTakodUpgradeContractGuardsPublication(t *testing.T) {
	script := beginTakodUpgradeScriptWithContract("/tmp/candidate", "digest", "contract", "lease-token")
	guard := "platform node upgrade-publication-guard"
	if !strings.Contains(script, guard) {
		t.Fatalf("protected publication contract guard is absent:\n%s", script)
	}
	if !strings.Contains(recoverPendingTakodUpgradeScript("lease-token"), "previous-takod") {
		t.Fatal("a new coordinator cannot recover expired pending evidence without reboot")
	}
	if !strings.Contains(script, "flock -x 9") || strings.Index(script, "flock -x 9") > strings.Index(script, guard) {
		t.Fatal("kernel upgrade exclusion is not held across protected publication")
	}
}

func TestRollbackTakodUpgradeRestoresBothServicesBeforeClearingMarker(t *testing.T) {
	script := rollbackTakodUpgradeScript()
	for _, required := range []string{
		"mv -f /usr/local/bin/.tako-rollback-next /usr/local/bin/tako",
		"mv -f /usr/local/lib/tako/.tako-rollback-next /usr/local/lib/tako/tako",
		"systemctl restart takod", "systemctl restart tako-platform-worker", "\"$dir/previous-takod\"",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("rollback is missing %q", required)
		}
	}
	if strings.Index(script, "systemctl restart takod") > strings.LastIndex(script, "rm -f --") {
		t.Fatal("rollback evidence was cleared before service restart")
	}
}

func TestTakodBinaryDownloadScriptRequiresReleaseChecksum(t *testing.T) {
	script := takodBinaryDownloadScript("v0.9.0", "tako-linux-amd64", "/tmp/candidate")
	for _, required := range []string{"checksums.txt", "sha256sum", "test \"$actual\" = \"$expected\"", "install -m 0755 \"$tmp\" '/tmp/candidate'"} {
		if !strings.Contains(script, required) {
			t.Fatalf("download script is missing %q:\n%s", required, script)
		}
	}
}

func TestVerifyTakodUpgradeServicesChecksWorkerHandoff(t *testing.T) {
	script := verifyTakodUpgradeServicesScript(true)
	for _, required := range []string{"systemctl is-active --quiet takod", "MainPID --value takod", "had-platform-worker", "systemctl is-active --quiet tako-platform-worker", "MainPID --value tako-platform-worker"} {
		if !strings.Contains(script, required) {
			t.Fatalf("service verification is missing %q", required)
		}
	}
	if !strings.Contains(script, "if true; then") {
		t.Fatal("controller verification did not require the protected platform worker")
	}
}

func TestNodeUpgradeMarkersAreCrashSafe(t *testing.T) {
	rollback := rollbackTakodUpgradeScript()
	if strings.Index(rollback, "mv \"$dir/pending\" \"$dir/rolled-back\"") > strings.LastIndex(rollback, "rm -f --") {
		t.Fatal("rollback evidence is removed before durable terminal marker publication")
	}
	boot := bootRecoverTakodUpgradeScript()
	for _, required := range []string{"previous-takod", "previous-platform-worker", "previous-version-manifest", "mv \"$dir/pending\" \"$dir/rolled-back\"", "sync -f \"$dir\""} {
		if !strings.Contains(boot, required) {
			t.Fatalf("boot recovery is missing %q", required)
		}
	}
	unit := takodUpgradeRecoveryUnit()
	if !strings.Contains(unit, "Before=takod.service tako-platform-worker.service") || strings.Contains(unit, "ConditionPathExists=") {
		t.Fatalf("recovery unit ordering is unsafe:\n%s", unit)
	}
	if !strings.Contains(boot, "rm -rf -- \"$dir/locks/node\"") || strings.Contains(boot, "locks/cluster") {
		t.Fatal("boot recovery must clear only the local transaction lease and preserve the external coordinator lease")
	}
}

func TestCommitTakodUpgradeResolvesLostSuccessResponseFromDurableMarker(t *testing.T) {
	executor := &scriptedUpgradeExecutor{errs: []error{errors.New("SSH response lost after remote commit"), nil}}
	if err := commitTakodUpgrade(executor, "lease-token"); err != nil {
		t.Fatalf("durably committed transaction was reported failed: %v", err)
	}
	if len(executor.commands) != 2 {
		t.Fatalf("commit uncertainty was not resolved through the durable marker: %#v", executor.commands)
	}
	commitScript := commitTakodUpgradeScript()
	if !strings.Contains(commitScript, "mv \"$dir/pending\" \"$dir/committed\"") || strings.Contains(commitScript, "rm -f") {
		t.Fatal("commit must retain its terminal marker for response-loss resolution")
	}
}

func TestConcurrentUpgradeLockAcquisitionAllowsOneCoordinator(t *testing.T) {
	executor := &contendedUpgradeLockExecutor{}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, token := range []string{"coordinator-a", "coordinator-b"} {
		go func(token string) {
			<-start
			results <- acquireTakodUpgradeLock(executor, takodUpgradeClusterLockScope, token, time.Hour)
		}(token)
	}
	close(start)
	successes := 0
	failures := 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		} else if strings.Contains(err.Error(), "acquire upgrade cluster lock") {
			failures++
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("concurrent acquisition successes=%d failures=%d, want 1/1", successes, failures)
	}
	script := acquireTakodUpgradeLockScript(takodUpgradeClusterLockScope, "token", time.Hour)
	for _, required := range []string{"umask 077", "flock -x 9", "chmod 0600 \"$base/.guard\"", "expiry", "owner", "exit 73", "stat -c '%u:%g:%a'"} {
		if !strings.Contains(script, required) {
			t.Fatalf("production lease script lacks %q", required)
		}
	}
	refresh := refreshTakodUpgradeLockScript(takodUpgradeClusterLockScope, "token", time.Hour)
	if !strings.Contains(refresh, "umask 077") || !strings.Contains(refresh, "chmod 0600 \"$base/.guard\"") || !strings.Contains(refresh, "flock -x 9") || strings.Index(refresh, "flock -x 9") > strings.Index(refresh, "cat \"$lock/owner\"") || !strings.Contains(refresh, "test \"$expiry\" -gt \"$now\"") {
		t.Fatalf("lease refresh is not serialized with expiry takeover:\n%s", refresh)
	}
	publication := beginTakodUpgradeScriptWithContract("/tmp/candidate", "", "", "token")
	for _, required := range []string{"umask 077", "trap renew_node_lease EXIT", "renew_ttl=180", "mv \"$lock/expiry.tmp\" \"$lock/expiry\""} {
		if !strings.Contains(publication, required) {
			t.Fatalf("guarded publication lacks %q", required)
		}
	}
}

func TestUpgradeLockAcquisitionResolvesLostSuccessResponseByOwnerToken(t *testing.T) {
	executor := &scriptedUpgradeExecutor{errs: []error{errors.New("SSH response lost after lease publication"), nil}}
	if err := acquireTakodUpgradeLock(executor, takodUpgradeClusterLockScope, "owner-token", time.Hour); err != nil {
		t.Fatalf("published owner token did not resolve ambiguous lease acquisition: %v", err)
	}
	if len(executor.commands) != 2 {
		t.Fatalf("lease acquisition commands = %d, want acquire then owner probe", len(executor.commands))
	}
	probe := probeTakodUpgradeLockScript(takodUpgradeClusterLockScope, "owner-token")
	for _, required := range []string{"owner", "expiry", "date +%s"} {
		if !strings.Contains(probe, required) {
			t.Fatalf("lease owner probe lacks %q", required)
		}
	}
}

type scriptedUpgradeExecutor struct {
	commands []string
	errs     []error
}

func (e *scriptedUpgradeExecutor) Execute(command string) (string, error) {
	e.commands = append(e.commands, command)
	if len(e.errs) == 0 {
		return "", nil
	}
	err := e.errs[0]
	e.errs = e.errs[1:]
	return "", err
}

type contendedUpgradeLockExecutor struct {
	mu   sync.Mutex
	held bool
}

func (e *contendedUpgradeLockExecutor) Execute(string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.held {
		return "", errors.New("remote lease is active")
	}
	e.held = true
	return "", nil
}
