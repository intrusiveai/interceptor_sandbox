package environment

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func adapterBlueprint(t *testing.T, language, module string) string {
	t.Helper()
	root := testBlueprint(t, "")
	b, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	b.Application.Interface = ApplicationInterface{Adapter: "code", StartupTimeoutMS: 30000, TurnTimeoutMS: 30000, Operations: []ApplicationOperation{{ID: "ask"}}, Code: &CodeInterface{Language: language, Module: module, AdapterAsset: "assets/adapter", Initialize: "initialize", Invoke: "invoke", Close: "close"}}
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, BlueprintFile), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "assets/adapter"), 0755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestAdapterSealAndDiscovery(t *testing.T) {
	for _, test := range []struct{ language, module, file string }{{"python", "adapter", "adapter.py"}, {"node", "./adapter.mjs", "adapter.mjs"}} {
		t.Run(test.language, func(t *testing.T) {
			root := adapterBlueprint(t, test.language, test.module)
			entry := filepath.Join(root, "assets/adapter", test.file)
			if err := os.WriteFile(entry, []byte("initial"), 0644); err != nil {
				t.Fatal(err)
			}
			t.Chdir(t.TempDir())
			first, _, err := Seal(root, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := Verify(root); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(entry, []byte("changed"), 0644); err != nil {
				t.Fatal(err)
			}
			if _, _, err := Verify(root); err == nil {
				t.Fatal("modified adapter accepted")
			}
			second, _, err := Seal(root, time.Now())
			if err != nil || second.EnvironmentDigest == first.EnvironmentDigest {
				t.Fatalf("adapter not bound to seal: %v", err)
			}
		})
	}
}

func TestAdapterValidation(t *testing.T) {
	for _, kind := range []string{"missing", "escape", "entry-escape", "symlink", "parent-link", "hardlink", "mount", "runtime-root", "missing-package"} {
		t.Run(kind, func(t *testing.T) {
			root := adapterBlueprint(t, "python", "adapter")
			entry := filepath.Join(root, "assets/adapter/adapter.py")
			if err := os.WriteFile(entry, []byte("source"), 0644); err != nil {
				t.Fatal(err)
			}
			b, err := Load(root)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "missing":
				b.Application.Interface.Code.Module = "missing"
			case "escape":
				b.Application.Interface.Code.AdapterAsset = "../outside"
			case "entry-escape":
				b.Application.Interface.Code.Language = "node"
				b.Application.Interface.Code.Module = "./../outside.mjs"
			case "symlink":
				err = os.Symlink(entry, filepath.Join(root, "assets/adapter/link"))
			case "parent-link":
				err = os.Symlink(filepath.Join(root, "assets"), filepath.Join(root, "linked"))
				b.Application.Interface.Code.AdapterAsset = "linked/adapter"
			case "hardlink":
				err = os.Link(entry, filepath.Join(root, "assets/adapter/linked.py"))
			case "mount":
				b.Application.Persistence.MutableRoots = []string{"/app"}
			case "runtime-root":
				b.Application.Launch.Workdir = "/tmp/app"
			case "missing-package":
				b.Application.Interface.Code.Module = "pkg.adapter"
				err = os.Mkdir(filepath.Join(root, "assets/adapter/pkg"), 0755)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := validateAdapter(root, b); err == nil {
				t.Fatal("unsafe/missing adapter accepted")
			}
		})
	}
}

func TestAdapterOptionalFieldsPreserveCanonicalJSON(t *testing.T) {
	code := CodeInterface{Language: "python", Module: "agent", Invoke: "invoke"}
	data, err := json.Marshal(code)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"language":"python","module":"agent","invoke":"invoke"}` {
		t.Fatalf("legacy serialization changed: %s", data)
	}
	if strings.Contains(string(data), "adapter_asset") {
		t.Fatal("empty adapter serialized")
	}
}
