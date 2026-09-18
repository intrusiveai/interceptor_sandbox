package hostlifetime

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestAssertionPIDMatching(t *testing.T) {
	text := "   pid 123(caffeinate): [0x1] PreventUserIdleSystemSleep named: test\n"
	if !hasAssertion(text, 123) || hasAssertion(text, 12) || hasAssertion(text, 1234) {
		t.Fatal("incorrect assertion ownership match")
	}
}
func TestRealCaffeinateLifecycle(t *testing.T) {
	if os.Getenv("INTERCEPTOR_TEST_REAL_POWER") != "1" {
		t.Skip("opt-in macOS power assertion check")
	}
	backend := macBackend{}
	before, err := backend.SleepMarker()
	if err != nil || before == "" {
		t.Fatal(before, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	guard, err := backend.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	pid := guard.(*caffeinate).cmd.Process.Pid
	group, err := syscall.Getpgid(pid)
	if err != nil || group != pid || group == syscall.Getpgrp() {
		t.Fatal("helper shares terminal signal group", group, err)
	}
	data, err := exec.Command("/usr/bin/pmset", "-g", "assertions").Output()
	if err != nil || !hasAssertion(string(data), pid) {
		t.Fatal("assertion missing", err)
	}
	guard.Close()
	data, err = exec.Command("/usr/bin/pmset", "-g", "assertions").Output()
	if err != nil || hasAssertion(string(data), pid) {
		t.Fatal("assertion survived release", err)
	}
}
