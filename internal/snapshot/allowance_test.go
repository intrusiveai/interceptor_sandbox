package snapshot

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func TestByteAllowanceWire(t *testing.T) {
	type request struct {
		Maximum ByteAllowance `json:"maximum_committed_bytes,omitzero"`
	}
	for _, raw := range []string{`{}`, `{"maximum_committed_bytes":0}`, `{"maximum_committed_bytes":8}`} {
		var input request
		if err := json.Unmarshal([]byte(raw), &input); err != nil {
			t.Fatal(err)
		}
		output, err := json.Marshal(input)
		if err != nil || string(output) != raw {
			t.Fatalf("round trip %s: %s %v", raw, output, err)
		}
	}
	for _, raw := range []string{`null`, `-1`, `1.5`, `"5"`, `9223372036854775808`} {
		var input request
		if err := json.Unmarshal([]byte(`{"maximum_committed_bytes":`+raw+`}`), &input); err == nil {
			t.Fatal("accepted", raw)
		}
	}
}
func TestRemainingBytes(t *testing.T) {
	for _, tt := range []struct {
		name              string
		blob, count, used int64
		caller            ByteAllowance
		want              int64
	}{
		{"native", 10, 2, 3, ByteAllowance{}, 17},
		{"caller", 10, 2, 3, ByteAllowance{5, true}, 5},
		{"native tighter", 10, 2, 3, ByteAllowance{100, true}, 17},
		{"explicit zero", 10, 2, 3, ByteAllowance{0, true}, 0},
		{"exhausted", 10, 2, 20, ByteAllowance{}, 0},
		{"overused", 10, 2, 21, ByteAllowance{}, 0},
		{"saturation", math.MaxInt64, math.MaxInt64, 0, ByteAllowance{}, 16 << 40},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RemainingBytes(tt.blob, tt.count, tt.used, tt.caller)
			if got != tt.want || tt.want == 0 && !errors.Is(err, ErrBytesExhausted) || tt.want > 0 && err != nil {
				t.Fatalf("%d %v", got, err)
			}
		})
	}
}
