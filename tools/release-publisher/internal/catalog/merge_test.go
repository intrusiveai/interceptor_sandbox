package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func catalogEntry(version, tag, digest string) []byte {
	return []byte(fmt.Sprintf(`{"contract_package_version":"1.3.0","version":%q,"latest":true,"image":%q,"tags":[%q],"digest":%q}`,
		version, "ghcr.io/intrusiveai/attack_harness:"+version, tag, "sha256:"+strings.Repeat(digest, 64)))
}

func TestCatalogCandidatePromotionAndRetries(t *testing.T) {
	candidate := catalogEntry("v1.0.0", "rc", "a")
	raw, err := MergeCatalogEntry("attack_harness", nil, candidate)
	if err != nil {
		t.Fatal(err)
	}
	// A newer candidate can arrive before an older version is promoted.
	raw, err = MergeCatalogEntry("attack_harness", raw, catalogEntry("v2.0.0", "rc", "b"))
	if err != nil {
		t.Fatal(err)
	}
	promoted, err := MergeCatalogEntry("attack_harness", raw, catalogEntry("v1.0.0", "release", "a"))
	if err != nil {
		t.Fatal(err)
	}
	var index Index
	if err := json.Unmarshal(promoted, &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Releases) != 2 || index.Releases[0].Version != "v2.0.0" || !index.Releases[0].Latest || index.Releases[1].Latest {
		t.Fatal("promotion reordered or lost another candidate")
	}
	again, err := MergeCatalogEntry("attack_harness", promoted, catalogEntry("v1.0.0", "release", "a"))
	if err != nil || !bytes.Equal(again, promoted) {
		t.Fatal("retry was not idempotent", err)
	}
	for _, rejected := range [][]byte{candidate, catalogEntry("v1.0.0", "release", "b")} {
		if _, err := MergeCatalogEntry("attack_harness", promoted, rejected); err == nil {
			t.Fatal("allowed demotion or replacement of a final image")
		}
	}
}

