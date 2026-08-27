//go:build linux

package sandboxd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

const (
	ioctlUnlockPTY = 0x40045431
	ioctlPTYNumber = 0x80045430
)

func attachPTY(command *exec.Cmd) (io.WriteCloser, io.ReadCloser, func(), error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open PTY master: %w", err)
	}
	unlock := int32(0)
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), ioctlUnlockPTY, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		_ = master.Close()
		return nil, nil, nil, fmt.Errorf("unlock PTY: %w", errno)
	}
	var number uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), ioctlPTYNumber, uintptr(unsafe.Pointer(&number))); errno != 0 {
		_ = master.Close()
		return nil, nil, nil, fmt.Errorf("get PTY number: %w", errno)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		_ = master.Close()
		return nil, nil, nil, fmt.Errorf("open PTY slave: %w", err)
	}
	command.Stdin, command.Stdout = slave, slave
	command.SysProcAttr.Setpgid = false
	command.SysProcAttr.Setsid = true
	command.SysProcAttr.Setctty = true
	command.SysProcAttr.Ctty = 0
	return master, master, func() { _ = slave.Close() }, nil
}
