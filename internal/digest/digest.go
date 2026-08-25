package digest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"regexp"
)

var pattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func Valid(value string) bool { return pattern.MatchString(value) }

func Bytes(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func Reader(reader io.Reader) (string, int64, error) {
	h := sha256.New()
	size, err := io.Copy(h, reader)
	if err != nil {
		return "", size, err
	}
	return FromHash(h), size, nil
}

func FromHash(h hash.Hash) string { return "sha256:" + hex.EncodeToString(h.Sum(nil)) }

func Hex(value string) (string, error) {
	if !Valid(value) {
		return "", fmt.Errorf("invalid SHA-256 digest %q", value)
	}
	return value[len("sha256:"):], nil
}
