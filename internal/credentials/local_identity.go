//go:build linux || darwin

package credentials

import (
	"encoding/json"
	"errors"
	"os"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"golang.org/x/oauth2/google"
)

// An omitted store selector preserves existing workload-only behavior. Local
// SDK/CLI credentials are enabled only by an explicit administrator selection.
func ValidStoreAuthentication(backend, authentication string) bool {
	switch backend {
	case "aws-secrets-manager":
		return authentication == "" || authentication == "workload-identity" || authentication == "aws-profile" || authentication == "aws-environment"
	case "azure-key-vault":
		return authentication == "" || authentication == "workload-identity" || authentication == "azure-cli" || authentication == "azure-client-secret"
	case "gcp-secret-manager":
		return authentication == "" || authentication == "workload-identity" || authentication == "google-adc"
	default:
		return authentication == ""
	}
}

func AzureIdentity(authentication string) (azcore.TokenCredential, error) {
	switch authentication {
	case "", "workload-identity":
		return AzureWorkloadIdentity()
	case "azure-cli":
		return azidentity.NewAzureCLICredential(nil)
	case "azure-client-secret":
		tenant, client, secret := os.Getenv("AZURE_TENANT_ID"), os.Getenv("AZURE_CLIENT_ID"), os.Getenv("AZURE_CLIENT_SECRET")
		if tenant != "" && client != "" && secret != "" {
			identity, err := azidentity.NewClientSecretCredential(tenant, client, secret, nil)
			if err == nil {
				return identity, nil
			}
		}
	}
	return nil, errors.New("Azure credential configuration unavailable")
}

// GoogleQuotaProject mirrors the Google client's environment-over-ADC precedence
// for our direct model HTTP transport. It is host-only billing metadata.
func GoogleQuotaProject(identity *google.Credentials) (string, error) {
	quota := os.Getenv("GOOGLE_CLOUD_QUOTA_PROJECT")
	if quota == "" && identity != nil && len(identity.JSON) > 0 {
		var fields struct {
			Quota string `json:"quota_project_id"`
		}
		if json.Unmarshal(identity.JSON, &fields) != nil {
			return "", errors.New("invalid Google quota configuration")
		}
		quota = fields.Quota
	}
	if quota != "" && !stableID.MatchString(quota) {
		return "", errors.New("invalid Google quota configuration")
	}
	return quota, nil
}
