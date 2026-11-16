// Package httpsseam forwards declared HTTPS exchanges without campaign observation.
package httpsseam

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"golang.org/x/net/http/httpguts"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Seams []Binding `yaml:"seams" json:"seams"`
}
type Binding struct {
	ID     string `yaml:"id" json:"id"`
	Origin string `yaml:"origin" json:"origin"`
	CAFile string `yaml:"ca_file,omitempty" json:"ca_file,omitempty"`
	Auth   Auth   `yaml:"auth" json:"auth"`
	Debug  Debug  `yaml:"debug,omitempty" json:"-"`
}
type Auth struct {
	Kind               string `yaml:"kind" json:"kind"`
	UsernameCredential string `yaml:"username_credential,omitempty" json:"username_credential,omitempty"`
	PasswordCredential string `yaml:"password_credential,omitempty" json:"password_credential,omitempty"`
	Header             string `yaml:"header,omitempty" json:"header,omitempty"`
	Credential         string `yaml:"credential,omitempty" json:"credential,omitempty"`
	Prefix             string `yaml:"prefix,omitempty" json:"prefix,omitempty"`
}
type Debug struct {
	Enabled          bool  `yaml:"enabled"`
	MaxExchangeBytes int64 `yaml:"max_exchange_bytes,omitempty"`
	MaxTotalBytes    int64 `yaml:"max_total_bytes,omitempty"`
}
type endpoint struct {
	binding Binding
	origin  *url.URL
	tls     *tls.Config
	headers http.Header
	secrets []string
}

// Prepared holds immutable host-only bindings and resolved credentials. It must
// never be serialized into container configuration or evidence.
type Prepared struct {
	endpoints     map[string]*endpoint
	bindingDigest string
}
type Resolve func(context.Context, string) (string, error)

