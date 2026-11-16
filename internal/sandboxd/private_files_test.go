package sandboxd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
)

func TestRestoredPrivateModesAreNotWidened(t *testing.T) {
	for _, restored := range []bool{false, true} {
		root := t.TempDir()
		dir := filepath.Join(root, "private")
		file := filepath.Join(dir, "value")
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("private"), 0600); err != nil {
			t.Fatal(err)
		}
		blueprint := &environment.Blueprint{}
		blueprint.Application.Persistence.MutableRoots = []string{root}
		server := &Server{blueprint: blueprint, metadata: session.Metadata{}}
		if restored {
			server.metadata.ParentSessionID = "source-session"
		}
		if err := server.prepareMutableRoots(); err != nil {
			t.Fatal(err)
		}
		for path, want := range map[string]os.FileMode{dir: 0700, file: 0600} {
			if !restored {
				if path == dir {
					want = 0770
				} else {
					want = 0660
				}
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != want {
				t.Fatalf("restored=%v %s: %o != %o", restored, path, info.Mode().Perm(), want)
			}
		}
	}
}

func TestFileReaderHelperPreservesErrorClasses(t *testing.T) {
	for _, test := range []struct {
		code string
		want error
	}{{"not_found", os.ErrNotExist}, {"permission_denied", os.ErrPermission}, {"", nil}} {
		raw, _ := json.Marshal(HelperResponse{ID: "snapshot", OK: false, Error: "read failed", FileError: test.code})
		helper := &snapshotHelper{command: &exec.Cmd{}, input: writeCloser{&bytes.Buffer{}}, output: bufio.NewReader(bytes.NewReader(append(raw, '\n')))}
		_, err := helper.read(context.Background(), snapshot.ReadRequest{Path: "/state/app/private/missing", Kind: "stat"})
		if err == nil {
			t.Fatal("failed helper read accepted")
		}
		if test.want != nil && !errors.Is(err, test.want) {
			t.Fatal(test.code, err)
		}
		if test.want == nil && (errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission)) {
			t.Fatal("unknown error guessed", err)
		}
	}
}

type writeCloser struct{ *bytes.Buffer }

func (w writeCloser) Close() error { return nil }
