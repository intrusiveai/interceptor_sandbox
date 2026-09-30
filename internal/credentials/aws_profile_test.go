//go:build linux || darwin

package credentials

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestExplicitAWSProfileSelectionAndNoFallback(t *testing.T) {
	for _, key := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN", "AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"} {
		t.Setenv(key, "")
	}
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config")
	keyPath := filepath.Join(dir, "credentials")
	os.WriteFile(cfgPath, []byte("[profile selected]\nregion=us-west-2\n[profile empty]\nregion=us-west-2\n[profile sso]\nsso_session=test\nsso_account_id=123456789012\nsso_role_name=Test\n[sso-session test]\nsso_start_url=https://example.awsapps.com/start\nsso_region=us-east-2\n"), 0600)
	os.WriteFile(keyPath, []byte("[default]\naws_access_key_id=wrong\naws_secret_access_key=wrong\n[selected]\naws_access_key_id=synthetic-selected\naws_secret_access_key=synthetic-secret\n"), 0600)
	t.Setenv("AWS_CONFIG_FILE", cfgPath)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", keyPath)
	t.Setenv("AWS_PROFILE", "default")
	cfg, err := AWSConfig(context.Background(), "us-east-2", "selected")
	if err != nil {
		t.Fatal(err)
	}
	value, err := cfg.Credentials.Retrieve(context.Background())
	if err != nil || value.AccessKeyID != "synthetic-selected" || cfg.Region != "us-east-2" {
		t.Fatal("explicit profile/region not selected")
	}
	for _, name := range []string{"missing", "empty", "invalid\nname"} {
		if _, err = AWSConfig(context.Background(), "us-east-2", name); err == nil {
			t.Fatal("accepted invalid/unconfigured profile")
		}
	}
	if _, err = AWSConfig(context.Background(), "us-east-2", "sso"); err != nil {
		t.Fatal("SSO profile not loaded", err)
	}
	t.Setenv("AWS_PROFILE", "")
	workload, err := AWSWorkloadConfig(context.Background(), "us-east-2")
	if err == nil {
		if _, err = workload.Credentials.Retrieve(context.Background()); err == nil {
			t.Fatal("workload mode loaded shared keys")
		}
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "ambient-key")
	if _, err = AWSConfig(context.Background(), "us-east-2", "selected"); err == nil {
		t.Fatal("accepted ambiguous environment keys")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = AWSConfig(canceled, "us-east-2", "selected"); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestAWSProfileSelectorValidation(t *testing.T) {
	config := validConfig()
	config.Profiles[0].AWSProfile = "selected"
	if config.Profiles[0].BackendKind == "aws-secrets-manager" {
		t.Fatal("fixture must be non-AWS")
	}
	if Validate(config) == nil {
		t.Fatal("non-AWS store accepted AWS selector")
	}
	for _, name := range []string{"", " leading", "trailing ", "bad\nname"} {
		if ValidAWSProfile(name) {
			t.Fatal("invalid profile name accepted")
		}
	}
}

func TestAWSEnvironmentIdentitySelectsActionSessionWithoutFallback(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "action-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "action-secret")
	t.Setenv("AWS_SESSION_TOKEN", "action-session")
	t.Setenv("AWS_PROFILE", "ambient-missing-profile")
	cfg, err := AWSIdentityConfig(context.Background(), "us-east-2", "", "aws-environment")
	if err != nil {
		t.Fatal(err)
	}
	value, err := cfg.Credentials.Retrieve(context.Background())
	if err != nil || value.AccessKeyID != "action-key" || value.SessionToken != "action-session" {
		t.Fatal("action session not selected")
	}
	if _, err = AWSIdentityConfig(context.Background(), "us-east-2", "selected", "aws-environment"); err == nil {
		t.Fatal("mixed sources accepted")
	}
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	if _, err = AWSIdentityConfig(context.Background(), "us-east-2", "", "aws-environment"); err == nil {
		t.Fatal("incomplete environment fell back")
	}
}
func TestCredentialSchemaNamespacesArePortable(t *testing.T) {
	config := validConfig()
	config.Profiles[0].APIVersion = "operator.dev/secret-store-profile/v1alpha1"
	config.Credentials[0].APIVersion = "operator.dev/host-credential-ref/v1alpha1"
	if err := Validate(config); err != nil {
		t.Fatal(err)
	}
}
