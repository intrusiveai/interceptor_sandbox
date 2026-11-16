package evaluation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
)

func fileOracleInput(root string, match map[string]any) Input {
	match["path"] = "/state/app/private/value"
	return Input{Blueprint: &environment.Blueprint{Limits: environment.Limits{MaxBlobBytes: 1024}, Oracles: []environment.Oracle{{ID: "file", Kind: "file_state_match", Match: match}}}, MutableRoot: []snapshot.Root{{ContainerPath: "/state/app", HostPath: root}}}
}

func TestFileOracleDoesNotTreatInaccessibleAsAbsent(t *testing.T) {
	for _, predicate := range []map[string]any{{"absent": true}, {"exists": false}, {"exists": true}, {"content_contains": "value"}, {"digest": digest.Bytes([]byte("value"))}} {
		input := fileOracleInput(t.TempDir(), predicate)
		for _, cause := range []error{os.ErrPermission, errors.New("helper unavailable"), context.Canceled} {
			input.ReadMutable = func(context.Context, snapshot.ReadRequest) (snapshot.ReadResponse, error) {
				return snapshot.ReadResponse{}, &os.PathError{Op: "stat", Path: "/state/app/private/value", Err: cause}
			}
			if _, err := Evaluate(input); !errors.Is(err, cause) {
				t.Fatalf("inaccessible state produced a verdict for %v: %v", predicate, err)
			}
		}
		input.ReadMutable = func(context.Context, snapshot.ReadRequest) (snapshot.ReadResponse, error) {
			return snapshot.ReadResponse{}, os.ErrNotExist
		}
		report, err := Evaluate(input)
		if err != nil {
			t.Fatal(err)
		}
		want := predicate["absent"] == true || predicate["exists"] == false
		if report.Oracles[0].Fired != want {
			t.Fatal("genuine absence misclassified", predicate, report)
		}
	}
}

func TestFileOracleRestrictedReads(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "private"), 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "private/value")
	if err := os.WriteFile(file, []byte("private-value"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, match := range []map[string]any{{"exists": true}, {"content_contains": "private-value"}, {"digest": digest.Bytes([]byte("private-value"))}} {
		input := fileOracleInput(root, match)
		report, err := Evaluate(input)
		if err != nil || !report.Oracles[0].Fired {
			t.Fatal("file read failed", report, err)
		}
	}
	input := fileOracleInput(root, map[string]any{"content_contains": "value"})
	input.Blueprint.Limits.MaxBlobBytes = 1
	if _, err := Evaluate(input); err == nil {
		t.Fatal("oversized read accepted")
	}
	input.Blueprint.Limits.MaxBlobBytes = 1024
	read := snapshot.RootReader(input.MutableRoot)
	input.ReadMutable = func(ctx context.Context, q snapshot.ReadRequest) (snapshot.ReadResponse, error) {
		if q.Kind == "read" {
			return snapshot.ReadResponse{}, os.ErrPermission
		}
		return read(ctx, q)
	}
	if _, err := Evaluate(input); !errors.Is(err, os.ErrPermission) {
		t.Fatal("unreadable content became a negative match", err)
	}
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Evaluate(input); !errors.Is(err, os.ErrPermission) {
		t.Fatal("unreadable empty file produced a verdict", err)
	}
	input.ReadMutable = nil
	for _, path := range []string{"/etc/passwd", "/state/app/../outside"} {
		input.Blueprint.Oracles[0].Match["path"] = path
		if _, err := Evaluate(input); err == nil {
			t.Fatal("outside-root oracle accepted", path)
		}
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	input.Blueprint.Oracles[0].Match = map[string]any{"path": "/state/app/escape/missing", "absent": true}
	if _, err := Evaluate(input); err == nil {
		t.Fatal("symlink traversal became an absence verdict")
	}
}
