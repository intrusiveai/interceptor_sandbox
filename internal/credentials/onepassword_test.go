//go:build linux || darwin

package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func onePasswordConfig(t *testing.T) Config {
	t.Helper()
	raw, err := os.ReadFile("testdata/onepassword.json")
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	if err = json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestOnePasswordConfiguration(t *testing.T) {
	for _, namespace := range []string{"operator.dev", "interceptor.dev"} {
		c := onePasswordConfig(t)
		c.Profiles[0].APIVersion = namespace + "/secret-store-profile/v1alpha1"
		c.Credentials[0].APIVersion = namespace + "/host-credential-ref/v1alpha1"
		if err := Validate(c); err != nil {
			t.Fatal(err)
		}
		c.Profiles[0].Authentication = onePasswordDesktop
		c.Profiles[0].OnePasswordAccount = "Development Team"
		c.Credentials[0].Locator.SecretReference = "op://development/Model keys/API credentials/key"
		if err := Validate(c); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]func(*Config){
		"mode omitted":          func(c *Config) { c.Profiles[0].Authentication = "" },
		"missing account":       func(c *Config) { c.Profiles[0].Authentication = onePasswordDesktop },
		"account in service":    func(c *Config) { c.Profiles[0].OnePasswordAccount = "unexpected" },
		"bad reference":         func(c *Config) { c.Credentials[0].Locator.SecretReference = "op://development/item/../key" },
		"query":                 func(c *Config) { c.Credentials[0].Locator.SecretReference += "?attribute=otp" },
		"prefix boundary":       func(c *Config) { c.Credentials[0].Locator.SecretReference = "op://development-other/item/key" },
		"empty prefix":          func(c *Config) { c.Profiles[0].AllowedLocatorPrefixes = []string{"op:///"} },
		"partial prefix":        func(c *Config) { c.Profiles[0].AllowedLocatorPrefixes = []string{"op://development"} },
		"locator mixing":        func(c *Config) { c.Credentials[0].Locator.SecretName = "other" },
		"profile mixing":        func(c *Config) { c.Profiles[0].VaultURL = "https://private.example" },
		"account other backend": func(c *Config) { *c = validConfig(); c.Profiles[0].OnePasswordAccount = "private-account" },
		"reference other backend": func(c *Config) {
			*c = validConfig()
			c.Credentials[0].Locator.SecretReference = "op://private-secret/secret/key"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := onePasswordConfig(t)
			mutate(&c)
			err := Validate(c)
			if err == nil {
				t.Fatal("accepted invalid configuration")
			}
			if strings.Contains(err.Error(), "op://") || strings.Contains(err.Error(), "private-account") {
				t.Fatal("unsafe diagnostic", err)
			}
		})
	}
}

type onePasswordReadFunc func(context.Context, string) (string, error)

func (f onePasswordReadFunc) Resolve(ctx context.Context, ref string) (string, error) {
	return f(ctx, ref)
}

// Invoked by the actual private-pipe subprocess path, without a live account.
func TestOnePasswordProcessHelper(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-1] != "onepassword-test-worker" {
		return
	}
	raw, _ := io.ReadAll(os.Stdin)
	var r onePasswordRequest
	if json.Unmarshal(raw, &r) != nil {
		os.Exit(2)
	}
	for _, v := range os.Environ() {
		if strings.HasPrefix(v, "OP_") {
			os.Exit(3)
		}
	}
	switch r.Reference {
	case "op://development/stall/key":
		time.Sleep(30 * time.Second)
	case "op://development/overflow/key":
		fmt.Print(strings.Repeat("SENSITIVE-OVERFLOW", 2000))
		time.Sleep(30 * time.Second)
	case "op://development/partial/key":
		fmt.Print(`{"value":"SENSITIVE-PARTIAL"}`)
		fmt.Fprint(os.Stderr, "SENSITIVE-STDERR")
		os.Exit(9)
	case "op://development/malformed/key":
		fmt.Print("SENSITIVE-MALFORMED")
		os.Exit(0)
	}
	code := serveOnePasswordChild(context.Background(), bytes.NewReader(raw), os.Stdout, "fixture-product", "1.2.3", func(ctx context.Context, r onePasswordRequest, product, version string) (onePasswordClient, error) {
		if product != "fixture-product" || version != "1.2.3" {
			os.Exit(4)
		}
		return onePasswordReadFunc(func(ctx context.Context, ref string) (string, error) {
			if strings.Contains(ref, "reject") {
				return "SENSITIVE-PARTIAL", errors.New("SENSITIVE-SDK-ERROR")
			}
			if r.Authentication == onePasswordDesktop {
				return r.Account, nil
			}
			return r.Token, nil
		}), nil
	})
	os.Exit(code)
}

func testOnePasswordBackend(t *testing.T, started *int) *onePasswordBackend {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &onePasswordBackend{command: func(ctx context.Context) (*exec.Cmd, error) {
		if started != nil {
			*started++
		}
		return exec.CommandContext(ctx, exe, "-test.run=^TestOnePasswordProcessHelper$", "--", "onepassword-test-worker"), nil
	}}
}

