package appimage

import (
	"os"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
)

func TestGeneratedTLSMaterialIsBoundAndVerifiable(t *testing.T) {
	blueprint := &environment.Blueprint{
		Metadata:         environment.Metadata{Name: "test", Version: "1"},
		Network:          environment.NetworkSimulation{TLS: environment.TLSConfig{Enabled: true, TrustMode: "auto"}},
		Services:         []environment.Service{{ID: "one", Hostnames: []string{"api.sandbox.local"}}},
		AttackerServices: []environment.AttackerService{{ID: "two", Hostnames: []string{"c2.sandbox.local"}}},
	}
	hostnames := simulatedHostnames(blueprint)
	material, err := generateTLSMaterial(time.Now().UTC(), hostnames)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := persistTLSMaterial(root, blueprint, material); err != nil {
		t.Fatal(err)
	}
	lock := Lock{TLS: &TLSLock{TrustMode: "auto", Hostnames: hostnames, RootCertificateDigest: digest.Bytes(material.ca), ServerCertificateDigest: digest.Bytes(material.certificate), ServerPrivateKeyDigest: digest.Bytes(material.privateKey)}}
	if err := VerifyTLSMaterial(root, blueprint, lock); err != nil {
		t.Fatalf("generated TLS material did not verify: %v", err)
	}
	_, _, keyPath := TLSMaterialFiles(root, blueprint)
	if err := os.WriteFile(keyPath, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyTLSMaterial(root, blueprint, lock); err == nil {
		t.Fatal("modified TLS private key was accepted")
	}
}
