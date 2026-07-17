//go:build linux

package provisioner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProductionUpgradeLeaseRefreshAndTakeoverAreSerialized(t *testing.T) {
	root := t.TempDir()
	oldToken := "old-coordinator"
	newToken := "new-coordinator"
	if output, err := runUpgradeLeaseScript(root, acquireTakodUpgradeLockScript(takodUpgradeClusterLockScope, oldToken, time.Second)); err != nil {
		t.Fatalf("initial production lease acquisition failed: %v\n%s", err, output)
	}
	time.Sleep(1100 * time.Millisecond)

	type result struct {
		name   string
		output []byte
		err    error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wait sync.WaitGroup
	for name, script := range map[string]string{
		"refresh-old":  refreshTakodUpgradeLockScript(takodUpgradeClusterLockScope, oldToken, time.Minute),
		"takeover-new": acquireTakodUpgradeLockScript(takodUpgradeClusterLockScope, newToken, time.Minute),
	} {
		wait.Add(1)
		go func(name string, script string) {
			defer wait.Done()
			<-start
			output, err := runUpgradeLeaseScript(root, script)
			results <- result{name: name, output: output, err: err}
		}(name, script)
	}
	close(start)
	wait.Wait()
	close(results)
	succeeded := 0
	for result := range results {
		if result.err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("serialized refresh/takeover successes=%d, want exactly 1", succeeded)
	}
	ownerPath := filepath.Join(root, "var", "lib", "tako", "node-upgrade", "locks", takodUpgradeClusterLockScope, "owner")
	owner, err := os.ReadFile(ownerPath)
	if err != nil || (strings.TrimSpace(string(owner)) != oldToken && strings.TrimSpace(string(owner)) != newToken) {
		t.Fatalf("production lease owner = %q, %v", owner, err)
	}
}

func TestExpiredNodeLeaseRecoversPublishedCandidateWithoutReboot(t *testing.T) {
	root := t.TempDir()
	if output, err := runUpgradeLeaseScript(root, acquireTakodUpgradeLockScript(takodUpgradeNodeLockScope, "dead-cli", time.Second)); err != nil {
		t.Fatalf("initial node lease failed: %v\n%s", err, output)
	}
	dir := filepath.Join(root, "var", "lib", "tako", "node-upgrade")
	takod := filepath.Join(root, "usr", "local", "bin", "tako")
	if err := os.MkdirAll(filepath.Dir(takod), 0700); err != nil {
		t.Fatal(err)
	}
	writeRecoveryFixture(t, takod, "bad-candidate", 0755)
	writeRecoveryFixture(t, filepath.Join(dir, "previous-takod"), "working-agent", 0755)
	writeRecoveryFixture(t, filepath.Join(dir, "pending"), "", 0600)

	time.Sleep(1100 * time.Millisecond)
	if output, err := runUpgradeLeaseScript(root, acquireTakodUpgradeLockScript(takodUpgradeNodeLockScope, "recovery-cli", time.Minute)); err != nil {
		t.Fatalf("expired node lease was not recoverable: %v\n%s", err, output)
	}
	fakeBin := filepath.Join(root, "fake-bin")
	if err := os.MkdirAll(fakeBin, 0700); err != nil {
		t.Fatal(err)
	}
	writeRecoveryFixture(t, filepath.Join(fakeBin, "systemctl"), "#!/bin/sh\nexit 0\n", 0755)
	script := rewriteUpgradeScriptRoot(root, recoverPendingTakodUpgradeScript("recovery-cli"))
	command := exec.Command("sh", "-c", script)
	command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("no-reboot recovery failed: %v\n%s", err, output)
	}
	data, err := os.ReadFile(takod)
	if err != nil || string(data) != "working-agent" {
		t.Fatalf("recovered takod = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pending")); !os.IsNotExist(err) {
		t.Fatalf("pending marker survived no-reboot recovery: %v", err)
	}
}