func TestOnePasswordPrivateReadCacheAndRotation(t *testing.T) {
	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "initial-private-token")
	t.Setenv("OP_CONNECT_TOKEN", "ambient-connect-secret")
	t.Setenv("OP_ACCOUNT", "wrong-account")
	t.Setenv("OP_SESSION_test", "ambient-session-secret")
	c := onePasswordConfig(t)
	calls := 0
	b := testOnePasswordBackend(t, &calls)
	resolver, err := New(c, map[string]BackendFactory{"onepassword": func(context.Context, SecretStoreProfile) (Backend, error) { return b, nil }})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	resolver.now = func() time.Time { return now }
	for _, want := range []string{"initial-private-token", "initial-private-token"} {
		r, e := resolver.Resolve(context.Background(), "model-key")
		if e != nil || r.Value != want || r.BackendVersion != "" {
			t.Fatal("read failed", e)
		}
	}
	if calls != 1 {
		t.Fatal("cache missed", calls)
	}
	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "rotated-private-token")
	now = now.Add(301 * time.Second)
	r, err := resolver.Resolve(context.Background(), "model-key")
	if err != nil || r.Value != "rotated-private-token" || calls != 2 {
		t.Fatal("refresh failed", err, calls)
	}
	resolver.Invalidate("model-key")
	if _, err = resolver.Resolve(context.Background(), "model-key"); err != nil || calls != 3 {
		t.Fatal("invalidation failed", err)
	}
}

func TestOnePasswordChildFailuresAreBoundedAndRedacted(t *testing.T) {
	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "private-token")
	c := onePasswordConfig(t)
	p := c.Profiles[0]
	l := c.Credentials[0].Locator
	p.MaxValueBytes = 128
	for _, test := range []struct {
		name, code string
		timeout    time.Duration
	}{
		{"stall", "deadline", 100 * time.Millisecond}, {"overflow", "value_invalid", 5 * time.Second},
		{"partial", "child_failed", 5 * time.Second}, {"malformed", "child_failed", 5 * time.Second}, {"reject", "access_rejected", 5 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), test.timeout)
			defer cancel()
			l.SecretReference = "op://development/" + test.name + "/key"
			started := time.Now()
			data, version, err := testOnePasswordBackend(t, nil).Read(ctx, p, l)
			if err == nil || len(data) != 0 || version != "" || strings.Contains(err.Error(), "SENSITIVE") {
				t.Fatal("unsafe failure", err)
			}
			if test.code == "deadline" {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
			} else {
				var safe onePasswordError
				if !errors.As(err, &safe) || string(safe) != test.code {
					t.Fatal(err)
				}
			}
			if time.Since(started) > 3*time.Second {
				t.Fatal("child was not stopped promptly")
			}
		})
	}
	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "")
	calls := 0
	if _, _, err := testOnePasswordBackend(t, &calls).Read(context.Background(), p, l); err == nil || calls != 0 {
		t.Fatal("missing bootstrap launched child")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := testOnePasswordBackend(t, &calls).Read(ctx, p, l); !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatal("cancellation ignored", err)
	}
}

func TestOnePasswordDesktopProfilesAreIsolated(t *testing.T) {
	c := onePasswordConfig(t)
	p := c.Profiles[0]
	p.Authentication = onePasswordDesktop
	p.MaxValueBytes = 256
	var group sync.WaitGroup
	for _, account := range []string{"First Account", "Second Account"} {
		group.Add(1)
		go func(account string) {
			defer group.Done()
			q := p
			q.OnePasswordAccount = account
			data, _, err := testOnePasswordBackend(t, nil).Read(context.Background(), q, c.Credentials[0].Locator)
			if err != nil || string(data) != account {
				t.Error("wrong account", err)
			}
		}(account)
	}
	group.Wait()
}

func TestOnePasswordSDKBoundaryValidation(t *testing.T) {
	request := onePasswordRequest{Authentication: onePasswordService, Token: "synthetic-private-token", Reference: "op://development/item/key", Maximum: 32}
	for _, test := range []struct {
		name, value string
		err         error
	}{
		{"exact bytes", "  value  ", nil}, {"oversized", strings.Repeat("x", 33), nil}, {"invalid utf8", string([]byte{0xff}), nil}, {"empty", "", nil}, {"nul", "a\x00b", nil}, {"error", "secret-value", errors.New("secret-error")},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, _ := json.Marshal(request)
			var output bytes.Buffer
			code := serveOnePasswordChild(context.Background(), bytes.NewReader(raw), &output, "product", "1.2.3", func(ctx context.Context, r onePasswordRequest, product, version string) (onePasswordClient, error) {
				if r.Token != request.Token || product != "product" || version != "1.2.3" {
					t.Fatal("wrong client options")
				}
				return onePasswordReadFunc(func(context.Context, string) (string, error) { return test.value, test.err }), nil
			})
			if code != 0 {
				t.Fatal(code)
			}
			var result onePasswordResponse
			json.Unmarshal(output.Bytes(), &result)
			if test.name == "exact bytes" {
				if result.Value != test.value || result.Error != "" {
					t.Fatal("value changed")
				}
			} else if result.Error == "" || result.Value != "" || strings.Contains(output.String(), "secret-") {
				t.Fatal("unsafe error response")
			}
		})
	}
}

func TestOnePasswordConfigFileAndValueSelection(t *testing.T) {
	c := onePasswordConfig(t)
	c.Credentials[0].Value = ValueSelection{Format: "json-string-field", Field: "api_key"}
	raw, _ := json.Marshal(c)
	file := filepath.Join(t.TempDir(), "credentials.json")
	os.WriteFile(file, raw, 0600)
	loaded, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{value: []byte(`{"api_key":"selected-value"}`)}
	r, err := New(loaded, map[string]BackendFactory{"onepassword": func(context.Context, SecretStoreProfile) (Backend, error) { return b, nil }})
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.Resolve(context.Background(), "model-key")
	if err != nil || result.Value != "selected-value" {
		t.Fatal("selection failed", err)
	}
}
