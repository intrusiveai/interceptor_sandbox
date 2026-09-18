package hostlifetime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type macBackend struct{}

func platformBackend() Backend { return macBackend{} }
func (macBackend) SleepMarker() (string, error) {
	// Compare exact kernel markers, not wall-clock gaps (which can also be CPU
	// scheduling delays or clock corrections). Both are readable without root.
	sleep, err := unix.SysctlTimeval("kern.sleeptime")
	if err != nil {
		return "", err
	}
	wake, err := unix.SysctlTimeval("kern.waketime")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d/%d:%d", sleep.Sec, sleep.Usec, wake.Sec, wake.Usec), nil
}

type caffeinate struct {
	cmd  *exec.Cmd
	done chan struct{}
	once sync.Once
}

func (c *caffeinate) Done() <-chan struct{} { return c.done }
func (c *caffeinate) Close()                { c.once.Do(func() { _ = c.cmd.Process.Kill(); <-c.done }) }
func (macBackend) Start(ctx context.Context) (Inhibitor, error) {
	cmd := exec.Command("/usr/bin/caffeinate", "-i", "-w", strconv.Itoa(os.Getpid()))
	// Do not inherit provider credentials into the power-management helper.
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	// Terminal Ctrl+C belongs to the host. Keep the helper alive until cleanup finishes.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &caffeinate{cmd: cmd, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(c.done) }()
	startup, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for {
		select {
		case <-c.done:
			return nil, errors.New("caffeinate exited before assertion readiness")
		default:
		}
		// Confirm the assertion by its helper PID rather than assuming Start means
		// macOS accepted it. pmset only inspects current assertions.
		probe := exec.CommandContext(startup, "/usr/bin/pmset", "-g", "assertions")
		probe.Env = cmd.Env
		raw, err := probe.Output()
		if err == nil && hasAssertion(string(raw), cmd.Process.Pid) {
			return c, nil
		}
		select {
		case <-startup.Done():
			c.Close()
			return nil, errors.New("idle-sleep assertion could not be confirmed")
		case <-time.After(25 * time.Millisecond):
		}
	}
}
func hasAssertion(output string, pid int) bool {
	prefix := "pid " + strconv.Itoa(pid) + "("
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) && strings.Contains(line, "PreventUserIdleSystemSleep") {
			return true
		}
	}
	return false
}
