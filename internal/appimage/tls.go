package appimage

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
)

const (
	ContainerCABundle       = "/opt/interceptor/tls/ca.pem"
	ContainerTLSCertificate = "/run/interceptor-tls/server.pem"
	ContainerTLSPrivateKey  = "/run/interceptor-tls/server-key.pem"
)

type tlsMaterial struct {
	ca, certificate, privateKey []byte
}

func TLSMaterialDirectory(dataRoot string, blueprint *environment.Blueprint) string {
	return filepath.Join(dataRoot, "apps", blueprint.Metadata.Name, blueprint.Metadata.Version, "tls")
}

func TLSMaterialFiles(dataRoot string, blueprint *environment.Blueprint) (string, string, string) {
	directory := TLSMaterialDirectory(dataRoot, blueprint)
	return filepath.Join(directory, "ca.pem"), filepath.Join(directory, "server.pem"), filepath.Join(directory, "server-key.pem")
}

func simulatedHostnames(blueprint *environment.Blueprint) []string {
	seen := map[string]struct{}{}
	for _, service := range blueprint.Services {
		for _, hostname := range service.Hostnames {
			seen[strings.ToLower(strings.TrimSuffix(hostname, "."))] = struct{}{}
		}
	}
	for _, service := range blueprint.AttackerServices {
		for _, hostname := range service.Hostnames {
			seen[strings.ToLower(strings.TrimSuffix(hostname, "."))] = struct{}{}
		}
	}
	for _, issuer := range blueprint.Identity.Issuers {
		for _, hostname := range issuer.Hostnames {
			seen[strings.ToLower(strings.TrimSuffix(hostname, "."))] = struct{}{}
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func generateTLSMaterial(now time.Time, hostnames []string) (tlsMaterial, error) {
	if len(hostnames) == 0 {
		return tlsMaterial{}, errors.New("TLS material requires at least one hostname")
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tlsMaterial{}, err
	}
	caSerial, err := randomSerial()
	if err != nil {
		return tlsMaterial{}, err
	}
	caTemplate := &x509.Certificate{
		SerialNumber: caSerial, Subject: pkix.Name{CommonName: "Interceptor Sandbox Environment CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(5, 0, 0), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return tlsMaterial{}, err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tlsMaterial{}, err
	}
	leafSerial, err := randomSerial()
	if err != nil {
		return tlsMaterial{}, err
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: leafSerial, Subject: pkix.Name{CommonName: hostnames[0]}, DNSNames: append([]string(nil), hostnames...),
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(2, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caTemplate, &leafKey.PublicKey, caKey)
	if err != nil {
		return tlsMaterial{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		return tlsMaterial{}, err
	}
	return tlsMaterial{
		ca:          pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		certificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		privateKey:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}

func persistTLSMaterial(dataRoot string, blueprint *environment.Blueprint, material tlsMaterial) error {
	directory := TLSMaterialDirectory(dataRoot, blueprint)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	caPath, certificatePath, keyPath := TLSMaterialFiles(dataRoot, blueprint)
	if err := fsutil.WriteFile(caPath, material.ca, 0o600); err != nil {
		return err
	}
	if err := fsutil.WriteFile(certificatePath, material.certificate, 0o600); err != nil {
		return err
	}
	return fsutil.WriteFile(keyPath, material.privateKey, 0o600)
}

func VerifyTLSMaterial(dataRoot string, blueprint *environment.Blueprint, lock Lock) error {
	if !blueprint.Network.TLS.Enabled {
		if lock.TLS != nil {
			return errors.New("application lock contains TLS material for a TLS-disabled Environment")
		}
		return nil
	}
	if lock.TLS == nil || lock.TLS.TrustMode != blueprint.Network.TLS.TrustMode {
		return errors.New("application lock TLS configuration does not match the Environment")
	}
	expectedHostnames := simulatedHostnames(blueprint)
	if strings.Join(lock.TLS.Hostnames, "\x00") != strings.Join(expectedHostnames, "\x00") {
		return errors.New("application lock TLS hostnames do not match the Environment")
	}
	caPath, certificatePath, keyPath := TLSMaterialFiles(dataRoot, blueprint)
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return fmt.Errorf("read sandbox CA: %w", err)
	}
	certificatePEM, err := os.ReadFile(certificatePath)
	if err != nil {
		return fmt.Errorf("read sandbox TLS certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("read sandbox TLS private key: %w", err)
	}
	if digest.Bytes(caPEM) != lock.TLS.RootCertificateDigest || digest.Bytes(certificatePEM) != lock.TLS.ServerCertificateDigest || digest.Bytes(keyPEM) != lock.TLS.ServerPrivateKeyDigest {
		return errors.New("sandbox TLS material digest does not match the application lock")
	}
	pair, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		return fmt.Errorf("verify sandbox TLS keypair: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}
	caBlock, _ := pem.Decode(caPEM)
	if caBlock == nil || caBlock.Type != "CERTIFICATE" {
		return errors.New("sandbox CA PEM is invalid")
	}
	ca, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	for _, hostname := range expectedHostnames {
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: hostname, Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			return fmt.Errorf("verify sandbox TLS hostname %s: %w", hostname, err)
		}
	}
	return nil
}

// InstallSystemCA appends the generated public CA to a recognized system PEM
// bundle while building the wrapper image. Runtime trust variables remain the
// portable fallback for images without one of these stores.
func InstallSystemCA(caPath string) error {
	ca, err := os.ReadFile(caPath)
	if err != nil {
		return err
	}
	if block, rest := pem.Decode(ca); block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return errors.New("CA file must contain exactly one PEM certificate")
	}
	for _, candidate := range []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/ca-bundle.pem"} {
		info, statErr := os.Stat(candidate)
		if statErr != nil || !info.Mode().IsRegular() {
			continue
		}
		existing, readErr := os.ReadFile(candidate)
		if readErr != nil {
			return readErr
		}
		if bytes.Contains(existing, bytes.TrimSpace(ca)) {
			return nil
		}
		combined := append(bytes.TrimRight(existing, "\n"), '\n')
		combined = append(combined, ca...)
		return os.WriteFile(candidate, combined, info.Mode().Perm())
	}
	return nil
}
