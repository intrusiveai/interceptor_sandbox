package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/credentials"
)

type checkSecretBackend struct {
	calls  int
	failed bool
}

func (b *checkSecretBackend) Read(context.Context, credentials.SecretStoreProfile, credentials.Locator) ([]byte, string, error) {
	b.calls++
	if b.failed {
		return nil, "", errors.New("private remote error with host-secret")
	}
	return []byte("host-secret"), "version-1", nil
}
func TestCredentialsCheckUsesProductionResolutionAndAudit(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[failed], func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("INTERCEPTOR_DATA_DIR", filepath.Join(root, "data"))
			cfg := credentials.Config{Profiles: []credentials.SecretStoreProfile{{APIVersion: credentials.StoreProfileVersion, Kind: "SecretStoreProfile", ID: "test", BackendKind: "aws-secrets-manager", Region: "us-west-2", AllowedLocatorPrefixes: []string{"aws://us-west-2/test"}, MaxCacheTTLSeconds: 300}}, Credentials: []credentials.HostCredentialRef{{APIVersion: credentials.CredentialRefVersion, Kind: "HostCredentialRef", CredentialID: "model-key", StoreProfileID: "test", Locator: credentials.Locator{BackendKind: "aws-secrets-manager", Region: "us-west-2", SecretID: "test"}, Value: credentials.ValueSelection{Format: "utf8"}, CacheTTLSeconds: 300}}}
			raw, _ := json.Marshal(cfg)
			config, output := filepath.Join(root, "config.json"), filepath.Join(root, "evidence.jsonl")
			if err := os.WriteFile(config, raw, 0600); err != nil {
				t.Fatal(err)
			}
			backend := &checkSecretBackend{failed: failed}
			resolve := func(ctx context.Context, path, id string) (string, error) {
				return resolveStoredCredentialWithFactories(ctx, path, id, map[string]credentials.BackendFactory{"aws-secrets-manager": func(context.Context, credentials.SecretStoreProfile) (credentials.Backend, error) {
					return backend, nil
				}})
			}
			args := []string{"check", "--live", "--credential-config", config, "--credential-id", "model-key", "--probe-id", "test-1", "--output", output}
			var stdout bytes.Buffer
			err := credentialsCheckCommand(args, &stdout, resolve)
			if (err != nil) != failed || backend.calls != 1 {
				t.Fatalf("err=%v calls=%d", err, backend.calls)
			}
			evidence, _ := os.ReadFile(output)
			audit, _ := os.ReadFile(filepath.Join(root, "data/audit/credential-resolutions.jsonl"))
			combined := string(evidence) + stdout.String() + string(audit)
			if strings.Contains(combined, "host-secret") || strings.Contains(combined, "private remote error") || strings.Count(string(evidence), "recorded_at") != 2 || len(audit) == 0 {
				t.Fatalf("unsafe or absent records: %s", combined)
			}
			var terminal credentialCheckRecord
			if json.Unmarshal(stdout.Bytes(), &terminal) != nil || terminal.ProbeID != "test-1" || terminal.Fresh == failed {
				t.Fatal("missing direct result")
			}
			if err := credentialsCheckCommand(args, &stdout, resolve); err == nil || backend.calls != 1 {
				t.Fatal("existing output must fail before resolution")
			}
			args[len(args)-1] = filepath.Join(root, "new-evidence.jsonl")
			if err := credentialsCheckCommand(args, &stdout, resolve); (err != nil) != failed || backend.calls != 2 {
				t.Fatal("each check must create a fresh production resolver")
			}
		})
	}
}
func TestCredentialsCheckRequiresLiveBeforeResolver(t *testing.T) {
	for _, args := range [][]string{nil, {"check"}, {"check", "--live", "--credential-config", "/missing", "--credential-id", "key", "--probe-id", "test", "--output", "/tmp/new"}} {
		if credentialsCheckCommand(args, &bytes.Buffer{}, func(context.Context, string, string) (string, error) {
			t.Fatal("unexpected resolution")
			return "", nil
		}) == nil {
			t.Fatal("must reject invalid input")
		}
	}
}
