package appimage

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
	"runtime"
	"strings"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/buildinfo"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
	"github.com/intrusive-ai/interceptor-sandbox/internal/ociruntime"
)

type Lock struct {
	SchemaVersion        int       `json:"schema_version"`
	EnvironmentDigest    string    `json:"environment_digest"`
	SourceImage          string    `json:"source_image"`
	SourceDigest         string    `json:"source_digest"`
	WrapperImage         string    `json:"wrapper_image"`
	WrapperDigest        string    `json:"wrapper_digest"`
	InterceptorVersion   string    `json:"interceptor_version"`
	InterceptorBinary    string    `json:"interceptor_binary_digest"`
	BlueprintSchema      string    `json:"blueprint_schema"`
	JournalSchemaVersion int       `json:"journal_schema_version"`
	TLS                  *TLSLock  `json:"tls,omitempty"`
	ConstructedAt        time.Time `json:"constructed_at"`
}

type TLSLock struct {
	TrustMode               string   `json:"trust_mode"`
	Hostnames               []string `json:"hostnames"`
	RootCertificateDigest   string   `json:"root_certificate_digest"`
	ServerCertificateDigest string   `json:"server_certificate_digest"`
	ServerPrivateKeyDigest  string   `json:"server_private_key_digest"`
}

func LockPath(dataRoot string, blueprint *environment.Blueprint) string {
	return filepath.Join(dataRoot, "apps", blueprint.Metadata.Name, blueprint.Metadata.Version, "app.lock.json")
}

func Build(ctx context.Context, directory, dataRoot, tag, version string, docker ociruntime.Docker) (Lock, error) {
	environmentLock, report, err := environment.Verify(directory)
	if err != nil {
		return Lock{}, err
	}
	if tag == "" {
		return Lock{}, errors.New("wrapper image tag is required")
	}
	temporary, err := os.MkdirTemp("", "interceptor-app-build-*")
	if err != nil {
		return Lock{}, err
	}
	defer os.RemoveAll(temporary)
	binaryPath := filepath.Join(temporary, "interceptor")
	if err := buildLinuxBinary(ctx, binaryPath); err != nil {
		return Lock{}, err
	}
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		return Lock{}, err
	}
	sourceReference := report.Blueprint.Application.Image
	if separator := strings.LastIndex(sourceReference, "@"); separator > 0 {
		localTag := sourceReference[:separator]
		if localID, inspectErr := docker.ImageID(ctx, localTag); inspectErr == nil && localID == sourceReference[separator+1:] {
			// BuildKit treats a bare sha256:<id> in FROM as an image name. Keep
			// the local tag after proving that it resolves to the sealed ID.
			sourceReference = localTag
		}
	}
	dockerfile := fmt.Sprintf("FROM %s\nCOPY --chmod=0555 interceptor /usr/local/bin/interceptor\n", sourceReference)
	var material tlsMaterial
	var tlsLock *TLSLock
	if report.Blueprint.Network.TLS.Enabled {
		hostnames := simulatedHostnames(report.Blueprint)
		material, err = generateTLSMaterial(time.Now().UTC(), hostnames)
		if err != nil {
			return Lock{}, fmt.Errorf("generate sandbox TLS material: %w", err)
		}
		if err := os.WriteFile(filepath.Join(temporary, "interceptor-ca.pem"), material.ca, 0o600); err != nil {
			return Lock{}, err
		}
		dockerfile += "COPY --chmod=0444 interceptor-ca.pem " + ContainerCABundle + "\n"
		if report.Blueprint.Network.TLS.TrustMode == "auto" {
			dockerfile += "RUN [\"/usr/local/bin/interceptor\",\"image-install-ca\",\"" + ContainerCABundle + "\"]\n"
		}
		tlsLock = &TLSLock{TrustMode: report.Blueprint.Network.TLS.TrustMode, Hostnames: hostnames, RootCertificateDigest: digest.Bytes(material.ca), ServerCertificateDigest: digest.Bytes(material.certificate), ServerPrivateKeyDigest: digest.Bytes(material.privateKey)}
	}
	dockerfile += "ENTRYPOINT [\"/usr/local/bin/interceptor\"]\n"
	if err := os.WriteFile(filepath.Join(temporary, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		return Lock{}, err
	}
	if err := docker.Build(ctx, temporary, tag); err != nil {
		return Lock{}, err
	}
	wrapperDigest, err := docker.ImageID(ctx, tag)
	if err != nil {
		return Lock{}, err
	}
	lock := Lock{
		SchemaVersion: 1, EnvironmentDigest: environmentLock.EnvironmentDigest,
		SourceImage: report.Blueprint.Application.Image, SourceDigest: environmentLock.ApplicationDigest,
		WrapperImage: tag, WrapperDigest: wrapperDigest, InterceptorVersion: version,
		InterceptorBinary: digest.Bytes(binary), BlueprintSchema: environment.APIVersion,
		JournalSchemaVersion: 1, TLS: tlsLock, ConstructedAt: time.Now().UTC(),
	}
	if report.Blueprint.Network.TLS.Enabled {
		if err := persistTLSMaterial(dataRoot, report.Blueprint, material); err != nil {
			return Lock{}, fmt.Errorf("persist sandbox TLS material: %w", err)
		}
	}
	if err := fsutil.WriteJSON(LockPath(dataRoot, report.Blueprint), lock, 0o640); err != nil {
		return Lock{}, err
	}
	return lock, nil
}

