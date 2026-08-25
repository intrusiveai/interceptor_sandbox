package recording

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
)

const ZeroHash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

type Channel string

const (
	Attack     Channel = "attack"
	Control    Channel = "control"
	Evaluation Channel = "evaluation"
)

type Provenance string

const (
	Application            Provenance = "application"
	EnvironmentSealed      Provenance = "environment_sealed"
	HarnessInjected        Provenance = "harness_injected"
	AttackerInfrastructure Provenance = "attacker_infrastructure"
	SandboxControl         Provenance = "sandbox_control"
)

type Attribution struct {
	ToolCallID string `json:"tool_call_id,omitempty"`
	PID        int    `json:"pid,omitempty"`
	Confidence string `json:"confidence,omitempty"`
}

type Draft struct {
	Channel     Channel
	Layer       string
	Kind        string
	Provenance  Provenance
	TurnID      string
	InjectionID string
	Attribution Attribution
	Semantic    any
	RawRef      string
}

type Event struct {
	SchemaVersion int             `json:"schema_version"`
	SessionID     string          `json:"session_id"`
	Seq           uint64          `json:"seq"`
	ObservedAt    time.Time       `json:"observed_at"`
	Channel       Channel         `json:"channel"`
	Layer         string          `json:"layer"`
	Kind          string          `json:"kind"`
	Provenance    Provenance      `json:"provenance"`
	TurnID        string          `json:"turn_id,omitempty"`
	InjectionID   string          `json:"injection_id,omitempty"`
	Attribution   Attribution     `json:"attribution,omitempty"`
	Semantic      json.RawMessage `json:"semantic"`
	RawRef        string          `json:"raw_ref,omitempty"`
	PreviousHash  string          `json:"previous_hash"`
	Hash          string          `json:"hash"`
}

type Writer struct {
	mu        sync.Mutex
	file      *os.File
	path      string
	sessionID string
	nextSeq   uint64
	head      string
	now       func() time.Time
}

func Open(path, sessionID string) (*Writer, error) {
	if sessionID == "" {
		return nil, errors.New("session ID is required")
	}
	existing, err := VerifyFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("open event log: %w", err)
	}
	writer := &Writer{file: file, path: path, sessionID: sessionID, nextSeq: 1, head: ZeroHash, now: time.Now}
	if len(existing) > 0 {
		last := existing[len(existing)-1]
		if last.SessionID != sessionID {
			file.Close()
			return nil, fmt.Errorf("event log belongs to session %s", last.SessionID)
		}
		writer.nextSeq, writer.head = last.Seq+1, last.Hash
	}
	return writer, nil
}

func (w *Writer) Append(draft Draft) (Event, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := validateDraft(draft); err != nil {
		return Event{}, err
	}
	semantic, err := json.Marshal(draft.Semantic)
	if err != nil {
		return Event{}, fmt.Errorf("encode event semantics: %w", err)
	}
	event := Event{
		SchemaVersion: 1, SessionID: w.sessionID, Seq: w.nextSeq, ObservedAt: w.now().UTC(),
		Channel: draft.Channel, Layer: draft.Layer, Kind: draft.Kind, Provenance: draft.Provenance,
		TurnID: draft.TurnID, InjectionID: draft.InjectionID, Attribution: draft.Attribution,
		Semantic: semantic, RawRef: draft.RawRef, PreviousHash: w.head,
	}
	event.Hash, err = eventHash(event)
	if err != nil {
		return Event{}, err
	}
	line, err := json.Marshal(event)
	if err != nil {
		return Event{}, fmt.Errorf("encode event: %w", err)
	}
	line = append(line, '\n')
	if _, err := w.file.Write(line); err != nil {
		return Event{}, fmt.Errorf("append event: %w", err)
	}
	w.nextSeq, w.head = event.Seq+1, event.Hash
	return event, nil
}

func (w *Writer) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.file.Sync(); err != nil {
		return fmt.Errorf("sync event log: %w", err)
	}
	return nil
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.file.Sync(); err != nil {
		w.file.Close()
		return fmt.Errorf("sync event log: %w", err)
	}
	return w.file.Close()
}

func (w *Writer) Head() (uint64, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.nextSeq - 1, w.head
}

func VerifyFile(path string) ([]Event, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 16<<20)
	result := []Event{}
	previous := ZeroHash
	var seq uint64 = 1
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		var event Event
		if err := decoder.Decode(&event); err != nil {
			return nil, fmt.Errorf("decode event %d: %w", seq, err)
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			return nil, fmt.Errorf("event %d contains trailing JSON", seq)
		}
		if event.SchemaVersion != 1 || event.Seq != seq {
			return nil, fmt.Errorf("event %d has invalid schema or sequence", seq)
		}
		if event.PreviousHash != previous {
			return nil, fmt.Errorf("event %d previous hash mismatch", seq)
		}
		if !validChannel(event.Channel) || !validProvenance(event.Provenance) || event.Layer == "" || event.Kind == "" || len(event.Semantic) == 0 {
			return nil, fmt.Errorf("event %d has invalid required fields", seq)
		}
		expected, err := eventHash(event)
		if err != nil {
			return nil, err
		}
		if event.Hash != expected {
			return nil, fmt.Errorf("event %d hash mismatch", seq)
		}
		result = append(result, event)
		previous, seq = event.Hash, seq+1
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read event log: %w", err)
	}
	return result, nil
}

func eventHash(event Event) (string, error) {
	event.Hash = ""
	encoded, err := json.Marshal(event)
	if err != nil {
		return "", fmt.Errorf("canonicalize event: %w", err)
	}
	return digest.Bytes(encoded), nil
}

func validateDraft(draft Draft) error {
	if !validChannel(draft.Channel) {
		return fmt.Errorf("invalid event channel %q", draft.Channel)
	}
	if !validProvenance(draft.Provenance) {
		return fmt.Errorf("invalid event provenance %q", draft.Provenance)
	}
	if draft.Layer == "" || draft.Kind == "" || draft.Semantic == nil {
		return errors.New("event layer, kind, and semantic data are required")
	}
	if draft.RawRef != "" && !digest.Valid(draft.RawRef) {
		return errors.New("event raw_ref must be a SHA-256 digest")
	}
	return nil
}

func validChannel(value Channel) bool {
	return value == Attack || value == Control || value == Evaluation
}

func validProvenance(value Provenance) bool {
	switch value {
	case Application, EnvironmentSealed, HarnessInjected, AttackerInfrastructure, SandboxControl:
		return true
	default:
		return false
	}
}

func References(event Event) ([]string, error) {
	seen := map[string]struct{}{}
	if event.RawRef != "" {
		seen[event.RawRef] = struct{}{}
	}
	var semantic any
	if err := json.Unmarshal(event.Semantic, &semantic); err != nil {
		return nil, err
	}
	collectReferences(semantic, seen)
	result := make([]string, 0, len(seen))
	for reference := range seen {
		result = append(result, reference)
	}
	sort.Strings(result)
	return result, nil
}

func collectReferences(value any, result map[string]struct{}) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if strings.HasSuffix(key, "_ref") {
				if reference, ok := child.(string); ok && digest.Valid(reference) {
					result[reference] = struct{}{}
				}
			}
			collectReferences(child, result)
		}
	case []any:
		for _, child := range typed {
			collectReferences(child, result)
		}
	}
}
