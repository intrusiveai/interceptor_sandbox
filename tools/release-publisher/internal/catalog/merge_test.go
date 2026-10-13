package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func catalogEntry(version, tag, digest string) []byte {
	return []byte(fmt.Sprintf(`{"version":%q,"latest":true,"image":%q,"tags":[%q],"digest":%q}`,
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
				raw, err := json.Marshal(Entry{Version: version, Tags: []string{tag}, Artifacts: []Artifact{
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
