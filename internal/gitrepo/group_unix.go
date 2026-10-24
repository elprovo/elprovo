//go:build unix

package gitrepo

import (
	"os/exec"
	"syscall"
)

// inOwnGroup makes cancelling cmd end every process it started, such as a
// checkout filter, not only git.
func inOwnGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
