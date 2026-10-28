package environment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPostgreSQLBlueprintAndSealedSeeds(t *testing.T) {
	for _, language := range []string{"python", "node"} {
		t.Run(language, func(t *testing.T) {
			root := t.TempDir()
			if err := os.CopyFS(root, os.DirFS("../../examples/postgresql/"+language+"/environment")); err != nil {
				t.Fatal(err)
			}
			report, err := Validate(root)
			if err != nil {
				t.Fatal(err)
			}
			if report.Blueprint.Services[0].PostgreSQL.MaxConnections != 16 {
				t.Fatal("database defaults absent")
			}
			if _, _, err = Seal(root, time.Now()); err != nil {
				t.Fatal(err)
			}
			if _, _, err = Verify(root); err != nil {
				t.Fatal(err)
			}
			seed := filepath.Join(root, "assets/seed.sql")
			if err = os.WriteFile(seed, []byte("SELECT 1"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err = Verify(root); err == nil {
				t.Fatal("changed seed accepted")
			}
		})
	}
	for _, replacement := range []string{"schema: [/outside.sql]", "schema: [../outside.sql]", "unknown_database_field: true"} {
		root := t.TempDir()
		if err := os.CopyFS(root, os.DirFS("../../examples/postgresql/python/environment")); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, BlueprintFile)
		raw, _ := os.ReadFile(path)
		text := strings.Replace(string(raw), "schema: [assets/schema.sql]", replacement, 1)
		os.WriteFile(path, []byte(text), 0600)
		if _, err := Validate(root); err == nil {
			t.Fatalf("accepted %s", replacement)
		}
	}
}
