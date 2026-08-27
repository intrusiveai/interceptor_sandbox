//go:build linux

package sandboxd

import (
	"os/exec"
	"syscall"
)

func configureHelperChild(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
