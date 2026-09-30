//go:build linux || darwin

package credentials

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/googleapis/gax-go/v2"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"hash/crc32"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	vault "github.com/hashicorp/vault/api"
)

func DefaultFactories() map[string]BackendFactory {
	return map[string]BackendFactory{
		"aws-secrets-manager": newAWSBackend,
		"azure-key-vault":     newAzureBackend,
		"gcp-secret-manager":  newGCPBackend,
		"hashicorp-vault":     newVaultBackend,
	}
}

type awsSecretsClient interface {
	GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}
type awsBackend struct{ client awsSecretsClient }

func newAWSBackend(ctx context.Context, profile SecretStoreProfile) (Backend, error) {
	cfg, err := AWSIdentityConfig(ctx, profile.Region, profile.AWSProfile, profile.Authentication)
	if err != nil {
		return nil, err
	}
	return &awsBackend{client: secretsmanager.NewFromConfig(cfg, func(o *secretsmanager.Options) {
		o.BaseEndpoint = nil
		o.EndpointResolver = nil
		o.EndpointResolverV2 = secretsmanager.NewDefaultEndpointResolverV2()
	})}, nil
}
func (b *awsBackend) Read(ctx context.Context, profile SecretStoreProfile, locator Locator) ([]byte, string, error) {
	if locator.Region != profile.Region {
		return nil, "", errors.New("AWS locator region differs from profile")
	}
	input := &secretsmanager.GetSecretValueInput{SecretId: aws.String(locator.SecretID)}
	if locator.VersionID != "" {
		input.VersionId = aws.String(locator.VersionID)
	}
	if locator.VersionStage != "" {
		input.VersionStage = aws.String(locator.VersionStage)
	}
	output, err := b.client.GetSecretValue(ctx, input)
	if err != nil {
		return nil, "", err
	}
	if output == nil || len(output.SecretBinary) > 0 || output.SecretString == nil {
		return nil, "", errors.New("binary or empty AWS secret is unsupported")
	}
	return []byte(*output.SecretString), aws.ToString(output.VersionId), nil
}

type azureSecretsClient interface {
	GetSecret(context.Context, string, string, *azsecrets.GetSecretOptions) (azsecrets.GetSecretResponse, error)
}
type azureBackend struct {
	client   azureSecretsClient
	vaultURL string
}

func newAzureBackend(_ context.Context, profile SecretStoreProfile) (Backend, error) {
	credential, err := AzureIdentity(profile.Authentication)
	if err != nil {
		return nil, err
	}
	client, err := azsecrets.NewClient(profile.VaultURL, credential, nil)
	if err != nil {
		return nil, err
	}
	return &azureBackend{client: client, vaultURL: strings.TrimSuffix(profile.VaultURL, "/")}, nil
}
func (b *azureBackend) Read(ctx context.Context, _ SecretStoreProfile, locator Locator) ([]byte, string, error) {
	if strings.TrimSuffix(locator.VaultURL, "/") != b.vaultURL {
		return nil, "", errors.New("Azure vault differs from profile")
	}
	response, err := b.client.GetSecret(ctx, locator.SecretName, locator.Version, nil)
	if err != nil {
		return nil, "", err
	}
	if response.Value == nil {
		return nil, "", errors.New("Azure secret has no value")
	}
	version := locator.Version
	if version == "" && response.ID != nil {
		parsed, _ := url.Parse(string(*response.ID))
		pieces := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(pieces) >= 3 {
			version = pieces[len(pieces)-1]
		}
	}
	return []byte(*response.Value), version, nil
}

type gcpSecretsClient interface {
	AccessSecretVersion(context.Context, *secretmanagerpb.AccessSecretVersionRequest, ...gax.CallOption) (*secretmanagerpb.AccessSecretVersionResponse, error)
}
type gcpBackend struct {
	client         gcpSecretsClient // Injected client for deterministic backend tests.
	authentication string
}

func newGCPBackend(ctx context.Context, profile SecretStoreProfile) (Backend, error) {
	if ctx.Err() != nil || !ValidStoreAuthentication("gcp-secret-manager", profile.Authentication) {
		return nil, errors.New("Google credential configuration unavailable")
	}
	return &gcpBackend{authentication: profile.Authentication}, nil
}