func Load(ctx context.Context, path string, declarations []environment.HTTPSSeam, resolve Resolve) (*Prepared, error) {
	var config Config
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(&config); err != nil {
			return nil, errors.New("invalid HTTPS seam configuration")
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			return nil, errors.New("HTTPS seam configuration requires one YAML document")
		}
	}
	return prepare(ctx, config, filepath.Dir(path), declarations, resolve)
}
func prepare(ctx context.Context, config Config, base string, declarations []environment.HTTPSSeam, resolve Resolve) (*Prepared, error) {
	p := &Prepared{endpoints: map[string]*endpoint{}}
	declared := map[string]bool{}
	for _, d := range declarations {
		declared[d.ID] = true
	}
	// Hash authority, credential references and actual trust roots, never secrets.
	identities := map[string]any{}
	for _, b := range config.Seams {
		if !declared[b.ID] || p.endpoints[b.ID] != nil {
			return nil, errors.New("HTTPS seam binding is undeclared or duplicated")
		}
		u, err := url.Parse(b.Origin)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
			return nil, fmt.Errorf("HTTPS seam %s requires a fixed HTTPS origin without path, credentials, query or fragment", b.ID)
		}
		e := &endpoint{binding: b, origin: u, tls: &tls.Config{MinVersion: tls.VersionTLS12}, headers: http.Header{}}
		var ca []byte
		if b.CAFile != "" {
			path := b.CAFile
			if !filepath.IsAbs(path) {
				path = filepath.Join(base, path)
			}
			ca, err = os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("HTTPS seam %s: cannot read private CA bundle", b.ID)
			}
			roots, err := x509.SystemCertPool()
			if err != nil {
				return nil, err
			}
			if !roots.AppendCertsFromPEM(ca) {
				return nil, fmt.Errorf("HTTPS seam %s: invalid private CA bundle", b.ID)
			}
			e.tls.RootCAs = roots
		}
		get := func(id string) (string, error) {
			if id == "" || resolve == nil {
				return "", errors.New("HTTPS seam authentication requires credential references and --credential-config")
			}
			value, err := resolve(ctx, id)
			if err != nil {
				return "", fmt.Errorf("HTTPS seam %s credential resolution failed", b.ID)
			}
			if value == "" || !httpguts.ValidHeaderFieldValue(value) {
				return "", errors.New("invalid HTTPS seam credential")
			}
			e.secrets = append(e.secrets, value)
			return value, nil
		}
		switch b.Auth.Kind {
		case "none":
			if b.Auth != (Auth{Kind: "none"}) {
				return nil, errors.New("unauthenticated seam cannot contain credential options")
			}
		case "basic":
			if b.Auth.Header != "" || b.Auth.Credential != "" || b.Auth.Prefix != "" {
				return nil, errors.New("basic seam authentication only accepts username/password references")
			}
			username, err := get(b.Auth.UsernameCredential)
			if err != nil {
				return nil, err
			}
			password, err := get(b.Auth.PasswordCredential)
			if err != nil {
				return nil, err
			}
			if strings.Contains(username, ":") {
				return nil, errors.New("basic username cannot contain a colon")
			}
			encoded := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
			e.headers.Set("Authorization", "Basic "+encoded)
			e.secrets = append(e.secrets, encoded)
		case "header":
			if b.Auth.UsernameCredential != "" || b.Auth.PasswordCredential != "" || !allowedAuthHeader(b.Auth.Header) || !httpguts.ValidHeaderFieldValue(b.Auth.Prefix) {
				return nil, errors.New("invalid HTTPS seam authentication header")
			}
			value, err := get(b.Auth.Credential)
			if err != nil {
				return nil, err
			}
			e.headers.Set(b.Auth.Header, b.Auth.Prefix+value)
			e.secrets = append(e.secrets, b.Auth.Prefix+value)
		default:
			return nil, errors.New("HTTPS seam auth.kind must be none, basic or header")
		}
		if b.Debug.MaxExchangeBytes < 0 || b.Debug.MaxTotalBytes < 0 {
			return nil, errors.New("HTTPS seam diagnostic limits must be positive")
		}
		if b.Debug.MaxExchangeBytes == 0 {
			b.Debug.MaxExchangeBytes = 64 << 10
		}
		if b.Debug.MaxTotalBytes == 0 {
			b.Debug.MaxTotalBytes = 16 << 20
		}
		if b.Debug.MaxExchangeBytes > b.Debug.MaxTotalBytes {
			return nil, errors.New("HTTPS seam diagnostic exchange limit exceeds total limit")
		}
		e.binding.Debug = b.Debug
		p.endpoints[b.ID] = e
		b.Debug = Debug{}
		b.CAFile = ""
		identities[b.ID] = struct {
			Binding Binding
			CA      string
		}{b, digest.Bytes(ca)}
	}
	if len(p.endpoints) != len(declared) {
		return nil, errors.New("each declared HTTPS seam requires a host binding")
	}
	encoded, _ := json.Marshal(identities)
	p.bindingDigest = digest.Bytes(encoded)
	return p, nil
}
func allowedAuthHeader(name string) bool {
	if !httpguts.ValidHeaderFieldName(name) {
		return false
	}
	switch strings.ToLower(name) {
	case "host", "connection", "upgrade", "content-length", "transfer-encoding", "trailer", "te", "keep-alive", "proxy-authorization", "proxy-authenticate", "cookie", "set-cookie", "content-type", "content-encoding", "accept-encoding", "expect":
		return false
	}
	return !strings.HasPrefix(strings.ToLower(name), "x-interceptor-") && !strings.HasPrefix(strings.ToLower(name), "forwarded") && !strings.HasPrefix(strings.ToLower(name), "x-forwarded-")
}
func (p *Prepared) Digest() string {
	if p == nil {
		return ""
	}
	return p.bindingDigest
}
func (p *Prepared) Check(declarations []environment.HTTPSSeam) error {
	if p == nil {
		if len(declarations) == 0 {
			return nil
		}
		return errors.New("declared HTTPS seams require --https-seam-config")
	}
	if len(declarations) != len(p.endpoints) {
		return errors.New("HTTPS seam bindings do not match sealed declarations")
	}
	for _, d := range declarations {
		if p.endpoints[d.ID] == nil {
			return errors.New("HTTPS seam binding is missing")
		}
	}
	return nil
}

// StrippedHeaders is the only runtime authentication information sent to the
// supervisor: header names, never values, origins, credential IDs or trust roots.
func (p *Prepared) StrippedHeaders() []string {
	names := []string{"Authorization", "Proxy-Authorization", "Cookie", "Cookie2", "X-Api-Key"}
	if p != nil {
		for _, e := range p.endpoints {
			if e.binding.Auth.Header != "" {
				names = append(names, e.binding.Auth.Header)
			}
		}
	}
	sort.Strings(names)
	return names
}
func Timeout(blueprint *environment.Blueprint) time.Duration {
	t := time.Duration(blueprint.Application.Interface.TurnTimeoutMS) * time.Millisecond
	if t <= 0 || t > 10*time.Minute {
		t = 10 * time.Minute
	}
	return t
}
