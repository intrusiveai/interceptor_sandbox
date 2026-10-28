package appimage

import (
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDatabaseBuildIncludesPinnedInputsAndNotices(t *testing.T) {
	dir := t.TempDir()
	b := &environment.Blueprint{}
	stage, copy, err := databaseBuild(b, dir)
	if err != nil || stage != "" || copy != "" {
		t.Fatal("database added to unrelated wrapper")
	}
	b.Services = []environment.Service{{Kind: "postgresql"}}
	stage, copy, err = databaseBuild(b, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"sha256sum -c", "postgresql-18.6", "pgvector-0.8.7", "GPL3.txt", "GCC-runtime.txt", "source.tar.bz2", "source.tar.gz", "musl.txt"} {
		if !strings.Contains(stage, required) {
			t.Fatalf("missing package input/notice %s", required)
		}
	}
	if !strings.Contains(copy, "/opt/interceptor/postgresql") {
		t.Fatal("runtime not copied")
	}
	for _, name := range []string{"musl.LICENSE", "GPL3.LICENSE", "GCC-runtime.LICENSE"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || len(raw) < 100 {
			t.Fatal(name, err)
		}
	}
}
