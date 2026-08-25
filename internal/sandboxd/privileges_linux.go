//go:build linux

package sandboxd

import (
	"fmt"
	"syscall"
	"unsafe"
)

const linuxCapabilityVersion3 = 0x20080522
const linuxCapabilityKill = 5
const linuxPrctlCapabilityAmbient = 47
const linuxPrctlCapabilityAmbientClearAll = 4
const linuxPrctlSetKeepCapabilities = 8

type capabilityHeader struct {
	Version uint32
	PID     int32
}

type capabilityData struct {
	Effective   uint32
	Permitted   uint32
	Inheritable uint32
}

func adoptSupervisorIdentity(uid, gid int) error {
	if uid <= 0 || gid < 0 {
		return fmt.Errorf("invalid supervisor uid/gid %d:%d", uid, gid)
	}
	_, _, errno := syscall.AllThreadsSyscall(syscall.SYS_PRCTL, linuxPrctlSetKeepCapabilities, 1, 0)
	if errno != 0 {
		return fmt.Errorf("retain launch capabilities: %w", errno)
	}
	if err := syscall.Setgroups([]int{gid}); err != nil {
		return fmt.Errorf("set supervisor groups: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("set supervisor gid: %w", err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("set supervisor uid: %w", err)
	}
	launch := uint32(1<<linuxCapabilityKill | 1<<6 | 1<<7) // KILL, SETGID, SETUID.
	header := capabilityHeader{Version: linuxCapabilityVersion3}
	data := [2]capabilityData{{Effective: launch, Permitted: launch}}
	_, _, errno = syscall.AllThreadsSyscall(syscall.SYS_CAPSET, uintptr(unsafe.Pointer(&header)), uintptr(unsafe.Pointer(&data[0])), 0)
	if errno != 0 {
		return fmt.Errorf("activate launch capabilities: %w", errno)
	}
	return nil
}

func dropLaunchCapabilities() error {
	header := capabilityHeader{Version: linuxCapabilityVersion3}
	data := [2]capabilityData{{Effective: 1 << linuxCapabilityKill, Permitted: 1 << linuxCapabilityKill}}
	_, _, errno := syscall.AllThreadsSyscall(syscall.SYS_CAPSET, uintptr(unsafe.Pointer(&header)), uintptr(unsafe.Pointer(&data[0])), 0)
	if errno != 0 {
		return fmt.Errorf("capset: %w", errno)
	}
	_, _, errno = syscall.AllThreadsSyscall(syscall.SYS_PRCTL, linuxPrctlCapabilityAmbient, linuxPrctlCapabilityAmbientClearAll, 0)
	if errno != 0 {
		return fmt.Errorf("clear ambient capabilities: %w", errno)
	}
	return nil
}
