package main

import (
	"io"
	"strings"
	"testing"
)

func TestRunSpoolFlagOverridesEnvironment(t *testing.T) {
	t.Setenv("INTERCEPTOR_SPOOL_MAX_BYTES", "invalid")
	base := []string{"--environment", "unused", "--allow-model", "model", "--model-provider", "invalid-provider"}
	if err := runCommand(base, io.Discard); err == nil || !strings.Contains(err.Error(), "spool-max-bytes") {
		t.Fatal("invalid environment quota not rejected", err)
	}
	if err := runCommand(append(base, "--spool-max-bytes", "1048576"), io.Discard); err == nil || !strings.Contains(err.Error(), "unsupported model provider") {
		t.Fatal("flag did not override environment", err)
	}
	if err := runCommand(append(base, "--spool-max-bytes", ""), io.Discard); err == nil || !strings.Contains(err.Error(), "spool-max-bytes") {
		t.Fatal("empty explicit flag was accepted", err)
	}
}
