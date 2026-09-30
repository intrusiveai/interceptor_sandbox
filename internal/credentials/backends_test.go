//go:build linux || darwin

package credentials

import (
	"context"
	"fmt"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/googleapis/gax-go/v2"
	vault "github.com/hashicorp/vault/api"
)

type awsFake struct {
	input *secretsmanager.GetSecretValueInput
}

func (f *awsFake) GetSecretValue(_ context.Context, in *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	f.input = in
	return &secretsmanager.GetSecretValueOutput{SecretString: aws.String("aws-test-secret"), VersionId: aws.String("version-1")}, nil
}

type azureFake struct{ name, version string }

func (f *azureFake) GetSecret(_ context.Context, name, version string, _ *azsecrets.GetSecretOptions) (azsecrets.GetSecretResponse, error) {
	f.name = name
	f.version = version
	return azsecrets.GetSecretResponse{Secret: azsecrets.Secret{Value: aws.String("azure-test-secret")}}, nil
}

type gcpFake struct {
	name    string
	corrupt bool
}

func (f *gcpFake) AccessSecretVersion(_ context.Context, in *secretmanagerpb.AccessSecretVersionRequest, _ ...gax.CallOption) (*secretmanagerpb.AccessSecretVersionResponse, error) {
	f.name = in.Name
	raw := []byte("gcp-test-secret")
	sum := int64(crc32.Checksum(raw, crc32.MakeTable(crc32.Castagnoli)))
	if f.corrupt {
		sum++
	}
	return &secretmanagerpb.AccessSecretVersionResponse{Name: strings.TrimSuffix(in.Name, "latest") + "3", Payload: &secretmanagerpb.SecretPayload{Data: raw, DataCrc32C: &sum}}, nil
}

func TestCloudBackendRequests(t *testing.T) {
	ctx := context.Background()
	a := &awsFake{}
	ab := awsBackend{a}
	raw, version, err := ab.Read(ctx, SecretStoreProfile{Region: "us-west-2"}, Locator{Region: "us-west-2", SecretID: "secret", VersionStage: "AWSCURRENT"})
	if err != nil || string(raw) != "aws-test-secret" || version != "version-1" || aws.ToString(a.input.SecretId) != "secret" || aws.ToString(a.input.VersionStage) != "AWSCURRENT" {
		t.Fatal("AWS request/response failed", err)
	}
	if _, _, err := ab.Read(ctx, SecretStoreProfile{Region: "us-east-1"}, Locator{Region: "us-west-2"}); err == nil {
		t.Fatal("region mismatch accepted")
	}
	z := &azureFake{}
	zb := azureBackend{client: z, vaultURL: "https://test.vault.azure.net"}
	raw, version, err = zb.Read(ctx, SecretStoreProfile{}, Locator{VaultURL: "https://test.vault.azure.net", SecretName: "secret", Version: "v2"})
	if err != nil || string(raw) != "azure-test-secret" || version != "v2" || z.name != "secret" || z.version != "v2" {
		t.Fatal("Azure request/response failed", err)
	}
	g := &gcpFake{}
	gb := gcpBackend{client: g}
	raw, version, err = gb.Read(ctx, SecretStoreProfile{}, Locator{Project: "project", SecretName: "secret", Version: "latest"})
	if err != nil || string(raw) != "gcp-test-secret" || version != "3" || g.name != "projects/project/secrets/secret/versions/latest" {
		t.Fatal("GCP request/response failed", err)
	}
	g.corrupt = true
	if _, _, err := gb.Read(ctx, SecretStoreProfile{}, Locator{Project: "project", SecretName: "secret", Version: "latest"}); err == nil {
		t.Fatal("bad CRC accepted")
	}
}

func TestVaultRotatesPrivateTokenSinkAndSupportsProxy(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("first-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	expected := "first-token"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/kv/data/models/test" || r.URL.Query().Get("version") != "2" || r.Header.Get("X-Vault-Token") != expected {
			t.Error("incorrect Vault request")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"data":{"api_key":"vault-test-secret"},"metadata":{"version":2}}}`)
	}))
	defer server.Close()
	client, err := vault.NewClient(&vault.Config{Address: server.URL, HttpClient: server.Client(), MaxRetries: 0, DisableRedirects: true})
	if err != nil {
		t.Fatal(err)
	}
	b := vaultBackend{client: client}
	p := SecretStoreProfile{VaultTokenFile: token}
	for _, next := range []string{"first-token", "second-token", ""} {
		expected = next
		if next == "" {
			p.VaultProxy = true
			p.VaultTokenFile = ""
		} else if err := os.WriteFile(token, []byte(next), 0600); err != nil {
			t.Fatal(err)
		}
		raw, version, err := b.Read(context.Background(), p, Locator{Mount: "kv", Path: "models/test", Version: "2"})
		if err != nil || version != "2" || !strings.Contains(string(raw), "vault-test-secret") {
			t.Fatal("Vault resolution failed", err)
		}
	}
	p.VaultProxy = false
	p.VaultTokenFile = token
	if err := os.Chmod(token, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Read(context.Background(), p, Locator{Mount: "kv", Path: "models/test"}); err == nil {
		t.Fatal("public token sink accepted")
	}
}
