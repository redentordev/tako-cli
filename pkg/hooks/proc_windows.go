//go:build windows

package hooks

import (
	"os/exec"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processTree assigns the hook to a Job Object with KILL_ON_JOB_CLOSE so a
// timeout or cancellation terminates every process the hook started.
type processTree struct {
	cmd *exec.Cmd
	job windows.Handle
}

func newProcessTree(cmd *exec.Cmd, _ time.Duration) *processTree {
	tree := &processTree{cmd: cmd}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return tree
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return tree
	}
	tree.job = job
	return tree
}

// started assigns the hook process to the job. Children the hook starts
// afterwards inherit the job.
func (t *processTree) started() {
	if t.job == 0 || t.cmd.Process == nil {
		return
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(t.cmd.Process.Pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(process)
	_ = windows.AssignProcessToJobObject(t.job, process)
}

// terminate kills the whole job. It is the exec.Cmd Cancel function.
func (t *processTree) terminate() error {
	if t.job != 0 {
		_ = windows.TerminateJobObject(t.job, 1)
	}
	if t.cmd.Process != nil {
		return t.cmd.Process.Kill()
	}
	return nil
}

// cleanup closes the job, which kills anything the hook left running:
// background children do not outlive their hook.
func (t *processTree) cleanup() {
	if t.job != 0 {
		_ = windows.CloseHandle(t.job)
		t.job = 0
	}
}
