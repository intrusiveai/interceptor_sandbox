package injection

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

func modelDefinition() Definition {
	return Definition{ID: "response", Surface: "model_response", Mode: "simulation", Scope: "once", Enabled: true, Selector: Selector{ModelCodec: "openai_chat_completions", CallOrdinal: "first"}, Placement: Placement{Operation: "replace", Pointer: "/name"}, Payload: json.RawMessage(`"lookup_order("`)}
}
func modelManager(t *testing.T) (*Manager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.jsonl")
	j, err := statejournal.Open(path, "sess-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	return NewManager(j, nil), path
}
func selectResponse(t *testing.T, m *Manager) ModelResponseSelection {
	t.Helper()
	s, err := m.SelectModelResponse("openai_chat_completions", "turn-1", []byte(`{"model":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestModelResponseLifetimesAndReplay(t *testing.T) {
	m, path := modelManager(t)
	d := modelDefinition()
	d.Selector.CallOrdinal = "2"
	d.Selector.RequestBodyPointer = "/model"
	d.Selector.RequestBodyEqual = json.RawMessage(`"test"`)
	d.Selector.TurnID = "turn-1"
	if err := m.Arm(d, ""); err != nil {
		t.Fatal(err)
	}
	s := selectResponse(t, m)
	body := []byte(`{"name":"lookup_order"}`)
	if out, r, err := m.RewriteModelResponse(context.Background(), s, []byte("not JSON"), 1024); err != nil || len(r) > 0 || string(out) != "not JSON" {
		t.Fatalf("non JSON: %s %v %v", out, r, err)
	}
	for _, tc := range []struct{ codec, turn, request string }{{"anthropic_messages", "turn-1", `{"model":"test"}`}, {d.Selector.ModelCodec, "", `{"model":"test"}`}, {d.Selector.ModelCodec, "turn-1", `{"model":"other"}`}} {
		miss, _ := m.SelectModelResponse(tc.codec, tc.turn, []byte(tc.request))
		if _, r, err := m.RewriteModelResponse(context.Background(), miss, body, 1024); err != nil || len(r) > 0 {
			t.Fatal(r, err)
		}
	}
	if _, r, err := m.RewriteModelResponse(context.Background(), s, body, 1024); err != nil || len(r) > 0 {
		t.Fatal(r, err)
	}
	if m.Evaluation()[0].ResponseCandidates != 1 {
		t.Fatal(m.Evaluation())
	}
	// Replay the checkpoint's ordinal before the second response.
	transactions, err := statejournal.VerifyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	restored, _ := modelManager(t)
	for _, tx := range transactions {
		for _, op := range tx.Operations {
			if err := restored.ApplyJournal(op); err != nil {
				t.Fatal(err)
			}
		}
	}
	rs := selectResponse(t, restored)
	out, r, err := restored.RewriteModelResponse(context.Background(), rs, body, 1024)
	if err != nil || len(r) != 1 || !r[0].Applied || !bytes.Contains(out, []byte("lookup_order(")) {
		t.Fatal(string(out), r, err)
	}
	if err := restored.RecordModelResponseWrite(context.Background(), rs, r, len(out), len(out), nil); err != nil {
		t.Fatal(err)
	}
	if got := restored.Evaluation()[0]; got.Delivery != "response_written" || !got.Consumed || got.Armed {
		t.Fatal(got)
	}
	if confirmed, err := restored.ConfirmDelivery(out, []string{d.ID}); err != nil || len(confirmed) > 0 {
		t.Fatal(confirmed, err)
	}
	if _, r, err := restored.RewriteModelResponse(context.Background(), rs, body, 1024); err != nil || len(r) > 0 {
		t.Fatal(r, err)
	}
}
func TestModelResponseAdmissionAndConcurrentOnce(t *testing.T) {
	m, _ := modelManager(t)
	before := selectResponse(t, m)
	d := modelDefinition()
	if err := m.Arm(d, ""); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"name":"safe"}`)
	if _, r, err := m.RewriteModelResponse(context.Background(), before, body, 1024); err != nil || len(r) > 0 {
		t.Fatal(r, err)
	}
	old := selectResponse(t, m)
	if err := m.Delete(d.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Arm(d, ""); err != nil {
		t.Fatal(err)
	}
	if _, r, err := m.RewriteModelResponse(context.Background(), old, body, 1024); err != nil || len(r) > 0 {
		t.Fatal(r, err)
	}
	s := selectResponse(t, m)
	var wg sync.WaitGroup
	applied := make(chan bool, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, r, err := m.RewriteModelResponse(context.Background(), s, body, 1024)
			if err != nil {
				t.Error(err)
			}
			applied <- len(r) > 0
		}()
	}
	wg.Wait()
	close(applied)
	count := 0
	for ok := range applied {
		if ok {
			count++
		}
	}
	if count != 1 {
		t.Fatal(count)
	}
}
func TestModelResponsePlacementsAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name                string
		placement           Placement
		payload, body, want string
		applied             bool
	}{
		{"missing", Placement{Operation: "replace", Pointer: "/absent"}, `"x"`, `{"name":"safe"}`, `{"name":"safe"}`, false},
		{"root scalar", Placement{Operation: "replace"}, `17`, `{}`, `17`, true},
		{"fake call", Placement{Operation: "insert_array", Pointer: "/calls"}, `{"name":"fake(","arguments":5}`, `{"calls":[]}`, `{"calls":[{"arguments":5,"name":"fake("}]}`, true},
		{"malformed arguments", Placement{Operation: "replace", Pointer: "/arguments"}, `"{"`, `{"arguments":"{}"}`, `{"arguments":"{"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := modelManager(t)
			d := modelDefinition()
			d.Placement = tc.placement
			d.Payload = json.RawMessage(tc.payload)
			if err := m.Arm(d, ""); err != nil {
				t.Fatal(err)
			}
			out, r, err := m.RewriteModelResponse(context.Background(), selectResponse(t, m), []byte(tc.body), 1024)
			if err != nil || len(r) != 1 || r[0].Applied != tc.applied || string(out) != tc.want || !m.Evaluation()[0].Consumed {
				t.Fatal(string(out), r, err, m.Evaluation())
			}
		})
	}
	m, _ := modelManager(t)
	d := modelDefinition()
	if err := m.Arm(d, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.RewriteModelResponse(context.Background(), selectResponse(t, m), []byte(`{"name":"x"}`), 1); err == nil {
		t.Fatal("accepted oversized response")
	}
}
func TestModelResponseValidation(t *testing.T) {
	for _, modify := range []func(*Definition){func(d *Definition) { d.Scope = "next_turn" }, func(d *Definition) { d.Mode = "fidelity" }, func(d *Definition) { d.Selector.ModelCodec = "openai_embeddings" }, func(d *Definition) { d.Selector.Service = "svc" }, func(d *Definition) { d.Selector.ToolName = "tool" }, func(d *Definition) { d.Selector.Path = "/" }, func(d *Definition) { d.Selector.ArgumentsPointer = "/name" }, func(d *Definition) { d.Selector.CallOrdinal = "0" }, func(d *Definition) { d.Selector.FileNamespace = "state" }} {
		d := modelDefinition()
		modify(&d)
		if Validate(d) == nil {
			t.Fatalf("accepted invalid definition: %+v", d)
		}
	}
}

func TestModelResponseStandingOrderAndFrozenPredicates(t *testing.T) {
	m, _ := modelManager(t)
	for _, id := range []string{"b", "a"} {
		d := modelDefinition()
		d.ID = id
		d.Scope = "standing"
		d.Selector.CallOrdinal = "every"
		d.Placement = Placement{Operation: "append_text", Pointer: "/name"}
		d.Payload = json.RawMessage(`"` + id + `"`)
		if err := m.Arm(d, ""); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		out, r, err := m.RewriteModelResponse(context.Background(), selectResponse(t, m), []byte(`{"name":"safe"}`), 1024)
		if err != nil || len(r) != 2 || string(out) != `{"name":"safeab"}` {
			t.Fatal(string(out), r, err)
		}
	}
	s := selectResponse(t, m)
	d := modelDefinition()
	d.ID = "a"
	d.Enabled = false
	if err := m.Arm(d, ""); err != nil {
		t.Fatal(err)
	}
	out, r, err := m.RewriteModelResponse(context.Background(), s, []byte(`{"name":"safe"}`), 1024)
	if err != nil || len(r) != 1 || string(out) != `{"name":"safeb"}` {
		t.Fatal(string(out), r, err)
	}
}