func TestProductionPublicationRenewsOwnerBeforeReleasingGuardPastLeaseTTL(t *testing.T) {
	root := t.TempDir()
	if output, err := runUpgradeLeaseScript(root, acquireTakodUpgradeLockScript(takodUpgradeNodeLockScope, "publishing-cli", time.Second)); err != nil {
		t.Fatalf("initial node lease failed: %v\n%s", err, output)
	}
	paused := filepath.Join(root, "publication-paused")
	release := filepath.Join(root, "publication-release")
	candidate := filepath.Join(root, "candidate")
	writeRecoveryFixture(t, candidate, "#!/bin/sh\ntouch '"+paused+"'\nwhile ! test -f '"+release+"'; do sleep 0.05; done\n", 0755)
	publication := beginTakodUpgradeScriptWithContract(candidate, "", "", "publishing-cli")
	publicationDone := make(chan error, 1)
	go func() {
		_, err := runUpgradeLeaseScript(root, publication)
		publicationDone <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(paused); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("production publication script did not reach the guarded pause")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(1100 * time.Millisecond)
	takeoverDone := make(chan error, 1)
	go func() {
		_, err := runUpgradeLeaseScript(root, acquireTakodUpgradeLockScript(takodUpgradeNodeLockScope, "takeover-cli", time.Minute))
		takeoverDone <- err
	}()
	select {
	case err := <-takeoverDone:
		t.Fatalf("expired directory lease bypassed in-flight kernel publication guard: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	writeRecoveryFixture(t, release, "", 0600)
	if err := <-publicationDone; err != nil {
		t.Fatalf("guarded publication fixture failed: %v", err)
	}
	if err := <-takeoverDone; err == nil {
		t.Fatal("takeover replaced the original owner after guarded publication")
	}
	if output, err := runUpgradeLeaseScript(root, refreshTakodUpgradeLockScript(takodUpgradeNodeLockScope, "publishing-cli", time.Minute)); err != nil {
		t.Fatalf("original transaction could not continue after long publication: %v\n%s", err, output)
	}
	guard := filepath.Join(root, "var", "lib", "tako", "node-upgrade", "locks", ".guard")
	if info, err := os.Stat(guard); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("upgrade guard mode=%v err=%v, want 0600", infoMode(info), err)
	}
	if output, err := runUpgradeLeaseScript(root, releaseTakodUpgradeLockScript(takodUpgradeNodeLockScope, "publishing-cli")); err != nil {
		t.Fatalf("original transaction lease release failed: %v\n%s", err, output)
	}
	if output, err := runUpgradeLeaseScript(root, acquireTakodUpgradeLockScript(takodUpgradeNodeLockScope, "takeover-cli", time.Minute)); err != nil {
		t.Fatalf("takeover did not proceed after original transaction released: %v\n%s", err, output)
	}
}

func infoMode(info os.FileInfo) os.FileMode {
	if info == nil {
		return 0
	}
	return info.Mode().Perm()
}

func runUpgradeLeaseScript(root string, script string) ([]byte, error) {
	command := exec.Command("sh", "-c", rewriteUpgradeScriptRoot(root, script))
	return command.CombinedOutput()
}

func rewriteUpgradeScriptRoot(root string, script string) string {
	for production, fixture := range map[string]string{
		"/var/lib/tako":       filepath.Join(root, "var", "lib", "tako"),
		"/usr/local/bin":      filepath.Join(root, "usr", "local", "bin"),
		"/usr/local/lib/tako": filepath.Join(root, "usr", "local", "lib", "tako"),
		"/etc/tako":           filepath.Join(root, "etc", "tako"),
	} {
		script = strings.ReplaceAll(script, production, fixture)
	}
	// The production scripts deliberately require root ownership. These
	// integration fixtures run as the CI account, so preserve every lock and
	// mode assertion while substituting only the fixture owner.
	script = strings.ReplaceAll(script, "install -d -m 0700 -o root -g root", "install -d -m 0700")
	return strings.ReplaceAll(script, "0:0:700", fmt.Sprintf("%d:%d:700", os.Geteuid(), os.Getegid()))
}
