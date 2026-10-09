package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/credentials"
)

// A factual result from the production resolver, never a secret/value hash.
type credentialCheckRecord struct {
	APIVersion   string `json:"api_version"`
	ProbeID      string `json:"probe_id"`
	CredentialID string `json:"credential_id"`
	Check        string `json:"check"`
	Status       string `json:"status"`
	Code         string `json:"code"`
	Fresh        bool   `json:"fresh"`
	RecordedAt   string `json:"recorded_at"`
}

func credentialsCheckCommand(args []string, stdout io.Writer, resolve func(context.Context, string, string) (string, error)) (returned error) {
	invalid := errors.New("invalid credential check; use private configuration, a new output and explicit --live")
	flags := flag.NewFlagSet("credentials check", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	live := flags.Bool("live", false, "explicitly permit a read-only secret-store request")
	config := flags.String("credential-config", "", "private credential configuration")
	id := flags.String("credential-id", "", "configured credential ID")
	probe := flags.String("probe-id", "", "nonsensitive probe identity")
	output := flags.String("output", "", "new private JSONL evidence file")
	seconds := flags.Int("timeout-seconds", 30, "bounded request deadline")
	label := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	if len(args) == 0 || args[0] != "check" || flags.Parse(args[1:]) != nil || flags.NArg() != 0 || !*live || !label.MatchString(*id) || !label.MatchString(*probe) || *seconds < 1 || *seconds > 600 || !filepath.IsAbs(*config) || filepath.Clean(*config) != *config || !filepath.IsAbs(*output) || filepath.Clean(*output) != *output {
		return invalid
	}
	info, err := os.Lstat(*config)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 65536 {
		return invalid
	}
	cfg, err := credentials.Load(*config)
	if err != nil {
		return invalid
	}
	known := false
	for _, ref := range cfg.Credentials {
		known = known || ref.CredentialID == *id
	}
	if !known {
		return invalid
	}
	file, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("credential check evidence unavailable")
	}
	defer func() {
		if err := file.Close(); returned == nil && err != nil {
			returned = errors.New("credential check evidence unavailable")
		}
	}()
	record := credentialCheckRecord{APIVersion: "interceptor.dev/credential-check/v1alpha1", ProbeID: *probe, CredentialID: *id, Check: "resolve", Status: "started", Code: "read_only_resolution"}
	emit := func() error {
		record.RecordedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if json.NewEncoder(file).Encode(record) != nil || file.Sync() != nil {
			return errors.New("credential check evidence unavailable")
		}
		return nil
	}
	if err := emit(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(*seconds)*time.Second)
	defer cancel()
	// The same resolver/audit path used by normal product model access creates
	// a new resolver per call, so no prior process/cache can satisfy this check.
	_, err = resolve(ctx, *config, *id)
	record.Status, record.Code = "passed", "resolved"
	record.Fresh = err == nil
	if err != nil {
		record.Status, record.Code = "failed", "resolution_failed"
		if ctx.Err() != nil {
			record.Code = "canceled"
		}
	}
	if emitErr := emit(); emitErr != nil {
		return emitErr
	}
	if json.NewEncoder(stdout).Encode(record) != nil {
		return errors.New("credential check output unavailable")
	}
	if err != nil {
		return errors.New("credential check failed; inspect sanitized evidence")
	}
	return nil
}
