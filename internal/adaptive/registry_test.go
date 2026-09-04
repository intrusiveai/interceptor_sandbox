package adaptive

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/evaluation"
	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
)

func TestArtifactAndAttemptRegistriesEnforceIdentityAndLineage(t *testing.T) {
	root := t.TempDir()
	store, err := blobstore.Open(filepath.Join(root, "blobs"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := OpenArtifactRegistry(filepath.Join(root, "artifacts.json"), store, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"message":"hello"}`)
	descriptor := ArtifactDescriptor{Digest: digest.Bytes(body), SizeBytes: int64(len(body)), MediaType: "application/json", Canonicalization: "raw"}
	if _, err := artifacts.Register(context.Background(), descriptor, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	attempts, err := OpenAttemptRegistry(filepath.Join(root, "attempts.json"), "black-box", 4, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	rootAttempt := AttemptContext{APIVersion: AttemptContextVersion, CampaignID: "campaign-1", ThreadID: "thread-1", AttemptID: "attempt-1", Generation: 1, AttemptIndex: 1, Payload: descriptor, Generator: Generator{Kind: "external-harness"}, FeedbackProfile: "black-box", CreatedAt: time.Now()}
	registered, err := attempts.Register(rootAttempt)
	if err != nil {
		t.Fatal(err)
	}
	child := rootAttempt
	child.AttemptID = "attempt-2"
	child.ParentAttemptID = registered.AttemptID
	child.Generation = 2
	child.AttemptIndex = 2
	child.CreatedAt = time.Now()
	if _, err := attempts.Register(child); err != nil {
		t.Fatal(err)
	}
	child.AttemptID = "attempt-3"
	child.ParentAttemptID = "missing"
	if _, err := attempts.Register(child); err == nil {
		t.Fatal("expected unknown parent rejection")
	}
	invalid := rootAttempt
	invalid.AttemptID = "attempt-missing-generator"
	invalid.AttemptIndex = 3
	invalid.Generator = Generator{}
	if _, err := attempts.Register(invalid); err == nil {
		t.Fatal("expected missing generator identity rejection")
	}
}

func TestArtifactRegistryRejectsPersistedSizeThatDisagreesWithBlob(t *testing.T) {
	root := t.TempDir()
	store, err := blobstore.Open(filepath.Join(root, "blobs"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("payload")
	object, err := store.Put(t.Context(), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "artifacts.json")
	item := ArtifactDescriptor{Digest: object.Digest, SizeBytes: object.Size + 1, MediaType: "text/plain", Canonicalization: "raw"}
	if err := fsutil.WriteJSON(path, artifactFile{SchemaVersion: 1, Artifacts: []ArtifactDescriptor{item}}, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenArtifactRegistry(path, store, 1<<20); err == nil {
		t.Fatal("expected persisted descriptor/blob size mismatch to fail")
	}
}

func TestCapabilityManifestIsDeterministicAndSecretFree(t *testing.T) {
	blueprint := &environment.Blueprint{Metadata: environment.Metadata{Name: "target", Version: "1"}, Application: environment.Application{Interface: environment.ApplicationInterface{Adapter: "http", Operations: []environment.ApplicationOperation{{ID: "chat", Method: "POST", Path: "/chat"}}}}, Network: environment.NetworkSimulation{DNS: environment.DNSConfig{Enabled: true, UnknownPolicy: "nxdomain"}, HTTP: environment.FrontDoorConfig{Enabled: true}, TLS: environment.TLSConfig{Enabled: true, TrustMode: "auto"}}, Filesystem: environment.Filesystem{InjectionNamespaces: []environment.FileInjectionNamespace{{ID: "inbox", Root: "/secret/container/path", AllowCreate: true, MaxFiles: 5, MaxFileBytes: 1024}}}, Services: []environment.Service{{ID: "tickets", Kind: "stateful_http", Injections: environment.ServiceInjectionPolicy{CreateCollections: []environment.ResourceCreationPolicy{{Collection: "tickets", MaxResources: 20}}}}}, AttackerServices: []environment.AttackerService{{ID: "exfil", Kind: "http_sink", Response: environment.SinkResponse{Mode: "static"}}, {ID: "c2", Kind: "http_sink", Response: environment.SinkResponse{Mode: "dynamic"}}}, Credentials: []environment.Credential{{ID: "synthetic", Value: "do-not-export"}}, Adaptive: environment.AdaptiveConfig{FeedbackProfiles: []string{"diagnostic", "black-box"}, MaxAttempts: 10, MaxArtifacts: 20}, Limits: environment.Limits{MaxTurns: 4, MaxSnapshots: 2, MaxBlobBytes: 1024}}
	binding := digest.Bytes([]byte("binding"))
	first, err := BuildCapabilityManifest(blueprint, binding, binding)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := BuildCapabilityManifest(blueprint, binding, binding)
	encoded, _ := json.Marshal(first)
	if first.Digest != second.Digest || bytes.Contains(encoded, []byte("do-not-export")) || bytes.Contains(encoded, []byte("/secret/container/path")) {
		t.Fatalf("manifest is unstable or contains secret material: %s", encoded)
	}
	if len(first.FileNamespaces) != 1 || first.FileNamespaces[0].ID != "inbox" || len(first.Services) != 3 || first.Services[0].ID != "c2" || len(first.Services[0].InjectionSurfaces) != 1 || first.Services[1].ID != "exfil" || len(first.Services[1].InjectionSurfaces) != 0 {
		t.Fatalf("manifest did not distinguish per-target injection capabilities: %#v", first)
	}
	for _, service := range first.Services {
		if !containsString(service.Transports, "http") || !containsString(service.Transports, "https") {
			t.Fatalf("service transport capability is incomplete: %#v", service)
		}
	}
	if !containsString(first.CrossVMOperations, "injection.delete") || len(first.InjectionProfiles) != len(first.InjectionSurfaces) {
		t.Fatalf("manifest omitted executable gateway or injection contract details: %#v", first)
	}
	var responseProfile *InjectionProfile
	for index := range first.InjectionProfiles {
		if first.InjectionProfiles[index].Surface == "service_response" {
			responseProfile = &first.InjectionProfiles[index]
		}
	}
	if responseProfile == nil || !containsString(responseProfile.SelectorFields, "request_body_json_pointer") || !containsString(responseProfile.Carriers, "dynamic_attacker_sink_response") {
		t.Fatalf("service response profile is incomplete: %#v", responseProfile)
	}
	if !first.Network.DNS.Enabled || first.Network.DNS.Upstream || !first.Network.HTTP || !first.Network.HTTPS.Enabled || first.Network.HTTPS.MinimumVersion != "TLS1.2" {
		t.Fatalf("network simulation capability is incomplete: %#v", first.Network)
	}
}

func containsString(items []string, wanted string) bool {
	for _, item := range items {
		if item == wanted {
			return true
		}
	}
	return false
}

func TestCanonicalArtifactUsesRFC8785NumberAndKeyRules(t *testing.T) {
	got, err := canonicalArtifact([]byte(`{"numbers":[333333333.33333329,1E30,4.50,2e-3],"z":0,"a":1}`), "jcs-v1")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":1,"numbers":[333333333.3333333,1e+30,4.5,0.002],"z":0}`
	if string(got) != want {
		t.Fatalf("canonical artifact = %s, want %s", got, want)
	}
}

func TestEvidenceBundleCarriesFinalEvaluationCompleteness(t *testing.T) {
	binding := digest.Bytes([]byte("binding"))
	metadata := session.Metadata{ID: "session-1", EnvironmentDigest: binding, AppDigest: binding, CapabilityManifestDigest: binding, FeedbackProfile: "black-box", UpdatedAt: time.Now()}
	report := evaluation.Report{Oracles: []evaluation.OracleResult{{ID: "oracle-1", Kind: "event_match", Fired: true}}, Injections: []evaluation.InjectionResult{{ID: "injection-1", Outcome: "DELIVERED_SUCCEEDED"}}, Coverage: []string{"event:2"}}
	bundle, err := BuildEvidenceBundle(metadata, nil, nil, nil, nil, report, true)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Completeness.State != "complete" || bundle.Completeness.Truncated || len(bundle.Oracles) != 1 || len(bundle.Injections) != 1 || len(bundle.CoverageGaps) != 1 {
		t.Fatalf("final evaluation was not embedded: %#v", bundle)
	}
	partial, err := BuildEvidenceBundle(metadata, nil, nil, nil, nil, evaluation.Report{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if partial.Completeness.State != "partial" || !partial.Completeness.Truncated {
		t.Fatalf("missing final evaluation was not marked partial: %#v", partial.Completeness)
	}
}
