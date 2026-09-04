package ociruntime

import (
	"reflect"
	"testing"
)

func TestRunArgumentsContainMandatoryIsolation(t *testing.T) {
	arguments, err := RunArguments(RunConfig{
		Name: "interceptor-test", Image: "wrapper:test", User: "501:20", NetworkNone: true, ReadOnly: true,
		DropAllCaps: true, AddCaps: []string{"SETUID", "SETGID", "KILL"}, NoNewPrivileges: true, PIDs: 128, MemoryBytes: 1024, CPUs: "1", TmpfsBytes: 4096,
		Hostnames: []string{"tickets.example.test"}, Environment: map[string]string{"SESSION": "sess-test"},
		DNS: []string{"127.0.0.1"}, Sysctls: map[string]string{"net.ipv4.ip_unprivileged_port_start": "53"},
		Mounts: []Mount{{Source: "/host/state", Target: "/state"}, {Source: "/host/env", Target: "/environment", ReadOnly: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, sequence := range [][]string{{"--network", "none"}, {"--read-only"}, {"--cap-drop", "ALL"}, {"--security-opt", "no-new-privileges"}, {"--pids-limit", "128"}, {"--dns", "127.0.0.1"}, {"--sysctl", "net.ipv4.ip_unprivileged_port_start=53"}} {
		if !containsSequence(arguments, sequence) {
			t.Fatalf("arguments lack %v: %#v", sequence, arguments)
		}
	}
	if got := arguments[len(arguments)-2:]; !reflect.DeepEqual(got, []string{"wrapper:test", "sandboxd"}) {
		t.Fatalf("unexpected command tail: %#v", got)
	}
}

func containsSequence(values, sequence []string) bool {
	for index := 0; index+len(sequence) <= len(values); index++ {
		if reflect.DeepEqual(values[index:index+len(sequence)], sequence) {
			return true
		}
	}
	return false
}