func TestCatalogRejectsBadEntriesAndChangedPromotion(t *testing.T) {
	valid := catalogEntry("v1.0.0", "rc", "a")
	for _, raw := range [][]byte{
		[]byte(`null`), []byte(`{}`), append(append([]byte{}, valid...), []byte(` {}`)...),
		bytes.Replace(valid, []byte(`"latest"`), []byte(`"unexpected"`), 1),
		bytes.Replace(valid, []byte("intrusiveai/attack_harness"), []byte("other/image"), 1),
		bytes.Replace(valid, []byte("sha256:"+strings.Repeat("a", 64)), []byte("bad"), 1),
	} {
		if _, err := MergeCatalogEntry("attack_harness", nil, raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, badCatalog := range []string{`null`, `{}`, `{"schema_version":1,"product":"attack_harness","kind":"oci_image","releases":null}`} {
		if _, err := MergeCatalogEntry("attack_harness", []byte(badCatalog), valid); err == nil {
			t.Fatal("reset malformed catalog")
		}
	}
	current, _ := MergeCatalogEntry("attack_harness", nil, valid)
	if _, err := MergeCatalogEntry("attack_harness", current, catalogEntry("v1.0.0", "release", "b")); err == nil {
		t.Fatal("promoted different candidate bytes")
	}
	if _, err := MergeCatalogEntry("attack_harness", []byte(`{"schema_version":99}`), valid); err == nil {
		t.Fatal("replaced corrupt existing catalog")
	}
	// Candidate rebuilds update the candidate entry until a final image freezes it.
	if _, err := MergeCatalogEntry("attack_harness", current, catalogEntry("v1.0.0", "rc", "b")); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadCatalogLifecycle(t *testing.T) {
	for _, product := range []string{"operator_sandbox", "interceptor_sandbox"} {
		t.Run(product, func(t *testing.T) {
			entry := func(version, tag, digest string) []byte {
				contract := catalogContractVersion(product)
				raw, err := json.Marshal(Entry{InterceptorContractVersion: "2.0.0", ContractPackageVersion: contract, Version: version, Tags: []string{tag}, Artifacts: []Artifact{
					{Platform: "linux/amd64", Filename: "product.tar.gz", URL: "https://github.com/intrusiveai/" + product + "/releases/download/" + version + "/product.tar.gz", SHA256: strings.Repeat(digest, 64)},
				}})
				if err != nil {
					t.Fatal(err)
				}
				return raw
			}
			var current []byte
			for _, version := range []string{"v1.0.0", "v2.0.0", "v3.0.0"} {
				var err error
				current, err = MergeCatalogEntry(product, current, entry(version, "rc", "a"))
				if err != nil {
					t.Fatal(err)
				}
			}
			var candidate Index
			json.Unmarshal(current, &candidate)
			// The catalog retains history while the website shows only two final versions.
			for _, version := range []string{"v1.0.0", "v2.0.0", "v3.0.0"} {
				var err error
				current, err = MergeCatalogEntry(product, current, entry(version, "release", "a"))
				if err != nil {
					t.Fatal(err)
				}
			}
			json.Unmarshal(current, &candidate)
			if len(candidate.Releases) != 3 {
				t.Fatal("lost catalog history")
			}
			for _, bad := range [][]byte{entry("v3.0.0", "rc", "a"), entry("v3.0.0", "release", "b")} {
				if _, err := MergeCatalogEntry(product, current, bad); err == nil {
					t.Fatal("changed final artifacts or demoted release")
				}
			}
			next, err := MergeCatalogEntry(product, current, entry("v4.0.0", "rc", "a"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := MergeCatalogEntry(product, next, entry("v4.0.0", "release", "b")); err == nil {
				t.Fatal("promoted different bytes")
			}
			if _, err := MergeCatalogEntry(product, nil, bytes.ReplaceAll(entry("v4.0.0", "rc", "a"), []byte("intrusiveai/"+product), []byte("intrusiveai/attack_harness"))); err == nil {
				t.Fatal("accepted foreign asset URL")
			}
			if _, err := MergeCatalogEntry("attack_harness", current, catalogEntry("v1.0.0", "rc", "a")); err == nil {
				t.Fatal("accepted another product's catalog")
			}
		})
	}
	if _, err := MergeCatalogEntry("../other", nil, catalogEntry("v1.0.0", "rc", "a")); err == nil {
		t.Fatal("accepted unsupported product")
	}
}

func catalogContractVersion(product string) ContractVersion {
	if product == "operator_sandbox" {
		return "1.3.0"
	}
	return ""
}

func TestSharedContractPublication(t *testing.T) {
	for _, product := range []string{"operator_sandbox", "attack_harness", "interceptor_sandbox"} {
		t.Run(product, func(t *testing.T) {
			entry := func(contract, tag, digest string) []byte {
				e := Entry{Version: "v2.7.0", Tags: []string{tag}, ContractPackageVersion: ContractVersion(contract)}
				if product != "attack_harness" {
					e.InterceptorContractVersion = "2.0.0"
				}
				if product == "attack_harness" {
					e.Image = "ghcr.io/intrusiveai/attack_harness:v2.7.0"
					e.Digest = "sha256:" + strings.Repeat(digest, 64)
				} else {
					e.Artifacts = []Artifact{{Platform: "linux/amd64", Filename: "product.tar.gz", URL: "https://github.com/intrusiveai/" + product + "/releases/download/v2.7.0/product.tar.gz", SHA256: strings.Repeat(digest, 64)}}
				}
				raw, _ := json.Marshal(e)
				return raw
			}
			valid := entry("1.3.0", "rc", "a")
			if product == "interceptor_sandbox" {
				if _, err := MergeCatalogEntry(product, nil, valid); err == nil {
					t.Fatal("accepted irrelevant contract metadata")
				}
				return
			}
			for _, version := range []string{"", "v1.3.0", "^1.3.0", "1.3", "01.3.0", "1.3.0-rc.1", "1.3.0+build", "1.3.0\n", strings.Repeat("1", 129) + ".0.0"} {
				if _, err := MergeCatalogEntry(product, nil, entry(version, "rc", "a")); err == nil {
					t.Fatalf("accepted contract %q", version)
				}
			}
			for _, value := range []string{`null`, `123`, `""`} {
				bad := bytes.Replace(valid, []byte(`"1.3.0"`), []byte(value), 1)
				if _, err := MergeCatalogEntry(product, nil, bad); err == nil {
					t.Fatalf("accepted %s", bad)
				}
			}
			current, err := MergeCatalogEntry(product, nil, valid)
			if err != nil {
				t.Fatal(err)
			}
			for _, bad := range [][]byte{entry("1.4.0", "rc", "a"), entry("1.4.0", "release", "a"), entry("1.4.0", "release", "b")} {
				if _, err := MergeCatalogEntry(product, current, bad); err == nil {
					t.Fatal("changed embedded contract without a candidate rebuild")
				}
			}
			rebuilt, err := MergeCatalogEntry(product, current, entry("1.4.0", "rc", "b"))
			if err != nil {
				t.Fatal(err)
			}
			final, err := MergeCatalogEntry(product, rebuilt, entry("1.4.0", "release", "b"))
			if err != nil {
				t.Fatal(err)
			}
			for _, bad := range [][]byte{entry("1.3.0", "release", "b"), entry("1.4.0", "rc", "b"), entry("1.4.0", "release", "c")} {
				if _, err := MergeCatalogEntry(product, final, bad); err == nil {
					t.Fatal("changed a final release")
				}
			}
			// A final entry may acquire missing metadata once without changing visibility.
			var oldFinal Index
			json.Unmarshal(final, &oldFinal)
			oldFinal.Releases[0].ContractPackageVersion = ""
			oldFinalJSON, _ := json.Marshal(oldFinal)
			completed, err := MergeCatalogEntry(product, oldFinalJSON, entry("1.4.0", "release", "b"))
			if err != nil || !bytes.Equal(completed, final) {
				t.Fatal("could not complete final metadata without changing identity", err)
			}
			// Missing historical metadata stays readable when a new candidate is added.
			var old Index
			json.Unmarshal(current, &old)
			old.Releases[0].ContractPackageVersion = ""
			old.Releases[0].Tags = nil // Preserve a deliberately hidden release.
			oldJSON, _ := json.Marshal(old)
			newEntry := bytes.ReplaceAll(valid, []byte("v2.7.0"), []byte("v2.8.0"))
			withNew, err := MergeCatalogEntry(product, oldJSON, newEntry)
			if err != nil {
				t.Fatal(err)
			}
			var result Index
			json.Unmarshal(withNew, &result)
			if len(result.Releases) != 2 || result.Releases[1].ContractPackageVersion != "" || len(result.Releases[1].Tags) != 0 {
				t.Fatal("rewrote unrelated history")
			}
			if _, err := MergeCatalogEntry(product, oldJSON, entry("1.3.0", "release", "a")); err == nil {
				t.Fatal("combined missing metadata with promotion")
			}
			var filled Entry
			json.Unmarshal(valid, &filled)
			filled.Tags = nil
			fillJSON, _ := json.Marshal(filled)
			filledJSON, err := MergeCatalogEntry(product, oldJSON, fillJSON)
			if err != nil {
				t.Fatal(err)
			}
			result = Index{}
			json.Unmarshal(filledJSON, &result)
			if result.Releases[0].ContractPackageVersion != "1.3.0" || len(result.Releases[0].Tags) != 0 {
				t.Fatal("metadata completion changed visibility")
			}
		})
	}
}

func TestInterceptorContractPublication(t *testing.T) {
	for _, product := range []string{"operator_sandbox", "interceptor_sandbox"} {
		t.Run(product, func(t *testing.T) {
			entry := func(version, tag, digest string) []byte {
				e := Entry{Version: "v3.0.0", Tags: []string{tag}, InterceptorContractVersion: ContractVersion(version), ContractPackageVersion: catalogContractVersion(product), Artifacts: []Artifact{{Platform: "linux/amd64", Filename: "product.tar.gz", URL: "https://github.com/intrusiveai/" + product + "/releases/download/v3.0.0/product.tar.gz", SHA256: strings.Repeat(digest, 64)}}}
				raw, err := json.Marshal(e)
				if err != nil {
					t.Fatal(err)
				}
				return raw
			}
			valid := entry("2.0.0", "rc", "a")
			for _, version := range []string{"", "v2.0.0", "^2.0.0", "2.0", "02.0.0", "2.0.0-rc.1", "2.0.0+build", "2.0.0\n", strings.Repeat("1", 129) + ".0.0"} {
				if _, err := MergeCatalogEntry(product, nil, entry(version, "rc", "a")); err == nil {
					t.Fatalf("accepted %q", version)
				}
			}
			for _, value := range []string{`null`, `123`, `""`} {
				if _, err := MergeCatalogEntry(product, nil, bytes.Replace(valid, []byte(`"2.0.0"`), []byte(value), 1)); err == nil {
					t.Fatalf("accepted %s", value)
				}
			}
			current, err := MergeCatalogEntry(product, nil, valid)
			if err != nil {
				t.Fatal(err)
			}
			for _, bad := range [][]byte{entry("2.1.0", "rc", "a"), entry("2.1.0", "release", "a"), entry("2.1.0", "release", "b")} {
				if _, err := MergeCatalogEntry(product, current, bad); err == nil {
					t.Fatal("changed contract without candidate rebuild and qualification")
				}
			}
			rebuilt, err := MergeCatalogEntry(product, current, entry("2.1.0", "rc", "b"))
			if err != nil {
				t.Fatal(err)
			}
			final, err := MergeCatalogEntry(product, rebuilt, entry("2.1.0", "release", "b"))
			if err != nil {
				t.Fatal(err)
			}
			for _, bad := range [][]byte{entry("2.0.0", "release", "b"), entry("2.1.0", "rc", "b"), entry("2.1.0", "release", "c")} {
				if _, err := MergeCatalogEntry(product, final, bad); err == nil {
					t.Fatal("changed final release")
				}
			}
			var index Index
			json.Unmarshal(final, &index)
			if index.Releases[0].ContractPackageVersion != catalogContractVersion(product) {
				t.Fatal("rewrote the other contract")
			}
			// Existing entries remain readable; completing metadata preserves identity and labels.
			index.Releases[0].InterceptorContractVersion = ""
			oldFinal, _ := json.Marshal(index)
			completed, err := MergeCatalogEntry(product, oldFinal, entry("2.1.0", "release", "b"))
			if err != nil || !bytes.Equal(completed, final) {
				t.Fatal("metadata completion changed identity", err)
			}
			index.Releases[0].Tags = nil
			oldHidden, _ := json.Marshal(index)
			if _, err := MergeCatalogEntry(product, oldHidden, entry("2.1.0", "release", "b")); err == nil {
				t.Fatal("combined missing metadata with promotion")
			}
			next, err := MergeCatalogEntry(product, oldHidden, bytes.ReplaceAll(valid, []byte("v3.0.0"), []byte("v4.0.0")))
			if err != nil {
				t.Fatal(err)
			}
			index = Index{}
			json.Unmarshal(next, &index)
			if len(index.Releases) != 2 || index.Releases[1].InterceptorContractVersion != "" || len(index.Releases[1].Tags) != 0 {
				t.Fatal("rewrote hidden history")
			}
		})
	}
	bad := bytes.Replace(catalogEntry("v1.0.0", "rc", "a"), []byte(`"latest"`), []byte(`"interceptor_contract_version":"2.0.0","latest"`), 1)
	if _, err := MergeCatalogEntry("attack_harness", nil, bad); err == nil {
		t.Fatal("accepted Interceptor contract on Attack Harness")
	}
}
