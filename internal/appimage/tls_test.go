package appimage

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
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
	caBlock, _ := pem.Decode(material.ca)
	ca, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	leafBlock, _ := pem.Decode(material.certificate)
	leaf, err := x509.ParseCertificate(leafBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(ca.SubjectKeyId) == 0 || !bytes.Equal(leaf.AuthorityKeyId, ca.SubjectKeyId) {
		t.Fatal("server authority key identifier must identify the generated CA")
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

func TestGeneratedTLSMaterialPythonStrictVerification(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python is required to exercise OpenSSL strict certificate verification")
	}
	material, err := generateTLSMaterial(time.Now().UTC(), []string{"api.sandbox.local"})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for name, data := range map[string][]byte{"ca.pem": material.ca, "server.pem": material.certificate, "key.pem": material.privateKey} {
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Memory BIOs exercise a real TLS handshake without opening a listener.
	// Enable strict verification on older CI Pythons as well as 3.13+ defaults.
	const handshake = `
import ssl, sys
from pathlib import Path
root = Path(sys.argv[1])
client_context = ssl.create_default_context(cafile=str(root / 'ca.pem'))
client_context.verify_flags |= ssl.VERIFY_X509_STRICT
server_context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
server_context.load_cert_chain(root / 'server.pem', root / 'key.pem')
ci, co, si, so = (ssl.MemoryBIO() for _ in range(4))
client = client_context.wrap_bio(ci, co, server_hostname='api.sandbox.local')
server = server_context.wrap_bio(si, so, server_side=True)
complete = set()
for _ in range(20):
    for peer in (client, server):
        try:
            peer.do_handshake()
            complete.add(peer)
        except ssl.SSLWantReadError:
            pass
    if co.pending: si.write(co.read())
    if so.pending: ci.write(so.read())
    if len(complete) == 2:
        break
else:
    raise AssertionError('TLS handshake did not complete')
print('Verified TLS with strict certificate and hostname checks on Python', sys.version.split()[0])
`
	output, err := exec.Command(python, "-c", handshake, root).CombinedOutput()
	if err != nil {
		t.Fatalf("strict Python TLS handshake: %v\n%s", err, output)
	}
	t.Log(string(output))
}
