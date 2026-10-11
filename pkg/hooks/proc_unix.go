//go:build !windows

package hooks

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// processTree runs the hook in its own process group so a timeout or
// cancellation terminates every process the hook started.
type processTree struct {
	cmd   *exec.Cmd
	grace time.Duration

	mu    sync.Mutex
	timer *time.Timer
}

func newProcessTree(cmd *exec.Cmd, grace time.Duration) *processTree {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &processTree{cmd: cmd, grace: grace}
}

func (t *processTree) started() {}

// terminate sends SIGTERM to the process group and SIGKILL after the grace
// period. It is the exec.Cmd Cancel function.
func (t *processTree) terminate() error {
	if t.cmd.Process == nil {
		return nil
	}
	pgid := t.cmd.Process.Pid
	err := syscall.Kill(-pgid, syscall.SIGTERM)
	t.mu.Lock()
	if t.timer == nil {
		t.timer = time.AfterFunc(t.grace, func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	}
	t.mu.Unlock()
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

// cleanup kills anything left in the hook's process group once the hook
// has exited: background children do not outlive their hook.
func (t *processTree) cleanup() {
	t.mu.Lock()
	if t.timer != nil {
		t.timer.Stop()
	}
	t.mu.Unlock()
	if t.cmd.Process != nil {
		_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
	}
}
