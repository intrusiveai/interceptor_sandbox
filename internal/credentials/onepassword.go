//go:build linux || darwin

package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	op "github.com/1password/onepassword-sdk-go"
)

// OnePasswordChildCommand is an internal, single-read entry point in this binary.
// Only the audited parent resolver discloses the result to a provider.
const OnePasswordChildCommand = "__onepassword-read"

const (
	onePasswordDesktop      = "onepassword-desktop"
	onePasswordService      = "onepassword-service-account"
	onePasswordRequestLimit = 64 << 10
)

type onePasswordError string

func (e onePasswordError) Error() string {
	switch e {
	case "bootstrap_missing":
		return "1Password service-account token is missing or invalid; provide OP_SERVICE_ACCOUNT_TOKEN to the host process"
	case "desktop_unavailable":
		return "1Password desktop integration is unavailable; install the supported app and enable SDK integration"
	case "access_rejected":
		return "1Password rejected the read; check SDK approval, vault read permission and the configured reference"
	case "value_invalid":
		return "1Password returned an invalid or oversized secret value"
	case "child_failed":
		return "1Password credential worker failed; verify the installed product and its runtime dependencies"
	default:
		return "1Password credential request failed"
	}
}

func safeOnePasswordCode(code string) error {
	switch code {
	case "bootstrap_missing", "desktop_unavailable", "access_rejected", "value_invalid", "child_failed":
		return onePasswordError(code)
	default:
		return onePasswordError("child_failed")
	}
}

func validOnePasswordReference(value string, prefix bool) bool {
	if !utf8.ValidString(value) || len(value) > 2048 || !strings.HasPrefix(value, "op://") || strings.ContainsAny(value, "?#") || strings.ContainsFunc(value, unicode.IsControl) {
		return false
	}
	path := strings.TrimPrefix(value, "op://")
	if prefix {
		if !strings.HasSuffix(path, "/") {
			return false
		}
		path = strings.TrimSuffix(path, "/")
	}
	parts := strings.Split(path, "/")
	if prefix && (len(parts) < 1 || len(parts) > 3) || !prefix && len(parts) != 3 && len(parts) != 4 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func validateOnePasswordProfile(p SecretStoreProfile) error {
	if p.BackendKind != "onepassword" {
		if p.OnePasswordAccount != "" {
			return errors.New("onepassword_account requires the onepassword backend")
		}
		return nil
	}
	if p.Region != "" || p.AWSProfile != "" || p.VaultURL != "" || p.VaultNamespace != "" || p.VaultProxy || p.VaultTokenFile != "" || p.VaultCACertificate != "" {
		return errors.New("onepassword profile contains fields for another backend")
	}
	if p.Authentication == onePasswordDesktop {
		if !utf8.ValidString(p.OnePasswordAccount) || strings.TrimSpace(p.OnePasswordAccount) == "" || len(p.OnePasswordAccount) > 256 || strings.ContainsFunc(p.OnePasswordAccount, unicode.IsControl) {
			return errors.New("onepassword_account must be a bounded desktop account name or UUID")
		}
	} else if p.Authentication != onePasswordService || p.OnePasswordAccount != "" {
		return errors.New("onepassword authentication must select desktop with onepassword_account or service-account without it")
	}
	if len(p.AllowedLocatorPrefixes) == 0 || len(p.AllowedLocatorPrefixes) > 128 {
		return errors.New("onepassword requires 1 to 128 allowed_locator_prefixes")
	}
	for _, prefix := range p.AllowedLocatorPrefixes {
		if !validOnePasswordReference(prefix, true) {
			return errors.New("onepassword allowed_locator_prefixes must end at a vault, item or section boundary")
		}
	}
	return nil
}

func onePasswordLocator(l Locator) (string, error) {
	other := l
	other.BackendKind, other.SecretReference = "", ""
	if other != (Locator{}) || !validOnePasswordReference(l.SecretReference, false) {
		return "", errors.New("onepassword locator requires only backend_kind and a valid secret_reference")
	}
	return l.SecretReference, nil
}

// The subprocess owns SDK-global state and native calls. A canceled request can
// be killed even when the desktop library ignores context cancellation.
type onePasswordBackend struct {
	command func(context.Context) (*exec.Cmd, error)
}

func newOnePasswordBackend(_ context.Context, _ SecretStoreProfile) (Backend, error) {
	return &onePasswordBackend{command: func(ctx context.Context) (*exec.Cmd, error) {
		executable, err := os.Executable()
		if err != nil {
			return nil, err
		}
		return exec.CommandContext(ctx, executable, OnePasswordChildCommand), nil
	}}, nil
}

type onePasswordRequest struct {
	Authentication string `json:"authentication"`
	Account        string `json:"account,omitempty"`
	Reference      string `json:"reference"`
	Token          string `json:"token,omitempty"`
	Maximum        int64  `json:"maximum"`
}
type onePasswordResponse struct {
	Value string `json:"value,omitempty"`
	Error string `json:"error,omitempty"`
}

// limitedSecretBuffer stops the child on overflow without retaining excess bytes.
type limitedSecretBuffer struct {
	data     bytes.Buffer
	limit    int
	overflow bool
	stop     context.CancelFunc
}

func (b *limitedSecretBuffer) Bytes() []byte { return b.data.Bytes() }

func (b *limitedSecretBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.data.Len() {
		b.overflow = true
		if b.stop != nil {
			b.stop()
		}
		return 0, errors.New("credential response exceeds limit")
	}
	return b.data.Write(p)
}

func (b *onePasswordBackend) Read(ctx context.Context, p SecretStoreProfile, l Locator) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if err := validateOnePasswordProfile(p); err != nil {
		return nil, "", onePasswordError("child_failed")
	}
	if _, err := onePasswordLocator(l); err != nil || p.MaxValueBytes < 1 || p.MaxValueBytes > 1<<20 {
		return nil, "", onePasswordError("child_failed")
	}
	r := onePasswordRequest{Authentication: p.Authentication, Account: p.OnePasswordAccount, Reference: l.SecretReference, Maximum: p.MaxValueBytes}
	if p.Authentication == onePasswordService {
		r.Token = os.Getenv("OP_SERVICE_ACCOUNT_TOKEN")
		if r.Token == "" || len(r.Token) > 16384 || strings.ContainsFunc(r.Token, unicode.IsControl) {
			return nil, "", onePasswordError("bootstrap_missing")
		}
	}
	input, err := json.Marshal(r)
	if err != nil || len(input) > onePasswordRequestLimit {
		return nil, "", onePasswordError("child_failed")
	}
	defer clear(input)
	childContext, stop := context.WithCancel(ctx)
	defer stop()
	cmd, err := b.command(childContext)
	if err != nil {
		return nil, "", onePasswordError("child_failed")
	}
	// Credentials travel only through a pipe. SDK authentication is explicit;
	// ambient op configuration is not forwarded to the worker.
	cmd.Env = nil
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "OP_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Stdin = bytes.NewReader(input)
	output := &limitedSecretBuffer{limit: int(p.MaxValueBytes)*6 + 1024, stop: stop}
	defer func() { clear(output.Bytes()) }()
	cmd.Stdout, cmd.Stderr = output, io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return os.ErrProcessDone
	}
	cmd.WaitDelay = time.Second
	if err = cmd.Run(); ctx.Err() != nil {
		return nil, "", ctx.Err()
	}
	if output.overflow {
		return nil, "", onePasswordError("value_invalid")
	}
	if err != nil {
		return nil, "", onePasswordError("child_failed")
	}
	var response onePasswordResponse
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&response) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, "", onePasswordError("child_failed")
	}
	if response.Error != "" {
		return nil, "", safeOnePasswordCode(response.Error)
	}
	if response.Value == "" || int64(len(response.Value)) > p.MaxValueBytes || !utf8.ValidString(response.Value) || strings.ContainsRune(response.Value, 0) {
		return nil, "", onePasswordError("value_invalid")
	}
	return []byte(response.Value), "", nil
}

