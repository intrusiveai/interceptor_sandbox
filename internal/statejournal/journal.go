package statejournal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
)

type Operation struct {
	Kind       string          `json:"kind"`
	Path       string          `json:"path,omitempty"`
	Mode       uint32          `json:"mode,omitempty"`
	Blob       string          `json:"blob,omitempty"`
	Size       int64           `json:"size,omitempty"`
	Target     string          `json:"target,omitempty"`
	Service    string          `json:"service,omitempty"`
	Collection string          `json:"collection,omitempty"`
	Key        string          `json:"key,omitempty"`
	Value      json.RawMessage `json:"value,omitempty"`
	ID         string          `json:"id,omitempty"`
}

type Transaction struct {
	SchemaVersion int         `json:"schema_version"`
	SessionID     string      `json:"session_id"`
	Seq           uint64      `json:"seq"`
	TxID          string      `json:"tx_id"`
	CommittedAt   time.Time   `json:"committed_at"`
	Reason        string      `json:"reason"`
	Operations    []Operation `json:"operations"`
	PreviousHash  string      `json:"previous_hash"`
	Hash          string      `json:"hash"`
}

type Writer struct {
	mu        sync.Mutex
	file      *os.File
	sessionID string
	nextSeq   uint64
	head      string
	now       func() time.Time
	newID     func() string
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
		return nil, fmt.Errorf("open state journal: %w", err)
	}
	writer := &Writer{
		file: file, sessionID: sessionID, nextSeq: 1, head: recording.ZeroHash,
		now: time.Now, newID: func() string { return fmt.Sprintf("tx-%d", time.Now().UnixNano()) },
	}
	if len(existing) > 0 {
		last := existing[len(existing)-1]
		if last.SessionID != sessionID {
			file.Close()
			return nil, fmt.Errorf("state journal belongs to session %s", last.SessionID)
		}
		writer.nextSeq, writer.head = last.Seq+1, last.Hash
	}
	return writer, nil
}

func (w *Writer) Append(reason string, operations []Operation) (Transaction, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if strings.TrimSpace(reason) == "" || len(operations) == 0 {
		return Transaction{}, errors.New("transaction reason and operations are required")
	}
	for index, operation := range operations {
		if err := ValidateOperation(operation); err != nil {
			return Transaction{}, fmt.Errorf("operation %d: %w", index, err)
		}
	}
	transaction := Transaction{
		SchemaVersion: 1, SessionID: w.sessionID, Seq: w.nextSeq, TxID: w.newID(),
		CommittedAt: w.now().UTC(), Reason: reason, Operations: operations, PreviousHash: w.head,
	}
	var err error
	transaction.Hash, err = transactionHash(transaction)
	if err != nil {
		return Transaction{}, err
	}
	line, err := json.Marshal(transaction)
	if err != nil {
		return Transaction{}, fmt.Errorf("encode transaction: %w", err)
	}
	if _, err := w.file.Write(append(line, '\n')); err != nil {
		return Transaction{}, fmt.Errorf("append transaction: %w", err)
	}
	// State mutations are write-ahead durable before the caller applies them.
	if err := w.file.Sync(); err != nil {
		return Transaction{}, fmt.Errorf("sync transaction: %w", err)
	}
	w.nextSeq, w.head = transaction.Seq+1, transaction.Hash
	return transaction, nil
}

func (w *Writer) Head() (uint64, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.nextSeq - 1, w.head
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.file.Sync(); err != nil {
		w.file.Close()
		return fmt.Errorf("sync state journal: %w", err)
	}
	return w.file.Close()
}

func VerifyFile(path string) ([]Transaction, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 32<<20)
	result := []Transaction{}
	previous := recording.ZeroHash
	var seq uint64 = 1
	for scanner.Scan() {
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		var transaction Transaction
		if err := decoder.Decode(&transaction); err != nil {
			return nil, fmt.Errorf("decode transaction %d: %w", seq, err)
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			return nil, fmt.Errorf("transaction %d contains trailing JSON", seq)
		}
		if transaction.SchemaVersion != 1 || transaction.Seq != seq || transaction.TxID == "" || transaction.Reason == "" || len(transaction.Operations) == 0 {
			return nil, fmt.Errorf("transaction %d has invalid required fields", seq)
		}
		if transaction.PreviousHash != previous {
			return nil, fmt.Errorf("transaction %d previous hash mismatch", seq)
		}
		for index, operation := range transaction.Operations {
			if err := ValidateOperation(operation); err != nil {
				return nil, fmt.Errorf("transaction %d operation %d: %w", seq, index, err)
			}
		}
		expected, err := transactionHash(transaction)
		if err != nil {
			return nil, err
		}
		if transaction.Hash != expected {
			return nil, fmt.Errorf("transaction %d hash mismatch", seq)
		}
		result = append(result, transaction)
		previous, seq = transaction.Hash, seq+1
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read state journal: %w", err)
	}
	return result, nil
}

