package credentials

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeBackend struct {
	value   []byte
	version string
	calls   int
	err     error
}

func (f *fakeBackend) Read(context.Context, SecretStoreProfile, Locator) ([]byte, string, error) {
	f.calls++
	return append([]byte(nil), f.value...), f.version, f.err
}

func validConfig() Config {
	return Config{Profiles: []SecretStoreProfile{{
		APIVersion: StoreProfileVersion, Kind: "SecretStoreProfile", ID: "prod-gcp", BackendKind: "gcp-secret-manager",
		AllowedLocatorPrefixes: []string{"gcp://projects/security-prod/secrets/interceptor-"}, MaxValueBytes: 1024, MaxCacheTTLSeconds: 60,
	}}, Credentials: []HostCredentialRef{{
		APIVersion: CredentialRefVersion, Kind: "HostCredentialRef", CredentialID: "openai", StoreProfileID: "prod-gcp",
		Locator: Locator{BackendKind: "gcp-secret-manager", Project: "security-prod", SecretName: "interceptor-openai", Version: "latest"},
		Value:   ValueSelection{Format: "json-string-field", Field: "api_key"}, CacheTTLSeconds: 30,
	}}}
}

func TestResolverSelectsValueCachesAndReturnsRedactedMetadata(t *testing.T) {
	backend := &fakeBackend{value: []byte(`{"api_key":"host-secret","other":"ignored"}`), version: "7"}
	resolver, err := New(validConfig(), map[string]BackendFactory{"gcp-secret-manager": func(context.Context, SecretStoreProfile) (Backend, error) { return backend, nil }})
	if err != nil {
		t.Fatal(err)
	}
	first, err := resolver.Resolve(context.Background(), "openai")
	if err != nil {
		t.Fatal(err)
	}
	second, err := resolver.Resolve(context.Background(), "openai")
	if err != nil {
		t.Fatal(err)
	}
	if first.Value != "host-secret" || second.Value != "host-secret" || first.BackendVersion != "7" || backend.calls != 1 {
		t.Fatalf("unexpected resolution/cache: %#v calls=%d", first, backend.calls)
	}
}

func TestResolverRejectsUnapprovedLocatorAndRedactsBackendErrors(t *testing.T) {
	config := validConfig()
	config.Credentials[0].Locator.SecretName = "other"
	if err := Validate(config); err == nil {
		t.Fatal("expected locator allowlist failure")
	}
	config = validConfig()
	backend := &fakeBackend{err: errors.New("remote error mentions super-secret and exact/path")}
	resolver, err := New(config, map[string]BackendFactory{"gcp-secret-manager": func(context.Context, SecretStoreProfile) (Backend, error) { return backend, nil }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = resolver.Resolve(context.Background(), "openai")
	if err == nil || strings.Contains(err.Error(), "super-secret") || strings.Contains(err.Error(), "exact/path") {
		t.Fatalf("backend error was not redacted: %v", err)
	}
}

func TestResolverRejectsMalformedAndOversizedValues(t *testing.T) {
	for _, value := range [][]byte{[]byte(`{"api_key":9}`), make([]byte, 1025)} {
		backend := &fakeBackend{value: value}
		resolver, err := New(validConfig(), map[string]BackendFactory{"gcp-secret-manager": func(context.Context, SecretStoreProfile) (Backend, error) { return backend, nil }})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := resolver.Resolve(context.Background(), "openai"); err == nil {
			t.Fatal("expected invalid value to fail")
		}
	}
}
