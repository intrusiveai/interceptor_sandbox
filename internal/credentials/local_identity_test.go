//go:build linux || darwin

package credentials

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"golang.org/x/oauth2/google"
)

func TestGoogleLocalADCAndWorkloadSeparation(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" {
			t.Error("unexpected token method")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"synthetic-token","token_type":"Bearer","expires_in":3600}`)
	}))
	defer server.Close()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	private := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	path := filepath.Join(t.TempDir(), "adc.json")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
	t.Setenv("GOOGLE_CLOUD_QUOTA_PROJECT", "")
	for _, kind := range []string{"authorized_user", "service_account"} {
		raw, _ := json.Marshal(map[string]any{"type": kind, "client_id": "synthetic-client", "client_secret": "synthetic-secret", "refresh_token": "synthetic-refresh", "token_uri": server.URL, "client_email": "test@example.invalid", "private_key": private, "quota_project_id": "billing-project"})
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{"", "workload-identity"} {
			if _, err = GoogleIdentity(context.Background(), mode); err == nil {
				t.Fatal("workload mode accepted local credentials")
			}
		}
		identity, err := GoogleIdentity(context.Background(), "google-adc")
		if err != nil {
			t.Fatal(err)
		}
		token, err := identity.TokenSource.Token()
		if err != nil || token.AccessToken != "synthetic-token" {
			t.Fatal("local ADC failed", err)
		}
		if quota, err := GoogleQuotaProject(identity); err != nil || quota != "billing-project" {
			t.Fatal("quota not preserved")
		}
	}
	if calls != 2 {
		t.Fatal("unexpected token requests", calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = GoogleIdentity(ctx, "google-adc"); err == nil {
		t.Fatal("ignored cancellation")
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "missing"))
	if _, err = GoogleIdentity(context.Background(), "google-adc"); err == nil {
		t.Fatal("missing ADC fell back")
	}
}

func TestGoogleFederationAndImpersonatedADC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "adc.json")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
	for _, raw := range []string{
		`{"type":"external_account","audience":"//iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/test/providers/test","subject_token_type":"urn:ietf:params:oauth:token-type:jwt","token_url":"https://sts.googleapis.com/v1/token","credential_source":{"file":"/nonexistent-synthetic-token"}}`,
		`{"type":"impersonated_service_account","service_account_impersonation_url":"https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/test@example.invalid:generateAccessToken","source_credentials":{"type":"authorized_user","client_id":"synthetic","client_secret":"synthetic","refresh_token":"synthetic"}}`,
	} {
		os.WriteFile(path, []byte(raw), 0600)
		if _, err := GoogleIdentity(context.Background(), "google-adc"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGoogleQuotaPrecedenceAndValidation(t *testing.T) {
	identity := &google.Credentials{JSON: []byte(`{"quota_project_id":"adc-project"}`)}
	t.Setenv("GOOGLE_CLOUD_QUOTA_PROJECT", "override-project")
	if quota, err := GoogleQuotaProject(identity); err != nil || quota != "override-project" {
		t.Fatal("quota precedence")
	}
	t.Setenv("GOOGLE_CLOUD_QUOTA_PROJECT", "invalid\r\nheader")
	if _, err := GoogleQuotaProject(identity); err == nil {
		t.Fatal("invalid quota accepted")
	}
}

func TestAzureExplicitCLIAndServicePrincipal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	// Only the SDK's fixed token command may be invoked; no interactive login.
	script := "#!/bin/sh\ncase \"$*\" in 'account get-access-token -o json --resource https://cognitiveservices.azure.com'|'account get-access-token -o json --resource https://vault.azure.net') ;; *) exit 9;; esac\nprintf '%s' '{\"accessToken\":\"synthetic-cli-token\",\"expires_on\":4102444800}'\n"
	if err := os.WriteFile(filepath.Join(dir, "az"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	identity, err := AzureIdentity("azure-cli")
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"https://cognitiveservices.azure.com/.default", "https://vault.azure.net/.default"} {
		token, err := identity.GetToken(context.Background(), policy.TokenRequestOptions{Scopes: []string{scope}})
		if err != nil || token.Token != "synthetic-cli-token" {
			t.Fatal("CLI credential failed", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = identity.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{"https://vault.azure.net/.default"}}); err == nil {
		t.Fatal("CLI ignored cancellation")
	}
	t.Setenv("AZURE_TENANT_ID", "synthetic-tenant")
	t.Setenv("AZURE_CLIENT_ID", "synthetic-client")
	t.Setenv("AZURE_CLIENT_SECRET", "")
	if _, err = AzureIdentity("azure-client-secret"); err == nil {
		t.Fatal("missing secret fell back to CLI")
	}
	t.Setenv("AZURE_CLIENT_SECRET", "synthetic-secret")
	identity, err = AzureIdentity("azure-client-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := identity.(*azidentity.ClientSecretCredential); !ok {
		t.Fatal("wrong service-principal mechanism")
	}
	t.Setenv("AZURE_FEDERATED_TOKEN_FILE", "")
	t.Setenv("AZURE_TENANT_ID", "")
	t.Setenv("AZURE_CLIENT_ID", "")
	identity, err = AzureIdentity("workload-identity")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := identity.(*azidentity.ManagedIdentityCredential); !ok {
		t.Fatal("workload mode selected local CLI/secret")
	}
	if _, err = AzureIdentity("unknown"); err == nil {
		t.Fatal("unknown mode accepted")
	}
}

func TestStoreAuthenticationSelectors(t *testing.T) {
	for _, backend := range []string{"aws-secrets-manager", "azure-key-vault", "gcp-secret-manager", "hashicorp-vault"} {
		for _, mode := range []string{"", "workload-identity", "google-adc", "azure-cli", "azure-client-secret", "aws-environment", "secret-store", "bogus"} {
			want := mode == "" || backend == "aws-secrets-manager" && (mode == "workload-identity" || mode == "aws-environment") || (backend == "azure-key-vault" && (mode == "workload-identity" || mode == "azure-cli" || mode == "azure-client-secret")) || (backend == "gcp-secret-manager" && (mode == "workload-identity" || mode == "google-adc"))
			config := Config{Profiles: []SecretStoreProfile{{APIVersion: StoreProfileVersion, Kind: "SecretStoreProfile", ID: "test", BackendKind: backend, Authentication: mode, Region: "us-east-2", VaultURL: "https://vault.example", VaultProxy: true, AllowedLocatorPrefixes: []string{"test"}}}}
			if got := Validate(config) == nil; got != want {
				t.Fatalf("%s %s accepted=%v", backend, mode, got)
			}
		}
	}
}

// GitHub's Google action publishes external-account ADC with a credential-source
// URL and Authorization header. Exercise that format through the official SDK.
func TestGitHubGoogleExternalAccountExchange(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/github-oidc":
			if r.Header.Get("Authorization") != "Bearer synthetic-job-token" {
				t.Error("missing job bootstrap header")
			}
			fmt.Fprint(w, `{"value":"synthetic-subject-token"}`)
		case "/sts":
			requests++
			if err := r.ParseForm(); err != nil || r.Form.Get("subject_token") != "synthetic-subject-token" {
				t.Error("wrong subject token")
			}
			fmt.Fprint(w, `{"access_token":"synthetic-federated-token","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","token_type":"Bearer","expires_in":3600}`)
		default:
			t.Error("unexpected identity request")
		}
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "gha-creds.json")
	raw, _ := json.Marshal(map[string]any{"type": "external_account", "audience": "//iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/test/providers/github", "subject_token_type": "urn:ietf:params:oauth:token-type:jwt", "token_url": server.URL + "/sts", "credential_source": map[string]any{"url": server.URL + "/github-oidc", "headers": map[string]string{"Authorization": "Bearer synthetic-job-token"}, "format": map[string]string{"type": "json", "subject_token_field_name": "value"}}})
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", file)
	for _, mode := range []string{"workload-identity", "google-adc"} {
		identity, err := GoogleIdentity(context.Background(), mode)
		if err != nil {
			t.Fatal(err)
		}
		token, err := identity.TokenSource.Token()
		if err != nil || token.AccessToken != "synthetic-federated-token" {
			t.Fatal("federated exchange failed", err)
		}
	}
	if requests != 2 {
		t.Fatal("exchange did not run")
	}
}