// OAuth token sources bind the supplied context. Create the client under the
// current read deadline, not the resolver's already-finished setup context.
func googleSecretClient(ctx context.Context, authentication string) (*secretmanager.Client, error) {
	identity, err := GoogleIdentity(ctx, authentication)
	if err != nil {
		return nil, err
	}
	quota, err := GoogleQuotaProject(identity)
	if err != nil {
		return nil, err
	}
	return secretmanager.NewClient(ctx, option.WithCredentials(identity), option.WithQuotaProject(quota))
}
func (b *gcpBackend) Read(ctx context.Context, _ SecretStoreProfile, locator Locator) ([]byte, string, error) {
	client := b.client
	if client == nil {
		live, err := googleSecretClient(ctx, b.authentication)
		if err != nil {
			return nil, "", err
		}
		defer live.Close()
		client = live
	}
	name := fmt.Sprintf("projects/%s/secrets/%s/versions/%s", locator.Project, locator.SecretName, locator.Version)
	response, err := client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: name})
	if err != nil {
		return nil, "", err
	}
	if response == nil || response.Payload == nil {
		return nil, "", errors.New("GCP secret has no payload")
	}
	if response.Payload.DataCrc32C != nil && int64(crc32.Checksum(response.Payload.Data, crc32.MakeTable(crc32.Castagnoli))) != *response.Payload.DataCrc32C {
		return nil, "", errors.New("GCP secret CRC32C verification failed")
	}
	parts := strings.Split(response.Name, "/")
	version := parts[len(parts)-1]
	return append([]byte(nil), response.Payload.Data...), version, nil
}

type vaultBackend struct{ client *vault.Client }

func newVaultBackend(_ context.Context, profile SecretStoreProfile) (Backend, error) {
	transport := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 5 * time.Second, MaxResponseHeaderBytes: 16 << 10}
	configuration := &vault.Config{Address: profile.VaultURL, Timeout: time.Duration(profile.TimeoutSeconds) * time.Second, MaxRetries: 0, DisableRedirects: true, HttpClient: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if profile.VaultCACertificate != "" {
		if err := configuration.ConfigureTLS(&vault.TLSConfig{CACert: profile.VaultCACertificate}); err != nil {
			return nil, err
		}
	}
	client, err := vault.NewClient(configuration)
	if err != nil {
		return nil, err
	}
	client.SetNamespace(profile.VaultNamespace)
	client.SetToken("")
	client.SetHeaders(http.Header{})
	return &vaultBackend{client: client}, nil
}

func (b *vaultBackend) Read(ctx context.Context, profile SecretStoreProfile, locator Locator) ([]byte, string, error) {
	version := 0
	var err error
	if locator.Version != "" {
		version, err = strconv.Atoi(locator.Version)
		if err != nil || version < 1 {
			return nil, "", errors.New("Vault version must be a positive integer")
		}
	}
	client, err := b.client.CloneWithHeaders()
	if err != nil {
		return nil, "", err
	}
	client.SetToken("")
	if !profile.VaultProxy {
		tokenBytes, err := readPrivate(profile.VaultTokenFile, 64<<10)
		if err != nil {
			return nil, "", err
		}
		token := strings.TrimSpace(string(tokenBytes))
		clear(tokenBytes)
		if token == "" || strings.ContainsAny(token, "\x00\r\n") {
			return nil, "", errors.New("invalid Vault token sink")
		}
		client.SetToken(token)
	}
	defer client.SetToken("")
	var secret *vault.KVSecret
	if version == 0 {
		secret, err = client.KVv2(locator.Mount).Get(ctx, locator.Path)
	} else {
		secret, err = client.KVv2(locator.Mount).GetVersion(ctx, locator.Path, version)
	}
	if err != nil {
		return nil, "", err
	}
	if secret == nil || secret.Data == nil {
		return nil, "", errors.New("Vault secret is empty or deleted")
	}
	actualVersion := version
	if secret.VersionMetadata != nil {
		actualVersion = secret.VersionMetadata.Version
	}
	encoded, err := json.Marshal(secret.Data)
	if err != nil {
		return nil, "", errors.New("Vault KV secret is not a JSON object")
	}
	return encoded, strconv.Itoa(actualVersion), nil
}

