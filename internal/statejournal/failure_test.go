package statejournal

import (
	"errors"
	"os"
	"testing"
)

type failingJournalFile struct {
	file  *os.File
	short bool
}

func (f failingJournalFile) Write(b []byte) (int, error) {
	if f.short {
		return f.file.Write(b[:len(b)/2])
	}
	return f.file.Write(b)
}
func (f failingJournalFile) Sync() error  { return errors.New("test sync failure") }
func (f failingJournalFile) Close() error { return f.file.Close() }
func TestJournalFailureLatchesAndNotifies(t *testing.T) {
	for _, short := range []bool{true, false} {
		path := t.TempDir() + "/state.jsonl"
		w, err := Open(path, "s")
		if err != nil {
			t.Fatal(err)
		}
		w.file = failingJournalFile{w.file.(*os.File), short}
		defer w.Close()
		var reported error
		w.SetFailureHandler(func(err error) { reported = err })
		op := []Operation{{Kind: "file_delete", Path: "/state/old"}}
		if _, err := w.Append("delete", op); err == nil || reported == nil {
			t.Fatal("failure ignored")
		}
		before, _ := os.ReadFile(path)
		if _, err := w.Append("retry", op); err == nil {
			t.Fatal("retry admitted")
		}
		after, _ := os.ReadFile(path)
		if string(before) != string(after) {
			t.Fatal("journal changed after failure")
		}
	}
}
