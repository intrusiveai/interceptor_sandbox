package snapshot

import (
	"bytes"
	"encoding/json"
	"errors"
)

// ByteAllowance distinguishes omission (native limit only) from an explicit zero
// (exhausted). The wire value is an optional, nonnegative int64, never null.
type ByteAllowance struct {
	Value int64
	Set   bool
}

func (a ByteAllowance) IsZero() bool                 { return !a.Set }
func (a ByteAllowance) MarshalJSON() ([]byte, error) { return json.Marshal(a.Value) }
func (a *ByteAllowance) UnmarshalJSON(raw []byte) error {
	var value int64
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("snapshot byte allowance must be a nonnegative integer")
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	if value < 0 {
		return errors.New("snapshot byte allowance must be a nonnegative integer")
	}
	a.Value, a.Set = value, true
	return nil
}

var ErrBytesExhausted = errors.New("snapshot byte allowance exhausted")

// RemainingBytes is evaluated with the snapshot lock held. Saturation preserves
// the existing native ceiling without overflowing multiplication or subtraction.
func RemainingBytes(maxBlob, maxSnapshots, used int64, caller ByteAllowance) (int64, error) {
	if maxBlob <= 0 || maxSnapshots <= 0 || used < 0 || caller.Set && caller.Value < 0 {
		return 0, errors.New("invalid snapshot byte limits or counters")
	}
	ceiling := int64(16 << 40)
	if maxBlob <= ceiling/maxSnapshots {
		ceiling = maxBlob * maxSnapshots
	}
	if used >= ceiling {
		return 0, ErrBytesExhausted
	}
	remaining := ceiling - used
	if caller.Set && caller.Value < remaining {
		remaining = caller.Value
	}
	if remaining == 0 {
		return 0, ErrBytesExhausted
	}
	return remaining, nil
}
