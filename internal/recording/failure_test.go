package recording

import (
	"errors"
	"os"
	"testing"
)

type failingFile struct {
	file            *os.File
	short, syncFail bool
	writes          int
}

func (f *failingFile) Write(b []byte) (int, error) {
	f.writes++
	if f.short {
		return f.file.Write(b[:len(b)/2])
	}
	return f.file.Write(b)
}
func (f *failingFile) Sync() error {
	if f.syncFail {
		return errors.New("test sync failure")
	}
	return f.file.Sync()
}
func (f *failingFile) Close() error { return f.file.Close() }
func TestFailedWriteOrFlushPoisonsWriter(t *testing.T) {
	for _, short := range []bool{true, false} {
		t.Run(map[bool]string{true: "short", false: "flush"}[short], func(t *testing.T) {
			path := t.TempDir() + "/events.jsonl"
			w, err := Open(path, "s")
			if err != nil {
				t.Fatal(err)
			}
			f := &failingFile{file: w.file.(*os.File), short: short, syncFail: !short}
			w.file = f
			defer w.Close()
			d := Draft{Channel: Control, Layer: "test", Kind: "test", Provenance: SandboxControl, Semantic: map[string]any{}}
			_, err = w.Append(d)
			if !short {
				err = w.Flush()
			}
			if err == nil || w.Err() == nil {
				t.Fatal("failure not latched")
			}
			writes := f.writes
			f.short = false
			f.syncFail = false
			if _, err := w.Append(d); err == nil || f.writes != writes {
				t.Fatal("append continued after lost evidence")
			}
			if short {
				if _, err := VerifyFile(path); err == nil {
					t.Fatal("partial record accepted")
				}
			}
		})
	}
}
