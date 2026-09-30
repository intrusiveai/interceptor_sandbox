package evidence

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestArchiveLimitIncludesPaddingAndTrailer(t *testing.T) {
	for _, maximum := range []int64{511, 1023, 2047, 2048} {
		var out bytes.Buffer
		w, err := NewWriter(t.Context(), &out, maximum)
		if err != nil {
			t.Fatal(err)
		}
		archive := tar.NewWriter(w)
		err = archive.WriteHeader(&tar.Header{Name: "payload", Size: 1, Mode: 0600})
		if err == nil {
			_, err = archive.Write([]byte("x"))
		}
		if err == nil {
			err = archive.Close()
		}
		if int64(out.Len()) > maximum {
			t.Fatal("archive exceeded capacity")
		}
		if maximum == 2048 {
			if err != nil || out.Len() != 2048 {
				t.Fatalf("exact boundary: %d %v", out.Len(), err)
			}
		} else if !errors.Is(err, ErrLimitExceeded) {
			t.Fatalf("expected capacity failure at %d: %v", maximum, err)
		}
	}
}
func TestEvidenceLimitParsingAndCancellation(t *testing.T) {
	for _, input := range []string{"", "0", "-1", "4GiB", "9007199254740992", "9223372036854775808"} {
		if _, err := ParseLimit(input); err == nil {
			t.Fatal("accepted", input)
		}
	}
	for _, input := range []string{"1", "4294967296", "8589934592", "9007199254740991"} {
		if _, err := ParseLimit(input); err != nil {
			t.Fatal(input, err)
		}
	}
	value, err := ResolveLimit(0)
	if err != nil || value != 4<<30 {
		t.Fatal(value, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var out bytes.Buffer
	w, _ := NewWriter(ctx, &out, 100)
	cancel()
	if _, err := w.Write([]byte("x")); !errors.Is(err, context.Canceled) || out.Len() != 0 {
		t.Fatal(err, out.Len())
	}
}