func (b *gcpBackend) Close() error {
	if c, ok := b.client.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// ValidAWSProfile validates an opaque SDK profile name, never a path or command.
func ValidAWSProfile(name string) bool {
	return name != "" && len(name) <= 128 && strings.TrimSpace(name) == name && strings.IndexFunc(name, func(r rune) bool { return r < 32 || r == 127 }) < 0
}

func AWSWorkloadConfig(ctx context.Context, region string) (aws.Config, error) {
	return AWSConfig(ctx, region, "")
}

// AWSConfig enables shared files only for an explicitly selected profile. Missing
// or credential-free profiles fail rather than falling back to ambient identities.
func AWSConfig(ctx context.Context, region, profile string) (aws.Config, error) {
	mode := "workload-identity"
	if profile != "" {
		mode = "aws-profile"
	}
	return AWSIdentityConfig(ctx, region, profile, mode)
}

// AWSIdentityConfig selects a single host credential source. Environment mode
// supports the temporary session exported by GitHub's AWS OIDC action, as well
// as explicitly selected local access keys. It never falls back to another source.
func AWSIdentityConfig(ctx context.Context, region, profile, authentication string) (aws.Config, error) {
	if authentication == "" {
		authentication = "workload-identity"
		if profile != "" {
			authentication = "aws-profile"
		}
	}
	invalid := errors.New("AWS credential configuration unavailable")
	if ctx.Err() != nil {
		return aws.Config{}, invalid
	}
	if authentication == "aws-environment" {
		key, secret, token := os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"), os.Getenv("AWS_SESSION_TOKEN")
		if profile != "" || key == "" || secret == "" {
			return aws.Config{}, invalid
		}
		if region == "" {
			region = os.Getenv("AWS_REGION")
			if region == "" {
				region = os.Getenv("AWS_DEFAULT_REGION")
			}
		}
		return aws.Config{Region: region, Credentials: aws.NewCredentialsCache(awscredentials.NewStaticCredentialsProvider(key, secret, token))}, nil
	}
	if authentication != "workload-identity" && authentication != "aws-profile" || (authentication == "aws-profile") != (profile != "") {
		return aws.Config{}, invalid
	}

	if ctx.Err() != nil || (profile != "" && !ValidAWSProfile(profile)) {
		return aws.Config{}, invalid
	}
	if os.Getenv("AWS_ACCESS_KEY_ID") != "" || os.Getenv("AWS_SECRET_ACCESS_KEY") != "" || os.Getenv("AWS_SESSION_TOKEN") != "" {
		return aws.Config{}, invalid
	}
	options := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	if profile == "" {
		options = append(options, awsconfig.WithSharedConfigFiles([]string{}), awsconfig.WithSharedCredentialsFiles([]string{}))
	} else {
		options = append(options, awsconfig.WithSharedConfigProfile(profile))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return aws.Config{}, invalid
	}
	if profile != "" {
		selected := false
		for _, source := range cfg.ConfigSources {
			if shared, ok := source.(awsconfig.SharedConfig); ok && shared.Profile == profile {
				selected = shared.Credentials.HasKeys() || shared.Source != nil || shared.CredentialSource != "" || shared.CredentialProcess != "" || shared.WebIdentityTokenFile != "" || shared.SSOSession != nil || shared.SSOStartURL != ""
			}
		}
		if !selected {
			return aws.Config{}, invalid
		}
	}
	return cfg, nil
}

func AzureWorkloadIdentity() (azcore.TokenCredential, error) {
	var credential azcore.TokenCredential
	workload, err := azidentity.NewWorkloadIdentityCredential(nil)
	if err == nil {
		credential = workload
	} else {
		credential, err = azidentity.NewManagedIdentityCredential(nil)
		if err != nil {
			return nil, err
		}
	}
	return credential, nil
}

func GoogleWorkloadIdentity(ctx context.Context) (*google.Credentials, error) {
	return GoogleIdentity(ctx, "workload-identity")
}

// GoogleIdentity keeps workload-only admission separate from explicitly selected
// ADC, which also permits local user, service-account and impersonated credentials.
func GoogleIdentity(ctx context.Context, authentication string) (*google.Credentials, error) {
	invalid := errors.New("Google credential configuration unavailable")
	if ctx.Err() != nil || !ValidStoreAuthentication("gcp-secret-manager", authentication) {
		return nil, invalid
	}
	identity, err := google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, invalid
	}
	if authentication != "google-adc" && len(identity.JSON) > 0 {
		var kind struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(identity.JSON, &kind) != nil || kind.Type != "external_account" {
			return nil, invalid
		}
	}
	return identity, nil
}