func Load(path string) (Lock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Lock{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var lock Lock
	if err := decoder.Decode(&lock); err != nil {
		return Lock{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Lock{}, errors.New("application lock contains trailing JSON")
	}
	if lock.SchemaVersion != 1 || lock.WrapperImage == "" || !digest.Valid(lock.WrapperDigest) || !digest.Valid(lock.SourceDigest) || !digest.Valid(lock.EnvironmentDigest) || !digest.Valid(lock.InterceptorBinary) {
		return Lock{}, errors.New("invalid application lock")
	}
	if lock.TLS != nil && (lock.TLS.TrustMode == "" || len(lock.TLS.Hostnames) == 0 || !digest.Valid(lock.TLS.RootCertificateDigest) || !digest.Valid(lock.TLS.ServerCertificateDigest) || !digest.Valid(lock.TLS.ServerPrivateKeyDigest)) {
		return Lock{}, errors.New("invalid application TLS lock")
	}
	return lock, nil
}

func Verify(ctx context.Context, lock Lock, environmentLock *environment.Lock, docker ociruntime.Docker) error {
	if lock.EnvironmentDigest != environmentLock.EnvironmentDigest || lock.SourceDigest != environmentLock.ApplicationDigest || lock.BlueprintSchema != environment.APIVersion || lock.JournalSchemaVersion != 1 {
		return errors.New("application lock is incompatible with Sealed Environment")
	}
	if lock.InterceptorVersion != buildinfo.Version {
		return fmt.Errorf("wrapper was built by interceptor %s, current version is %s", lock.InterceptorVersion, buildinfo.Version)
	}
	actual, err := docker.ImageID(ctx, lock.WrapperImage)
	if err != nil {
		return err
	}
	if actual != lock.WrapperDigest {
		return fmt.Errorf("wrapper image digest changed: expected %s, got %s", lock.WrapperDigest, actual)
	}
	return nil
}

func buildLinuxBinary(ctx context.Context, destination string) error {
	if runtime.GOOS == "linux" {
		current, err := os.Executable()
		if err == nil {
			data, readErr := os.ReadFile(current)
			if readErr == nil {
				return os.WriteFile(destination, data, 0o555)
			}
		}
	}
	root, err := moduleRoot()
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", destination, "./cmd/interceptor")
	command.Dir = root
	command.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("build Linux interceptor binary: %w\n%s", err, output)
	}
	return nil
}

func moduleRoot() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		data, err := os.ReadFile(filepath.Join(directory, "go.mod"))
		if err == nil && strings.Contains(string(data), "module github.com/intrusive-ai/interceptor-sandbox") {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", errors.New("cannot locate interceptor Go module; use a Linux release binary")
		}
		directory = parent
	}
}