func ValidateOperation(operation Operation) error {
	switch operation.Kind {
	case "file_upsert":
		if err := validMutablePath(operation.Path); err != nil || !digest.Valid(operation.Blob) || operation.Size < 0 {
			return errors.New("file_upsert requires a clean absolute path, blob digest, and nonnegative size")
		}
		return rejectFields(operation, "kind", "path", "mode", "blob", "size")
	case "file_delete":
		if err := validMutablePath(operation.Path); err != nil {
			return fmt.Errorf("%s requires a clean absolute path", operation.Kind)
		}
		return rejectFields(operation, "kind", "path")
	case "directory_ensure":
		if err := validMutablePath(operation.Path); err != nil {
			return fmt.Errorf("%s requires a clean absolute path", operation.Kind)
		}
		return rejectFields(operation, "kind", "path", "mode")
	case "directory_delete":
		if err := validMutablePath(operation.Path); err != nil {
			return fmt.Errorf("%s requires a clean absolute path", operation.Kind)
		}
		return rejectFields(operation, "kind", "path")
	case "symlink_set":
		if err := validMutablePath(operation.Path); err != nil || operation.Target == "" || filepath.IsAbs(operation.Target) {
			return errors.New("symlink_set requires a clean absolute path and relative target")
		}
		return rejectFields(operation, "kind", "path", "target")
	case "service_put":
		if operation.Service == "" || operation.Collection == "" || operation.Key == "" || len(operation.Value) == 0 || !json.Valid(operation.Value) {
			return errors.New("service_put requires service, collection, key, and JSON value")
		}
		return rejectFields(operation, "kind", "service", "collection", "key", "value")
	case "service_delete":
		if operation.Service == "" || operation.Collection == "" || operation.Key == "" {
			return errors.New("service_delete requires service, collection, and key")
		}
		return rejectFields(operation, "kind", "service", "collection", "key")
	case "injection_put":
		if operation.ID == "" || len(operation.Value) == 0 || !json.Valid(operation.Value) {
			return errors.New("injection_put requires id and JSON value")
		}
		return rejectFields(operation, "kind", "id", "value")
	case "artifact_put", "attempt_put":
		if operation.ID == "" || len(operation.Value) == 0 || !json.Valid(operation.Value) {
			return fmt.Errorf("%s requires id and JSON value", operation.Kind)
		}
		return rejectFields(operation, "kind", "id", "value")
	case "clock_set", "oracle_state_set", "budget_set", "idempotency_commit", "restored_from", "environment_materialized":
		if len(operation.Value) == 0 || !json.Valid(operation.Value) {
			return fmt.Errorf("%s requires a JSON value", operation.Kind)
		}
		return rejectFields(operation, "kind", "value")
	case "injection_delete":
		if operation.ID == "" {
			return errors.New("injection_delete requires id")
		}
		return rejectFields(operation, "kind", "id")
	default:
		return fmt.Errorf("unknown operation kind %q", operation.Kind)
	}
}

func rejectFields(operation Operation, allowed ...string) error {
	allowedSet := map[string]bool{}
	for _, field := range allowed {
		allowedSet[field] = true
	}
	present := map[string]bool{
		"kind": operation.Kind != "", "path": operation.Path != "", "mode": operation.Mode != 0,
		"blob": operation.Blob != "", "size": operation.Size != 0, "target": operation.Target != "",
		"service": operation.Service != "", "collection": operation.Collection != "", "key": operation.Key != "",
		"value": len(operation.Value) != 0, "id": operation.ID != "",
	}
	for field, exists := range present {
		if exists && !allowedSet[field] {
			return fmt.Errorf("%s contains unexpected field %s", operation.Kind, field)
		}
	}
	return nil
}

func transactionHash(transaction Transaction) (string, error) {
	transaction.Hash = ""
	encoded, err := json.Marshal(transaction)
	if err != nil {
		return "", fmt.Errorf("canonicalize transaction: %w", err)
	}
	return digest.Bytes(encoded), nil
}

func validMutablePath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return errors.New("invalid mutable path")
	}
	return nil
}
