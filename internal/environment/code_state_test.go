package environment

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCodeStateValidationAndSeal(t *testing.T) {
	for _, kind := range []string{"valid", "missing-save", "missing-restore", "disabled", "concurrency", "outside", "traversal", "code", "injection", "seed", "root"} {
		t.Run(kind, func(t *testing.T) {
			root := adapterBlueprint(t, "python", "adapter")
			if err := os.WriteFile(filepath.Join(root, "assets/adapter/adapter.py"), []byte("# adapter"), 0644); err != nil {
				t.Fatal(err)
			}
			b, err := Load(root)
			if err != nil {
				t.Fatal(err)
			}
			b.Application.Persistence = Persistence{SnapshotCapable: true, MutableRoots: []string{"/app-state"}, Quiesce: QuiesceConfig{Kind: "none"}}
			c := b.Application.Interface.Code
			c.State = &CodeState{Save: "save_state", Restore: "restore_state", Path: "/app-state/agent.json"}
			switch kind {
			case "missing-save":
				c.State.Save = ""
			case "missing-restore":
				c.State.Restore = ""
			case "disabled":
				b.Application.Persistence.SnapshotCapable = false
			case "concurrency":
				c.Concurrency = 2
			case "outside":
				c.State.Path = "/other/state.json"
			case "traversal":
				c.State.Path = "/app-state/../app-state/agent.json"
			case "code":
				c.State.Path = b.Application.Launch.Workdir + "/agent.json"
			case "injection":
				b.Filesystem.InjectionNamespaces = []FileInjectionNamespace{{ID: "state", Root: "/app-state"}}
			case "seed":
				b.Filesystem.Seed = []SeedEntry{{Target: "/app-state/agent.json"}}
			case "root":
				c.State.Path = "/app-state"
			}
			if err := validateCodeState(b); (err == nil) != (kind == "valid") {
				t.Fatalf("validation: %v", err)
			}
			if kind != "valid" {
				return
			}
			b.Filesystem.Seed = nil
			data, _ := json.Marshal(b)
			if err := os.WriteFile(filepath.Join(root, BlueprintFile), data, 0600); err != nil {
				t.Fatal(err)
			}
			first, _, err := Seal(root, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			c.State.Save = "save_other"
			data, _ = json.Marshal(b)
			os.WriteFile(filepath.Join(root, BlueprintFile), data, 0600)
			if _, _, err := Verify(root); err == nil {
				t.Fatal("state config change did not invalidate seal")
			}
			second, _, err := Seal(root, time.Now())
			if err != nil || first.EnvironmentDigest == second.EnvironmentDigest {
				t.Fatalf("state not bound to seal: %v", err)
			}
		})
	}
}
