package blobstore

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestPutDeduplicatesAndVerifies(t *testing.T) {
	store, err := Open(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Put(context.Background(), strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put(context.Background(), strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("deduplicated object mismatch: %#v %#v", first, second)
	}
	reader, object, err := store.Open(first.Digest)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(reader)
	reader.Close()
	if string(data) != "hello" || object.Size != 5 {
		t.Fatalf("unexpected blob: %q %#v", data, object)
	}
}

func TestPutEnforcesLimitAndDigestPaths(t *testing.T) {
	store, err := Open(t.TempDir(), 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), strings.NewReader("hello")); err == nil {
		t.Fatal("expected size limit error")
	}
	if _, _, err := store.Open("../../etc/passwd"); err == nil {
		t.Fatal("expected invalid digest rejection")
	}
}