type onePasswordClient interface {
	Resolve(context.Context, string) (string, error)
}
type onePasswordFactory func(context.Context, onePasswordRequest, string, string) (onePasswordClient, error)

func sdkOnePasswordClient(ctx context.Context, r onePasswordRequest, product, version string) (onePasswordClient, error) {
	options := []op.ClientOption{op.WithIntegrationInfo(product, version)}
	if r.Authentication == onePasswordDesktop {
		options = append(options, op.WithDesktopAppIntegration(r.Account))
	} else {
		options = append(options, op.WithServiceAccountToken(r.Token))
	}
	client, err := op.NewClient(ctx, options...)
	if err != nil {
		return nil, err
	}
	return client.Secrets(), nil
}

// ServeOnePasswordChild handles one private request and exits with its caller.
// It never writes unrestricted SDK diagnostics or executes another command.
func ServeOnePasswordChild(ctx context.Context, in io.Reader, out io.Writer, product, version string) int {
	return serveOnePasswordChild(ctx, in, out, product, version, sdkOnePasswordClient)
}

func serveOnePasswordChild(ctx context.Context, in io.Reader, out io.Writer, product, version string, factory onePasswordFactory) int {
	var request onePasswordRequest
	raw, err := io.ReadAll(io.LimitReader(in, onePasswordRequestLimit+1))
	defer clear(raw)
	if err != nil || len(raw) > onePasswordRequestLimit {
		return 1
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&request) != nil || d.Decode(new(any)) != io.EOF {
		return 1
	}
	profile := SecretStoreProfile{BackendKind: "onepassword", Authentication: request.Authentication, OnePasswordAccount: request.Account, AllowedLocatorPrefixes: []string{"op://validation/"}}
	if validateOnePasswordProfile(profile) != nil || !validOnePasswordReference(request.Reference, false) || request.Maximum < 1 || request.Maximum > 1<<20 {
		return 1
	}
	if request.Authentication == onePasswordService && (request.Token == "" || len(request.Token) > 16384 || strings.ContainsFunc(request.Token, unicode.IsControl)) || request.Authentication == onePasswordDesktop && request.Token != "" {
		return 1
	}
	// Also bound manually invoked workers; the parent enforces the shorter real
	// request deadline by killing the process if native SDK code does not return.
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	response := onePasswordResponse{}
	client, err := factory(ctx, request, product, version)
	if err != nil {
		if request.Authentication == onePasswordDesktop {
			response.Error = "desktop_unavailable"
		} else {
			response.Error = "access_rejected"
		}
	} else {
		value, err := client.Resolve(ctx, request.Reference)
		if err != nil || ctx.Err() != nil {
			response.Error = "access_rejected"
		} else if value == "" || int64(len(value)) > request.Maximum || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			response.Error = "value_invalid"
		} else {
			response.Value = value
		}
	}
	if json.NewEncoder(out).Encode(response) != nil {
		return 1
	}
	return 0
}
