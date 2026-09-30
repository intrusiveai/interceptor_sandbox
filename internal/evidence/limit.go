// Package evidence defines the archive capacity shared by local API and CLI exports.
package evidence

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
)

const DefaultMaxBytes int64 = 4 << 30
const MaximumConfigBytes int64 = 1<<53 - 1

var ErrLimitExceeded = errors.New("evidence_limit_exceeded")

func ParseLimit(value string) (int64, error) {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 || n > MaximumConfigBytes {
		return 0, errors.New("evidence-max-bytes must be an integer byte count from 1 through 9007199254740991")
	}
	return n, nil
}

// ResolveLimit lets internal callers omit configuration; explicit CLI zero is invalid.
func ResolveLimit(n int64) (int64, error) {
	if n == 0 {
		return DefaultMaxBytes, nil
	}
	if n < 0 || n > MaximumConfigBytes {
		return 0, errors.New("invalid evidence byte limit")
	}
	return n, nil
}

type Writer struct {
	ctx       context.Context
	dst       io.Writer
	remaining int64
	maximum   int64
}

func NewWriter(ctx context.Context, dst io.Writer, maximum int64) (*Writer, error) {
	maximum, err := ResolveLimit(maximum)
	if err != nil {
		return nil, err
	}
	return &Writer{ctx: ctx, dst: dst, remaining: maximum, maximum: maximum}, nil
}

// Check refuses an oversized archive member before its content is copied.
func (w *Writer) Check(size int64) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if size < 0 {
		return errors.New("invalid evidence size")
	}
	if size > w.remaining {
		return fmt.Errorf("%w: maximum_bytes=%d; retained source evidence is unchanged", ErrLimitExceeded, w.maximum)
	}
	return nil
}
func (w *Writer) Write(p []byte) (int, error) {
	if err := w.Check(int64(len(p))); err != nil {
		return 0, err
	}
	n, err := w.dst.Write(p)
	w.remaining -= int64(n)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}
